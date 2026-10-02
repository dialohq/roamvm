package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/dialohq/roamvm/internal/controller"
	"github.com/dialohq/roamvm/internal/disk"
	"github.com/dialohq/roamvm/internal/fileio"
	"github.com/dialohq/roamvm/internal/images"
	"golang.org/x/sys/unix"
	core "k8s.io/api/core/v1"
)

var errDiskBusy = errors.New("waiting for the previous runner or checkpoint worker to release the local disk")

func lockDisk(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, "runner.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errDiskBusy
		}
		return nil, err
	}
	return f, nil
}

// stopLocal is called only after QEMU exits, while the caller holds runner.lock.
// No object-store operation is needed to acknowledge a local stop.
func (s *Server) stopLocal(ctx context.Context, pod *core.Pod, vm *api.VirtualMachine) error {
	p, err := s.load(string(pod.UID))
	if err != nil {
		return err
	}
	if p.Session.Head.VMID != string(vm.UID) || p.Session.Head.Owner != string(pod.UID) || p.Session.Head.Node != s.Node {
		return errors.New("local stop identity mismatch")
	}
	f, err := os.OpenFile(filepath.Join(p.Dir, "overlay.qcow2"), os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err = fileio.SyncClose(f, nil); err != nil {
		return err
	}
	p.Stopped = true
	if err = s.save(p); err != nil {
		return err
	}
	// Persist the directory entries as well as the disk and session contents.
	for _, path := range []string{p.Dir, filepath.Dir(p.Dir), s.Root} {
		dir, err := os.Open(path)
		if err != nil {
			return err
		}
		if err = fileio.SyncClose(dir, nil); err != nil {
			return err
		}
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[controller.LocalStopAnnotation] = string(pod.UID)
	message := "Guest stopped; local disk retained, background checkpoint pending"
	if reason := pod.Annotations[controller.ExitAnnotation]; reason != "" {
		message = reason + "; " + message
	}
	return s.annotate(ctx, pod, "Stopped", message, nil)
}

func (s *Server) stoppedLocal(owner string, vm *api.VirtualMachine) (Prepared, error) {
	p, err := s.load(owner)
	if err != nil {
		return p, err
	}
	local := vm.Status.Local
	if local == nil || local.Owner != owner || local.NodeName != s.Node || !p.Stopped ||
		p.PodUID != owner || p.Session.Head.Owner != owner || p.Session.Head.Node != s.Node ||
		p.Session.Head.VMID != string(vm.UID) || p.Session.Head.Base != vm.Spec.Image ||
		p.Dir != filepath.Join(s.Root, "running", string(vm.UID)) {
		return p, errors.New("local checkpoint identity mismatch")
	}
	return p, nil
}

func (s *Server) resumeLocal(ctx context.Context, pod *core.Pod, vm *api.VirtualMachine, base images.Base, spec api.VirtualMachineSpec) (*Prepared, error) {
	owner := pod.Annotations[controller.LocalOwnerAnnotation]
	p, err := s.stoppedLocal(owner, vm)
	if err != nil {
		return nil, err
	}
	if controller.WorkingClaim(pod) != vm.Status.Local.ClaimName {
		return nil, errors.New("local checkpoint claim mismatch")
	}
	lock, err := lockDisk(p.Dir)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	// Transfer ownership before any disk mutation. A delayed old worker fails
	// its epoch check even if it starts after this lock is released.
	session, err := s.State.ResumeLocal(ctx, p.Session, string(pod.UID), s.Node)
	if err != nil {
		return nil, err
	}
	if err = disk.Rebase(ctx, base, filepath.Join(p.Dir, "overlay.qcow2")); err != nil {
		return nil, err
	}
	prepared := Prepared{Session: session, Base: base, Dir: p.Dir, Spec: spec, PodUID: string(pod.UID)}
	if err = s.save(prepared); err != nil {
		return nil, err
	}
	return &prepared, nil
}

// CheckpointLocal runs in a separate, restartable worker Pod. The lock spans
// validation, upload, verification and CAS. Cancellation releases it before a
// resumed guest can write. The retained overlay is never deleted here.
func (s *Server) CheckpointLocal(ctx context.Context) error {
	pod, vm, err := s.incarnation(ctx)
	if err != nil {
		return err
	}
	if pod.Labels[controller.WorkerLabel] != "true" || pod.DeletionTimestamp != nil {
		return errors.New("not an active checkpoint worker")
	}
	p, err := s.stoppedLocal(pod.Annotations[controller.LocalOwnerAnnotation], vm)
	if err != nil {
		return err
	}
	if controller.WorkingClaim(pod) != vm.Status.Local.ClaimName {
		return errors.New("checkpoint worker claim mismatch")
	}
	lock, err := lockDisk(p.Dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	current, err := s.State.Read(ctx, string(vm.UID))
	if err != nil {
		return err
	}
	if current.Head.Epoch != p.Session.Head.Epoch {
		return errors.New("checkpoint worker superseded by a newer epoch")
	}
	if current.Head.State == "Stopped" && current.Head.Checkpoint != nil {
		// Reconcile a lost commit response or a restarted worker after CAS.
		err = s.State.Prune(ctx, current)
	} else {
		if err = s.State.Check(ctx, p.Session); err != nil {
			return err
		}
		if err = s.annotate(ctx, pod, "Uploading", "", nil); err != nil {
			return err
		}
		path := filepath.Join(p.Dir, "overlay.qcow2")
		if err = disk.Check(ctx, path); err == nil {
			current, err = s.State.Commit(ctx, p.Session, path)
		}
	}
	if err != nil {
		_ = s.annotate(ctx, pod, "Error", fmt.Sprintf("local disk retained; checkpoint retry required: %v", err), nil)
		return err
	}
	pod.Annotations[controller.CheckpointReadyAnnotation] = "true"
	return s.annotate(ctx, pod, "Stopped", "Checkpoint verified in object storage", current.Head.Checkpoint)
}

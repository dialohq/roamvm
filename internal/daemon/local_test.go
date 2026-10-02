package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/dialohq/roamvm/internal/controller"
	"github.com/dialohq/roamvm/internal/disk"
	"github.com/dialohq/roamvm/internal/images"
	"github.com/dialohq/roamvm/internal/state"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type localObjects struct {
	*overlayStore
	gets  atomic.Int32
	block chan struct{}
}

func (s *localObjects) Get(ctx context.Context, key string) (state.Object, error) {
	s.gets.Add(1)
	return s.overlayStore.Get(ctx, key)
}

func (s *localObjects) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, match string) (state.Object, error) {
	if s.block != nil {
		close(s.block)
		<-ctx.Done()
		return state.Object{}, ctx.Err()
	}
	return s.overlayStore.Put(ctx, key, body, size, match)
}

func TestLocalStopUploadAndRestart(t *testing.T) {
	if _, err := exec.LookPath("qemu-io"); err != nil {
		t.Skip("qemu-io not installed")
	}
	for _, uploaded := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending-upload", true: "verified-upload"}[uploaded], func(t *testing.T) {
			ctx := t.Context()
			scheme := runtime.NewScheme()
			require.NoError(t, core.AddToScheme(scheme))
			require.NoError(t, api.AddToScheme(scheme))
			vm := &api.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "default", UID: "vm-uid"}, Spec: api.VirtualMachineSpec{PowerState: "Running", Image: "base"}}
			pod := &core.Pod{ObjectMeta: metav1.ObjectMeta{Name: "runner", Namespace: vm.Namespace, UID: "old-owner", Annotations: map[string]string{}}, Spec: core.PodSpec{NodeName: "node-a", Volumes: []core.Volume{{Name: "working", VolumeSource: core.VolumeSource{PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "cache"}}}}}}
			require.NoError(t, controllerutil.SetControllerReference(vm, pod, scheme))
			c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(vm).WithObjects(vm, pod).Build()
			s := &Server{Client: c, Root: t.TempDir(), Node: "node-a"}
			base := images.Base{Dir: t.TempDir(), Manifest: images.Manifest{Format: "raw"}}
			require.NoError(t, os.WriteFile(base.Disk(), bytes.Repeat([]byte{0x55}, 4<<20), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(base.Dir, "manifest.json"), []byte(`{"format":"raw"}`), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(base.Dir, "vmlinux"), []byte("kernel"), 0o600))
			s.BaseDir = base.Dir
			dir := filepath.Join(s.Root, "running", string(vm.UID))
			require.NoError(t, os.MkdirAll(dir, 0o700))
			path := filepath.Join(dir, "overlay.qcow2")
			require.NoError(t, disk.Create(ctx, base, path))
			out, err := exec.Command("qemu-io", "-f", "qcow2", "-c", "write -P 0x42 0 64k", "-c", "write -z 128k 64k", path).CombinedOutput()
			require.NoError(t, err, "%s", out)
			objects := &localObjects{overlayStore: &overlayStore{t: t, path: path}}
			s.State = state.Manager{Store: &state.Kubernetes{Client: c, Namespace: "default", Objects: objects}}
			session, err := s.State.Acquire(ctx, string(vm.UID), vm.Spec.Image, string(pod.UID), s.Node)
			require.NoError(t, err)
			require.NoError(t, s.save(Prepared{Session: session, Base: base, Dir: dir, PodUID: string(pod.UID)}))
			// A stop must work even with no object-store implementation at all.
			backend := s.State.Store
			s.State.Store = nil
			require.NoError(t, s.finish(ctx, pod, vm))
			s.State.Store = backend
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(pod), pod))
			require.Equal(t, "Stopped", pod.Annotations[controller.Phase])
			require.FileExists(t, path)
			vm.Status.Local = &api.LocalCheckpoint{ClaimName: "cache", NodeName: s.Node, Owner: string(pod.UID)}
			require.NoError(t, c.Status().Update(ctx, vm))
			worker := pod.DeepCopy()
			worker.Name, worker.UID, worker.ResourceVersion = "worker", "worker-uid", ""
			worker.Labels = map[string]string{controller.WorkerLabel: "true"}
			worker.Annotations = map[string]string{controller.LocalOwnerAnnotation: string(pod.UID)}
			require.NoError(t, c.Create(ctx, worker))
			w := &Server{Client: c, Root: s.Root, Node: s.Node, State: s.State, Pod: client.ObjectKeyFromObject(worker), PodUID: string(worker.UID), VMUID: string(vm.UID)}
			if uploaded {
				require.NoError(t, w.CheckpointLocal(ctx))
				require.NoError(t, w.CheckpointLocal(ctx), "lost response must reconcile without upload")
			} else {
				objects.block = make(chan struct{})
				uploadCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- w.CheckpointLocal(uploadCtx) }()
				select {
				case <-objects.block:
				case <-time.After(5 * time.Second):
					t.Fatal("upload did not start")
				}
				_, err := lockDisk(dir)
				require.ErrorIs(t, err, errDiskBusy, "cannot make upload source writable")
				cancel()
				select {
				case err := <-done:
					require.True(t, errors.Is(err, context.Canceled))
				case <-time.After(5 * time.Second):
					t.Fatal("upload did not cancel")
				}
			}
			next := pod.DeepCopy()
			next.Name, next.UID, next.ResourceVersion = "next", "new-owner", ""
			b, err := json.Marshal(vm.Spec)
			require.NoError(t, err)
			next.Annotations = map[string]string{controller.LocalOwnerAnnotation: string(pod.UID), controller.SpecAnnotation: string(b)}
			require.NoError(t, c.Create(ctx, next))
			gets := objects.gets.Load()
			prepared, err := s.prepare(ctx, next, vm)
			require.NoError(t, err)
			require.Equal(t, session.Head.Epoch+1, prepared.Session.Head.Epoch)
			require.Equal(t, gets, objects.gets.Load(), "local resume must not download any checkpoint bytes")
			require.NoError(t, s.State.Check(ctx, prepared.Session))
			require.Error(t, w.CheckpointLocal(ctx), "stale worker must not upload the writable disk")
			out, err = exec.Command("qemu-io", "-f", "qcow2", "-c", "read -P 0x42 0 64k", "-c", "read -P 0 128k 64k", "-c", "read -P 0x55 256k 64k", path).CombinedOutput()
			require.NoError(t, err, "%s", out)
		})
	}
}

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/dialohq/roamvm/internal/controller"
	"github.com/dialohq/roamvm/internal/disk"
	"github.com/dialohq/roamvm/internal/fileio"
	"github.com/dialohq/roamvm/internal/images"
	"github.com/dialohq/roamvm/internal/state"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Request struct {
	Phase       string `json:"phase,omitempty"`
	Message     string `json:"message,omitempty"`
	DiskSize    int64  `json:"diskSize,omitempty"`
	ResizeError string `json:"resizeError,omitempty"`
}
type Prepared struct {
	Session state.Session
	Base    images.Base
	Dir     string
	Spec    api.VirtualMachineSpec
	PodUID  string
}
type Response struct {
	Prepared    *Prepared `json:"prepared,omitempty"`
	DiskSize    int64     `json:"diskSize,omitempty"`
	ResizeError string    `json:"resizeError,omitempty"`
	Stop        bool      `json:"stop,omitempty"`
	Error       string    `json:"error,omitempty"`
}
type Server struct {
	Client                    client.Client
	Pod                       types.NamespacedName
	Node, PodUID, VMUID, Root string
	State                     state.Manager
	BaseDir                   string
	mu                        sync.Mutex
}

func (s *Server) Serve(ctx context.Context, socket string) error {
	if err := os.MkdirAll(filepath.Dir(socket), 0o755); err != nil {
		return err
	}
	// A restarted sidecar replaces only its own stale socket.
	_ = os.Remove(socket)
	l, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	if err = os.Chmod(socket, 0o600); err != nil {
		return err
	}
	mux := http.NewServeMux()
	for _, action := range []string{"prepare", "heartbeat", "status", "finish"} {
		mux.HandleFunc("POST /"+action, s.handler(action))
	}
	parent := ctx
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	if s.Pod.Name != "" {
		go s.watchRunner(ctx, cancel)
	} else {
		go func() {
			select {
			case <-parent.Done():
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	err = srv.Serve(l)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) handler(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req Request
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&req); err != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		pod, vm, err := s.incarnation(r.Context())
		if err != nil {
			w.WriteHeader(403)
			_ = json.NewEncoder(w).Encode(Response{Error: err.Error()})
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		var response Response
		switch action {
		case "prepare":
			response.Prepared, err = s.prepare(r.Context(), pod, vm)
		case "heartbeat":
			response.Stop = vm.Spec.PowerState == "Stopped" || vm.DeletionTimestamp != nil ||
				pod.DeletionTimestamp != nil ||
				pod.Annotations[controller.Stop] == "true"
			if response.Stop {
				break
			} // Stopping never depends on S3 availability.
			p, e := s.load(string(pod.UID))
			if e != nil {
				err = e
			} else {
				err = s.State.Check(r.Context(), p.Session)
				if errors.Is(err, state.ErrOwned) {
					response.Stop = true
				}
			}
			if err == nil && !response.Stop {
				response.DiskSize, response.ResizeError = s.resizeTarget(r.Context(), pod, vm)
			}
		case "status":
			if req.Phase != "Running" && req.Phase != "Stopping" && req.Phase != "Checkpointing" &&
				req.Phase != "Error" {
				err = errors.New("invalid runtime phase")
			} else {
				pod.Annotations[controller.DiskSizeAnnotation] = strconv.FormatInt(req.DiskSize, 10)
				pod.Annotations[controller.ResizeAnnotation] = req.ResizeError
				err = s.annotate(r.Context(), pod, req.Phase, req.Message, nil)
			}
		case "finish":
			err = s.finish(r.Context(), pod, vm)
		}
		if err != nil {
			w.WriteHeader(409)
			response.Error = err.Error()
		}
		_ = json.NewEncoder(w).Encode(response)
	}
}

// incarnation is selected only by the sidecar's downward API identity. The
// private Pod socket grants access; a request cannot select another runner.
func (s *Server) incarnation(ctx context.Context) (*core.Pod, *api.VirtualMachine, error) {
	var pod core.Pod
	if err := s.Client.Get(ctx, s.Pod, &pod); err != nil {
		return nil, nil, err
	}
	if string(pod.UID) != s.PodUID || pod.Spec.NodeName != s.Node {
		return nil, nil, errors.New("runner incarnation changed")
	}
	owner := metav1.GetControllerOf(&pod)
	if owner == nil || owner.Kind != "VirtualMachine" || owner.APIVersion != api.GroupVersion.String() || string(owner.UID) != s.VMUID {
		return nil, nil, errors.New("not a VM runner")
	}
	var vm api.VirtualMachine
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: owner.Name}, &vm); err != nil {
		return nil, nil, err
	}
	if owner.UID != vm.UID {
		return nil, nil, errors.New("VM identity mismatch")
	}
	return &pod, &vm, nil
}
func (s *Server) meta(uid string) string { return filepath.Join(s.Root, "sessions", uid+".json") }
func (s *Server) load(uid string) (Prepared, error) {
	var p Prepared
	b, err := os.ReadFile(s.meta(uid))
	if err == nil {
		err = json.Unmarshal(b, &p)
	}
	return p, err
}

func (s *Server) save(p Prepared) error {
	path := s.meta(p.PodUID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path+".partial", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err = fileio.SyncClose(f, err); err != nil {
		return err
	}
	return os.Rename(path+".partial", path)
}

func (s *Server) prepare(ctx context.Context, pod *core.Pod, vm *api.VirtualMachine) (*Prepared, error) {
	started := time.Now()
	if vm.Spec.PowerState != "Running" || vm.DeletionTimestamp != nil || pod.DeletionTimestamp != nil {
		return nil, errors.New("VM no longer requests a start")
	}
	if p, err := s.load(string(pod.UID)); err == nil {
		if err = s.State.Check(ctx, p.Session); err != nil {
			return nil, err
		}
		return &p, nil
	}
	// Use the exact configuration for which this Pod reserved resources. Changes
	// to the VM spec while it is queued or running apply to the next incarnation.
	spec, err := bootSpec(vm, pod)
	if err != nil {
		return nil, err
	}
	if err := s.annotate(ctx, pod, "Restoring", "", nil); err != nil {
		return nil, err
	}
	base, err := images.Open(ctx, s.BaseDir)
	if err != nil {
		return nil, err
	}
	log.Printf("startup pod=%s stage=base elapsed=%s", pod.Name, time.Since(started))
	session, err := s.State.Acquire(ctx, string(vm.UID), vm.Spec.Image, string(pod.UID), s.Node)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(s.Root, "running", string(vm.UID))
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	overlay := filepath.Join(dir, "overlay.qcow2")
	// If preparation failed earlier, its partial file is not a valid runtime.
	// Once save() succeeds, retries return above and never overwrite working data.
	_ = os.Remove(overlay + ".partial")
	if session.Head.Checkpoint != nil {
		if err = s.State.Restore(ctx, session, overlay); err != nil {
			return nil, err
		}
		if err = disk.Rebase(ctx, base, overlay); err != nil {
			return nil, err
		}
	} else {
		_ = os.Remove(overlay)
		if err = disk.Create(ctx, base, overlay); err != nil {
			return nil, err
		}
	}
	prepared := Prepared{session, base, dir, spec, string(pod.UID)}
	if err = s.save(prepared); err != nil {
		return nil, err
	}
	log.Printf("startup pod=%s stage=prepared elapsed=%s", pod.Name, time.Since(started))
	return &prepared, nil
}

func bootSpec(vm *api.VirtualMachine, pod *core.Pod) (api.VirtualMachineSpec, error) {
	var spec api.VirtualMachineSpec
	if err := json.Unmarshal([]byte(pod.Annotations[controller.SpecAnnotation]), &spec); err != nil {
		return spec, fmt.Errorf("invalid runner boot configuration: %w", err)
	}
	if spec.Image != vm.Spec.Image {
		return spec, errors.New("runner base identity changed")
	}
	return spec, nil
}

func (s *Server) finish(ctx context.Context, pod *core.Pod, vm *api.VirtualMachine) error {
	p, err := s.load(string(pod.UID))
	if err != nil {
		return err
	}
	current, err := s.State.Read(ctx, string(vm.UID))
	if err != nil {
		return err
	}
	if current.Head.State == "Stopped" && current.Head.Epoch == p.Session.Head.Epoch && current.Head.Checkpoint != nil {
		if err = s.State.Prune(ctx, current); err != nil {
			return err
		}
		return s.complete(ctx, pod, p, current)
	}
	if err = s.State.Check(ctx, p.Session); err != nil {
		return err
	}
	if err = s.annotate(ctx, pod, "Checkpointing", "", nil); err != nil {
		return err
	}
	checkpoint, err := disk.Compact(ctx, p.Base, filepath.Join(p.Dir, "overlay.qcow2"))
	if err != nil {
		return err
	}
	committed, err := s.State.Commit(ctx, p.Session, checkpoint)
	if err != nil {
		return err
	}
	return s.complete(ctx, pod, p, committed)
}

func (s *Server) complete(ctx context.Context, pod *core.Pod, p Prepared, committed state.Session) error {
	message := ""
	if reason := pod.Annotations[controller.ExitAnnotation]; reason != "" {
		message = reason + "; crash-consistent working disk checkpointed"
	}
	if err := s.annotate(ctx, pod, "Stopped", message, committed.Head.Checkpoint); err != nil {
		return err
	}
	// Cleanup follows both the durable commit and its Kubernetes projection.
	// The runner still holds its lock fd, so remove files rather than its directory.
	for _, name := range []string{"overlay.qcow2", "checkpoint.qcow2", "checkpoint.qcow2.partial"} {
		if err := os.Remove(filepath.Join(p.Dir, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (s *Server) annotate(ctx context.Context, pod *core.Pod, phase, message string, cp *api.Checkpoint) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current core.Pod
		if err := s.Client.Get(ctx, client.ObjectKeyFromObject(pod), &current); err != nil {
			return err
		}
		if current.UID != pod.UID {
			return errors.New("runner was replaced")
		}
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		for _, key := range []string{controller.DiskSizeAnnotation, controller.ResizeAnnotation} {
			if value, ok := pod.Annotations[key]; ok {
				current.Annotations[key] = value
			}
		}
		current.Annotations[controller.Phase] = phase
		current.Annotations[controller.Message] = message
		if cp != nil {
			b, err := json.Marshal(cp)
			if err != nil {
				return err
			}
			current.Annotations[controller.CheckpointAnnotation] = string(b)
		}
		return s.Client.Update(ctx, &current)
	})
}

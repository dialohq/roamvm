package daemon

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/dialohq/roamvm/internal/controller"
	"github.com/dialohq/roamvm/internal/disk"
	"github.com/dialohq/roamvm/internal/fileio"
	"github.com/dialohq/roamvm/internal/images"
	"github.com/dialohq/roamvm/internal/state"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Request struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	UID       string `json:"uid"`
	Token     string `json:"token"`
	Phase     string `json:"phase,omitempty"`
	Message   string `json:"message,omitempty"`
}
type Prepared struct {
	Session state.Session
	Base    images.Base
	Dir     string
	Spec    api.VirtualMachineSpec
	PodUID  string
}
type Response struct {
	Prepared *Prepared `json:"prepared,omitempty"`
	Stop     bool      `json:"stop,omitempty"`
	Error    string    `json:"error,omitempty"`
}
type Server struct {
	Client     client.Client
	Node, Root string
	State      state.Manager
	Cache      images.Cache
	locks      sync.Map
}

func (s *Server) Serve(ctx context.Context, socket string) error {
	if err := os.MkdirAll(filepath.Dir(socket), 0755); err != nil {
		return err
	}
	// DaemonSet has one instance per node. Restart replaces only its stale socket.
	_ = os.Remove(socket)
	l, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	if err = os.Chmod(socket, 0600); err != nil {
		return err
	}
	mux := http.NewServeMux()
	for _, action := range []string{"prepare", "heartbeat", "status", "finish"} {
		mux.HandleFunc("POST /"+action, s.handler(action))
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
		pod, vm, err := s.authenticate(r.Context(), req)
		if err != nil {
			w.WriteHeader(403)
			_ = json.NewEncoder(w).Encode(Response{Error: err.Error()})
			return
		}
		lock, _ := s.locks.LoadOrStore(string(vm.UID), &sync.Mutex{})
		lock.(*sync.Mutex).Lock()
		defer lock.(*sync.Mutex).Unlock()
		var response Response
		switch action {
		case "prepare":
			response.Prepared, err = s.prepare(r.Context(), pod, vm)
		case "heartbeat":
			response.Stop = vm.Spec.PowerState == "Stopped" || vm.DeletionTimestamp != nil || pod.DeletionTimestamp != nil || pod.Annotations[controller.Stop] == "true"
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
		case "status":
			if req.Phase != "Running" && req.Phase != "Stopping" && req.Phase != "Checkpointing" && req.Phase != "Error" {
				err = errors.New("invalid runtime phase")
			} else {
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
func (s *Server) authenticate(ctx context.Context, req Request) (*core.Pod, *api.VirtualMachine, error) {
	var pod core.Pod
	if req.Namespace == "" || req.Pod == "" || req.UID == "" || req.Token == "" {
		return nil, nil, errors.New("missing runner identity")
	}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.Pod}, &pod); err != nil {
		return nil, nil, err
	}
	if string(pod.UID) != req.UID || pod.Spec.NodeName != s.Node {
		return nil, nil, errors.New("runner does not belong to this node")
	}
	owner := metav1.GetControllerOf(&pod)
	if owner == nil || owner.Kind != "VirtualMachine" || owner.APIVersion != api.GroupVersion.String() {
		return nil, nil, errors.New("not a VM runner")
	}
	var vm api.VirtualMachine
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: owner.Name}, &vm); err != nil {
		return nil, nil, err
	}
	if owner.UID != vm.UID {
		return nil, nil, errors.New("VM identity mismatch")
	}
	var secret core.Secret
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Annotations[controller.SecretAnnotation]}, &secret); err != nil {
		return nil, nil, err
	}
	if !metav1.IsControlledBy(&secret, &vm) || subtle.ConstantTimeCompare(secret.Data["token"], []byte(req.Token)) != 1 {
		return nil, nil, errors.New("invalid runner credential")
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
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path+".partial", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
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
	bootVM, err := incarnation(vm, pod)
	if err != nil {
		return nil, err
	}
	if err := s.annotate(ctx, pod, "Restoring", "", nil); err != nil {
		return nil, err
	}
	keys, err := s.registryKeys(ctx, bootVM)
	if err != nil {
		return nil, err
	}
	base, err := s.Cache.Ensure(ctx, vm.Spec.Image, keys)
	if err != nil {
		return nil, err
	}
	session, err := s.State.Acquire(ctx, string(vm.UID), vm.Spec.Image, string(pod.UID), s.Node)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(s.Root, "running", string(vm.UID))
	if err = os.MkdirAll(dir, 0700); err != nil {
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
	prepared := Prepared{session, base, dir, bootVM.Spec, string(pod.UID)}
	if err = s.save(prepared); err != nil {
		return nil, err
	}
	return &prepared, nil
}

func incarnation(vm *api.VirtualMachine, pod *core.Pod) (*api.VirtualMachine, error) {
	boot := vm.DeepCopy()
	boot.Spec = api.VirtualMachineSpec{}
	if err := json.Unmarshal([]byte(pod.Annotations[controller.SpecAnnotation]), &boot.Spec); err != nil {
		return nil, fmt.Errorf("invalid runner boot configuration: %w", err)
	}
	if boot.Spec.Image != vm.Spec.Image {
		return nil, errors.New("runner base identity changed")
	}
	return boot, nil
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
	if err := s.annotate(ctx, pod, "Stopped", "", committed.Head.Checkpoint); err != nil {
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

type keychain map[string]authn.AuthConfig

func (k keychain) Resolve(r authn.Resource) (authn.Authenticator, error) {
	if c, ok := k[r.RegistryStr()]; ok {
		return authn.FromConfig(c), nil
	}
	return authn.Anonymous, nil
}
func (s *Server) registryKeys(ctx context.Context, vm *api.VirtualMachine) (authn.Keychain, error) {
	k := keychain{}
	for _, ref := range vm.Spec.ImagePullSecrets {
		var secret core.Secret
		if err := s.Client.Get(ctx, types.NamespacedName{Namespace: vm.Namespace, Name: ref.Name}, &secret); err != nil {
			return nil, err
		}
		var config struct {
			Auths map[string]authn.AuthConfig `json:"auths"`
		}
		if err := json.Unmarshal(secret.Data[core.DockerConfigJsonKey], &config); err != nil {
			return nil, fmt.Errorf("invalid image pull secret %s", ref.Name)
		}
		for host, auth := range config.Auths {
			host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
			host = strings.SplitN(host, "/", 2)[0]
			if host == "docker.io" || host == "registry-1.docker.io" {
				host = "index.docker.io"
			}
			reg, err := name.NewRegistry(host)
			if err != nil {
				return nil, err
			}
			k[reg.RegistryStr()] = auth
		}
	}
	return k, nil
}

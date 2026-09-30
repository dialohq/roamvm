package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/dialohq/roamvm/internal/controller"
	"github.com/dialohq/roamvm/internal/state"
	"golang.org/x/sys/unix"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func crashFixture(t *testing.T) (*Server, *core.Pod, *committedStore) {
	t.Helper()
	scheme := runtime.NewScheme()
	core.AddToScheme(scheme)
	api.AddToScheme(scheme)
	vm := &api.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "default", UID: "vm-uid"}}
	pod := &core.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "runner", Namespace: "default", UID: "pod-uid"},
		Spec:       core.PodSpec{NodeName: "node", RestartPolicy: core.RestartPolicyNever, Containers: []core.Container{{Name: "runner"}}},
		Status:     core.PodStatus{ContainerStatuses: []core.ContainerStatus{{Name: "runner", State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}}}}},
	}
	controllerutil.SetControllerReference(vm, pod, scheme)
	store := &committedStore{head: state.Head{Schema: 1, VMID: "vm-uid", State: "Stopped", Epoch: 1, Checkpoint: &api.Checkpoint{Key: "vm/vm-uid/overlay/00000000000000000001-00000000000000000001-" + strings.Repeat("a", 64) + ".qcow2", Generation: 1}}}
	s := &Server{Namespace: pod.Namespace, PodName: pod.Name, PodUID: string(pod.UID), Node: pod.Spec.NodeName, Root: t.TempDir(), State: state.Manager{Store: store}, Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(vm, pod).Build()}
	for _, f := range []string{"overlay.qcow2", "runner.lock"} {
		if err := os.WriteFile(filepath.Join(s.Root, f), []byte("retained"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.save(Prepared{Session: state.Session{Head: state.Head{Epoch: 1}}, PodUID: s.PodUID, Dir: s.Root}); err != nil {
		t.Fatal(err)
	}
	return s, pod, store
}

func TestCrashCheckpointRetriesAfterCommitWithoutDiscardingDisk(t *testing.T) {
	s, pod, store := crashFixture(t)
	store.old = "vm/vm-uid/overlay/00000000000000000000-00000000000000000000-" + strings.Repeat("b", 64) + ".qcow2"
	store.fail = true
	if err := s.CheckpointTerminatedRunner(context.Background()); err == nil {
		t.Fatal("ignored unavailable object store")
	}
	if _, err := os.Stat(filepath.Join(s.Root, "overlay.qcow2")); err != nil {
		t.Fatal("lost working disk", err)
	}
	store.fail = false
	if err := s.CheckpointTerminatedRunner(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Client.Get(context.Background(), client.ObjectKeyFromObject(pod), pod); err != nil {
		t.Fatal(err)
	}
	if pod.Annotations[controller.Phase] != "Stopped" || !strings.Contains(pod.Annotations[controller.Message], "OOMKilled") {
		t.Fatal(pod.Annotations)
	}
	if err := s.CheckpointTerminatedRunner(context.Background()); err != nil {
		t.Fatal("retry after complete", err)
	}
}

func TestCrashCheckpointRequiresTerminatedSameIncarnation(t *testing.T) {
	for _, kind := range []string{"running", "uid", "node", "owner", "restart", "metadata", "locked"} {
		t.Run(kind, func(t *testing.T) {
			s, pod, _ := crashFixture(t)
			switch kind {
			case "running":
				pod.Status.ContainerStatuses[0].State = core.ContainerState{Running: &core.ContainerStateRunning{}}
			case "uid":
				s.PodUID = "other"
			case "node":
				s.Node = "other"
			case "owner":
				pod.OwnerReferences[0].UID = "other"
			case "restart":
				pod.Spec.RestartPolicy = core.RestartPolicyOnFailure
			case "metadata":
				if err := os.Remove(s.meta(s.PodUID)); err != nil {
					t.Fatal(err)
				}
			case "locked":
				f, err := os.OpenFile(filepath.Join(s.Root, "runner.lock"), os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
					t.Fatal(err)
				}
			}
			status := pod.Status
			if err := s.Client.Update(context.Background(), pod); err != nil {
				t.Fatal(err)
			}
			pod.Status = status
			if err := s.Client.Status().Update(context.Background(), pod); err != nil {
				t.Fatal(err)
			}
			if err := s.CheckpointTerminatedRunner(context.Background()); err == nil {
				t.Fatal("unsafe checkpoint allowed")
			}
			if _, err := os.Stat(filepath.Join(s.Root, "overlay.qcow2")); err != nil {
				t.Fatal("working data removed", err)
			}
		})
	}
}

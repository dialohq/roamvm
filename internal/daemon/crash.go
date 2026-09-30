package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/dialohq/roamvm/internal/controller"
	"golang.org/x/sys/unix"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (s *Server) watchRunner(ctx context.Context, done context.CancelFunc) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			finished, err := s.runnerFinished(ctx)
			if err != nil {
				log.Printf("retaining working disk: %v", err)
			}
			if finished {
				done()
				return
			}
		}
	}
}

// Only this Pod's terminated, non-restarting runner can be checkpointed here.
// Node readiness and elapsed time never authorize ownership takeover.
func (s *Server) runnerFinished(ctx context.Context) (bool, error) {
	var pod core.Pod
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: s.PodName}, &pod); err != nil {
		return false, err
	}
	if string(pod.UID) != s.PodUID || pod.Spec.NodeName != s.Node {
		return false, errors.New("runtime Pod identity changed")
	}
	var terminated *core.ContainerStateTerminated
	for _, c := range pod.Status.ContainerStatuses {
		if c.Name == "runner" {
			terminated = c.State.Terminated
		}
	}
	if terminated == nil {
		return false, nil
	}
	for _, c := range pod.Spec.Containers {
		if c.Name == "runner" && (c.RestartPolicy != nil || len(c.RestartPolicyRules) != 0) {
			return false, errors.New("runner may restart")
		}
	}
	if pod.Spec.RestartPolicy != core.RestartPolicyNever {
		return false, errors.New("Pod may restart runner")
	}
	owner := metav1.GetControllerOf(&pod)
	if owner == nil || owner.Kind != "VirtualMachine" || owner.APIVersion != api.GroupVersion.String() {
		return false, errors.New("not a VM runner")
	}
	var vm api.VirtualMachine
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: owner.Name}, &vm); err != nil {
		return false, err
	}
	if vm.UID != owner.UID {
		return false, errors.New("VM identity changed")
	}
	lock, _ := s.locks.LoadOrStore(string(vm.UID), &sync.Mutex{})
	// Shared with the HTTP finish path, including an interrupted response.
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()
	if pod.Annotations[controller.Phase] == "Stopped" {
		return true, nil
	}
	p, err := s.load(string(pod.UID))
	if err != nil {
		return false, s.crashError(ctx, &pod, err)
	}
	f, err := os.OpenFile(filepath.Join(p.Dir, "runner.lock"), os.O_RDWR, 0)
	if err != nil {
		return false, s.crashError(ctx, &pod, err)
	}
	defer f.Close()
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return false, s.crashError(ctx, &pod, err)
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[controller.ExitAnnotation] = fmt.Sprintf("runner exited: %s (exit %d)", terminated.Reason, terminated.ExitCode)
	if err = s.Client.Update(ctx, &pod); err != nil {
		return false, err
	}
	if err = s.finish(ctx, &pod, &vm); err != nil {
		return false, s.crashError(ctx, &pod, err)
	}
	return true, nil
}

func (s *Server) crashError(ctx context.Context, pod *core.Pod, err error) error {
	_ = s.annotate(ctx, pod, "RecoveryRequired", "runner exited; working disk retained: "+err.Error(), nil)
	return err
}

// CheckpointTerminatedRunner salvages an existing incarnation from a rescue Pod
// mounting its original working PVC and immutable base on the same node.
func (s *Server) CheckpointTerminatedRunner(ctx context.Context) error {
	if s.PodName == "" || s.Namespace == "" {
		return errors.New("POD_NAME and POD_NAMESPACE identify the terminated runner Pod")
	}
	finished, err := s.runnerFinished(ctx)
	if err != nil {
		return err
	}
	if !finished {
		return errors.New("runner has not terminated; refusing checkpoint")
	}
	return nil
}

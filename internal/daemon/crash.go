package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/dialohq/roamvm/internal/controller"
	"golang.org/x/sys/unix"
	core "k8s.io/api/core/v1"
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
	pod, vm, err := s.incarnation(ctx)
	if err != nil {
		return false, err
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
	// Shared with the HTTP finish path, including an interrupted response.
	s.mu.Lock()
	defer s.mu.Unlock()
	if pod.Annotations[controller.Phase] == "Stopped" {
		return true, nil
	}
	p, err := s.load(string(pod.UID))
	if err != nil {
		return false, s.crashError(ctx, pod, err)
	}
	f, err := os.OpenFile(filepath.Join(p.Dir, "runner.lock"), os.O_RDWR, 0)
	if err != nil {
		return false, s.crashError(ctx, pod, err)
	}
	defer f.Close()
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return false, s.crashError(ctx, pod, err)
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[controller.ExitAnnotation] = fmt.Sprintf("runner exited: %s (exit %d)", terminated.Reason, terminated.ExitCode)
	if err = s.Client.Update(ctx, pod); err != nil {
		return false, err
	}
	if err = s.finish(ctx, pod, vm); err != nil {
		return false, s.crashError(ctx, pod, err)
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
	if s.Pod.Name == "" || s.Pod.Namespace == "" {
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

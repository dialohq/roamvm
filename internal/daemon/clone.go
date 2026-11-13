package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/dialohq/roamvm/internal/controller"
	"github.com/dialohq/roamvm/internal/disk"
	"github.com/dialohq/roamvm/internal/images"
	"github.com/dialohq/roamvm/internal/state"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CloneDisk exports only the source's current disk, never RAM or snapshot
// history. New objects live under the destination UID so source deletion is safe.
func (s *Server) CloneDisk(ctx context.Context) error {
	pod, vm, err := s.incarnation(ctx)
	if err != nil {
		return err
	}
	if !api.IsV2(s.Client.Scheme()) || vm.Spec.CloneFrom == nil || pod.Labels[controller.CloneLabel] != "true" ||
		pod.DeletionTimestamp != nil || vm.DeletionTimestamp != nil {
		return errors.New("not an active clone worker")
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	current, err := s.State.Read(ctx, string(vm.UID))
	if err != nil && !errors.Is(err, state.ErrNotFound) {
		return err
	}
	if err == nil && current.Head.State == "Stopped" && current.Head.Checkpoint != nil {
		pod.Annotations[controller.CheckpointReadyAnnotation] = "true"
		return s.annotate(ctx, pod, "Stopped", "Disk clone committed", current.Head.Checkpoint)
	}
	var source api.VirtualMachine
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: vm.Namespace, Name: vm.Spec.CloneFrom.Name}, &source); err != nil {
		return err
	}
	if string(source.UID) != vm.Spec.CloneFrom.UID || source.Spec.Image != vm.Spec.Image {
		return errors.New("clone source UID or base image mismatch")
	}
	saved, err := s.State.Read(ctx, string(source.UID))
	if err != nil {
		return err
	}
	if saved.Head.State != "Stopped" || saved.Head.Owner != "" || saved.Head.Checkpoint == nil {
		return errors.New("clone source has no durable stopped checkpoint")
	}
	base, err := images.Open(ctx, s.BaseDir)
	if err != nil {
		return err
	}
	// Copy workers never launch a VMM. A stable import identity makes a lost
	// Pod/reply retryable; conditional publication still fences duplicate writers.
	current, err = s.State.Acquire(ctx, string(vm.UID), vm.Spec.Image, "clone-"+string(vm.UID), "clone")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return err
	}
	input, output := filepath.Join(s.Root, "source.qcow2"), filepath.Join(s.Root, "clone.qcow2")
	for _, path := range []string{input, input + ".partial", output} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err = s.State.Restore(ctx, saved, input); err != nil {
		return err
	}
	if err = disk.Rebase(ctx, base, input); err != nil {
		return err
	}
	if err = disk.Export(ctx, base, input, output); err != nil {
		return err
	}
	current, err = s.State.Snapshot(ctx, current, output, nil)
	if err != nil {
		return err
	}
	pod.Annotations[controller.CheckpointReadyAnnotation] = "true"
	return s.annotate(ctx, pod, "Stopped", "Disk clone committed", current.Head.Checkpoint)
}

// Package orbs adapts Amp's spawn/pause/delete contract to the v2 VM API.
package orbs

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const identity = "amp.roamvm.io/identity"

type Job struct {
	Operation, ID, Type, SnapshotID, Token, AMPURL string
}

type Dispatcher struct {
	client.Client
	Namespace, Image string
	Bootstrap        func(context.Context, *api.VirtualMachine, string, string) error
}

func Name(kind, id string) string {
	return fmt.Sprintf("%s-%x", kind, sha256.Sum256([]byte(id)))[:len(kind)+1+40]
}

func Spec(size, image string) (api.VirtualMachineSpec, error) {
	if size == "" {
		size = "medium"
	}
	var cpus int32
	var memory string
	switch size {
	case "small":
		cpus, memory = 2, "4Gi"
	case "medium":
		cpus, memory = 4, "8Gi"
	case "large":
		cpus, memory = 8, "16Gi"
	default:
		return api.VirtualMachineSpec{}, fmt.Errorf("unknown orb type %q; choose small, medium or large", size)
	}
	return api.VirtualMachineSpec{PowerState: "Running", Image: image, CPUs: cpus, Memory: memory, RootDiskSize: "64Gi", Hibernate: true, GuestAgent: true, ReadinessPort: 22, ShutdownTimeoutSeconds: 120}, nil
}

func (d *Dispatcher) get(ctx context.Context, kind, id string) (*api.VirtualMachine, error) {
	vm := &api.VirtualMachine{}
	if err := d.Get(ctx, client.ObjectKey{Namespace: d.Namespace, Name: Name(kind, id)}, vm); err != nil {
		return nil, err
	}
	if vm.Annotations[identity] != kind+":"+id {
		return nil, fmt.Errorf("refusing to adopt a foreign VM")
	}
	return vm, nil
}

func (d *Dispatcher) wait(ctx context.Context, kind, id string, ready func(*api.VirtualMachine) bool) (*api.VirtualMachine, error) {
	for {
		vm, err := d.get(ctx, kind, id)
		if err != nil {
			return nil, err
		}
		if ready(vm) {
			return vm, nil
		}
		if vm.Status.Phase == "Blocked" || vm.Status.Phase == "RecoveryRequired" || vm.DeletionTimestamp != nil {
			return nil, fmt.Errorf("orb %s: %s", vm.Status.Phase, vm.Status.Message)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func stopped(vm *api.VirtualMachine) bool {
	return vm.Status.Phase == "Stopped" && vm.Status.ObservedGeneration == vm.Generation
}

func durable(vm *api.VirtualMachine) bool {
	return stopped(vm) && vm.Status.Checkpoint != nil && meta.IsStatusConditionTrue(vm.Status.Conditions, "CheckpointReady")
}

func (d *Dispatcher) Run(ctx context.Context, job Job) error {
	if !api.IsV2(d.Scheme()) || job.ID == "" {
		return fmt.Errorf("orb dispatch requires the v2 API and ORB_ID")
	}
	if d.Namespace == "" {
		return fmt.Errorf("orb namespace is required")
	}
	switch job.Operation {
	case "spawn":
		if job.Token == "" || d.Bootstrap == nil {
			return fmt.Errorf("spawn requires ORB_TOKEN and guest bootstrap")
		}
		vm, err := d.get(ctx, "orb", job.ID)
		if apierrors.IsNotFound(err) {
			spec, err := Spec(job.Type, d.Image)
			if err != nil {
				return err
			}
			if job.SnapshotID != "" {
				snapshot, err := d.get(ctx, "snapshot", job.SnapshotID)
				if err != nil && !apierrors.IsNotFound(err) {
					return err
				}
				if err == nil {
					if !durable(snapshot) {
						return fmt.Errorf("project snapshot is not ready")
					}
					spec.Image = snapshot.Spec.Image
					spec.CloneFrom = &api.CloneSource{Name: snapshot.Name, UID: string(snapshot.UID)}
				}
			}
			if spec.Image == "" {
				return fmt.Errorf("set ORB_VM_IMAGE to a pinned, bootable orb image")
			}
			spec.Hostname = Name("orb", job.ID)
			kind := job.Type
			if kind == "" {
				kind = "medium"
			}
			vm = &api.VirtualMachine{ObjectMeta: metav1.ObjectMeta{
				Name: spec.Hostname, Namespace: d.Namespace,
				Annotations: map[string]string{identity: "orb:" + job.ID, "amp.roamvm.io/type": kind},
			}, Spec: spec}
			if err := d.Create(ctx, vm); err != nil && !apierrors.IsAlreadyExists(err) {
				return err
			}
		} else if err != nil {
			return err
		}
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			current, err := d.get(ctx, "orb", job.ID)
			if err != nil {
				return err
			}
			if job.Type != "" && job.Type != current.Annotations["amp.roamvm.io/type"] {
				return fmt.Errorf("an orb's type cannot change")
			}
			before := current.DeepCopy()
			current.Spec.PowerState = "Running"
			return d.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
		}); err != nil {
			return err
		}
		vm, err = d.wait(ctx, "orb", job.ID, func(v *api.VirtualMachine) bool {
			return v.Status.Phase == "Running" && v.Status.ObservedGeneration == v.Generation && meta.IsStatusConditionTrue(v.Status.Conditions, "Ready")
		})
		if err != nil {
			return err
		}
		return d.Bootstrap(ctx, vm, job.Token, job.AMPURL)
	case "pause", "delete":
		vm, err := d.get(ctx, "orb", job.ID)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if job.Operation == "delete" {
			if err := d.Delete(ctx, vm, client.Preconditions{UID: &vm.UID}); client.IgnoreNotFound(err) != nil {
				return err
			}
			for {
				_, err := d.get(ctx, "orb", job.ID)
				if apierrors.IsNotFound(err) {
					return nil
				}
				if err != nil {
					return err
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(200 * time.Millisecond):
				}
			}
		}
		before := vm.DeepCopy()
		vm.Spec.PowerState = "Stopped"
		if err := d.Patch(ctx, vm, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		if job.SnapshotID == "" {
			_, err := d.wait(ctx, "orb", job.ID, stopped)
			return err
		}
		vm, err = d.wait(ctx, "orb", job.ID, durable)
		if err != nil {
			return err
		}
		spec := vm.Spec
		spec.PowerState, spec.Hibernate = "Stopped", false
		spec.CloneFrom = &api.CloneSource{Name: vm.Name, UID: string(vm.UID)}
		snapshot := &api.VirtualMachine{ObjectMeta: metav1.ObjectMeta{
			Name: Name("snapshot", job.SnapshotID), Namespace: d.Namespace,
			Annotations: map[string]string{identity: "snapshot:" + job.SnapshotID},
		}, Spec: spec}
		if err := d.Create(ctx, snapshot); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		_, err = d.wait(ctx, "snapshot", job.SnapshotID, durable)
		return err
	default:
		return fmt.Errorf("unknown ORB_OPERATION %q", job.Operation)
	}
}

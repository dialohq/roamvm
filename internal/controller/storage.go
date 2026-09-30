package controller

import (
	"context"
	"fmt"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func workingSize(floor, root string) (resource.Quantity, error) {
	if floor == "" {
		floor = "64Gi"
	}
	size, err := resource.ParseQuantity(floor)
	if err != nil || size.Sign() <= 0 {
		return size, fmt.Errorf("invalid working storage size %q", floor)
	}
	required, err := api.WorkingBytes(root)
	if err != nil {
		return size, err
	}
	if size.Value() < required {
		size = *resource.NewQuantity(required, resource.BinarySI)
	}
	return size, nil
}

func (r *Reconciler) expandWorkingPVC(ctx context.Context, vm *api.VirtualMachine, pod *core.Pod) error {
	if vm.Spec.RootDiskSize == "" {
		return nil
	}
	desired, err := workingSize(r.StorageSize, vm.Spec.RootDiskSize)
	if err != nil {
		return err
	}
	var pvc core.PersistentVolumeClaim
	if err = r.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: pod.Name + "-working"}, &pvc); err != nil {
		return err
	}
	if !metav1.IsControlledBy(&pvc, pod) {
		return fmt.Errorf("working PVC is not owned by runner")
	}
	requested := pvc.Spec.Resources.Requests[core.ResourceStorage]
	if requested.Cmp(desired) < 0 {
		before := pvc.DeepCopy()
		pvc.Spec.Resources.Requests[core.ResourceStorage] = desired
		if err = r.Patch(ctx, &pvc, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}
	capacity := pvc.Status.Capacity[core.ResourceStorage]
	if capacity.Cmp(desired) < 0 {
		return fmt.Errorf("waiting for working PVC expansion to %s", desired.String())
	}
	return nil
}

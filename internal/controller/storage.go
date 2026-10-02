package controller

import (
	"context"
	"fmt"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func podUnschedulable(pod *core.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == core.PodScheduled && c.Status == core.ConditionFalse && c.Reason == core.PodReasonUnschedulable {
			return true
		}
	}
	return false
}

func (r *Reconciler) localNodeUnavailable(ctx context.Context, name string) (bool, error) {
	if name == "" {
		return true, nil
	}
	var node core.Node
	if err := r.Get(ctx, client.ObjectKey{Name: name}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	if node.Spec.Unschedulable {
		return true, nil
	}
	for _, c := range node.Status.Conditions {
		if c.Type == core.NodeReady {
			return c.Status != core.ConditionTrue, nil
		}
	}
	return true, nil
}

func (r *Reconciler) discardLocal(ctx context.Context, vm *api.VirtualMachine) error {
	if vm.Status.Local == nil {
		return nil
	}
	pvc := &core.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: vm.Status.Local.ClaimName, Namespace: vm.Namespace}}
	if err := client.IgnoreNotFound(r.Delete(ctx, pvc)); err != nil {
		return err
	}
	vm.Status.Local = nil
	vm.Status.PodName = ""
	vm.Status.Phase = "Stopped"
	return r.Status().Update(ctx, vm)
}

// WorkingClaim returns the actual root-disk claim used by a runner. Explicit
// claims are preferred; the generated claim name is retained for old Pods.
func WorkingClaim(pod *core.Pod) string {
	for _, volume := range pod.Spec.Volumes {
		if volume.Name != "working" {
			continue
		}
		if volume.PersistentVolumeClaim != nil {
			return volume.PersistentVolumeClaim.ClaimName
		}
		if volume.Ephemeral != nil {
			return pod.Name + "-working"
		}
	}
	return ""
}

func (r *Reconciler) ensureWorkingPVC(ctx context.Context, vm *api.VirtualMachine, name string) error {
	var pvc core.PersistentVolumeClaim
	err := r.Get(ctx, client.ObjectKey{Namespace: vm.Namespace, Name: name}, &pvc)
	if err == nil {
		if !metav1.IsControlledBy(&pvc, vm) {
			return fmt.Errorf("working PVC is not owned by VM")
		}
		return nil
	}
	if client.IgnoreNotFound(err) != nil {
		return err
	}
	if vm.Status.Local != nil && vm.Status.Local.ClaimName == name {
		return fmt.Errorf("retained working PVC is missing; refusing an empty local restart")
	}
	size, err := workingSize(r.StorageSize, vm.Spec.RootDiskSize)
	if err != nil {
		return err
	}
	var storageClass *string
	if r.StorageClass != "" {
		storageClass = &r.StorageClass
	}
	pvc = core.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: vm.Namespace}, Spec: core.PersistentVolumeClaimSpec{
		StorageClassName: storageClass, AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
		Resources: core.VolumeResourceRequirements{Requests: core.ResourceList{core.ResourceStorage: size}},
	}}
	if err := controllerutil.SetControllerReference(vm, &pvc, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, &pvc)
}

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
	if err = r.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: WorkingClaim(pod)}, &pvc); err != nil {
		return err
	}
	if !metav1.IsControlledBy(&pvc, vm) && !metav1.IsControlledBy(&pvc, pod) {
		return fmt.Errorf("working PVC is not owned by VM or legacy runner")
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

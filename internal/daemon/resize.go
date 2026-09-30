package daemon

import (
	"context"
	"fmt"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Resize failures must never interrupt ownership heartbeats or stop a guest.
func (s *Server) resizeTarget(ctx context.Context, pod *core.Pod, vm *api.VirtualMachine) (int64, string) {
	size, err := api.DiskBytes(vm.Spec.RootDiskSize)
	if err != nil {
		return 0, err.Error()
	}
	if size == 0 {
		return 0, ""
	}
	var pvc core.PersistentVolumeClaim
	if err = s.Client.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: pod.Name + "-working"}, &pvc); err != nil {
		return 0, err.Error()
	}
	if !metav1.IsControlledBy(&pvc, pod) {
		return 0, "working PVC is not owned by runner"
	}
	capacity := pvc.Status.Capacity[core.ResourceStorage]
	required, _ := api.WorkingBytes(vm.Spec.RootDiskSize)
	if capacity.Value() < required {
		return 0, fmt.Sprintf("waiting for working PVC capacity: need %d bytes", required)
	}
	for _, condition := range pvc.Status.Conditions {
		if condition.Status == core.ConditionTrue && (condition.Type == core.PersistentVolumeClaimResizing || condition.Type == core.PersistentVolumeClaimFileSystemResizePending) {
			return 0, "waiting for PVC filesystem expansion"
		}
	}
	return size, ""
}

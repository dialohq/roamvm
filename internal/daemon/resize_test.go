package daemon

import (
	"testing"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestResizeWaitsForPVCFilesystem(t *testing.T) {
	scheme := runtime.NewScheme()
	core.AddToScheme(scheme)
	pod := &core.Pod{ObjectMeta: metav1.ObjectMeta{Name: "runner", Namespace: "default", UID: "pod-id"}}
	vm := &api.VirtualMachine{Spec: api.VirtualMachineSpec{RootDiskSize: "2Gi"}}
	for _, test := range []struct {
		name, capacity            string
		pending, foreign, allowed bool
	}{
		{name: "insufficient", capacity: "4Gi"},
		{name: "node expansion pending", capacity: "5Gi", pending: true},
		{name: "foreign PVC", capacity: "5Gi", foreign: true},
		{name: "ready", capacity: "5Gi", allowed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			pvc := &core.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "runner-working", Namespace: "default", OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID, Controller: ptr.To(true)}}},
				Status:     core.PersistentVolumeClaimStatus{Capacity: core.ResourceList{core.ResourceStorage: resource.MustParse(test.capacity)}},
			}
			if test.pending {
				pvc.Status.Conditions = []core.PersistentVolumeClaimCondition{{Type: core.PersistentVolumeClaimFileSystemResizePending, Status: core.ConditionTrue}}
			}
			if test.foreign {
				pvc.OwnerReferences = nil
			}
			s := &Server{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc).Build()}
			size, problem := s.resizeTarget(t.Context(), pod, vm)
			if test.allowed {
				if size != 2<<30 || problem != "" {
					t.Fatalf("%d: %s", size, problem)
				}
			} else if size != 0 || problem == "" {
				t.Fatalf("unsafe resize: %d: %s", size, problem)
			}
		})
	}
}

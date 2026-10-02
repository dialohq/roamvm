package controller

import (
	"testing"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestWorkingPVCExpansion(t *testing.T) {
	r, vm := setup(t)
	r.StorageSize = "1Gi"
	vm.Spec.RootDiskSize = "2Gi"
	require.NoError(t, r.createPod(t.Context(), vm))
	var pod core.Pod
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Namespace: vm.Namespace, Name: vm.Status.PodName}, &pod))
	pvc := &core.PersistentVolumeClaim{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Namespace: vm.Namespace, Name: WorkingClaim(&pod)}, pvc))
	require.EqualValues(t, 3<<30, pvc.Spec.Resources.Requests.Storage().Value())
	pvc.Status.Capacity = core.ResourceList{core.ResourceStorage: resource.MustParse("3Gi")}
	require.NoError(t, r.Status().Update(t.Context(), pvc))
	vm.Spec.RootDiskSize = "4Gi"
	require.Error(t, r.expandWorkingPVC(t.Context(), vm, &pod), "accepted unfinished expansion")
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(pvc), pvc))
	if pvc.Spec.Resources.Requests.Storage().Value() != 5<<30 {
		t.Fatal("PVC not expanded")
	}
	// An unrelated PVC with the expected name must never be mutated.
	pvc.OwnerReferences = nil
	require.NoError(t, r.Update(t.Context(), pvc))
	require.Error(t, r.expandWorkingPVC(t.Context(), vm, &pod), "accepted foreign PVC")
}

func TestDiskSizeValidation(t *testing.T) {
	for _, size := range []string{"0", "-1Gi", "17Ti", "513", "1m", "junk"} {
		if _, err := api.DiskBytes(size); err == nil {
			t.Fatalf("accepted %q", size)
		}
	}
	for _, size := range []string{"", "512", "1.5Gi", "16Ti"} {
		if _, err := api.DiskBytes(size); err != nil {
			t.Fatalf("rejected %q: %v", size, err)
		}
	}
}

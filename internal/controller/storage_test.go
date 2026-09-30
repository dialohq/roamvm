package controller

import (
	"testing"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestWorkingPVCExpansion(t *testing.T) {
	r, vm := setup(t)
	r.StorageSize = "1Gi"
	vm.Spec.RootDiskSize = "2Gi"
	require.NoError(t, r.createPod(t.Context(), vm))
	var pod core.Pod
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Namespace: vm.Namespace, Name: vm.Status.PodName}, &pod))
	for _, v := range pod.Spec.Volumes {
		if v.Name == "working" && v.Ephemeral.VolumeClaimTemplate.Spec.Resources.Requests.Storage().Value() != 5<<30 {
			t.Fatal("missing checkpoint headroom")
		}
	}
	pod.UID = "pod-id"
	pvc := &core.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: pod.Name + "-working", Namespace: pod.Namespace, OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID, Controller: ptr.To(true)}}},
		Spec:       core.PersistentVolumeClaimSpec{Resources: core.VolumeResourceRequirements{Requests: core.ResourceList{core.ResourceStorage: resource.MustParse("5Gi")}}},
		Status:     core.PersistentVolumeClaimStatus{Capacity: core.ResourceList{core.ResourceStorage: resource.MustParse("5Gi")}},
	}
	require.NoError(t, r.Create(t.Context(), pvc))
	vm.Spec.RootDiskSize = "4Gi"
	require.Error(t, r.expandWorkingPVC(t.Context(), vm, &pod), "accepted unfinished expansion")
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(pvc), pvc))
	if pvc.Spec.Resources.Requests.Storage().Value() != 9<<30 {
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

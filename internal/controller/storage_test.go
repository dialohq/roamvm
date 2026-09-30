package controller

import (
	"testing"

	api "github.com/dialohq/roamvm/api/v1alpha1"
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
	if err := r.createPod(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	var pod core.Pod
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: vm.Namespace, Name: vm.Status.PodName}, &pod); err != nil {
		t.Fatal(err)
	}
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
	if err := r.Create(t.Context(), pvc); err != nil {
		t.Fatal(err)
	}
	vm.Spec.RootDiskSize = "4Gi"
	if err := r.expandWorkingPVC(t.Context(), vm, &pod); err == nil {
		t.Fatal("accepted unfinished expansion")
	}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(pvc), pvc); err != nil {
		t.Fatal(err)
	}
	if pvc.Spec.Resources.Requests.Storage().Value() != 9<<30 {
		t.Fatal("PVC not expanded")
	}
	// An unrelated PVC with the expected name must never be mutated.
	pvc.OwnerReferences = nil
	if err := r.Update(t.Context(), pvc); err != nil {
		t.Fatal(err)
	}
	if err := r.expandWorkingPVC(t.Context(), vm, &pod); err == nil {
		t.Fatal("accepted foreign PVC")
	}
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

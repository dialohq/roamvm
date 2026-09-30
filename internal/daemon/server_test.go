package daemon

import (
	"context"
	"encoding/json"
	"testing"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/dialohq/roamvm/internal/controller"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func TestRunnerAuthenticationIsBoundToPodNodeAndVM(t *testing.T) {
	scheme := runtime.NewScheme()
	core.AddToScheme(scheme)
	api.AddToScheme(scheme)
	vm := &api.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "a", UID: "vm-uid"}}
	pod := &core.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "runner",
			Namespace:   "a",
			UID:         "pod-uid",
			Annotations: map[string]string{controller.SecretAnnotation: "auth"},
		},
		Spec: core.PodSpec{NodeName: "node-a"},
	}
	secret := &core.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "auth", Namespace: "a"},
		Data:       map[string][]byte{"token": []byte("private-token")},
	}
	controllerutil.SetControllerReference(vm, pod, scheme)
	controllerutil.SetControllerReference(vm, secret, scheme)
	server := &Server{
		Node:   "node-a",
		PodUID: "pod-uid",
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(vm, pod, secret).Build(),
	}
	valid := Request{Namespace: "a", Pod: "runner", UID: "pod-uid", Token: "private-token"}
	if _, _, e := server.authenticate(context.Background(), valid); e != nil {
		t.Fatal(e)
	}
	for _, bad := range []Request{
		{Namespace: "a", Pod: "runner", UID: "pod-uid", Token: "wrong"},
		{Namespace: "a", Pod: "runner", UID: "previous-pod-uid", Token: "private-token"},
		{Namespace: "b", Pod: "runner", UID: "pod-uid", Token: "private-token"},
	} {
		if _, _, e := server.authenticate(context.Background(), bad); e == nil {
			t.Fatal("forged runner accepted")
		}
	}
	server.PodUID = "other-pod"
	if _, _, e := server.authenticate(context.Background(), valid); e == nil {
		t.Fatal("runtime accepted another Pod's identity")
	}
	server.PodUID = "pod-uid"
	server.Node = "node-b"
	if _, _, e := server.authenticate(context.Background(), valid); e == nil {
		t.Fatal("runner from another node accepted")
	}
}

func TestQueuedIncarnationDoesNotAdoptUnaccountedSpecChanges(t *testing.T) {
	original := api.VirtualMachineSpec{Image: "base@sha256:fixed", CPUs: 2, Memory: "1Gi"}
	b, e := json.Marshal(original)
	if e != nil {
		t.Fatal(e)
	}
	pod := &core.Pod{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{controller.SpecAnnotation: string(b)}},
	}
	vm := &api.VirtualMachine{Spec: original}
	vm.Spec.CPUs = 8
	vm.Spec.Memory = "16Gi"
	vm.Spec.Hugepages = "1Gi"
	boot, e := incarnation(vm, pod)
	if e != nil {
		t.Fatal(e)
	}
	if boot.Spec.CPUs != 2 || boot.Spec.Memory != "1Gi" || boot.Spec.Hugepages != "" {
		t.Fatal("VM spec update escaped Pod resource accounting", boot.Spec)
	}
	vm.Spec.Image = "different"
	if _, e = incarnation(vm, pod); e == nil {
		t.Fatal("base changed")
	}
}

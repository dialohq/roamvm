package daemon

import (
	"context"
	"encoding/json"
	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/dialohq/roamvm/internal/controller"
	"github.com/google/go-containerregistry/pkg/name"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"testing"
)

func TestRunnerAuthenticationIsBoundToPodNodeAndVM(t *testing.T) {
	scheme := runtime.NewScheme()
	core.AddToScheme(scheme)
	api.AddToScheme(scheme)
	vm := &api.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "a", UID: "vm-uid"}}
	pod := &core.Pod{ObjectMeta: metav1.ObjectMeta{Name: "runner", Namespace: "a", UID: "pod-uid", Annotations: map[string]string{controller.SecretAnnotation: "auth"}}, Spec: core.PodSpec{NodeName: "node-a"}}
	secret := &core.Secret{ObjectMeta: metav1.ObjectMeta{Name: "auth", Namespace: "a"}, Data: map[string][]byte{"token": []byte("private-token")}}
	controllerutil.SetControllerReference(vm, pod, scheme)
	controllerutil.SetControllerReference(vm, secret, scheme)
	server := &Server{Node: "node-a", Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(vm, pod, secret).Build()}
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
	pod := &core.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{controller.SpecAnnotation: string(b)}}}
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

func TestDockerConfigRegistryAliases(t *testing.T) {
	for _, host := range []string{"https://index.docker.io/v1/", "docker.io", "registry-1.docker.io", "index.docker.io"} {
		scheme := runtime.NewScheme()
		core.AddToScheme(scheme)
		secret := &core.Secret{ObjectMeta: metav1.ObjectMeta{Name: "registry", Namespace: "dev"}, Data: map[string][]byte{core.DockerConfigJsonKey: []byte(`{"auths":{"` + host + `":{"auth":"dXNlcjpwYXNz"}}}`)}}
		server := &Server{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()}
		vm := &api.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Namespace: "dev"}, Spec: api.VirtualMachineSpec{ImagePullSecrets: []core.LocalObjectReference{{Name: "registry"}}}}
		keys, e := server.registryKeys(context.Background(), vm)
		if e != nil {
			t.Fatal(e)
		}
		ref, _ := name.NewTag("library/alpine:3.23")
		auth, e := keys.Resolve(ref.Context().Registry)
		if e != nil {
			t.Fatal(e)
		}
		config, e := auth.Authorization()
		if e != nil || config.Username != "user" || config.Password != "pass" {
			t.Fatalf("%s: %#v %v", host, config, e)
		}
	}
}

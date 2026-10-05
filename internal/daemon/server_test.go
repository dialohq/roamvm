package daemon

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/dialohq/roamvm/internal/controller"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func TestRunnerIdentityIsBoundToPodNodeAndVM(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, core.AddToScheme(scheme))
	require.NoError(t, api.AddToScheme(scheme))
	vm := &api.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "a", UID: "vm-uid"}}
	pod := &core.Pod{ObjectMeta: metav1.ObjectMeta{Name: "runner", Namespace: "a", UID: "pod-uid", Annotations: map[string]string{controller.Phase: "Running"}}, Spec: core.PodSpec{NodeName: "node-a"}}
	require.NoError(t, controllerutil.SetControllerReference(vm, pod, scheme))
	for _, tc := range []struct {
		name   string
		change func(*Server, *core.Pod)
	}{
		{"valid", func(*Server, *core.Pod) {}},
		{"replaced pod", func(s *Server, _ *core.Pod) { s.PodUID = "previous-pod" }},
		{"different VM", func(s *Server, _ *core.Pod) { s.VMUID = "another-vm" }},
		{"different node", func(s *Server, _ *core.Pod) { s.Node = "node-b" }},
		{"different namespace", func(s *Server, _ *core.Pod) { s.Pod.Namespace = "b" }},
		{"missing pod", func(s *Server, _ *core.Pod) { s.Pod.Name = "missing" }},
		{"replaced VM", func(_ *Server, p *core.Pod) { p.OwnerReferences[0].UID = "previous-vm" }},
		{"wrong kind", func(_ *Server, p *core.Pod) { p.OwnerReferences[0].Kind = "Secret" }},
		{"no owner", func(_ *Server, p *core.Pod) { p.OwnerReferences = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := pod.DeepCopy()
			s := &Server{Node: "node-a", PodUID: "pod-uid", VMUID: "vm-uid", Pod: types.NamespacedName{Namespace: "a", Name: "runner"}}
			tc.change(s, p)
			s.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(vm, p).Build()
			actualPod, actualVM, err := s.incarnation(t.Context())
			if tc.name == "valid" {
				require.NoError(t, err)
				require.Equal(t, client.ObjectKeyFromObject(p), client.ObjectKeyFromObject(actualPod))
				require.Equal(t, vm.UID, actualVM.UID)
				// Caller-supplied identities from the former multi-VM protocol
				// cannot redirect a Pod-local runtime to another incarnation.
				request := httptest.NewRequest("POST", "/status", strings.NewReader(`{"namespace":"b","pod":"other","uid":"other-uid","token":"anything","phase":"Stopping"}`))
				response := httptest.NewRecorder()
				s.handler("status")(response, request)
				require.Equal(t, 200, response.Code, response.Body.String())
				require.NoError(t, s.Client.Get(t.Context(), s.Pod, actualPod))
				require.Equal(t, "Stopping", actualPod.Annotations[controller.Phase])
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestQueuedIncarnationDoesNotAdoptUnaccountedSpecChanges(t *testing.T) {
	original := api.VirtualMachineSpec{Image: "base@sha256:fixed", CPUs: 2, Memory: "1Gi", GuestServiceAccountToken: &api.GuestServiceAccountToken{Name: "original-guest", Audience: "original-service"}}
	b, e := json.Marshal(original)
	require.NoError(t, e)
	pod := &core.Pod{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{controller.SpecAnnotation: string(b)}},
	}
	vm := &api.VirtualMachine{Spec: original}
	vm.Spec.CPUs = 8
	vm.Spec.Memory = "16Gi"
	vm.Spec.Hugepages = "1Gi"
	vm.Spec.GuestServiceAccountToken = &api.GuestServiceAccountToken{Name: "different-account", Audience: "different-service"}
	boot, e := bootSpec(vm, pod)
	require.NoError(t, e)
	require.Equal(t, original.GuestServiceAccountToken, boot.GuestServiceAccountToken)
	if boot.CPUs != 2 || boot.Memory != "1Gi" || boot.Hugepages != "" {
		t.Fatal("VM spec update escaped Pod resource accounting", boot)
	}
	vm.Spec.Image = "different"
	if _, e = bootSpec(vm, pod); e == nil {
		t.Fatal("base changed")
	}
}

func TestStoppingDoesNotDependOnGuestTokenAcquisition(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, core.AddToScheme(scheme))
	require.NoError(t, api.AddToScheme(scheme))
	vm := &api.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "a", UID: "vm-uid"}, Spec: api.VirtualMachineSpec{
		PowerState: "Stopped", GuestServiceAccountToken: &api.GuestServiceAccountToken{Name: "guest", Audience: "rgw"},
	}}
	pod := &core.Pod{ObjectMeta: metav1.ObjectMeta{Name: "runner", Namespace: "a", UID: "pod-uid"}, Spec: core.PodSpec{NodeName: "node-a"}}
	require.NoError(t, controllerutil.SetControllerReference(vm, pod, scheme))
	s := &Server{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(vm, pod).Build(),
		Node:   "node-a", PodUID: "pod-uid", VMUID: "vm-uid", Pod: client.ObjectKeyFromObject(pod), GuestTokenDir: t.TempDir(),
	}
	response := httptest.NewRecorder()
	s.handler("heartbeat")(response, httptest.NewRequest("POST", "/heartbeat", strings.NewReader(`{}`)))
	require.Equal(t, 200, response.Code, response.Body.String())
	var result Response
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	require.True(t, result.Stop)
	require.True(t, s.guestTokenRefresh.IsZero())
}

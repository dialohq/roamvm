package controller

import (
	"context"
	"encoding/json"
	api "github.com/dialohq/roamvm/api/v1alpha1"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func setup(t *testing.T) (*Reconciler, *api.VirtualMachine) {
	t.Helper()
	s := runtime.NewScheme()
	core.AddToScheme(s)
	api.AddToScheme(s)
	vm := &api.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "devbox", Namespace: "default", UID: "vm-uid", Finalizers: []string{Finalizer}}, Spec: api.VirtualMachineSpec{PowerState: "Running", Image: "registry/base@sha256:abc", CPUs: 4, Memory: "1Gi", Resources: core.ResourceRequirements{Requests: core.ResourceList{core.ResourceCPU: resource.MustParse("250m")}}}}
	vm.Status.PodName = "devbox-runner"
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&api.VirtualMachine{}).WithObjects(vm).Build()
	return &Reconciler{Client: c, Scheme: s, Image: "runner:test"}, vm
}
func TestPodUsesSchedulerResourcesAndEphemeralRoot(t *testing.T) {
	r, vm := setup(t)
	ctx := context.Background()
	if e := r.createPod(ctx, vm); e != nil {
		t.Fatal(e)
	}
	var pods core.PodList
	if e := r.List(ctx, &pods); e != nil {
		t.Fatal(e)
	}
	if len(pods.Items) != 1 {
		t.Fatal("expected one Pod")
	}
	pod := pods.Items[0]
	if pod.Spec.NodeName != "" || pod.Spec.Affinity != nil {
		t.Fatal("root disk constrained placement")
	}
	if pod.Spec.Containers[0].Resources.Requests.Cpu().MilliValue() != 250 {
		t.Fatal("CPU overcommit request lost")
	}
	if pod.Spec.Containers[0].Resources.Requests.Memory().Value() != (1216 << 20) {
		t.Fatal("guest memory overhead not accounted")
	}
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			t.Fatal("root created a PVC")
		}
	}
	if *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("runner has Kubernetes credentials")
	}
	if !metav1.IsControlledBy(&pod, vm) {
		t.Fatal("missing lifecycle owner")
	}
}
func TestRejectMemoryUnderAccounting(t *testing.T) {
	r, vm := setup(t)
	vm.Spec.Resources.Requests[core.ResourceMemory] = resource.MustParse("512Mi")
	if e := r.createPod(context.Background(), vm); e == nil {
		t.Fatal("memory request below guest RAM accepted")
	}
}

func TestCPUReservationDoesNotSetGuestSizeOrImplicitQuota(t *testing.T) {
	for _, limit := range []string{"", "4", "500m"} {
		t.Run("limit="+limit, func(t *testing.T) {
			r, vm := setup(t)
			if limit != "" {
				vm.Spec.Resources.Limits = core.ResourceList{core.ResourceCPU: resource.MustParse(limit)}
			}
			if err := r.createPod(context.Background(), vm); err != nil {
				t.Fatal(err)
			}
			var pod core.Pod
			if err := r.Get(context.Background(), client.ObjectKey{Namespace: vm.Namespace, Name: vm.Status.PodName}, &pod); err != nil {
				t.Fatal(err)
			}
			resources := pod.Spec.Containers[0].Resources
			var boot api.VirtualMachineSpec
			if err := json.Unmarshal([]byte(pod.Annotations[SpecAnnotation]), &boot); err != nil {
				t.Fatal(err)
			}
			if resources.Requests.Cpu().MilliValue() != 250 || boot.CPUs != 4 {
				t.Fatal("guest size and scheduler request were coupled")
			}
			quota, present := resources.Limits[core.ResourceCPU]
			if present != (limit != "") || (present && quota.Cmp(resource.MustParse(limit)) != 0) {
				t.Fatal("explicit CPU quota was not preserved")
			}
		})
	}
}

func TestCreateRetryDoesNotCreateSecondIncarnation(t *testing.T) {
	r, vm := setup(t)
	ctx := context.Background()
	if e := r.createPod(ctx, vm); e != nil {
		t.Fatal(e)
	}
	var original core.Secret
	if e := r.Get(ctx, client.ObjectKey{Namespace: vm.Namespace, Name: vm.Status.PodName}, &original); e != nil {
		t.Fatal(e)
	}
	if e := r.createPod(ctx, vm); e != nil {
		t.Fatal(e)
	}
	var pods core.PodList
	r.List(ctx, &pods)
	if len(pods.Items) != 1 {
		t.Fatal("retry created a second incarnation")
	}
	var secret core.Secret
	r.Get(ctx, client.ObjectKeyFromObject(&original), &secret)
	if string(secret.Data["token"]) != string(original.Data["token"]) {
		t.Fatal("retry rotated a running Pod's credential")
	}
}

func TestHugepagesUseNativeAccounting(t *testing.T) {
	r, vm := setup(t)
	vm.Spec.Hugepages = "2Mi"
	if e := r.createPod(context.Background(), vm); e != nil {
		t.Fatal(e)
	}
	var pods core.PodList
	r.List(context.Background(), &pods)
	resources := pods.Items[0].Spec.Containers[0].Resources
	if resources.Requests.Memory().Value() != 192<<20 {
		t.Fatal("ordinary RAM should reserve only VMM overhead")
	}
	q := resources.Requests["hugepages-2Mi"]
	if q.Value() != 1<<30 {
		t.Fatal("guest RAM not reserved as hugepages")
	}
}
func TestStopRequestsCheckpointWithoutDeletingPod(t *testing.T) {
	r, vm := setup(t)
	ctx := context.Background()
	if e := r.createPod(ctx, vm); e != nil {
		t.Fatal(e)
	}
	var running core.Pod
	if e := r.Get(ctx, client.ObjectKey{Namespace: vm.Namespace, Name: vm.Status.PodName}, &running); e != nil {
		t.Fatal(e)
	}
	running.Spec.NodeName = "node-a"
	if e := r.Update(ctx, &running); e != nil {
		t.Fatal(e)
	}

	vm.Spec.PowerState = "Stopped"
	if e := r.Update(ctx, vm); e != nil {
		t.Fatal(e)
	}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vm)}
	if _, e := r.Reconcile(ctx, req); e != nil {
		t.Fatal(e)
	}
	var pods core.PodList
	r.List(ctx, &pods)
	if len(pods.Items) != 1 || pods.Items[0].Annotations[Stop] != "true" || pods.Items[0].DeletionTimestamp != nil {
		t.Fatal("stop removed working disk before checkpoint")
	}
	var current api.VirtualMachine
	r.Get(ctx, req.NamespacedName, &current)
	if current.Status.Phase == "Stopped" {
		t.Fatal("reported stopped before commit")
	}
}
func TestMissingRunnerNeverReportsDurableStop(t *testing.T) {
	r, vm := setup(t)
	ctx := context.Background()
	vm.Spec.PowerState = "Stopped"
	r.Update(ctx, vm)
	vm.Status.Phase = "Running"
	r.Status().Update(ctx, vm)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vm)}
	if _, e := r.Reconcile(ctx, req); e != nil {
		t.Fatal(e)
	}
	var current api.VirtualMachine
	r.Get(ctx, req.NamespacedName, &current)
	if current.Status.Phase != "RecoveryRequired" {
		t.Fatal(current.Status.Phase)
	}
}

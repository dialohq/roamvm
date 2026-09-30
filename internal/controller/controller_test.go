package controller

import (
	"context"
	"encoding/json"
	"testing"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func setup(t *testing.T) (*Reconciler, *api.VirtualMachine) {
	t.Helper()
	s := runtime.NewScheme()
	core.AddToScheme(s)
	api.AddToScheme(s)
	vm := &api.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "devbox",
			Namespace:  "default",
			UID:        "vm-uid",
			Finalizers: []string{Finalizer},
		},
		Spec: api.VirtualMachineSpec{
			PowerState: "Running",
			Image:      "registry/base@sha256:abc",
			CPUs:       4,
			Memory:     "1Gi",
			Resources: core.ResourceRequirements{
				Requests: core.ResourceList{core.ResourceCPU: resource.MustParse("250m")},
			},
		},
	}
	vm.Status.PodName = "devbox-runner"
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&api.VirtualMachine{}).WithObjects(vm).Build()
	return &Reconciler{Client: c, Scheme: s, Image: "runner:test"}, vm
}

func TestPodUsesSchedulerResourcesAndEphemeralRoot(t *testing.T) {
	r, vm := setup(t)
	ctx := context.Background()
	require.NoError(t, r.createPod(ctx, vm))
	var pods core.PodList
	require.NoError(t, r.List(ctx, &pods))
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
	if pod.Spec.Containers[0].Resources.Requests.Memory().Value() != (1568 << 20) {
		t.Fatal("guest memory overhead not accounted")
	}
	var working, base bool
	for _, v := range pod.Spec.Volumes {
		if v.HostPath != nil {
			t.Fatal("hostPath mounted into VM Pod")
		}
		if v.PersistentVolumeClaim != nil {
			t.Fatal("root reused a persistent claim across incarnations")
		}
		if v.Name == "working" {
			working = v.Ephemeral != nil &&
				v.Ephemeral.VolumeClaimTemplate.Spec.Resources.Requests.Storage().Cmp(resource.MustParse("64Gi")) == 0
		}
		if v.Name == "base" {
			base = v.Image != nil && v.Image.Reference == vm.Spec.Image && v.Image.PullPolicy == core.PullIfNotPresent
		}
	}
	if !working || !base {
		t.Fatal("missing ephemeral PVC or immutable image volume")
	}
	if len(pod.Spec.InitContainers) != 0 || len(pod.Spec.Containers) != 2 || pod.Spec.Containers[1].RestartPolicy == nil ||
		*pod.Spec.Containers[1].RestartPolicy != core.ContainerRestartPolicyOnFailure {
		t.Fatal("runtime must survive runner failure and restart independently")
	}
	config := pod.Spec.Containers[1].EnvFrom
	if len(config) != 2 || config[0].ConfigMapRef == nil || config[0].ConfigMapRef.Name != "roamvm-runtime" ||
		config[1].SecretRef.Name != "roamvm-object-store" {
		t.Fatal("runtime namespace settings/credentials missing")
	}
	runner := pod.Spec.Containers[0]
	for _, mount := range runner.VolumeMounts {
		if mount.Name == "kube-api" {
			t.Fatal("runner can access runtime Kubernetes credentials")
		}
	}
	if len(runner.EnvFrom) != 0 {
		t.Fatal("runner receives object-store credentials")
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
	require.Error(t, r.createPod(context.Background(), vm), "memory request below guest RAM accepted")
}

func TestMemoryReservationAndOptionalLimit(t *testing.T) {
	for _, tc := range []struct {
		name, request, limit string
		hugepages, invalid   bool
	}{
		{name: "default"},
		{name: "larger reservation", request: "2Gi"},
		{name: "explicit limit", limit: "3Gi"},
		{name: "explicit request and limit", request: "2Gi", limit: "3Gi"},
		{name: "limit below overhead allowance", limit: "1Gi", invalid: true},
		{name: "limit below custom reservation", request: "3Gi", limit: "2Gi", invalid: true},
		{name: "hugepages", hugepages: true},
		{name: "hugepages with host limit", hugepages: true, limit: "1Gi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, vm := setup(t)
			if tc.hugepages {
				vm.Spec.Hugepages = "2Mi"
			}
			if tc.request != "" {
				vm.Spec.Resources.Requests[core.ResourceMemory] = resource.MustParse(tc.request)
			}
			if tc.limit != "" {
				vm.Spec.Resources.Limits = core.ResourceList{core.ResourceMemory: resource.MustParse(tc.limit)}
			}
			err := r.createPod(context.Background(), vm)
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid memory budget accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var pod core.Pod
			if err := r.Get(context.Background(), client.ObjectKey{Namespace: vm.Namespace, Name: vm.Status.PodName}, &pod); err != nil {
				t.Fatal(err)
			}
			resources := pod.Spec.Containers[0].Resources
			request := "1568Mi"
			if tc.hugepages {
				request = "512Mi"
				pages := resources.Limits["hugepages-2Mi"]
				if pages.Cmp(resource.MustParse("1Gi")) != 0 {
					t.Fatal("hugepage limit lost")
				}
			}
			if tc.request != "" {
				request = tc.request
			}
			if resources.Requests.Memory().Cmp(resource.MustParse(request)) != 0 {
				t.Fatal("memory reservation changed")
			}
			limit, present := resources.Limits[core.ResourceMemory]
			if present != (tc.limit != "") || (present && limit.Cmp(resource.MustParse(tc.limit)) != 0) {
				t.Fatal("memory limit must be explicitly requested")
			}
			var boot api.VirtualMachineSpec
			if err := json.Unmarshal([]byte(pod.Annotations[SpecAnnotation]), &boot); err != nil {
				t.Fatal(err)
			}
			if boot.Memory != "1Gi" {
				t.Fatal("host memory budget changed guest RAM")
			}
		})
	}
}

func TestCPUReservationDoesNotSetGuestSizeOrImplicitQuota(t *testing.T) {
	for _, limit := range []string{"", "4", "500m"} {
		t.Run("limit="+limit, func(t *testing.T) {
			r, vm := setup(t)
			if limit != "" {
				vm.Spec.Resources.Limits = core.ResourceList{core.ResourceCPU: resource.MustParse(limit)}
			}
			require.NoError(t, r.createPod(context.Background(), vm))
			var pod core.Pod
			require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: vm.Namespace, Name: vm.Status.PodName}, &pod))
			resources := pod.Spec.Containers[0].Resources
			var boot api.VirtualMachineSpec
			require.NoError(t, json.Unmarshal([]byte(pod.Annotations[SpecAnnotation]), &boot))
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
	require.NoError(t, r.createPod(ctx, vm))
	var original core.Pod
	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: vm.Namespace, Name: vm.Status.PodName}, &original))
	require.NoError(t, r.createPod(ctx, vm))
	var pods core.PodList
	require.NoError(t, r.List(ctx, &pods))
	require.Equal(t, []core.Pod{original}, pods.Items, "retry changed the existing incarnation")
}

func TestHugepagesUseNativeAccounting(t *testing.T) {
	r, vm := setup(t)
	vm.Spec.Hugepages = "2Mi"
	require.NoError(t, r.createPod(context.Background(), vm))
	var pods core.PodList
	r.List(context.Background(), &pods)
	resources := pods.Items[0].Spec.Containers[0].Resources
	if resources.Requests.Memory().Value() != 512<<20 {
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
	require.NoError(t, r.createPod(ctx, vm))
	var running core.Pod
	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: vm.Namespace, Name: vm.Status.PodName}, &running))
	running.Spec.NodeName = "node-a"
	require.NoError(t, r.Update(ctx, &running))

	vm.Spec.PowerState = "Stopped"
	require.NoError(t, r.Update(ctx, vm))
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

func TestGuestCannotProjectRuntimeCredentials(t *testing.T) {
	for _, source := range []core.VolumeProjection{
		{Secret: &core.SecretProjection{LocalObjectReference: core.LocalObjectReference{Name: "roamvm-object-store"}}},
		{ServiceAccountToken: &core.ServiceAccountTokenProjection{Path: "token"}},
		{PodCertificate: &core.PodCertificateProjection{}},
	} {
		for _, labelled := range []bool{false, true} {
			r, vm := setup(t)
			projection := core.ProjectedVolumeSource{Sources: []core.VolumeProjection{source}}
			if labelled {
				vm.Spec.ConfigDisks = []api.ConfigDisk{{Name: "guest", Label: "guest", Projection: projection}}
			} else {
				vm.Spec.Config = &projection
			}
			require.Error(t, r.createPod(context.Background(), vm), "guest received runtime credentials")
			var pods core.PodList
			if err := r.List(context.Background(), &pods); err != nil || len(pods.Items) != 0 {
				t.Fatal("credential-bearing Pod was created", err)
			}
		}
	}
}

func TestCheckpointedCrashStopsInsteadOfBootLooping(t *testing.T) {
	r, vm := setup(t)
	ctx := context.Background()
	if err := r.createPod(ctx, vm); err != nil {
		t.Fatal(err)
	}
	var pod core.Pod
	if err := r.Get(ctx, client.ObjectKey{Namespace: vm.Namespace, Name: vm.Status.PodName}, &pod); err != nil {
		t.Fatal(err)
	}
	pod.Spec.NodeName = "node"
	pod.Annotations[Phase] = "Stopped"
	pod.Annotations[Message] = "OOMKilled; crash-consistent working disk checkpointed"
	pod.Annotations[CheckpointAnnotation] = `{"generation":1,"key":"checkpoint"}`
	if err := r.Update(ctx, &pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = core.PodFailed
	if err := r.Status().Update(ctx, &pod); err != nil {
		t.Fatal(err)
	}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vm)}
	for range 3 {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Get(ctx, req.NamespacedName, vm); err != nil {
		t.Fatal(err)
	}
	if vm.Spec.PowerState != "Stopped" || vm.Status.Phase != "Stopped" || vm.Status.Checkpoint == nil || vm.Status.Message == "" {
		t.Fatal(vm)
	}
	var pods core.PodList
	if err := r.List(ctx, &pods); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 0 {
		t.Fatal("crashed incarnation was restarted", pods.Items)
	}
}

func TestCompletedPodDoesNotOverwriteNewStart(t *testing.T) {
	r, vm := setup(t)
	ctx := context.Background()
	if err := r.createPod(ctx, vm); err != nil {
		t.Fatal(err)
	}
	var pod core.Pod
	if err := r.Get(ctx, client.ObjectKey{Namespace: vm.Namespace, Name: vm.Status.PodName}, &pod); err != nil {
		t.Fatal(err)
	}
	pod.Spec.NodeName = "node"
	pod.Annotations[Phase] = "Stopped"
	pod.Annotations[CheckpointAnnotation] = `{"generation":1,"key":"checkpoint"}`
	if err := r.Update(ctx, &pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = core.PodFailed
	if err := r.Status().Update(ctx, &pod); err != nil {
		t.Fatal(err)
	}
	vm.Generation = 2
	if err := r.Update(ctx, vm); err != nil {
		t.Fatal(err)
	}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vm)}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, req.NamespacedName, vm); err != nil {
		t.Fatal(err)
	}
	if vm.Spec.PowerState != "Running" {
		t.Fatal("old crash canceled a newer start")
	}
}

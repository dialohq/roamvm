package controller

import (
	"context"
	"testing"
	"time"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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

func TestHibernationReservesStorageAndRejectsUnsupportedDevices(t *testing.T) {
	r, vm := setup(t, true)
	r.StorageSize = "1Gi"
	vm.Spec.RootDiskSize, vm.Spec.Memory = "2Gi", "512Mi"
	vm.Spec.Hibernate = true
	require.NoError(t, r.createPod(t.Context(), vm))
	var pod core.Pod
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Namespace: vm.Namespace, Name: vm.Status.PodName}, &pod))
	var pvc core.PersistentVolumeClaim
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Namespace: vm.Namespace, Name: WorkingClaim(&pod)}, &pvc))
	require.EqualValues(t, (3<<30)+(640<<20), pvc.Spec.Resources.Requests.Storage().Value())
	vm.Spec.Disks = []api.SecondaryDisk{{Name: "data", ClaimName: "data"}}
	require.ErrorContains(t, r.createPod(t.Context(), vm), "root-only")
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

func cachedVM(t *testing.T, r *Reconciler, name, node, size string, stopped time.Time) *api.VirtualMachine {
	t.Helper()
	when := metav1.NewTime(stopped)
	vm := &api.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name), Finalizers: []string{Finalizer}},
		Spec:       api.VirtualMachineSpec{PowerState: "Stopped", Image: "base", CPUs: 2, Memory: "1Gi"},
		Status: api.VirtualMachineStatus{
			Phase: "Stopped", Checkpoint: &api.Checkpoint{Key: "durable-" + name},
			Local: &api.LocalCheckpoint{ClaimName: name + "-disk", NodeName: node, Owner: "old-owner", Durable: true, StoppedAt: &when},
		},
	}
	require.NoError(t, r.Create(t.Context(), vm))
	pvc := &core.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: vm.Status.Local.ClaimName, Namespace: vm.Namespace},
		Spec:       core.PersistentVolumeClaimSpec{Resources: core.VolumeResourceRequirements{Requests: core.ResourceList{core.ResourceStorage: resource.MustParse(size)}}},
	}
	require.NoError(t, controllerutil.SetControllerReference(vm, pvc, r.Scheme))
	require.NoError(t, r.Create(t.Context(), pvc))
	return vm
}

func TestWarmCacheAgeAndSafety(t *testing.T) {
	for _, tc := range []struct {
		name  string
		age   time.Duration
		alter func(*api.VirtualMachine)
		busy  bool
		evict bool
	}{
		{name: "before expiry", age: time.Hour - time.Second},
		{name: "at expiry", age: time.Hour, evict: true},
		{name: "unpublished", age: 2 * time.Hour, alter: func(v *api.VirtualMachine) { v.Status.Local.Durable = false }},
		{name: "missing checkpoint", age: 2 * time.Hour, alter: func(v *api.VirtualMachine) { v.Status.Checkpoint = nil }},
		{name: "running request", age: 2 * time.Hour, alter: func(v *api.VirtualMachine) { v.Spec.PowerState = "Running" }},
		{name: "recovery required", age: 2 * time.Hour, alter: func(v *api.VirtualMachine) { v.Status.Phase = "RecoveryRequired" }},
		{name: "unbound claim user", age: 2 * time.Hour, busy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := setup(t, true)
			r.WarmTTL = time.Hour
			now := time.Now().Truncate(time.Second)
			vm := cachedVM(t, r, "cached", "node-a", "10Gi", now.Add(-tc.age))
			if tc.alter != nil {
				tc.alter(vm)
				require.NoError(t, r.Update(t.Context(), vm))
				tc.alter(vm) // The status subresource is not changed by Update.
				require.NoError(t, r.Status().Update(t.Context(), vm))
			}
			claim := client.ObjectKey{Namespace: vm.Namespace, Name: vm.Status.Local.ClaimName}
			if tc.busy {
				require.NoError(t, r.Create(t.Context(), &core.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "unbound", Namespace: vm.Namespace},
					Spec:       core.PodSpec{Volumes: []core.Volume{{Name: "disk", VolumeSource: core.VolumeSource{PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: claim.Name}}}}},
				}))
			}
			require.NoError(t, r.collectWarm(t.Context(), now))
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(vm), vm))
			require.Equal(t, tc.evict, vm.Status.Local.Evicting)
			if !tc.evict {
				require.NoError(t, r.Get(t.Context(), claim, &core.PersistentVolumeClaim{}))
				return
			}
			// A wake after intent must complete eviction, never reuse the old disk.
			vm.Spec.PowerState = "Running"
			require.NoError(t, r.Update(t.Context(), vm))
			reconcileVM(t, r, vm, 2)
			require.Nil(t, vm.Status.Local)
			require.Equal(t, "durable-cached", vm.Status.Checkpoint.Key)
			require.True(t, apierrors.IsNotFound(r.Get(t.Context(), claim, &core.PersistentVolumeClaim{})))
			reconcileVM(t, r, vm, 1)
			var pod core.Pod
			require.NoError(t, r.Get(t.Context(), client.ObjectKey{Namespace: vm.Namespace, Name: vm.Status.PodName}, &pod))
			require.NotEqual(t, claim.Name, WorkingClaim(&pod))
			require.Empty(t, pod.Annotations[LocalOwnerAnnotation])
		})
	}
}

func TestWarmBudgetKeepsNewestReservationsPerNode(t *testing.T) {
	r, _ := setup(t, true)
	r.WarmBytes = 26 << 30
	now := time.Now()
	newest := cachedVM(t, r, "newest", "node-a", "12Gi", now.Add(-time.Minute))
	middle := cachedVM(t, r, "middle", "node-a", "10Gi", now.Add(-2*time.Minute))
	oldest := cachedVM(t, r, "oldest", "node-a", "5Gi", now.Add(-3*time.Minute))
	other := cachedVM(t, r, "other-node", "node-b", "20Gi", now.Add(-4*time.Minute))
	// Provisioned capacity can exceed the request; budget the larger value.
	pvc := &core.PersistentVolumeClaim{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Namespace: newest.Namespace, Name: newest.Status.Local.ClaimName}, pvc))
	pvc.Status.Capacity = core.ResourceList{core.ResourceStorage: resource.MustParse("16Gi")}
	require.NoError(t, r.Status().Update(t.Context(), pvc))
	require.NoError(t, r.collectWarm(t.Context(), now))
	for _, vm := range []*api.VirtualMachine{newest, middle, oldest, other} {
		require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(vm), vm))
		require.Equal(t, vm.Name == "oldest", vm.Status.Local.Evicting, vm.Name)
	}
	// Restarting collection does not invent another victim or reset cache ages.
	require.NoError(t, r.collectWarm(t.Context(), now.Add(time.Minute)))
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(middle), middle))
	require.False(t, middle.Status.Local.Evicting)
}

func TestEvictionWaitsForClaimUsersAndDeletion(t *testing.T) {
	r, _ := setup(t, true)
	r.WarmTTL = time.Hour
	now := time.Now()
	vm := cachedVM(t, r, "cached", "node-a", "10Gi", now.Add(-2*time.Hour))
	require.NoError(t, r.collectWarm(t.Context(), now))
	key := client.ObjectKey{Namespace: vm.Namespace, Name: vm.Status.Local.ClaimName}
	var pvc core.PersistentVolumeClaim
	require.NoError(t, r.Get(t.Context(), key, &pvc))
	pvc.Finalizers = []string{"test/hold-deletion"}
	require.NoError(t, r.Update(t.Context(), &pvc))
	// A claim consumer that appears after intent still prevents deletion.
	pod := &core.Pod{ObjectMeta: metav1.ObjectMeta{Name: "claim-user", Namespace: vm.Namespace}, Spec: core.PodSpec{
		Volumes: []core.Volume{{Name: "working", VolumeSource: core.VolumeSource{PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: key.Name}}}},
	}}
	require.NoError(t, r.Create(t.Context(), pod))
	reconcileVM(t, r, vm, 1)
	require.NoError(t, r.Get(t.Context(), key, &pvc))
	require.Nil(t, pvc.DeletionTimestamp)
	require.NoError(t, r.Delete(t.Context(), pod))
	reconcileVM(t, r, vm, 2)
	require.NoError(t, r.Get(t.Context(), key, &pvc))
	require.NotNil(t, pvc.DeletionTimestamp)
	require.NotNil(t, vm.Status.Local, "cleared the disk before deletion completed")
	pvc.Finalizers = nil
	require.NoError(t, r.Update(t.Context(), &pvc))
	reconcileVM(t, r, vm, 1)
	require.Nil(t, vm.Status.Local)
	require.NotNil(t, vm.Status.Checkpoint)
}

func TestWakeInvalidatesStaleEvictionDecision(t *testing.T) {
	r, _ := setup(t, true)
	r.WarmTTL = time.Hour
	now := time.Now()
	vm := cachedVM(t, r, "cached", "node-a", "10Gi", now.Add(-2*time.Hour))
	r.APIReader = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			if _, ok := list.(*api.VirtualMachineList); ok {
				// Collection sees the old stopped object; wake wins before intent.
				vm.Spec.PowerState = "Running"
				require.NoError(t, c.Update(ctx, vm))
			}
			return nil
		},
	})
	require.NoError(t, r.collectWarm(t.Context(), now))
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(vm), vm))
	require.Equal(t, "Running", vm.Spec.PowerState)
	require.False(t, vm.Status.Local.Evicting)
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Namespace: vm.Namespace, Name: vm.Status.Local.ClaimName}, &core.PersistentVolumeClaim{}))
}

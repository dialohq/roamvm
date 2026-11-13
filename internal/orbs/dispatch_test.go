package orbs

import (
	"context"
	"testing"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestProfiles(t *testing.T) {
	for _, tc := range []struct {
		name string
		cpu  int32
		ram  string
	}{
		{"small", 2, "4Gi"}, {"medium", 4, "8Gi"}, {"", 4, "8Gi"}, {"large", 8, "16Gi"},
	} {
		spec, err := Spec(tc.name, "base")
		require.NoError(t, err)
		require.Equal(t, tc.cpu, spec.CPUs)
		require.Equal(t, tc.ram, spec.Memory)
		require.Equal(t, "64Gi", spec.RootDiskSize)
		require.True(t, spec.Hibernate && spec.GuestAgent)
	}
	_, err := Spec("typo", "base")
	require.Error(t, err)
}

func TestDispatchPreparedSnapshotAndResume(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, api.AddV2ToScheme(scheme))
	snapshot := &api.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: Name("snapshot", "prepared"), Namespace: "default", UID: "template-uid", Annotations: map[string]string{identity: "snapshot:prepared"}},
		Spec:       api.VirtualMachineSpec{Image: "old-base", PowerState: "Stopped"},
		Status:     api.VirtualMachineStatus{Phase: "Stopped", Checkpoint: &api.Checkpoint{Key: "template-checkpoint"}},
	}
	meta.SetStatusCondition(&snapshot.Status.Conditions, metav1.Condition{Type: "CheckpointReady", Status: metav1.ConditionTrue, Reason: "Stopped"})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snapshot).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			vm := obj.(*api.VirtualMachine)
			if vm.Spec.PowerState == "Running" {
				vm.Status.Phase, vm.Status.ObservedGeneration = "Running", vm.Generation
				meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Running"})
			}
			return nil
		},
	}).Build()
	var bootstraps int
	d := Dispatcher{Client: c, Namespace: "default", Image: "new-base", Bootstrap: func(_ context.Context, vm *api.VirtualMachine, token, ampURL string) error {
		bootstraps++
		require.Equal(t, "fresh-token", token)
		require.Equal(t, "https://amp.test", ampURL)
		require.Equal(t, "old-base", vm.Spec.Image)
		require.Equal(t, &api.CloneSource{Name: snapshot.Name, UID: string(snapshot.UID)}, vm.Spec.CloneFrom)
		return nil
	}}
	job := Job{Operation: "spawn", ID: "thread/with:unsafe chars", SnapshotID: "prepared", Type: "small", Token: "fresh-token", AMPURL: "https://amp.test"}
	require.NoError(t, d.Run(t.Context(), job))
	require.NoError(t, d.Run(t.Context(), job))
	require.Equal(t, 2, bootstraps, "resume must refresh the thread credential")
	var list api.VirtualMachineList
	require.NoError(t, c.List(t.Context(), &list))
	require.Len(t, list.Items, 2, "repeated spawn created another VM")
	job.Type = "large"
	require.ErrorContains(t, d.Run(t.Context(), job), "type cannot change")
	job.Operation = "delete"
	require.NoError(t, d.Run(t.Context(), job))
	require.NoError(t, d.Run(t.Context(), job))
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(snapshot), snapshot), "orb deletion removed the project snapshot")
}

func TestDispatchRejectsForeignVM(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, api.AddV2ToScheme(scheme))
	vm := &api.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: Name("orb", "thread"), Namespace: "default"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vm).Build()
	d := Dispatcher{Client: c, Namespace: "default", Bootstrap: func(context.Context, *api.VirtualMachine, string, string) error {
		t.Fatal("foreign VM bootstrapped")
		return nil
	}}
	for _, operation := range []string{"spawn", "pause", "delete"} {
		require.ErrorContains(t, d.Run(t.Context(), Job{Operation: operation, ID: "thread", Token: "token"}), "foreign VM")
	}
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(vm), vm))
}

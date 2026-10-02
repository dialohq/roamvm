package controller

import (
	"encoding/json"
	"testing"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func reconcileVM(t *testing.T, r *Reconciler, vm *api.VirtualMachine, times int) {
	t.Helper()
	for range times {
		_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vm)})
		require.NoError(t, err)
	}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(vm), vm))
}

func localStopFixture(t *testing.T) (*Reconciler, *api.VirtualMachine, *core.Pod) {
	t.Helper()
	r, vm := setup(t)
	require.NoError(t, r.createPod(t.Context(), vm))
	pod := &core.Pod{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Namespace: vm.Namespace, Name: vm.Status.PodName}, pod))
	pod.UID = "stopped-owner"
	pod.Spec.NodeName = "node-a"
	pod.Annotations[Phase] = "Stopped"
	pod.Annotations[LocalStopAnnotation] = string(pod.UID)
	require.NoError(t, r.Update(t.Context(), pod))
	pod.Status.Phase = core.PodSucceeded
	require.NoError(t, r.Status().Update(t.Context(), pod))
	vm.Spec.PowerState = "Stopped"
	require.NoError(t, r.Update(t.Context(), vm))
	require.NoError(t, r.Create(t.Context(), &core.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}, Status: core.NodeStatus{Conditions: []core.NodeCondition{{Type: core.NodeReady, Status: core.ConditionTrue}}}}))
	reconcileVM(t, r, vm, 3)
	return r, vm, pod
}

func TestLocalStopReleasesRunnerButRetainsDiskAndRecreatesWorker(t *testing.T) {
	r, vm, old := localStopFixture(t)
	require.Equal(t, "Stopped", vm.Status.Phase)
	require.Nil(t, vm.Status.Checkpoint)
	require.NotNil(t, vm.Status.Local)
	require.False(t, vm.Status.Local.Durable)
	require.True(t, meta.IsStatusConditionFalse(vm.Status.Conditions, "CheckpointReady"))
	require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKeyFromObject(old), &core.Pod{})))
	var pvc core.PersistentVolumeClaim
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Namespace: vm.Namespace, Name: vm.Status.Local.ClaimName}, &pvc))
	require.True(t, metav1.IsControlledBy(&pvc, vm))
	worker := &core.Pod{}
	key := client.ObjectKey{Namespace: vm.Namespace, Name: workerName(vm, vm.Status.Local.Owner)}
	require.NoError(t, r.Get(t.Context(), key, worker))
	require.Len(t, worker.Spec.Containers, 1)
	require.Equal(t, []string{"checkpoint-worker"}, worker.Spec.Containers[0].Args)
	require.NotContains(t, worker.Spec.Containers[0].Resources.Limits, core.ResourceName("vm.roamvm.io/kvm"))
	require.Equal(t, core.RestartPolicyOnFailure, worker.Spec.RestartPolicy)
	require.Equal(t, vm.Status.Local.ClaimName, WorkingClaim(worker))
	require.NoError(t, r.Delete(t.Context(), worker))
	reconcileVM(t, r, vm, 1)
	require.NoError(t, r.Get(t.Context(), key, worker), "lost background worker must be recreated")
}

func TestLocalRestartKeepsUploadUntilScheduledAndReusesClaim(t *testing.T) {
	r, vm, _ := localStopFixture(t)
	claim := vm.Status.Local.ClaimName
	vm.Spec.PowerState = "Running"
	vm.Generation++
	vm.Spec.NodeSelector = map[string]string{"workload": "vm"}
	require.NoError(t, r.Update(t.Context(), vm))
	reconcileVM(t, r, vm, 1)
	pod := &core.Pod{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Namespace: vm.Namespace, Name: vm.Status.PodName}, pod))
	require.Equal(t, claim, WorkingClaim(pod))
	require.Equal(t, "stopped-owner", pod.Annotations[LocalOwnerAnnotation])
	require.Equal(t, vm.Spec.NodeSelector, pod.Spec.NodeSelector)
	require.Empty(t, pod.Spec.NodeName, "scheduler still admits local restarts")
	require.Equal(t, "node-a", pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchFields[0].Values[0])
	key := client.ObjectKey{Namespace: vm.Namespace, Name: workerName(vm, vm.Status.Local.Owner)}
	require.NoError(t, r.Get(t.Context(), key, &core.Pod{}))
	pod.Spec.NodeName = "node-a"
	require.NoError(t, r.Update(t.Context(), pod))
	reconcileVM(t, r, vm, 1)
	require.True(t, apierrors.IsNotFound(r.Get(t.Context(), key, &core.Pod{})), "assigned restart cancels worker")
	var claims core.PersistentVolumeClaimList
	require.NoError(t, r.List(t.Context(), &claims))
	require.Len(t, claims.Items, 1, "local restart must not reserve another disk")
}

func completeWorker(t *testing.T, r *Reconciler, vm *api.VirtualMachine) {
	t.Helper()
	worker := &core.Pod{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Namespace: vm.Namespace, Name: workerName(vm, vm.Status.Local.Owner)}, worker))
	b, err := json.Marshal(api.Checkpoint{Generation: 1, Key: "verified-object", SHA256: "hash", Size: 100})
	require.NoError(t, err)
	worker.Annotations[CheckpointReadyAnnotation] = "true"
	worker.Annotations[CheckpointAnnotation] = string(b)
	require.NoError(t, r.Update(t.Context(), worker))
	reconcileVM(t, r, vm, 1)
	require.True(t, vm.Status.Local.Durable)
}

func TestRemoteFallbackWaitsForLatestCheckpoint(t *testing.T) {
	for _, cause := range []string{"cordoned", "unschedulable"} {
		t.Run(cause, func(t *testing.T) {
			r, vm, _ := localStopFixture(t)
			oldClaim := vm.Status.Local.ClaimName
			vm.Spec.PowerState = "Running"
			vm.Generation++
			require.NoError(t, r.Update(t.Context(), vm))
			if cause == "cordoned" {
				node := &core.Node{}
				require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: "node-a"}, node))
				node.Spec.Unschedulable = true
				require.NoError(t, r.Update(t.Context(), node))
			} else {
				reconcileVM(t, r, vm, 1)
				pod := &core.Pod{}
				require.NoError(t, r.Get(t.Context(), client.ObjectKey{Namespace: vm.Namespace, Name: vm.Status.PodName}, pod))
				pod.Status.Conditions = []core.PodCondition{{Type: core.PodScheduled, Status: core.ConditionFalse, Reason: core.PodReasonUnschedulable}}
				require.NoError(t, r.Status().Update(t.Context(), pod))
			}
			reconcileVM(t, r, vm, 2)
			require.NotNil(t, vm.Status.Local, "uncommitted cache must not be discarded")
			require.Nil(t, vm.Status.Checkpoint)
			completeWorker(t, r, vm)
			reconcileVM(t, r, vm, 3)
			require.Nil(t, vm.Status.Local)
			pod := &core.Pod{}
			require.NoError(t, r.Get(t.Context(), client.ObjectKey{Namespace: vm.Namespace, Name: vm.Status.PodName}, pod))
			require.NotEqual(t, oldClaim, WorkingClaim(pod))
			require.Empty(t, pod.Annotations[LocalOwnerAnnotation])
			require.Nil(t, pod.Spec.Affinity)
		})
	}
}

func TestStaleWorkerCannotMarkNewerStopDurable(t *testing.T) {
	r, vm, _ := localStopFixture(t)
	worker := &core.Pod{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Namespace: vm.Namespace, Name: workerName(vm, vm.Status.Local.Owner)}, worker))
	worker.Annotations[CheckpointReadyAnnotation] = "true"
	worker.Annotations[CheckpointAnnotation] = `{"key":"stale"}`
	require.NoError(t, r.Update(t.Context(), worker))
	vm.Status.Local.Owner = "new-owner"
	require.NoError(t, r.Status().Update(t.Context(), vm))
	reconcileVM(t, r, vm, 1)
	require.False(t, vm.Status.Local.Durable)
	require.Nil(t, vm.Status.Checkpoint)
}

func TestDeletionWaitsForBackgroundDurability(t *testing.T) {
	r, vm, _ := localStopFixture(t)
	require.NoError(t, r.Delete(t.Context(), vm))
	reconcileVM(t, r, vm, 2)
	require.Contains(t, vm.Finalizers, Finalizer)
	completeWorker(t, r, vm)
	for range 4 {
		_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vm)})
		require.NoError(t, err)
	}
	require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKeyFromObject(vm), &api.VirtualMachine{})))
}

func TestRetainedCacheDoesNotHideVanishedRunningVM(t *testing.T) {
	r, vm, _ := localStopFixture(t)
	vm.Status.Phase = "Running"
	vm.Status.PodName = "vanished-new-runner"
	require.NoError(t, r.Status().Update(t.Context(), vm))
	reconcileVM(t, r, vm, 1)
	require.Equal(t, "RecoveryRequired", vm.Status.Phase)
}

func TestQueuedCancellationReleasesOnlyFreshClaim(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "cached"}[cached], func(t *testing.T) {
			var r *Reconciler
			var vm *api.VirtualMachine
			if cached {
				r, vm, _ = localStopFixture(t)
				vm.Spec.PowerState = "Running"
				vm.Generation++
				require.NoError(t, r.Update(t.Context(), vm))
			} else {
				r, vm = setup(t)
			}
			reconcileVM(t, r, vm, 1)
			pod := &core.Pod{}
			require.NoError(t, r.Get(t.Context(), client.ObjectKey{Namespace: vm.Namespace, Name: vm.Status.PodName}, pod))
			require.Empty(t, pod.Spec.NodeName)
			claim := client.ObjectKey{Namespace: vm.Namespace, Name: WorkingClaim(pod)}
			vm.Spec.PowerState = "Stopped"
			require.NoError(t, r.Update(t.Context(), vm))
			reconcileVM(t, r, vm, 2)
			err := r.Get(t.Context(), claim, &core.PersistentVolumeClaim{})
			if cached {
				require.NoError(t, err, "cancelled restart must preserve stopped data")
			} else {
				require.True(t, apierrors.IsNotFound(err), "cancelled start must release unused storage")
			}
		})
	}
}

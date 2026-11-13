package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type warmCache struct{ *Reconciler }

func (*warmCache) NeedLeaderElection() bool { return true }

func (c *warmCache) Start(ctx context.Context) error {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		if err := c.collectWarm(ctx, time.Now()); err != nil && ctx.Err() == nil {
			ctrl.LoggerFrom(ctx).Error(err, "warm cache collection failed; retained local data")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func (r *Reconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func claimInUse(pods []core.Pod, namespace, claim string) bool {
	for _, pod := range pods {
		if pod.Namespace != namespace {
			continue
		}
		for _, v := range pod.Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim {
				// Even an unbound or terminating Pod can still use this disk.
				return true
			}
		}
	}
	return false
}

// collectWarm budgets paused PVC reservations, not running disks or free bytes
// on the host filesystem. The storage provisioner still admits live volumes.
// Only the per-VM reconciler deletes claims, after a durable eviction intent.
func (r *Reconciler) collectWarm(ctx context.Context, now time.Time) error {
	if !api.IsV2(r.Scheme) || (r.WarmTTL == 0 && r.WarmBytes == 0) {
		return nil
	}
	var vms api.VirtualMachineList
	var pods core.PodList
	var claims core.PersistentVolumeClaimList
	for _, list := range []client.ObjectList{&vms, &pods, &claims} {
		if err := r.reader().List(ctx, list); err != nil {
			return err
		}
	}
	pvcs := map[client.ObjectKey]*core.PersistentVolumeClaim{}
	for i := range claims.Items {
		pvc := &claims.Items[i]
		pvcs[client.ObjectKeyFromObject(pvc)] = pvc
	}
	type entry struct {
		vm    *api.VirtualMachine
		bytes int64
	}
	var candidates []entry
	pinned := map[string]int64{}
	for i := range vms.Items {
		vm := &vms.Items[i]
		local := vm.Status.Local
		if local == nil || local.Evicting || vm.Spec.PowerState != "Stopped" || vm.DeletionTimestamp != nil || vm.Status.Phase != "Stopped" {
			continue
		}
		pvc := pvcs[client.ObjectKey{Namespace: vm.Namespace, Name: local.ClaimName}]
		if pvc == nil || pvc.DeletionTimestamp != nil || !metav1.IsControlledBy(pvc, vm) {
			continue
		}
		bytes := max(pvc.Spec.Resources.Requests.Storage().Value(), pvc.Status.Capacity.Storage().Value())
		if !local.Durable || vm.Status.Checkpoint == nil || vm.Status.PodName != "" || claimInUse(pods.Items, vm.Namespace, local.ClaimName) {
			pinned[local.NodeName] += bytes
			continue
		}
		if local.StoppedAt == nil {
			// Existing caches have no trustworthy age. Start their timer now.
			timestamp := metav1.NewTime(now)
			local.StoppedAt = &timestamp
			if err := r.Status().Update(ctx, vm); err != nil {
				return err
			}
		}
		candidates = append(candidates, entry{vm, bytes})
	}
	// Newest stopped caches win. Deterministic ties prevent competing victims.
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i].vm, candidates[j].vm
		if a.Status.Local.StoppedAt.Equal(b.Status.Local.StoppedAt) {
			return string(a.UID) < string(b.UID)
		}
		return a.Status.Local.StoppedAt.After(b.Status.Local.StoppedAt.Time)
	})
	for _, candidate := range candidates {
		vm := candidate.vm
		local := vm.Status.Local
		expired := r.WarmTTL > 0 && !now.Before(local.StoppedAt.Add(r.WarmTTL))
		overBudget := r.WarmBytes > 0 && candidate.bytes > r.WarmBytes-pinned[local.NodeName]
		if !expired && !overBudget {
			pinned[local.NodeName] += candidate.bytes
			continue
		}
		// A concurrent spawn changes the VM resourceVersion and invalidates this
		// status update. Once committed, Reconcile cannot create a local runner.
		local.Evicting = true
		if err := r.Status().Update(ctx, vm); err != nil && !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *Reconciler) evictLocal(ctx context.Context, vm *api.VirtualMachine) (ctrl.Result, error) {
	local := vm.Status.Local
	if !local.Durable || vm.Status.Checkpoint == nil {
		return ctrl.Result{}, fmt.Errorf("refusing to evict a checkpoint without remote durability")
	}
	var pods core.PodList
	if err := r.reader().List(ctx, &pods, client.InNamespace(vm.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	if claimInUse(pods.Items, vm.Namespace, local.ClaimName) {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	pvc := &core.PersistentVolumeClaim{}
	err := r.reader().Get(ctx, client.ObjectKey{Namespace: vm.Namespace, Name: local.ClaimName}, pvc)
	if apierrors.IsNotFound(err) {
		vm.Status.Local = nil
		return r.status(ctx, vm, "Stopped", "Warm cache evicted; checkpoint retained in object storage", nil)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if !metav1.IsControlledBy(pvc, vm) {
		return ctrl.Result{}, fmt.Errorf("refusing to evict a foreign PVC")
	}
	if pvc.DeletionTimestamp == nil {
		err = r.Delete(ctx, pvc, client.Preconditions{UID: &pvc.UID, ResourceVersion: &pvc.ResourceVersion})
	}
	return ctrl.Result{RequeueAfter: time.Second}, client.IgnoreNotFound(err)
}

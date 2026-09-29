package controller

import (
	"context"
	"errors"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/dialohq/roamvm/internal/state"
	core "k8s.io/api/core/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Recover is an operator action, called only with an explicit assertion that
// the previous VMM is terminated or its node fenced. It is never a reconciler.
func Recover(ctx context.Context, c client.Client, m state.Manager, id, owner string) (state.Session, error) {
	head, err := m.Read(ctx, id)
	missing := errors.Is(err, state.ErrNotFound)
	if err != nil && !missing {
		return state.Session{}, err
	}
	if missing {
		head.Head = state.Head{Schema: 1, VMID: id, State: "Stopped"}
	}
	if head.Head.Owner != owner && head.Head.State != "Stopped" {
		return state.Session{}, state.ErrConflict
	}
	var vms api.VirtualMachineList
	if err = c.List(ctx, &vms); err != nil {
		return state.Session{}, err
	}
	var vm *api.VirtualMachine
	for i := range vms.Items {
		if string(vms.Items[i].UID) == id {
			vm = &vms.Items[i]
			break
		}
	}
	if vm == nil {
		return state.Session{}, errors.New("VM UID not found in this Kubernetes cluster")
	}
	if missing {
		head.Head.Base = vm.Spec.Image
	}
	old := vm.DeepCopy()
	vm.Spec.PowerState = "Stopped"
	if err = c.Patch(ctx, vm, client.MergeFrom(old)); err != nil {
		return state.Session{}, err
	}
	var pods core.PodList
	if err = c.List(ctx, &pods, client.InNamespace(vm.Namespace), client.MatchingLabels{Label: id}); err != nil {
		return state.Session{}, err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if string(pod.UID) != owner {
			return state.Session{}, errors.New("a different runner exists; refusing recovery")
		}
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var current core.Pod
			if e := c.Get(ctx, client.ObjectKeyFromObject(pod), &current); e != nil {
				return client.IgnoreNotFound(e)
			}
			if current.UID != pod.UID {
				return errors.New("runner changed during recovery")
			}
			controllerutil.RemoveFinalizer(&current, Finalizer)
			return c.Update(ctx, &current)
		})
		if err != nil {
			return state.Session{}, err
		}
		if err = c.Delete(ctx, pod, client.GracePeriodSeconds(0)); client.IgnoreNotFound(err) != nil {
			return state.Session{}, err
		}
		secret := &core.Secret{}
		secret.Name = pod.Annotations[SecretAnnotation]
		secret.Namespace = pod.Namespace
		if secret.Name != "" {
			if err = c.Delete(ctx, secret); client.IgnoreNotFound(err) != nil {
				return state.Session{}, err
			}
		}
	}
	if head.Head.Owner != "" {
		head, err = m.Recover(ctx, id, owner)
		if err != nil {
			return state.Session{}, err
		}
	}
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &api.VirtualMachine{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(vm), current); err != nil {
			return err
		}
		if current.UID != vm.UID {
			return errors.New("VM was replaced")
		}
		current.Status.Phase = "Stopped"
		current.Status.Message = "Recovered last durable generation after explicit fencing; uncommitted changes were discarded"
		current.Status.PodName = ""
		current.Status.NodeName = ""
		current.Status.Checkpoint = head.Head.Checkpoint
		return c.Status().Update(ctx, current)
	})

	return head, err
}

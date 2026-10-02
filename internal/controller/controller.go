package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	Label                     = "vm.roamvm.io/uid"
	Phase                     = "vm.roamvm.io/phase"
	Stop                      = "vm.roamvm.io/stop"
	Finalizer                 = "vm.roamvm.io/checkpoint"
	CheckpointAnnotation      = "vm.roamvm.io/checkpoint"
	Message                   = "vm.roamvm.io/message"
	DiskSizeAnnotation        = "vm.roamvm.io/root-disk-size"
	ResizeAnnotation          = "vm.roamvm.io/resize-error"
	GenerationAnnotation      = "vm.roamvm.io/boot-generation"
	ExitAnnotation            = "vm.roamvm.io/runner-exit"
	SpecAnnotation            = "vm.roamvm.io/boot-spec"
	LocalStopAnnotation       = "vm.roamvm.io/local-stop"
	LocalOwnerAnnotation      = "vm.roamvm.io/local-owner"
	CheckpointReadyAnnotation = "vm.roamvm.io/checkpoint-ready"
	WorkerLabel               = "vm.roamvm.io/checkpoint-worker"
)

type Reconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Image        string
	StorageClass string
	StorageSize  string
}

func (r *Reconciler) Setup(m ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(m).For(&api.VirtualMachine{}).Owns(&core.Pod{}).Owns(&core.PersistentVolumeClaim{}).Complete(r)
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	defer func() {
		if apierrors.IsConflict(err) {
			// Recompute from fresh state rather than retrying a stale lifecycle decision.
			result, err = ctrl.Result{RequeueAfter: time.Second}, nil
		} else if err != nil {
			result = ctrl.Result{}
		}
	}()
	var vm api.VirtualMachine
	if err := r.Get(ctx, req.NamespacedName, &vm); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if vm.DeletionTimestamp == nil && !controllerutil.ContainsFinalizer(&vm, Finalizer) {
		before := vm.DeepCopy()
		controllerutil.AddFinalizer(&vm, Finalizer)
		return ctrl.Result{}, r.Patch(
			ctx,
			&vm,
			client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}),
		)
	}
	var pods core.PodList
	if err := r.List(ctx, &pods, client.InNamespace(vm.Namespace), client.MatchingLabels{Label: string(vm.UID)}); err != nil {
		return ctrl.Result{}, err
	}
	var runners, workers []core.Pod
	for i := range pods.Items {
		if !metav1.IsControlledBy(&pods.Items[i], &vm) {
			return r.status(ctx, &vm, "Blocked", "Pod is not owned by this VM", nil)
		}
		if pods.Items[i].Labels[WorkerLabel] == "true" {
			workers = append(workers, pods.Items[i])
		} else {
			runners = append(runners, pods.Items[i])
		}
	}
	// Accept completion only from the worker for the currently retained owner.
	for i := range workers {
		worker := &workers[i]
		if vm.Status.Local != nil && worker.Annotations[LocalOwnerAnnotation] == vm.Status.Local.Owner &&
			WorkingClaim(worker) == vm.Status.Local.ClaimName && worker.Spec.NodeName == vm.Status.Local.NodeName &&
			worker.Annotations[CheckpointReadyAnnotation] == "true" {
			var cp api.Checkpoint
			if err := json.Unmarshal([]byte(worker.Annotations[CheckpointAnnotation]), &cp); err != nil {
				return ctrl.Result{}, err
			}
			if !vm.Status.Local.Durable || vm.Status.Checkpoint == nil || !equality.Semantic.DeepEqual(*vm.Status.Checkpoint, cp) {
				vm.Status.Local.Durable = true
				vm.Status.Checkpoint = &cp
				if err := r.Status().Update(ctx, &vm); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
		}
	}
	if len(runners) > 1 {
		return r.status(ctx, &vm, "Blocked", "multiple runner Pods; refusing to select an owner", nil)
	}
	stopping := vm.Spec.PowerState == "Stopped" || vm.DeletionTimestamp != nil
	for i := range workers {
		if vm.Status.Local == nil || workers[i].Annotations[LocalOwnerAnnotation] != vm.Status.Local.Owner || vm.Status.Local.Durable {
			if workers[i].DeletionTimestamp == nil {
				if err := client.IgnoreNotFound(r.Delete(ctx, &workers[i])); err != nil {
					return ctrl.Result{}, err
				}
			}
		}
	}
	if len(runners) == 0 {
		// An older cached checkpoint must never disguise a vanished active VM.
		if vm.Status.PodName != "" && vm.Status.Phase != "" && vm.Status.Phase != "Pending" && vm.Status.Phase != "Stopped" {
			return r.status(ctx, &vm, "RecoveryRequired", "runner vanished; fence its runtime before recovery", nil)
		}
		if vm.Status.Local != nil && !vm.Status.Local.Durable {
			if err := r.createWorker(ctx, &vm); err != nil {
				return ctrl.Result{}, err
			}
		}
		if stopping {
			if vm.Status.Local != nil && !vm.Status.Local.Durable {
				return r.status(ctx, &vm, "Stopped", vm.Status.Message, nil)
			}
			if vm.Status.Local == nil && vm.Status.Phase != "" && vm.Status.Phase != "Stopped" {
				return r.status(
					ctx,
					&vm,
					"RecoveryRequired",
					"runner vanished without a committed checkpoint; fence its runtime before recovery",
					nil,
				)
			}
			if vm.DeletionTimestamp != nil {
				for i := range workers {
					if err := client.IgnoreNotFound(r.Delete(ctx, &workers[i])); err != nil {
						return ctrl.Result{}, err
					}
				}
				if len(workers) > 0 {
					return ctrl.Result{RequeueAfter: time.Second}, nil
				}
				if vm.Status.Local != nil {
					pvc := &core.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: vm.Status.Local.ClaimName, Namespace: vm.Namespace}}
					if err := client.IgnoreNotFound(r.Delete(ctx, pvc)); err != nil {
						return ctrl.Result{}, err
					}
				}
				before := vm.DeepCopy()
				controllerutil.RemoveFinalizer(&vm, Finalizer)
				return ctrl.Result{}, r.Patch(
					ctx,
					&vm,
					client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}),
				)
			}
			return r.status(ctx, &vm, "Stopped", vm.Status.Message, nil)
		}
		if vm.Status.Local != nil {
			unavailable, err := r.localNodeUnavailable(ctx, vm.Status.Local.NodeName)
			if err != nil {
				return ctrl.Result{}, err
			}
			var pvc core.PersistentVolumeClaim
			if err := r.Get(ctx, client.ObjectKey{Namespace: vm.Namespace, Name: vm.Status.Local.ClaimName}, &pvc); err != nil {
				if !apierrors.IsNotFound(err) {
					return ctrl.Result{}, err
				}
				unavailable = true
			} else if pvc.DeletionTimestamp != nil {
				unavailable = true
			}
			if unavailable {
				if !vm.Status.Local.Durable {
					return r.status(ctx, &vm, "Pending", "local checkpoint unavailable; waiting for durable checkpoint or explicit recovery", nil)
				}
				if err := r.discardLocal(ctx, &vm); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
		}
		if vm.Status.Phase != "" && vm.Status.Phase != "Stopped" && vm.Status.Phase != "Pending" {
			return r.status(
				ctx,
				&vm,
				"RecoveryRequired",
				"previous runner disappeared; automatic ownership takeover is disabled",
				nil,
			)
		}
		// Persist one runner name before creation; informer lag must not create
		// two incarnations while the first Pod is not yet visible in the cache.
		if vm.Status.PodName == "" {
			suffix := make([]byte, 6)
			rand.Read(suffix)
			name := vm.Name
			if len(name) > 40 {
				name = name[:40]
			}
			vm.Status.PodName = name + "-" + hex.EncodeToString(suffix)
			if err := r.Status().Update(ctx, &vm); err != nil {
				return ctrl.Result{}, err
			}
		}
		// Persist start intent before a Pod can acquire durable ownership.
		if _, err := r.status(ctx, &vm, "Pending", "", nil); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.createPod(ctx, &vm); err != nil {
			return r.status(ctx, &vm, "Blocked", err.Error(), nil)
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	pod := &runners[0]
	// Never trust a label alone to establish ownership.
	if !metav1.IsControlledBy(pod, &vm) {
		return r.status(ctx, &vm, "Blocked", "runner is not owned by this VM", nil)
	}
	if !stopping && vm.Status.Local != nil && pod.Spec.NodeName == "" && podUnschedulable(pod) {
		if !vm.Status.Local.Durable {
			if err := r.createWorker(ctx, &vm); err != nil {
				return ctrl.Result{}, err
			}
			return r.status(ctx, &vm, "Pending", "local restart is unschedulable; waiting for durable checkpoint", pod)
		}
		if err := client.IgnoreNotFound(r.Delete(ctx, pod, client.Preconditions{UID: &pod.UID, ResourceVersion: &pod.ResourceVersion})); err != nil {
			return ctrl.Result{}, err
		}
		// Deletion prevents a scheduler bind. Refetch before removing the
		// finalizer; never authorize cache deletion from a stale unbound Pod.
		if err := r.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
			return ctrl.Result{}, err
		}
		if _, err := r.releasePod(ctx, pod); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.discardLocal(ctx, &vm); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	// An unassigned Pod cannot have acquired state. Delete with an RV
	// precondition so a concurrent scheduler binding invalidates this decision.
	if (stopping || pod.DeletionTimestamp != nil) && pod.Spec.NodeName == "" {
		if pod.DeletionTimestamp == nil {
			err := r.Delete(ctx, pod, client.Preconditions{UID: &pod.UID, ResourceVersion: &pod.ResourceVersion})
			return ctrl.Result{RequeueAfter: time.Second}, client.IgnoreNotFound(err)
		}
		// Fresh explicit claims no longer have Pod garbage collection. A
		// cancelled cached restart must keep its previous stopped disk, though.
		if claim := WorkingClaim(pod); claim != "" && (vm.Status.Local == nil || vm.Status.Local.ClaimName != claim) {
			var pvc core.PersistentVolumeClaim
			if err := r.Get(ctx, client.ObjectKey{Namespace: vm.Namespace, Name: claim}, &pvc); err != nil {
				if !apierrors.IsNotFound(err) {
					return ctrl.Result{}, err
				}
			} else if metav1.IsControlledBy(&pvc, &vm) {
				if err := client.IgnoreNotFound(r.Delete(ctx, &pvc, client.Preconditions{UID: &pvc.UID, ResourceVersion: &pvc.ResourceVersion})); err != nil {
					return ctrl.Result{}, err
				}
			}
		}
		if _, err := r.status(ctx, &vm, "Stopped", "Cancelled before scheduling", nil); err != nil {
			return ctrl.Result{}, err
		}
		return r.releasePod(ctx, pod)
	}
	if pod.Annotations[Phase] == "Stopped" {
		if owner := pod.Annotations[LocalStopAnnotation]; owner != "" {
			if owner != string(pod.UID) {
				return r.status(ctx, &vm, "RecoveryRequired", "local stop owner does not match runner", pod)
			}
			if vm.Spec.PowerState != "Stopped" && (pod.Annotations[GenerationAnnotation] == "" || pod.Annotations[GenerationAnnotation] == strconv.FormatInt(vm.Generation, 10)) {
				before := vm.DeepCopy()
				vm.Spec.PowerState = "Stopped"
				return ctrl.Result{}, r.Patch(ctx, &vm, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
			}
			local := &api.LocalCheckpoint{ClaimName: WorkingClaim(pod), NodeName: pod.Spec.NodeName, Owner: owner}
			if vm.Status.Local == nil || vm.Status.Local.Owner != owner {
				vm.Status.Local = local
				if err := r.Status().Update(ctx, &vm); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			if _, err := r.status(ctx, &vm, "Stopped", pod.Annotations[Message], nil); err != nil {
				return ctrl.Result{}, err
			}
			if pod.Status.Phase != core.PodSucceeded && pod.Status.Phase != core.PodFailed {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			if !vm.Status.Local.Durable {
				if err := r.createWorker(ctx, &vm); err != nil {
					return ctrl.Result{}, err
				}
			}
			return r.releasePod(ctx, pod)
		}
		if pod.Status.Phase != core.PodSucceeded && pod.Status.Phase != core.PodFailed {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		var cp api.Checkpoint
		if err := json.Unmarshal([]byte(pod.Annotations[CheckpointAnnotation]), &cp); err != nil {
			return ctrl.Result{}, err
		}
		if vm.Spec.PowerState != "Stopped" && (pod.Annotations[GenerationAnnotation] == "" || pod.Annotations[GenerationAnnotation] == strconv.FormatInt(vm.Generation, 10)) {
			before := vm.DeepCopy()
			vm.Spec.PowerState = "Stopped"
			return ctrl.Result{}, r.Patch(ctx, &vm, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
		}
		// Record the durable stop before removing its Pod. A controller crash between
		// these writes can retry without inventing a lost-runtime recovery event.
		if _, err := r.status(ctx, &vm, "Stopped", pod.Annotations[Message], pod, &cp); err != nil {
			return ctrl.Result{}, err
		}
		return r.releasePod(ctx, pod)
	}
	if stopping || pod.DeletionTimestamp != nil {
		if pod.Annotations[Stop] != "true" {
			before := pod.DeepCopy()
			if pod.Annotations == nil {
				pod.Annotations = map[string]string{}
			}
			pod.Annotations[Stop] = "true"
			if err := r.Patch(ctx, pod, client.MergeFrom(before)); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	if pod.Spec.NodeName != "" && len(workers) > 0 {
		for i := range workers {
			if workers[i].DeletionTimestamp == nil {
				if err := client.IgnoreNotFound(r.Delete(ctx, &workers[i])); err != nil {
					return ctrl.Result{}, err
				}
			}
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	phase := pod.Annotations[Phase]
	message := pod.Annotations[Message]
	if phase == "" {
		phase = "Pending"
	}
	if pod.Status.Phase == core.PodFailed || pod.Status.Phase == core.PodSucceeded {
		phase = "RecoveryRequired"
		message = "runner exited without durable commit; local data and ownership are retained"
	}
	return r.status(ctx, &vm, phase, message, pod)
}

func (r *Reconciler) releasePod(ctx context.Context, pod *core.Pod) (ctrl.Result, error) {
	controllerutil.RemoveFinalizer(pod, Finalizer)
	if err := r.Update(ctx, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	if pod.DeletionTimestamp == nil {
		if err := client.IgnoreNotFound(r.Delete(ctx, pod)); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: time.Second}, nil
}

func (r *Reconciler) status(
	ctx context.Context,
	vm *api.VirtualMachine,
	phase, message string,
	pod *core.Pod,
	checkpoint ...*api.Checkpoint,
) (ctrl.Result, error) {
	old := vm.Status.DeepCopy()
	if pod != nil && vm.Spec.PowerState == "Running" && vm.DeletionTimestamp == nil && pod.DeletionTimestamp == nil && phase == "Running" {
		resizeErr := r.expandWorkingPVC(ctx, vm, pod)
		condition := metav1.Condition{Type: "DiskReady", Status: metav1.ConditionTrue, Reason: "CapacityReady", ObservedGeneration: vm.Generation}
		size, _ := strconv.ParseInt(pod.Annotations[DiskSizeAnnotation], 10, 64)
		vm.Status.RootDiskSize = size
		desired, _ := api.DiskBytes(vm.Spec.RootDiskSize)
		if size < desired || pod.Annotations[ResizeAnnotation] != "" || resizeErr != nil {
			condition.Status = metav1.ConditionFalse
			condition.Reason = "ResizePending"
			condition.Message = pod.Annotations[ResizeAnnotation]
			if resizeErr != nil {
				condition.Message = resizeErr.Error()
			}
		}
		meta.SetStatusCondition(&vm.Status.Conditions, condition)
	}
	if len(checkpoint) > 0 {
		vm.Status.Checkpoint = checkpoint[0]
	}
	vm.Status.Phase = phase
	vm.Status.Message = message
	vm.Status.ObservedGeneration = vm.Generation
	if pod != nil {
		vm.Status.PodName = pod.Name
		vm.Status.NodeName = pod.Spec.NodeName
	}
	if phase == "Stopped" {
		vm.Status.PodName = ""
		vm.Status.NodeName = ""
	}
	ready := metav1.ConditionFalse
	if phase == "Running" && pod != nil {
		for _, condition := range pod.Status.Conditions {
			if condition.Type == core.PodReady && condition.Status == core.ConditionTrue {
				ready = metav1.ConditionTrue
			}
		}
	}
	stopped := metav1.ConditionFalse
	if phase == "Stopped" {
		stopped = metav1.ConditionTrue
	}
	checkpointReady := metav1.ConditionFalse
	if phase == "Stopped" && vm.Status.Local != nil && vm.Status.Local.Durable {
		checkpointReady = metav1.ConditionTrue
	}
	if phase == "Stopped" && vm.Status.Local == nil && vm.Status.Checkpoint != nil {
		checkpointReady = metav1.ConditionTrue
	}
	for _, condition := range []struct {
		name   string
		status metav1.ConditionStatus
	}{{"Ready", ready}, {"Stopped", stopped}, {"CheckpointReady", checkpointReady}} {
		meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
			Type: condition.name, Status: condition.status, Reason: phase,
			Message: message, ObservedGeneration: vm.Generation,
		})
	}
	if equality.Semantic.DeepEqual(*old, vm.Status) {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	return ctrl.Result{RequeueAfter: 2 * time.Second}, r.Status().Update(ctx, vm)
}

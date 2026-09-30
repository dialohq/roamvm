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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	Label                = "vm.roamvm.io/uid"
	Phase                = "vm.roamvm.io/phase"
	Stop                 = "vm.roamvm.io/stop"
	Finalizer            = "vm.roamvm.io/checkpoint"
	CheckpointAnnotation = "vm.roamvm.io/checkpoint"
	Message              = "vm.roamvm.io/message"
	DiskSizeAnnotation   = "vm.roamvm.io/root-disk-size"
	ResizeAnnotation     = "vm.roamvm.io/resize-error"
	GenerationAnnotation = "vm.roamvm.io/boot-generation"
	ExitAnnotation       = "vm.roamvm.io/runner-exit"
	SpecAnnotation       = "vm.roamvm.io/boot-spec"
)

type Reconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Image        string
	StorageClass string
	StorageSize  string
}

func (r *Reconciler) Setup(m ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(m).For(&api.VirtualMachine{}).Owns(&core.Pod{}).Complete(r)
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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
	if len(pods.Items) > 1 {
		return r.status(ctx, &vm, "Blocked", "multiple runner Pods; refusing to select an owner", nil)
	}
	stopping := vm.Spec.PowerState == "Stopped" || vm.DeletionTimestamp != nil
	if len(pods.Items) == 0 {
		if stopping {
			if vm.Status.Phase != "" && vm.Status.Phase != "Stopped" {
				return r.status(
					ctx,
					&vm,
					"RecoveryRequired",
					"runner vanished without a committed checkpoint; fence its runtime before recovery",
					nil,
				)
			}
			if vm.DeletionTimestamp != nil {
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
	pod := &pods.Items[0]
	// Never trust a label alone to establish ownership.
	if !metav1.IsControlledBy(pod, &vm) {
		return r.status(ctx, &vm, "Blocked", "runner is not owned by this VM", nil)
	}
	// An unassigned Pod cannot have acquired state. Delete with an RV
	// precondition so a concurrent scheduler binding invalidates this decision.
	if stopping && pod.Spec.NodeName == "" {
		if pod.DeletionTimestamp == nil {
			err := r.Delete(ctx, pod, client.Preconditions{UID: &pod.UID, ResourceVersion: &pod.ResourceVersion})
			return ctrl.Result{RequeueAfter: time.Second}, client.IgnoreNotFound(err)
		}
		if _, err := r.status(ctx, &vm, "Stopped", "Cancelled before scheduling", nil); err != nil {
			return ctrl.Result{}, err
		}
		return r.releasePod(ctx, pod)
	}
	if pod.Annotations[Phase] == "Stopped" {
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
	for _, condition := range []struct {
		name   string
		status metav1.ConditionStatus
	}{{"Ready", ready}, {"Stopped", stopped}} {
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

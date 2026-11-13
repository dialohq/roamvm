package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const CloneLabel = "vm.roamvm.io/clone-worker"

func (r *Reconciler) clone(ctx context.Context, vm *api.VirtualMachine, workers []core.Pod) (ctrl.Result, error) {
	if !api.IsV2(r.Scheme) {
		return r.status(ctx, vm, "Blocked", "cloning requires the experimental v2 API", nil)
	}
	for _, worker := range workers {
		if worker.Labels[CloneLabel] == "true" && worker.Annotations[CheckpointReadyAnnotation] == "true" {
			var cp api.Checkpoint
			if err := json.Unmarshal([]byte(worker.Annotations[CheckpointAnnotation]), &cp); err != nil {
				return ctrl.Result{}, err
			}
			if cp.Hibernation != nil || cp.Key == "" || cp.SHA256 == "" {
				return ctrl.Result{}, fmt.Errorf("invalid disk-only clone checkpoint")
			}
			return r.status(ctx, vm, "Stopped", "Independent disk clone committed", nil, &cp)
		}
	}
	if len(workers) > 0 {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	var source api.VirtualMachine
	if err := r.Get(ctx, client.ObjectKey{Namespace: vm.Namespace, Name: vm.Spec.CloneFrom.Name}, &source); err != nil {
		return ctrl.Result{}, err
	}
	if string(source.UID) != vm.Spec.CloneFrom.UID || source.Spec.Image != vm.Spec.Image {
		return r.status(ctx, vm, "Blocked", "clone source UID or base image mismatch", nil)
	}
	if source.Status.Checkpoint == nil || source.Spec.PowerState != "Stopped" || source.Status.Phase != "Stopped" ||
		(source.Status.Local != nil && !source.Status.Local.Durable) {
		return r.status(ctx, vm, "Cloning", "Waiting for a durable stopped source", nil)
	}
	size, err := workingSize(r.StorageSize, vm.Spec)
	if err != nil {
		return ctrl.Result{}, err
	}
	// Scratch holds the restored source (including RAM) and disk-only output.
	size.Add(*resource.NewQuantity(source.Status.Checkpoint.Size, resource.BinarySI))
	var storageClass *string
	if r.StorageClass != "" {
		storageClass = &r.StorageClass
	}
	pod := &core.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "roamvm-clone-" + string(vm.UID), Namespace: vm.Namespace,
		Labels: map[string]string{Label: string(vm.UID), WorkerLabel: "true", CloneLabel: "true"},
	}, Spec: core.PodSpec{
		ServiceAccountName: runtimeName(r.Scheme, "runtime"), RestartPolicy: core.RestartPolicyOnFailure,
		ImagePullSecrets: vm.Spec.ImagePullSecrets,
		Volumes: []core.Volume{
			{Name: "base", VolumeSource: core.VolumeSource{Image: &core.ImageVolumeSource{Reference: vm.Spec.Image, PullPolicy: core.PullIfNotPresent}}},
			{Name: "working", VolumeSource: core.VolumeSource{Ephemeral: &core.EphemeralVolumeSource{VolumeClaimTemplate: &core.PersistentVolumeClaimTemplate{
				Spec: core.PersistentVolumeClaimSpec{
					StorageClassName: storageClass, AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
					Resources: core.VolumeResourceRequirements{Requests: core.ResourceList{core.ResourceStorage: size}},
				},
			}}}},
		},
		Containers: []core.Container{{
			Name: "clone-worker", Image: r.Image, Args: runtimeArgs(r.Scheme, "clone-worker"), SecurityContext: containerSecurityContext(),
			Env: []core.EnvVar{fieldEnv("NODE_NAME", "spec.nodeName"), fieldEnv("POD_NAME", "metadata.name"), fieldEnv("POD_NAMESPACE", "metadata.namespace"), fieldEnv("POD_UID", "metadata.uid"), {Name: "VM_UID", Value: string(vm.UID)}, {Name: "GOMEMLIMIT", Value: "256MiB"}},
			EnvFrom: []core.EnvFromSource{
				{ConfigMapRef: &core.ConfigMapEnvSource{LocalObjectReference: core.LocalObjectReference{Name: runtimeName(r.Scheme, "runtime")}, Optional: ptr.To(true)}},
				{SecretRef: &core.SecretEnvSource{LocalObjectReference: core.LocalObjectReference{Name: runtimeName(r.Scheme, "object-store")}}},
			},
			Resources:    core.ResourceRequirements{Requests: core.ResourceList{core.ResourceCPU: resource.MustParse("100m"), core.ResourceMemory: resource.MustParse("128Mi")}, Limits: core.ResourceList{core.ResourceMemory: resource.MustParse("512Mi")}},
			VolumeMounts: []core.VolumeMount{{Name: "base", MountPath: "/base", ReadOnly: true}, {Name: "working", MountPath: "/var/lib/roamvm"}},
		}},
	}}
	if err := controllerutil.SetControllerReference(vm, pod, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	// Record cloning before creation so a lost Create response is reconstructible.
	if _, err := r.status(ctx, vm, "Cloning", "Copying a disk-only project snapshot", nil); err != nil {
		return ctrl.Result{}, err
	}
	err = r.Create(ctx, pod)
	if apierrors.IsAlreadyExists(err) {
		err = nil // The next reconcile checks ownership before accepting completion.
	}
	return ctrl.Result{RequeueAfter: time.Second}, err
}

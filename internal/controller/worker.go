package controller

import (
	"context"
	"fmt"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func workerName(vm *api.VirtualMachine, owner string) string { return "roamvm-checkpoint-" + owner }

// Workers are reconstructible from durable Kubernetes intent, even after their
// original runner or a previous worker disappears. They never reserve KVM/RAM
// for the guest and cannot expose a guest network endpoint.
func (r *Reconciler) createWorker(ctx context.Context, vm *api.VirtualMachine) error {
	local := vm.Status.Local
	if local == nil || local.Owner == "" || local.ClaimName == "" || local.NodeName == "" {
		return fmt.Errorf("local checkpoint is not recorded")
	}
	worker := &core.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: workerName(vm, local.Owner), Namespace: vm.Namespace,
		Labels:      map[string]string{Label: string(vm.UID), WorkerLabel: "true"},
		Annotations: map[string]string{LocalOwnerAnnotation: local.Owner},
	}, Spec: core.PodSpec{
		ServiceAccountName: "roamvm-runtime", RestartPolicy: core.RestartPolicyOnFailure,
		AutomountServiceAccountToken: ptr.To(true), TerminationGracePeriodSeconds: ptr.To(int64(5)),
		NodeName: local.NodeName, ImagePullSecrets: vm.Spec.ImagePullSecrets,
		Volumes: []core.Volume{
			{Name: "base", VolumeSource: core.VolumeSource{Image: &core.ImageVolumeSource{Reference: vm.Spec.Image, PullPolicy: core.PullIfNotPresent}}},
			{Name: "working", VolumeSource: core.VolumeSource{PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: local.ClaimName}}},
		},
		Containers: []core.Container{{
			Name: "checkpoint-worker", Image: r.Image, ImagePullPolicy: core.PullIfNotPresent,
			Args: []string{"checkpoint-worker"}, SecurityContext: containerSecurityContext(),
			Env: []core.EnvVar{fieldEnv("NODE_NAME", "spec.nodeName"), fieldEnv("POD_NAME", "metadata.name"), fieldEnv("POD_NAMESPACE", "metadata.namespace"), fieldEnv("POD_UID", "metadata.uid"), {Name: "VM_UID", Value: string(vm.UID)}, {Name: "GOMEMLIMIT", Value: "256MiB"}},
			EnvFrom: []core.EnvFromSource{
				{ConfigMapRef: &core.ConfigMapEnvSource{LocalObjectReference: core.LocalObjectReference{Name: "roamvm-runtime"}, Optional: ptr.To(true)}},
				{SecretRef: &core.SecretEnvSource{LocalObjectReference: core.LocalObjectReference{Name: "roamvm-object-store"}}},
			},
			Resources:    core.ResourceRequirements{Requests: core.ResourceList{core.ResourceCPU: resource.MustParse("100m"), core.ResourceMemory: resource.MustParse("128Mi")}, Limits: core.ResourceList{core.ResourceMemory: resource.MustParse("512Mi")}},
			VolumeMounts: []core.VolumeMount{{Name: "base", MountPath: "/base", ReadOnly: true}, {Name: "working", MountPath: "/var/lib/roamvm"}},
		}},
	}}
	if err := controllerutil.SetControllerReference(vm, worker, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, worker); !apierrors.IsAlreadyExists(err) {
		return err
	}
	var existing core.Pod
	if err := r.Get(ctx, client.ObjectKeyFromObject(worker), &existing); err != nil {
		return err
	}
	if !metav1.IsControlledBy(&existing, vm) || existing.Labels[WorkerLabel] != "true" ||
		existing.Annotations[LocalOwnerAnnotation] != local.Owner || WorkingClaim(&existing) != local.ClaimName || existing.Spec.NodeName != local.NodeName {
		return fmt.Errorf("checkpoint worker identity mismatch")
	}
	return nil
}

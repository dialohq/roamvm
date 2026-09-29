package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const Label = "vm.roamvm.io/uid"
const Phase = "vm.roamvm.io/phase"
const Stop = "vm.roamvm.io/stop"
const Finalizer = "vm.roamvm.io/checkpoint"
const CheckpointAnnotation = "vm.roamvm.io/checkpoint"
const Message = "vm.roamvm.io/message"
const SecretAnnotation = "vm.roamvm.io/auth-secret"
const SpecAnnotation = "vm.roamvm.io/boot-spec"

type Reconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Image  string
	Root   string
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
		controllerutil.AddFinalizer(&vm, Finalizer)
		return ctrl.Result{}, r.Update(ctx, &vm)
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
				return r.status(ctx, &vm, "RecoveryRequired", "runner vanished without a committed checkpoint; fence its runtime before recovery", nil)
			}
			if vm.DeletionTimestamp != nil {
				controllerutil.RemoveFinalizer(&vm, Finalizer)
				return ctrl.Result{}, r.Update(ctx, &vm)
			}
			return r.status(ctx, &vm, "Stopped", "", nil)
		}
		if vm.Status.Phase != "" && vm.Status.Phase != "Stopped" && vm.Status.Phase != "Pending" {
			return r.status(ctx, &vm, "RecoveryRequired", "previous runner disappeared; automatic ownership takeover is disabled", nil)
		}
		// Persist one runner name before creation; informer lag must not create
		// two incarnations while the first Pod is not yet visible in the cache.
		if vm.Status.PodName == "" {
			suffix := make([]byte, 6)
			if _, err := rand.Read(suffix); err != nil {
				return ctrl.Result{}, err
			}
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
		controllerutil.RemoveFinalizer(pod, Finalizer)
		if err := r.Update(ctx, pod); err != nil {
			return ctrl.Result{}, err
		}
		secret := &core.Secret{ObjectMeta: metav1.ObjectMeta{Name: pod.Annotations[SecretAnnotation], Namespace: pod.Namespace}}
		return ctrl.Result{RequeueAfter: time.Second}, client.IgnoreNotFound(r.Delete(ctx, secret))
	}
	if pod.Annotations[Phase] == "Stopped" {
		if pod.Status.Phase != core.PodSucceeded && pod.Status.Phase != core.PodFailed {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		var cp api.Checkpoint
		if err := json.Unmarshal([]byte(pod.Annotations[CheckpointAnnotation]), &cp); err != nil {
			return ctrl.Result{}, err
		}
		// Record the durable stop before removing its Pod. A controller crash between
		// these writes can retry without inventing a lost-runtime recovery event.
		if _, err := r.status(ctx, &vm, "Stopped", "", pod, &cp); err != nil {
			return ctrl.Result{}, err
		}
		controllerutil.RemoveFinalizer(pod, Finalizer)
		if err := r.Update(ctx, pod); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		secret := &core.Secret{ObjectMeta: metav1.ObjectMeta{Name: pod.Annotations[SecretAnnotation], Namespace: vm.Namespace}}
		if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
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

func (r *Reconciler) status(ctx context.Context, vm *api.VirtualMachine, phase, message string, pod *core.Pod, checkpoint ...*api.Checkpoint) (ctrl.Result, error) {

	old := vm.DeepCopy()
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
	meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{Type: "Ready", Status: ready, Reason: phase, Message: message, ObservedGeneration: vm.Generation})
	meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{Type: "Stopped", Status: stopped, Reason: phase, Message: message, ObservedGeneration: vm.Generation})
	if equality.Semantic.DeepEqual(old.Status, vm.Status) {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	return ctrl.Result{RequeueAfter: 2 * time.Second}, r.Status().Update(ctx, vm)
}

func (r *Reconciler) createPod(ctx context.Context, vm *api.VirtualMachine) error {
	memory, err := resource.ParseQuantity(vm.Spec.Memory)
	if err != nil || memory.Sign() <= 0 {
		return fmt.Errorf("invalid guest memory %q", vm.Spec.Memory)
	}
	if vm.Spec.CPUs < 1 {
		return fmt.Errorf("CPUs must be positive")
	}
	resources := *vm.Spec.Resources.DeepCopy()
	if resources.Requests == nil {
		resources.Requests = core.ResourceList{}
	}
	if resources.Limits == nil {
		resources.Limits = core.ResourceList{}
	}
	overhead := resource.MustParse("192Mi")
	required := memory.DeepCopy()
	required.Add(overhead)
	if vm.Spec.Hugepages != "" {
		size, e := resource.ParseQuantity(vm.Spec.Hugepages)
		if e != nil || size.Sign() <= 0 || memory.Value()%size.Value() != 0 {
			return fmt.Errorf("guest RAM must be a multiple of hugepage size")
		}
		key := core.ResourceName("hugepages-" + vm.Spec.Hugepages)
		resources.Requests[key] = memory
		resources.Limits[key] = memory
		required = overhead
	}
	deviceCounts := map[core.ResourceName]int64{}
	for _, device := range vm.Spec.Devices {
		deviceCounts[core.ResourceName(device.ResourceName)]++
	}
	for key, n := range deviceCounts {
		if key == "vm.roamvm.io/kvm" {
			return fmt.Errorf("reserved KVM resource")
		}
		q, ok := resources.Limits[key]
		if !ok || q.Value() < n {
			return fmt.Errorf("request VFIO device resource %s in resources.limits", key)
		}
	}
	if q, ok := resources.Requests[core.ResourceMemory]; !ok {
		resources.Requests[core.ResourceMemory] = required
	} else if q.Cmp(required) < 0 {
		return fmt.Errorf("memory request must cover guest RAM plus 192Mi hypervisor overhead")
	}
	if q, ok := resources.Limits[core.ResourceMemory]; !ok {
		resources.Limits[core.ResourceMemory] = required
	} else if q.Cmp(required) < 0 {
		return fmt.Errorf("memory limit is below guest RAM plus overhead")
	}
	if _, ok := resources.Requests[core.ResourceCPU]; !ok {
		resources.Requests[core.ResourceCPU] = *resource.NewQuantity(int64(vm.Spec.CPUs), resource.DecimalSI)
	}
	resources.Requests["vm.roamvm.io/kvm"] = resource.MustParse("1")
	resources.Limits["vm.roamvm.io/kvm"] = resource.MustParse("1")
	name := vm.Status.PodName
	if name == "" {
		return fmt.Errorf("runner name must be persisted before Pod creation")
	}
	token := make([]byte, 32)
	if _, err = rand.Read(token); err != nil {
		return err
	}
	secret := &core.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: vm.Namespace}, Data: map[string][]byte{"token": []byte(hex.EncodeToString(token))}}
	if err = controllerutil.SetControllerReference(vm, secret, r.Scheme); err != nil {
		return err
	}
	if err = r.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	bootSpec, err := json.Marshal(vm.Spec)
	if err != nil {
		return err
	}
	pod := &core.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: vm.Namespace, Labels: map[string]string{Label: string(vm.UID), "vm.roamvm.io/name": vm.Name}, Annotations: map[string]string{SecretAnnotation: name, SpecAnnotation: string(bootSpec)}, Finalizers: []string{Finalizer}}}
	dirType := core.HostPathDirectoryOrCreate
	root := r.Root
	if root == "" {
		root = "/var/lib/roamvm"
	}
	pod.Spec = core.PodSpec{
		RestartPolicy: core.RestartPolicyNever, AutomountServiceAccountToken: ptr.To(false), TerminationGracePeriodSeconds: ptr.To(int64(3600)),
		NodeSelector: vm.Spec.NodeSelector, Affinity: vm.Spec.Affinity, Tolerations: vm.Spec.Tolerations, TopologySpreadConstraints: vm.Spec.TopologySpreadConstraints,
		SecurityContext: &core.PodSecurityContext{Sysctls: []core.Sysctl{{Name: "net.ipv4.ip_forward", Value: "1"}, {Name: "net.ipv4.conf.all.route_localnet", Value: "1"}}},
		Volumes: []core.Volume{
			{Name: "socket", VolumeSource: core.VolumeSource{HostPath: &core.HostPathVolumeSource{Path: "/run/roamvm", Type: &dirType}}},
			{Name: "images", VolumeSource: core.VolumeSource{HostPath: &core.HostPathVolumeSource{Path: root + "/images", Type: &dirType}}},
			{Name: "working", VolumeSource: core.VolumeSource{HostPath: &core.HostPathVolumeSource{Path: root + "/running/" + string(vm.UID), Type: &dirType}}},
			{Name: "auth", VolumeSource: core.VolumeSource{Secret: &core.SecretVolumeSource{SecretName: name, DefaultMode: ptr.To(int32(0400))}}},
		},
		Containers: []core.Container{{Name: "runner", Image: r.Image, ImagePullPolicy: core.PullIfNotPresent, Args: []string{"runner"}, Resources: resources,
			Env:             []core.EnvVar{{Name: "POD_NAME", Value: name}, {Name: "POD_NAMESPACE", Value: vm.Namespace}, {Name: "POD_UID", ValueFrom: &core.EnvVarSource{FieldRef: &core.ObjectFieldSelector{FieldPath: "metadata.uid"}}}},
			SecurityContext: &core.SecurityContext{RunAsUser: ptr.To(int64(0)), AllowPrivilegeEscalation: ptr.To(false), Capabilities: &core.Capabilities{Drop: []core.Capability{"ALL"}, Add: []core.Capability{"NET_ADMIN", "NET_RAW"}}, SeccompProfile: &core.SeccompProfile{Type: core.SeccompProfileTypeRuntimeDefault}},
			VolumeMounts:    []core.VolumeMount{{Name: "socket", MountPath: "/run/roamvm", ReadOnly: true}, {Name: "images", MountPath: root + "/images", ReadOnly: true}, {Name: "working", MountPath: root + "/running/" + string(vm.UID)}, {Name: "auth", MountPath: "/run/roamvm-auth", ReadOnly: true}},
			ReadinessProbe:  &core.Probe{ProbeHandler: core.ProbeHandler{Exec: &core.ExecAction{Command: []string{"/bin/sh", "-c", "test -f /tmp/guest-ready"}}}, PeriodSeconds: 1, FailureThreshold: 1},
		}},
	}
	if vm.Spec.Hugepages != "" || len(vm.Spec.Devices) > 0 {
		pod.Spec.Containers[0].SecurityContext.Capabilities.Add = append(pod.Spec.Containers[0].SecurityContext.Capabilities.Add, "IPC_LOCK")
	}
	if len(vm.Spec.Devices) > 0 {
		pod.Spec.Containers[0].SecurityContext.Capabilities.Add = append(pod.Spec.Containers[0].SecurityContext.Capabilities.Add, "SYS_RESOURCE")
	}
	for _, d := range vm.Spec.Disks {
		pod.Spec.Volumes = append(pod.Spec.Volumes, core.Volume{Name: d.Name, VolumeSource: core.VolumeSource{PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: d.ClaimName, ReadOnly: d.ReadOnly}}})
		if d.VolumeMode == "Block" {
			pod.Spec.Containers[0].VolumeDevices = append(pod.Spec.Containers[0].VolumeDevices, core.VolumeDevice{Name: d.Name, DevicePath: "/dev/disks/" + d.Name})
		} else {
			pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, core.VolumeMount{Name: d.Name, MountPath: "/disks/" + d.Name, ReadOnly: d.ReadOnly})
		}
	}
	if vm.Spec.Config != nil {
		pod.Spec.Volumes = append(pod.Spec.Volumes, core.Volume{Name: "config", VolumeSource: core.VolumeSource{Projected: vm.Spec.Config}})
		pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, core.VolumeMount{Name: "config", MountPath: "/config", ReadOnly: true})
	}
	if err = controllerutil.SetControllerReference(vm, pod, r.Scheme); err != nil {
		return err
	}
	if err = r.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

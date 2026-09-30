package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strconv"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func (r *Reconciler) createPod(ctx context.Context, vm *api.VirtualMachine) error {
	for _, disk := range vm.Spec.ProjectedDisks() {
		for _, source := range disk.Projection.Sources {
			if source.ServiceAccountToken != nil || source.PodCertificate != nil ||
				(source.Secret != nil && source.Secret.Name == "roamvm-object-store") {
				return fmt.Errorf("VM configuration cannot project runtime credentials")
			}
		}
	}
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
	overhead := resource.MustParse("512Mi")
	if vm.Spec.Hugepages == "" {
		overhead.Add(*resource.NewQuantity(memory.Value()/32, resource.BinarySI))
	}
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
	resources.Requests["vm.roamvm.io/kvm"] = resource.MustParse("1")
	resources.Limits["vm.roamvm.io/kvm"] = resource.MustParse("1")
	if q, ok := resources.Requests[core.ResourceMemory]; !ok {
		resources.Requests[core.ResourceMemory] = required
	} else if q.Cmp(required) < 0 {
		return fmt.Errorf("memory request must cover guest RAM plus hypervisor overhead")
	}
	if q, ok := resources.Limits[core.ResourceMemory]; ok && q.Cmp(resources.Requests[core.ResourceMemory]) < 0 {
		return fmt.Errorf("memory limit must cover the memory request")
	}
	if _, ok := resources.Requests[core.ResourceCPU]; !ok {
		resources.Requests[core.ResourceCPU] = *resource.NewQuantity(int64(vm.Spec.CPUs), resource.DecimalSI)
	}
	name := vm.Status.PodName
	if name == "" {
		return fmt.Errorf("runner name must be persisted before Pod creation")
	}
	bootSpec, err := json.Marshal(vm.Spec)
	if err != nil {
		return err
	}
	pod := &core.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   vm.Namespace,
			Labels:      map[string]string{},
			Annotations: map[string]string{SpecAnnotation: string(bootSpec), GenerationAnnotation: strconv.FormatInt(vm.Generation, 10)},
			Finalizers:  []string{Finalizer},
		},
	}
	maps.Copy(pod.Labels, vm.Labels)
	pod.Labels[Label] = string(vm.UID)
	pod.Labels["vm.roamvm.io/name"] = vm.Name
	pod.Annotations["kubectl.kubernetes.io/default-container"] = "runner"
	root := "/var/lib/roamvm"
	storage, err := workingSize(r.StorageSize, vm.Spec.RootDiskSize)
	if err != nil {
		return err
	}
	var storageClass *string
	if r.StorageClass != "" {
		storageClass = &r.StorageClass
	}
	pod.Spec = core.PodSpec{
		ServiceAccountName:            "roamvm-runtime",
		RestartPolicy:                 core.RestartPolicyNever,
		AutomountServiceAccountToken:  ptr.To(false),
		TerminationGracePeriodSeconds: ptr.To(int64(3600)),
		ImagePullSecrets:              vm.Spec.ImagePullSecrets,
		NodeSelector:                  vm.Spec.NodeSelector,
		Affinity:                      vm.Spec.Affinity,
		Tolerations:                   vm.Spec.Tolerations,
		TopologySpreadConstraints:     vm.Spec.TopologySpreadConstraints,
		SecurityContext: &core.PodSecurityContext{
			Sysctls: []core.Sysctl{
				{Name: "net.ipv4.ip_forward", Value: "1"},
				{Name: "net.ipv4.conf.all.route_localnet", Value: "1"},
			},
		},
		Containers: []core.Container{{
			Name:            "runner",
			Image:           r.Image,
			ImagePullPolicy: core.PullIfNotPresent,
			Args:            []string{"runner"},
			Resources:       resources,
			SecurityContext: containerSecurityContext("NET_ADMIN", "NET_RAW"),
			ReadinessProbe: &core.Probe{
				ProbeHandler: core.ProbeHandler{
					Exec: &core.ExecAction{Command: []string{"/bin/sh", "-c", "test -f /tmp/guest-ready"}},
				},
				PeriodSeconds:    1,
				FailureThreshold: 1,
			},
		}},
	}
	runner := &pod.Spec.Containers[0]
	mount := func(name, path string, source core.VolumeSource, readOnly bool) {
		pod.Spec.Volumes = append(pod.Spec.Volumes, core.Volume{Name: name, VolumeSource: source})
		runner.VolumeMounts = append(
			runner.VolumeMounts,
			core.VolumeMount{Name: name, MountPath: path, ReadOnly: readOnly},
		)
	}
	mount("tmp", "/tmp", core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{}}, false)
	mount("socket", "/run/roamvm", core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{}}, false)
	mount(
		"base",
		"/base",
		core.VolumeSource{Image: &core.ImageVolumeSource{Reference: vm.Spec.Image, PullPolicy: core.PullIfNotPresent}},
		true,
	)
	mount(
		"working",
		root,
		core.VolumeSource{
			Ephemeral: &core.EphemeralVolumeSource{
				VolumeClaimTemplate: &core.PersistentVolumeClaimTemplate{Spec: core.PersistentVolumeClaimSpec{
					StorageClassName: storageClass,
					AccessModes:      []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
					Resources: core.VolumeResourceRequirements{
						Requests: core.ResourceList{core.ResourceStorage: storage},
					},
				}},
			},
		},
		false,
	)
	pod.Spec.Volumes = append(
		pod.Spec.Volumes,
		core.Volume{
			Name: "kube-api",
			VolumeSource: core.VolumeSource{Projected: &core.ProjectedVolumeSource{Sources: []core.VolumeProjection{
				{
					ServiceAccountToken: &core.ServiceAccountTokenProjection{
						Path:              "token",
						ExpirationSeconds: ptr.To(int64(3600)),
					},
				},
				{
					ConfigMap: &core.ConfigMapProjection{
						LocalObjectReference: core.LocalObjectReference{Name: "kube-root-ca.crt"},
						Items:                []core.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
					},
				},
				{
					DownwardAPI: &core.DownwardAPIProjection{
						Items: []core.DownwardAPIVolumeFile{
							{
								Path:     "namespace",
								FieldRef: &core.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"},
							},
						},
					},
				},
			}}},
		},
	)
	runtimeContainer := core.Container{
		Name:            "runtime",
		Image:           r.Image,
		ImagePullPolicy: core.PullIfNotPresent,
		Args:            []string{"daemon"},
		RestartPolicy:   ptr.To(core.ContainerRestartPolicyOnFailure),
		Env: []core.EnvVar{
			fieldEnv("NODE_NAME", "spec.nodeName"),
			fieldEnv("POD_NAME", "metadata.name"),
			fieldEnv("POD_NAMESPACE", "metadata.namespace"),
			fieldEnv("POD_UID", "metadata.uid"),
			{Name: "VM_UID", Value: string(vm.UID)},
			{Name: "GOMEMLIMIT", Value: "256MiB"},
		},
		EnvFrom: []core.EnvFromSource{
			{
				ConfigMapRef: &core.ConfigMapEnvSource{
					LocalObjectReference: core.LocalObjectReference{Name: "roamvm-runtime"},
					Optional:             ptr.To(true),
				},
			},
			{
				SecretRef: &core.SecretEnvSource{
					LocalObjectReference: core.LocalObjectReference{Name: "roamvm-object-store"},
				},
			},
		},
		Resources: core.ResourceRequirements{
			Requests: core.ResourceList{
				core.ResourceCPU:    resource.MustParse("100m"),
				core.ResourceMemory: resource.MustParse("128Mi"),
			},
			Limits: core.ResourceList{core.ResourceMemory: resource.MustParse("512Mi")},
		},
		SecurityContext: containerSecurityContext(),
		VolumeMounts: []core.VolumeMount{
			{Name: "socket", MountPath: "/run/roamvm"},
			{Name: "base", MountPath: "/base", ReadOnly: true},
			{Name: "working", MountPath: root},
			{Name: "kube-api", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true},
		},
	}
	if vm.Spec.Hugepages != "" || len(vm.Spec.Devices) > 0 {
		runner.SecurityContext.Capabilities.Add = append(runner.SecurityContext.Capabilities.Add, "IPC_LOCK")
	}
	if len(vm.Spec.Devices) > 0 {
		runner.SecurityContext.Capabilities.Add = append(runner.SecurityContext.Capabilities.Add, "SYS_RESOURCE")
	}
	for _, d := range vm.Spec.Disks {
		source := core.VolumeSource{
			PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{
				ClaimName: d.ClaimName,
				ReadOnly:  d.ReadOnly,
			},
		}
		if d.VolumeMode == "Block" {
			pod.Spec.Volumes = append(pod.Spec.Volumes, core.Volume{Name: d.Name, VolumeSource: source})
			runner.VolumeDevices = append(
				runner.VolumeDevices,
				core.VolumeDevice{Name: d.Name, DevicePath: "/dev/disks/" + d.Name},
			)
		} else {
			mount(d.Name, "/disks/"+d.Name, source, d.ReadOnly)
		}
	}
	for _, disk := range vm.Spec.ProjectedDisks() {
		mount(disk.VolumeName(), disk.MountPath(), core.VolumeSource{Projected: &disk.Projection}, true)
	}
	pod.Spec.Containers = append(pod.Spec.Containers, runtimeContainer)
	if err := controllerutil.SetControllerReference(vm, pod, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, pod); !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func fieldEnv(name, path string) core.EnvVar {
	return core.EnvVar{Name: name, ValueFrom: &core.EnvVarSource{FieldRef: &core.ObjectFieldSelector{FieldPath: path}}}
}

func containerSecurityContext(capabilities ...core.Capability) *core.SecurityContext {
	return &core.SecurityContext{
		RunAsUser:                ptr.To(int64(0)),
		AllowPrivilegeEscalation: ptr.To(false),
		Capabilities:             &core.Capabilities{Drop: []core.Capability{"ALL"}, Add: capabilities},
		SeccompProfile:           &core.SeccompProfile{Type: core.SeccompProfileTypeRuntimeDefault},
	}
}

// Package v1alpha1 defines the VM API.
// +kubebuilder:object:generate=true
// +groupName=vm.roamvm.io
package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "vm.roamvm.io", Version: "v1alpha1"}
	SchemeBuilder = runtime.NewSchemeBuilder(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &VirtualMachine{}, &VirtualMachineList{})
		metav1.AddToGroupVersion(s, GroupVersion)
		return nil
	})
)
var AddToScheme = SchemeBuilder.AddToScheme

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rvm
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.status.nodeName`
// +kubebuilder:printcolumn:name="Generation",type=integer,JSONPath=`.status.checkpoint.generation`
type VirtualMachine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              VirtualMachineSpec   `json:"spec"`
	Status            VirtualMachineStatus `json:"status,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(oldSelf.rootDiskSize) || has(self.rootDiskSize)",message="rootDiskSize cannot be removed"
type VirtualMachineSpec struct {
	// Hostname is applied at guest boot; it does not change the Pod's DNS name.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Hostname string `json:"hostname,omitempty"`
	// +kubebuilder:validation:Enum=Running;Stopped
	// +kubebuilder:default=Stopped
	PowerState string `json:"powerState"`
	// Image is an OCI image containing /disk/root.raw or root.qcow2 and boot files.
	// +kubebuilder:validation:Pattern=`^.+@sha256:[a-f0-9]{64}$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="the immutable base cannot change; create a new VM"
	Image            string                        `json:"image"`
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
	// BootMode selects image-provided boot files or the bootloader on the writable root disk.
	// Changes take effect at the next start; Disk preserves the immutable base and checkpoints.
	// +kubebuilder:validation:Enum=Image;Disk
	BootMode string `json:"bootMode,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=256
	// +kubebuilder:default=2
	CPUs int32 `json:"cpus"`
	// Memory is guest RAM; resource requests must include hypervisor overhead.
	// +kubebuilder:default="1Gi"
	Memory string `json:"memory"`
	// RootDiskSize is a minimum capacity. Growth applies to a running VM; shrinking is forbidden.
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:XValidation:rule="isQuantity(self) && quantity(self).isInteger() && quantity(self).compareTo(quantity('0')) > 0 && quantity(self).compareTo(quantity('16Ti')) <= 0 && quantity(self).asInteger() % 512 == 0",message="rootDiskSize must be a positive sector-aligned size up to 16Ti"
	// +kubebuilder:validation:XValidation:rule="quantity(self).compareTo(quantity(oldSelf)) >= 0",message="rootDiskSize cannot shrink"
	RootDiskSize string `json:"rootDiskSize,omitempty"`
	// Hugepages uses native Kubernetes hugepages resource accounting.
	// +kubebuilder:validation:Enum="2Mi";"1Gi"
	Hugepages string `json:"hugepages,omitempty"`
	// Devices require a VFIO-capable device plugin, not CUDA container devices.
	Devices                   []Device                          `json:"devices,omitempty"`
	Resources                 corev1.ResourceRequirements       `json:"resources,omitempty"`
	NodeSelector              map[string]string                 `json:"nodeSelector,omitempty"`
	Affinity                  *corev1.Affinity                  `json:"affinity,omitempty"`
	Tolerations               []corev1.Toleration               `json:"tolerations,omitempty"`
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`
	// +kubebuilder:default=120
	// +kubebuilder:validation:Minimum=10
	ShutdownTimeoutSeconds int32 `json:"shutdownTimeoutSeconds"`
	// Readiness waits for this guest TCP port before adding the Pod to Services.
	// +kubebuilder:default=22
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	ReadinessPort int32 `json:"readinessPort"`
	// Secondary disks use ordinary Kubernetes PVCs, independently of the root.
	Disks []SecondaryDisk `json:"disks,omitempty"`
	// Projected files become a read-only ISO disk labelled ROAMVM_CONFIG.
	// Secrets and ConfigMaps are resolved by kubelet and refreshed at next boot.
	Config *corev1.ProjectedVolumeSource `json:"config,omitempty"`
	// ConfigDisks exposes named projected sources as individually labelled ISO disks.
	// +kubebuilder:validation:MaxItems=8
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:XValidation:rule="self.all(x, self.filter(y, y.label == x.label).size() == 1)",message="config disk labels must be unique"
	ConfigDisks []ConfigDisk `json:"configDisks,omitempty"`
}

type ConfigDisk struct {
	// +kubebuilder:validation:MaxLength=31
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]{0,30}$`
	Name string `json:"name"`
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_-]{1,32}$`
	Label      string                       `json:"label"`
	Projection corev1.ProjectedVolumeSource `json:"projection"`
}

type SecondaryDisk struct {
	Name      string `json:"name"`
	ClaimName string `json:"claimName"`
	// +kubebuilder:validation:Enum=Block;Filesystem
	// +kubebuilder:default=Block
	VolumeMode string `json:"volumeMode"`
	ReadOnly   bool   `json:"readOnly,omitempty"`
}

type Device struct {
	ResourceName string `json:"resourceName"`
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`
	PCIAddress string `json:"pciAddress"`
}

type Checkpoint struct {
	Generation int64  `json:"generation"`
	Key        string `json:"key"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
	VersionID  string `json:"versionID,omitempty"`
}
type VirtualMachineStatus struct {
	RootDiskSize       int64              `json:"rootDiskSize,omitempty"`
	Phase              string             `json:"phase,omitempty"`
	Message            string             `json:"message,omitempty"`
	PodName            string             `json:"podName,omitempty"`
	NodeName           string             `json:"nodeName,omitempty"`
	Checkpoint         *Checkpoint        `json:"checkpoint,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
type VirtualMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VirtualMachine `json:"items"`
}

package v1alpha1

// ProjectedDisks includes the original single configuration disk alongside
// named disks, preserving their volume names, mount paths and guest labels.
func (s VirtualMachineSpec) ProjectedDisks() []ConfigDisk {
	if s.Config == nil {
		return s.ConfigDisks
	}
	return append([]ConfigDisk{{Label: "ROAMVM_CONFIG", Projection: *s.Config}}, s.ConfigDisks...)
}

func (d ConfigDisk) VolumeName() string {
	if d.Name == "" {
		return "config"
	}
	return "config-" + d.Name
}

func (d ConfigDisk) MountPath() string {
	if d.Name == "" {
		return "/config"
	}
	return "/config-disks/" + d.Name
}

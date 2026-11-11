package v1alpha1

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
)

func DiskBytes(size string) (int64, error) {
	if size == "" {
		return 0, nil
	}
	q, err := resource.ParseQuantity(size)
	if err != nil {
		return 0, err
	}
	n := q.Value()
	ok := q.Cmp(*resource.NewQuantity(n, resource.BinarySI)) == 0
	if !ok || n <= 0 || n > 16<<40 || n%512 != 0 {
		return 0, fmt.Errorf("invalid root disk size %q", size)
	}
	return n, nil
}

// WorkingBytes includes the live disk, filesystem metadata, and worst-case RAM
// snapshot space. The immutable base lives outside the working PVC.
func WorkingBytes(spec VirtualMachineSpec) (int64, error) {
	n, err := DiskBytes(spec.RootDiskSize)
	if err != nil {
		return 0, err
	}
	if spec.Hibernate {
		memory, err := resource.ParseQuantity(spec.Memory)
		if err != nil || memory.Sign() <= 0 {
			return 0, fmt.Errorf("invalid guest memory %q", spec.Memory)
		}
		n += memory.Value() + (128 << 20)
	}
	if n == 0 {
		return 0, nil
	}
	return n + (1 << 30), nil
}

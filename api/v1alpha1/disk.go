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

// WorkingBytes reserves space for both the live overlay and its compacted
// checkpoint, plus filesystem/QCOW2 metadata. The immutable base lives elsewhere.
func WorkingBytes(size string) (int64, error) {
	n, err := DiskBytes(size)
	if err != nil || n == 0 {
		return 0, err
	}
	return 2*n + (1 << 30), nil
}

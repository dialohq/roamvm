package hibernate

import (
	"os/exec"
	"testing"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/stretchr/testify/require"
)

func TestHibernationIdentityAndCaptureOwnership(t *testing.T) {
	if _, err := exec.LookPath("qemu-system-x86_64"); err != nil {
		t.Skip("QEMU not installed")
	}
	spec := api.VirtualMachineSpec{Hibernate: true, Image: "immutable-base", CPUs: 2, Memory: "512Mi"}
	saved, err := Identity(t.Context(), spec)
	require.NoError(t, err)
	for _, change := range []func(*api.VirtualMachineSpec){
		func(s *api.VirtualMachineSpec) { s.Memory = "1Gi" },
		func(s *api.VirtualMachineSpec) { s.CPUs = 3 },
		func(s *api.VirtualMachineSpec) { s.Image = "different-base" },
		func(s *api.VirtualMachineSpec) { s.RootDiskSize = "16Gi" },
	} {
		changed := spec
		change(&changed)
		require.Error(t, Check(t.Context(), changed, &saved))
		changed.Hibernate = false
		require.NoError(t, Check(t.Context(), changed, &saved), "explicit cold boot must remain possible")
	}
	spec.NodeSelector = map[string]string{"host": "different-node"}
	spec.PowerState = "Running"
	require.NoError(t, Check(t.Context(), spec, &saved), "placement must not change the guest identity")
	for _, field := range []*string{&saved.CPU, &saved.QEMU} {
		original := *field
		*field = "incompatible"
		require.Error(t, Check(t.Context(), spec, &saved))
		*field = original
	}
	dir := t.TempDir()
	require.NoError(t, Save(dir, "capturing-runner", saved))
	actual, err := Load(dir, "capturing-runner")
	require.NoError(t, err)
	require.Equal(t, saved, *actual)
	_, err = Load(dir, "different-runner")
	require.Error(t, err, "an old capture cannot authenticate a new stopped incarnation")
	require.NoError(t, Clear(dir))
	actual, err = Load(dir, "capturing-runner")
	require.NoError(t, err)
	require.Nil(t, actual)
}

package disk

import (
	"context"
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/dialohq/roamvm/internal/images"
	"github.com/stretchr/testify/require"
)

func TestOverlayPortabilityPreservesWritesAndZeroes(t *testing.T) {
	if _, e := exec.LookPath("qemu-img"); e != nil {
		t.Skip("qemu-img not installed")
	}
	if _, e := exec.LookPath("qemu-io"); e != nil {
		t.Skip("qemu-io not installed")
	}
	ctx := context.Background()
	a := t.TempDir()
	b := t.TempDir()
	baseData := make([]byte, 4<<20)
	for i := range baseData {
		baseData[i] = 0x55
	}
	base := images.Base{Dir: a, Manifest: images.Manifest{Format: "raw"}}
	require.NoError(t, os.WriteFile(base.Disk(), baseData, 0o444))
	overlay := filepath.Join(a, "overlay.qcow2")
	require.NoError(t, Create(ctx, base, overlay))
	out, e := exec.Command("qemu-io", "-f", "qcow2", "-c", "write -P 0x42 0 64k", "-c", "write -z 128k 64k", overlay).
		CombinedOutput()
	if e != nil {
		t.Fatalf("write: %v %s", e, out)
	}
	cp, e := Compact(ctx, base, overlay)
	require.NoError(t, e)
	other := images.Base{Dir: b, Manifest: base.Manifest}
	require.NoError(t, os.WriteFile(other.Disk(), baseData, 0o444))
	data, e := os.ReadFile(cp)
	require.NoError(t, e)
	moved := filepath.Join(b, "overlay.qcow2")
	require.NoError(t, os.WriteFile(moved, data, 0o600))
	require.NoError(t, Rebase(ctx, other, moved))
	out, e = exec.Command("qemu-img", "compare", "-f", "qcow2", "-F", "qcow2", overlay, moved).CombinedOutput()
	if e != nil {
		t.Fatalf("relocated disk differs: %v %s", e, out)
	}
	after, _ := os.ReadFile(base.Disk())
	if sha256.Sum256(after) != sha256.Sum256(baseData) {
		t.Fatal("base modified")
	}
	if len(data) >= len(baseData)/2 {
		t.Fatalf("checkpoint contains base: %d", len(data))
	}
}

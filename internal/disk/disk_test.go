package disk

import (
	"context"
	"crypto/sha256"
	"github.com/dialohq/roamvm/internal/images"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
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
	if e := os.WriteFile(base.Disk(), baseData, 0444); e != nil {
		t.Fatal(e)
	}
	overlay := filepath.Join(a, "overlay.qcow2")
	if e := Create(ctx, base, overlay); e != nil {
		t.Fatal(e)
	}
	out, e := exec.Command("qemu-io", "-f", "qcow2", "-c", "write -P 0x42 0 64k", "-c", "write -z 128k 64k", overlay).CombinedOutput()
	if e != nil {
		t.Fatalf("write: %v %s", e, out)
	}
	cp, e := Compact(ctx, base, overlay)
	if e != nil {
		t.Fatal(e)
	}
	other := images.Base{Dir: b, Manifest: base.Manifest}
	if e = os.WriteFile(other.Disk(), baseData, 0444); e != nil {
		t.Fatal(e)
	}
	data, e := os.ReadFile(cp)
	if e != nil {
		t.Fatal(e)
	}
	moved := filepath.Join(b, "overlay.qcow2")
	if e = os.WriteFile(moved, data, 0600); e != nil {
		t.Fatal(e)
	}
	if e = Rebase(ctx, other, moved); e != nil {
		t.Fatal(e)
	}
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

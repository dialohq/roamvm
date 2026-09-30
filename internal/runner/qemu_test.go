package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/dialohq/roamvm/internal/daemon"
	"github.com/dialohq/roamvm/internal/images"
)

func TestQEMUBlockGraphAndResources(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vmlinux"), []byte("kernel"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &daemon.Prepared{Dir: dir, Base: images.Base{Dir: dir, Manifest: images.Manifest{Format: "qcow2", Cmdline: "root=/dev/vda"}}, Spec: api.VirtualMachineSpec{CPUs: 8, Memory: "2Gi", Hugepages: "2Mi", Hostname: "devbox", Disks: []api.SecondaryDisk{{Name: "shared", VolumeMode: "Filesystem", ReadOnly: true}}, Devices: []api.Device{{PCIAddress: "0000:01:00.0"}}}}
	args, err := qemuArgs(t.Context(), p, filepath.Join(dir, "qmp"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(args, " ")
	for _, want := range []string{"-smp 8", "-m 2048", "memory-backend-memfd", "hugetlbsize=2097152", "vfio-pci,host=0000:01:00.0", "systemd.hostname=devbox", "-no-shutdown"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %s in %s", want, text)
		}
	}
	var nodes []map[string]any
	for i, arg := range args {
		if arg == "-blockdev" {
			var node map[string]any
			if err := json.Unmarshal([]byte(args[i+1]), &node); err != nil {
				t.Fatal(err)
			}
			nodes = append(nodes, node)
		}
	}
	if len(nodes) != 2 {
		t.Fatalf("got %d block nodes", len(nodes))
	}
	base := nodes[0]["backing"].(map[string]any)
	if base["read-only"] != true || base["backing"] != nil || nodes[0]["node-name"] != "root" {
		t.Fatalf("unsafe backing graph: %v", nodes)
	}
	if nodes[1]["read-only"] != true || nodes[1]["file"].(map[string]any)["filename"] != "/disks/shared/disk.img" {
		t.Fatalf("secondary disk changed: %v", nodes[1])
	}
}

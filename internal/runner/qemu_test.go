package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/dialohq/roamvm/internal/daemon"
	"github.com/dialohq/roamvm/internal/images"
	"github.com/stretchr/testify/require"
)

func TestQEMUBlockGraphAndResources(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "vmlinux"), []byte("kernel"), 0o600))
	p := &daemon.Prepared{Dir: dir, Base: images.Base{Dir: dir, Manifest: images.Manifest{Format: "qcow2", Cmdline: "root=/dev/vda"}}, Spec: api.VirtualMachineSpec{CPUs: 8, Memory: "2Gi", Hugepages: "2Mi", Hostname: "devbox", Disks: []api.SecondaryDisk{{Name: "shared", VolumeMode: "Filesystem", ReadOnly: true}, {Name: "data", VolumeMode: "Filesystem"}}, Devices: []api.Device{{PCIAddress: "0000:01:00.0"}}}}
	args, err := qemuArgs(t.Context(), p, filepath.Join(dir, "qmp"))
	require.NoError(t, err)
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
			require.NoError(t, json.Unmarshal([]byte(args[i+1]), &node))
			nodes = append(nodes, node)
		}
	}
	if len(nodes) != 3 {
		t.Fatalf("got %d block nodes", len(nodes))
	}
	for _, i := range []int{0, 2} {
		require.Equal(t, map[string]any{"direct": true, "no-flush": false}, nodes[i]["file"].(map[string]any)["cache"])
	}
	require.NotContains(t, nodes[1]["file"], "cache")
	base := nodes[0]["backing"].(map[string]any)
	require.NotContains(t, base["file"], "cache")
	if base["read-only"] != true || base["backing"] != nil || nodes[0]["node-name"] != "root" {
		t.Fatalf("unsafe backing graph: %v", nodes)
	}
	if nodes[1]["read-only"] != true || nodes[1]["file"].(map[string]any)["filename"] != "/disks/shared/disk.img" {
		t.Fatalf("secondary disk changed: %v", nodes[1])
	}
}

func TestQEMUBootMode(t *testing.T) {
	for _, tc := range []struct {
		name, mode       string
		kernel, firmware bool
		direct           bool
	}{
		{name: "existing image", kernel: true, direct: true},
		{name: "explicit image", mode: "Image", kernel: true, firmware: true, direct: true},
		{name: "firmware image", firmware: true},
		{name: "existing disk migration", mode: "Disk", kernel: true},
		{name: "disk with image firmware", mode: "Disk", kernel: true, firmware: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for file, present := range map[string]bool{"vmlinux": tc.kernel, "initrd": tc.kernel, "firmware": tc.firmware} {
				if present {
					require.NoError(t, os.WriteFile(filepath.Join(dir, file), []byte(file), 0o600))
				}
			}
			p := &daemon.Prepared{Dir: dir, Base: images.Base{Dir: dir, Manifest: images.Manifest{Format: "qcow2", Cmdline: "init=/original/init"}}, Spec: api.VirtualMachineSpec{CPUs: 2, Memory: "2Gi", Hostname: "workspace-123", BootMode: tc.mode}}
			args, err := qemuArgs(t.Context(), p, filepath.Join(dir, "qmp"))
			require.NoError(t, err)
			require.Contains(t, args, "virtio-blk-pci,drive=root,bootindex=1")
			if tc.direct {
				require.Contains(t, args, "-kernel")
				require.Contains(t, args, "-initrd")
				require.Contains(t, args, "init=/original/init systemd.hostname=workspace-123")
				require.NotContains(t, args, "-bios")
			} else {
				require.NotContains(t, args, "-kernel")
				require.NotContains(t, args, "-initrd")
				require.NotContains(t, args, "-append")
				require.Contains(t, args, "type=11,value=io.systemd.credential:system.hostname=workspace-123")
				if tc.firmware {
					require.Contains(t, args, "-bios")
					require.Contains(t, args, filepath.Join(dir, "firmware"))
				} else {
					require.NotContains(t, args, "-bios")
				}
			}
		})
	}
}

func TestQEMURejectsUnknownBootMode(t *testing.T) {
	_, err := qemuArgs(t.Context(), &daemon.Prepared{Spec: api.VirtualMachineSpec{BootMode: "unknown"}}, "")
	require.ErrorContains(t, err, "unsupported boot mode")
}

func TestStatusAcknowledgement(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error":"temporary update conflict"}`)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	// Keep the production request builder and substitute only its dial target.
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}}
	defer transport.CloseIdleConnections()
	c := &Client{HTTP: &http.Client{Transport: transport}}
	if c.status("Running", "") {
		t.Fatal("failed status update was acknowledged")
	}
	if !c.status("Running", "") {
		t.Fatal("retry was not acknowledged")
	}
}

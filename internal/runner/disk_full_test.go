//go:build integration && linux

package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/dialohq/roamvm/internal/daemon"
	"github.com/dialohq/roamvm/internal/disk"
	"github.com/dialohq/roamvm/internal/images"
	"github.com/dialohq/roamvm/internal/qmp"
	"github.com/stretchr/testify/require"
)

func TestDiskFullRecovery(t *testing.T) {
	baseDir := os.Getenv("ROAMVM_TEST_DISK_FULL_BASE")
	if baseDir == "" {
		t.Skip("set ROAMVM_TEST_DISK_FULL_BASE to the unpacked test guest disk directory")
	}
	require.Equal(t, 0, os.Geteuid(), "loopback filesystem test requires root")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	run := func(name string, args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
		require.NoError(t, err, "%s %v: %s", name, args, out)
		return strings.TrimSpace(string(out))
	}
	base, err := images.Open(ctx, baseDir)
	require.NoError(t, err)
	dir := t.TempDir()
	volume, mount := filepath.Join(dir, "host.ext4"), filepath.Join(dir, "working")
	f, err := os.Create(volume)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(64<<20))
	require.NoError(t, f.Close())
	run("mkfs.ext4", "-q", "-F", "-m", "0", volume)
	loop := run("losetup", "--find", "--show", volume)
	t.Cleanup(func() {
		out, err := exec.Command("losetup", "--detach", loop).CombinedOutput()
		require.NoError(t, err, "%s", out)
	})
	require.NoError(t, os.Mkdir(mount, 0o700))
	run("mount", loop, mount)
	t.Cleanup(func() {
		out, err := exec.Command("umount", mount).CombinedOutput()
		require.NoError(t, err, "%s", out)
	})
	overlay := filepath.Join(mount, "overlay.qcow2")
	require.NoError(t, disk.Create(ctx, base, overlay))
	p := &daemon.Prepared{Dir: mount, Base: base, Spec: api.VirtualMachineSpec{CPUs: 1, Memory: "512Mi"}}
	boot := func(name string) (qmp.Client, io.WriteCloser, func(string), func()) {
		t.Helper()
		socket := filepath.Join(dir, name+".qmp")
		args, err := qemuArgs(ctx, p, socket)
		require.NoError(t, err)
		for i, arg := range args {
			if arg == "-netdev" {
				args[i+1] = "user,id=net0"
			}
		}
		logPath := filepath.Join(dir, name+".log")
		log, err := os.Create(logPath)
		require.NoError(t, err)
		t.Cleanup(func() { log.Close() })
		cmd := exec.CommandContext(ctx, "qemu-system-x86_64", args...)
		cmd.Stdout, cmd.Stderr = log, log
		stdin, err := cmd.StdinPipe()
		require.NoError(t, err)
		require.NoError(t, cmd.Start())
		done := false
		t.Cleanup(func() {
			if !done {
				cmd.Process.Kill()
				cmd.Wait()
			}
			if t.Failed() {
				data, _ := os.ReadFile(logPath)
				t.Logf("%s: %s", name, data)
			}
		})
		wait := func(marker string) {
			t.Helper()
			require.Eventually(t, func() bool {
				data, _ := os.ReadFile(logPath)
				return strings.Contains(string(data), marker)
			}, 45*time.Second, 100*time.Millisecond, "waiting for %s", marker)
		}
		wait("Please press Enter to activate this console.")
		_, err = io.WriteString(stdin, "\n")
		require.NoError(t, err)
		wait("# ")
		q := qmp.Client(socket)
		stop := func() {
			t.Helper()
			require.NoError(t, q.Call(ctx, "system_powerdown", nil, nil))
			require.Eventually(t, func() bool {
				var status struct{ Status string }
				return q.Call(ctx, "query-status", nil, &status) == nil && status.Status == "shutdown"
			}, 20*time.Second, 100*time.Millisecond)
			require.NoError(t, q.Call(ctx, "quit", nil, nil))
			require.NoError(t, cmd.Wait())
			done = true
		}
		return q, stdin, wait, stop
	}
	q, stdin, wait, stop := boot("before")
	var blocks []struct {
		Driver string `json:"drv"`
		File   string `json:"file"`
		Cache  struct {
			Direct  bool
			NoFlush bool `json:"no-flush"`
		}
	}
	require.NoError(t, q.Call(ctx, "query-named-block-nodes", nil, &blocks))
	found := false
	for _, block := range blocks {
		if block.Driver == "file" && block.File == overlay {
			found = true
			require.True(t, block.Cache.Direct)
			require.False(t, block.Cache.NoFlush)
		}
	}
	require.True(t, found, "writable file node missing: %+v", blocks)
	digest := sha256.New()
	for range 96 {
		digest.Write(bytes.Repeat([]byte("x"), 1<<20))
	}
	checksum := hex.EncodeToString(digest.Sum(nil))
	_, err = io.WriteString(stdin, "dd if=/dev/zero bs=1M count=96 | tr '\\000' x > /payload; sync; printf '"+checksum+"  /payload\\n' > /payload.sha256; sync; sha256sum -c /payload.sha256 && printf 'WRITE_%s\\n' DONE\n")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		var status struct{ Status string }
		return q.Call(ctx, "query-status", nil, &status) == nil && status.Status == "io-error"
	}, 45*time.Second, 100*time.Millisecond, "guest must pause when host storage fills")
	var paused []struct {
		IOStatus string `json:"io-status"`
	}
	require.NoError(t, q.Call(ctx, "query-block", nil, &paused))
	require.NotEmpty(t, paused)
	require.Equal(t, "nospace", paused[0].IOStatus)
	t.Log("guest paused with ENOSPC; expanding only the disposable host filesystem")
	require.NoError(t, os.Truncate(volume, 320<<20))
	run("losetup", "--set-capacity", loop)
	run("resize2fs", loop)
	require.NoError(t, q.Call(ctx, "cont", nil, nil))
	wait("WRITE_DONE")
	_, err = io.WriteString(stdin, "sha256sum -c /payload.sha256 && printf 'VERIFY_%s\\n' BEFORE\n")
	require.NoError(t, err)
	wait("VERIFY_BEFORE")
	stop()
	require.NoError(t, disk.Check(ctx, overlay))
	_, stdin, wait, stop = boot("restored")
	_, err = io.WriteString(stdin, "sha256sum -c /payload.sha256 && printf 'VERIFY_%s\\n' RESTORED\n")
	require.NoError(t, err)
	wait("VERIFY_RESTORED")
	stop()
	require.NoError(t, disk.Check(ctx, overlay))
	t.Log("96 MiB payload verified after resume, clean shutdown and reboot")
}

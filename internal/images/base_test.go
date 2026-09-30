package images

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, data := range map[string][]byte{"root.raw": make([]byte, 8192), "manifest.json": []byte(`{"format":"raw"}`), "vmlinux": []byte("kernel fixture")} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o600))
	}
	return dir
}

func TestMountedBase(t *testing.T) {
	dir := fixture(t)
	base, err := Open(context.Background(), dir)
	if err != nil || base.Disk() != filepath.Join(dir, "root.raw") {
		t.Fatal(base, err)
	}
	if data, err := os.ReadFile(base.Disk()); err != nil || len(data) != 8192 {
		t.Fatal("base was modified", err)
	}
}

func TestRejectUnsafeMountedBase(t *testing.T) {
	for _, file := range []string{"root.raw", "manifest.json", "vmlinux", "initrd", "firmware"} {
		t.Run("symlink-"+file, func(t *testing.T) {
			dir := fixture(t)
			target := filepath.Join(dir, file)
			_ = os.Remove(target)
			require.NoError(t, os.Symlink("/etc/passwd", target))
			if _, err := Open(context.Background(), dir); err == nil {
				t.Fatal("artifact symlink accepted")
			}
		})
	}
	t.Run("directory-symlink", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "disk")
		require.NoError(t, os.Symlink(fixture(t), path))
		if _, err := Open(context.Background(), path); err == nil {
			t.Fatal("linked disk directory accepted")
		}
	})
	for _, invalid := range []string{"empty", "missing-kernel", "format", "unexpected", "oversized"} {
		t.Run(invalid, func(t *testing.T) {
			dir := fixture(t)
			var err error
			switch invalid {
			case "empty":
				err = os.Truncate(filepath.Join(dir, "root.raw"), 0)
			case "missing-kernel":
				err = os.Remove(filepath.Join(dir, "vmlinux"))
			case "format":
				err = os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"format":"../unsafe"}`), 0o600)
			case "unexpected":
				err = os.WriteFile(filepath.Join(dir, "extra"), nil, 0o600)
			case "oversized":
				err = os.Truncate(filepath.Join(dir, "root.raw"), 129<<30)
			}
			require.NoError(t, err)
			if _, err := Open(context.Background(), dir); err == nil {
				t.Fatal("invalid base accepted")
			}
		})
	}
}

func TestRejectMountedBaseWithBackingFile(t *testing.T) {
	dir := fixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"format":"qcow2"}`), 0o600))
	output, err := exec.Command("qemu-img", "create", "-f", "qcow2", "-F", "raw", "-b", filepath.Join(dir, "root.raw"), filepath.Join(dir, "root.qcow2")).
		CombinedOutput()
	if err != nil {
		t.Fatal(err, string(output))
	}
	if _, err := Open(context.Background(), dir); err == nil {
		t.Fatal("backing file accepted in an immutable base")
	}
}

package disk

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/dialohq/roamvm/internal/images"
	"github.com/dialohq/roamvm/internal/qcow"
)

func run(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "qemu-img", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img %s: %w: %s", args[0], err, out)
	}
	return nil
}

func Create(ctx context.Context, base images.Base, path string) error {
	return run(
		ctx,
		"create",
		"-f",
		"qcow2",
		"-F",
		base.Manifest.Format,
		"-b",
		base.Disk(),
		"-o",
		"compat=1.1,lazy_refcounts=off",
		path,
	)
}

// Rebase changes only a verified immutable backing identity's local path. It
// never trusts backing paths embedded in a downloaded QCOW2 image.
func Rebase(ctx context.Context, base images.Base, path string) error {
	if err := qcow.Validate(path, false); err != nil {
		return err
	}
	if err := run(ctx, "rebase", "-u", "-f", "qcow2", "-F", base.Manifest.Format, "-b", base.Disk(), path); err != nil {
		return err
	}
	return Check(ctx, path)
}
func Check(ctx context.Context, path string) error { return run(ctx, "check", "-f", "qcow2", path) }
func Compact(ctx context.Context, base images.Base, path string) (string, error) {
	target := filepath.Join(filepath.Dir(path), "checkpoint.qcow2")
	_ = os.Remove(target + ".partial")
	if err := Check(ctx, path); err != nil {
		return "", err
	}
	if err := run(ctx, "convert", "-t", "directsync", "-f", "qcow2", "-O", "qcow2", "-B", base.Disk(), "-F", base.Manifest.Format, path, target+".partial"); err != nil {
		return "", err
	}
	if err := Check(ctx, target+".partial"); err != nil {
		return "", err
	}
	if err := os.Rename(target+".partial", target); err != nil {
		return "", err
	}
	f, err := os.OpenFile(target, os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err = f.Sync(); err != nil {
		return "", err
	}
	return target, nil
}

package images

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/dialohq/roamvm/internal/qcow"
)

type Manifest struct {
	Format  string `json:"format"`
	Cmdline string `json:"cmdline,omitempty"`
}
type Base struct {
	Dir      string
	Manifest Manifest
}

func (b Base) Disk() string { return filepath.Join(b.Dir, "root."+b.Manifest.Format) }

// Open validates the disk payload mounted read-only by Kubernetes' image volume.
func Open(ctx context.Context, dir string) (Base, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return Base{}, err
	}
	if !info.IsDir() {
		return Base{}, errors.New("disk payload must be a directory, not a link")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Base{}, err
	}
	var total int64
	for _, entry := range entries {
		switch entry.Name() {
		case "root.raw", "root.qcow2", "manifest.json", "vmlinux", "initrd", "firmware":
		default:
			return Base{}, fmt.Errorf("unexpected disk artifact entry %q", entry.Name())
		}
		info, err := entry.Info()
		if err != nil {
			return Base{}, err
		}
		if !info.Mode().IsRegular() {
			return Base{}, errors.New("disk artifact links/devices are forbidden")
		}
		total += info.Size()
		if total > 128<<30 {
			return Base{}, errors.New("image exceeds unpacked size limit")
		}
	}
	b, err := readBase(dir)
	if err != nil {
		return Base{}, err
	}
	if b.Manifest.Format == "qcow2" {
		if err := qcow.Validate(b.Disk(), true); err != nil {
			return Base{}, err
		}
	}
	output, err := exec.CommandContext(ctx, "qemu-img", "info", "--output=json", "-f", b.Manifest.Format, b.Disk()).Output()
	if err != nil {
		return Base{}, fmt.Errorf("inspect base: %w", err)
	}
	var disk struct {
		Backing string `json:"backing-filename"`
		Size    int64  `json:"virtual-size"`
	}
	if err = json.Unmarshal(output, &disk); err != nil {
		return Base{}, err
	}
	if disk.Backing != "" || disk.Size <= 0 {
		return Base{}, errors.New("base must be a standalone disk without a backing file")
	}
	return b, nil
}
func readBase(dir string) (Base, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return Base{}, err
	}
	var m Manifest
	if err = json.Unmarshal(data, &m); err != nil {
		return Base{}, err
	}
	if m.Format != "raw" && m.Format != "qcow2" {
		return Base{}, errors.New("manifest format must be raw or qcow2")
	}
	if _, err = os.Stat(filepath.Join(dir, "root."+m.Format)); err != nil {
		return Base{}, err
	}
	_, kernel := os.Stat(filepath.Join(dir, "vmlinux"))
	_, firmware := os.Stat(filepath.Join(dir, "firmware"))
	if kernel != nil && firmware != nil {
		return Base{}, errors.New("base needs disk/vmlinux or disk/firmware")
	}
	return Base{dir, m}, nil
}

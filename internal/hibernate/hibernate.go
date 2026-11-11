// Package hibernate binds a QEMU snapshot to its guest configuration and host
// compatibility. The marker is published only after capture, while CPUs stay stopped.
package hibernate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/dialohq/roamvm/internal/fileio"
)

const Tag = "roamvm-hibernate"

func Validate(spec api.VirtualMachineSpec) error {
	if spec.Hibernate && (len(spec.Disks) != 0 || len(spec.Devices) != 0 || len(spec.ProjectedDisks()) != 0 || spec.GuestServiceAccountToken != nil) {
		return errors.New("hibernation currently requires a root-only guest without projected disks, guest tokens or passthrough devices")
	}
	return nil
}

func digest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func Identity(ctx context.Context, spec api.VirtualMachineSpec) (api.Hibernation, error) {
	if err := Validate(spec); err != nil {
		return api.Hibernation{}, err
	}
	config, err := json.Marshal(struct {
		Image, BootMode, Hostname, Memory, Hugepages, RootDiskSize string
		CPUs                                                       int32
	}{spec.Image, spec.BootMode, spec.Hostname, spec.Memory, spec.Hugepages, spec.RootDiskSize, spec.CPUs})
	if err != nil {
		return api.Hibernation{}, err
	}
	version, err := exec.CommandContext(ctx, "qemu-system-x86_64", "--version").Output()
	if err != nil {
		return api.Hibernation{}, err
	}
	info, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return api.Hibernation{}, err
	}
	var cpu []string
	for line := range strings.SplitSeq(strings.SplitN(string(info), "\n\n", 2)[0], "\n") {
		key, value, _ := strings.Cut(line, ":")
		switch strings.TrimSpace(key) {
		case "vendor_id", "cpu family", "model", "stepping", "flags":
			cpu = append(cpu, strings.TrimSpace(key)+":"+strings.Join(strings.Fields(value), " "))
		}
	}
	if len(cpu) != 5 {
		return api.Hibernation{}, errors.New("cannot establish host CPU compatibility")
	}
	return api.Hibernation{Config: digest(config), QEMU: string(version), CPU: digest([]byte(strings.Join(cpu, "\n")))}, nil
}

type marker struct {
	Owner    string
	Identity api.Hibernation
}

func path(dir string) string { return filepath.Join(dir, "hibernation.json") }

// Clear invalidates the previous capture before any new QEMU can mutate disk.
func Clear(dir string) error {
	if err := os.Remove(path(dir)); err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	return fileio.SyncClose(f, nil)
}

func Save(dir, owner string, identity api.Hibernation) error {
	b, err := json.Marshal(marker{owner, identity})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path(dir)+".partial", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err = fileio.SyncClose(f, err); err != nil {
		return err
	}
	if err = os.Rename(path(dir)+".partial", path(dir)); err != nil {
		return err
	}
	f, err = os.Open(dir)
	if err != nil {
		return err
	}
	return fileio.SyncClose(f, nil)
}

func Load(dir, owner string) (*api.Hibernation, error) {
	b, err := os.ReadFile(path(dir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m marker
	if err = json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.Owner != owner || m.Identity.Config == "" || m.Identity.QEMU == "" || m.Identity.CPU == "" {
		return nil, errors.New("hibernation marker does not belong to the stopped runner")
	}
	return &m.Identity, nil
}

func Check(ctx context.Context, spec api.VirtualMachineSpec, saved *api.Hibernation) error {
	if !spec.Hibernate || saved == nil {
		return nil
	}
	current, err := Identity(ctx, spec)
	if err != nil {
		return err
	}
	if current != *saved {
		return errors.New("hibernation configuration, QEMU or CPU changed; disable hibernation to deliberately cold boot")
	}
	return nil
}

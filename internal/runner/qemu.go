package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"

	"github.com/dialohq/roamvm/internal/daemon"
	"k8s.io/apimachinery/pkg/api/resource"
)

func qemuArgs(ctx context.Context, p *daemon.Prepared, socket string) ([]string, error) {
	memory, err := resource.ParseQuantity(p.Spec.Memory)
	if err != nil {
		return nil, err
	}
	args := []string{
		"-machine", "q35,accel=kvm", "-cpu", "host", "-smp", strconv.Itoa(int(p.Spec.CPUs)),
		"-m", strconv.FormatInt((memory.Value()+(1<<20)-1)>>20, 10),
		"-nodefaults", "-no-user-config", "-display", "none", "-serial", "stdio", "-no-shutdown",
		"-qmp", "unix:" + socket + ",server=on,wait=off",
		"-netdev", "tap,id=net0,ifname=vm-tap,script=no,downscript=no",
		"-device", "virtio-net-pci,netdev=net0,mac=02:00:00:00:00:02",
		"-device", "virtio-rng-pci",
	}
	if p.Spec.Hugepages != "" {
		pages := resource.MustParse(p.Spec.Hugepages)
		args = append(args, "-object", "memory-backend-memfd,id=ram,size="+strconv.FormatInt(memory.Value(), 10)+",hugetlb=on,hugetlbsize="+strconv.FormatInt(pages.Value(), 10)+",prealloc=on,share=on", "-machine", "memory-backend=ram")
	}
	file := func(path string) map[string]any { return map[string]any{"driver": "file", "filename": path} }
	base := map[string]any{"driver": p.Base.Manifest.Format, "file": file(p.Base.Disk()), "read-only": true}
	if p.Base.Manifest.Format == "qcow2" {
		base["backing"] = nil
	}
	root := map[string]any{"driver": "qcow2", "node-name": "root", "file": file(filepath.Join(p.Dir, "overlay.qcow2")), "backing": base}
	addDisk := func(node map[string]any, name string) error {
		encoded, err := json.Marshal(node)
		if err != nil {
			return err
		}
		args = append(args, "-blockdev", string(encoded), "-device", "virtio-blk-pci,drive="+name)
		return nil
	}
	if err = addDisk(root, "root"); err != nil {
		return nil, err
	}
	addRaw := func(path, name string, readonly bool) error {
		return addDisk(map[string]any{"driver": "raw", "node-name": name, "file": file(path), "read-only": readonly}, name)
	}
	if _, err = os.Stat(filepath.Join(p.Base.Dir, "vmlinux")); err == nil {
		cmdline := p.Base.Manifest.Cmdline
		if p.Spec.Hostname != "" {
			cmdline += " systemd.hostname=" + p.Spec.Hostname
		}
		args = append(args, "-kernel", filepath.Join(p.Base.Dir, "vmlinux"), "-append", cmdline)
		if _, err = os.Stat(filepath.Join(p.Base.Dir, "initrd")); err == nil {
			args = append(args, "-initrd", filepath.Join(p.Base.Dir, "initrd"))
		}
	} else {
		args = append(args, "-bios", filepath.Join(p.Base.Dir, "firmware"))
	}
	for i, d := range p.Spec.Disks {
		path := "/dev/disks/" + d.Name
		if d.VolumeMode == "Filesystem" {
			path = "/disks/" + d.Name + "/disk.img"
		}
		if err = addRaw(path, "disk"+strconv.Itoa(i), d.ReadOnly); err != nil {
			return nil, err
		}
	}
	for _, d := range p.Spec.Devices {
		args = append(args, "-device", "vfio-pci,host="+d.PCIAddress)
	}
	for i, d := range p.Spec.ProjectedDisks() {
		path := "/tmp/" + d.VolumeName() + ".iso"
		if err = command(ctx, "genisoimage", "-quiet", "-follow-links", "-rock", "-joliet", "-V", d.Label, "-o", path, d.MountPath()); err != nil {
			return nil, err
		}
		if err = addRaw(path, "config"+strconv.Itoa(i), true); err != nil {
			return nil, err
		}
	}
	return args, nil
}

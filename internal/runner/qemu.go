package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/dialohq/roamvm/internal/daemon"
	"github.com/dialohq/roamvm/internal/fileio"
	"github.com/dialohq/roamvm/internal/hibernate"
	"k8s.io/apimachinery/pkg/api/resource"
)

func qemuArgs(ctx context.Context, p *daemon.Prepared, socket string) ([]string, error) {
	if p.Spec.BootMode != "" && p.Spec.BootMode != "Image" && p.Spec.BootMode != "Disk" {
		return nil, fmt.Errorf("unsupported boot mode %q", p.Spec.BootMode)
	}
	memory, err := resource.ParseQuantity(p.Spec.Memory)
	if err != nil {
		return nil, err
	}
	args := []string{
		"-machine", "q35,accel=kvm", "-cpu", "host", "-smp", strconv.Itoa(int(p.Spec.CPUs)),
		"-m", strconv.FormatInt((memory.Value()+(1<<20)-1)>>20, 10),
		"-nodefaults", "-no-user-config", "-display", "none", "-no-shutdown",
		"-chardev", "socket,id=serial0,path=" + filepath.Join(p.Dir, "serial.sock") + ",server=on,wait=off,logfile=/dev/stdout",
		"-serial", "chardev:serial0",
		"-qmp", "unix:" + socket + ",server=on,wait=off",
		"-netdev", "tap,id=net0,ifname=vm-tap,script=no,downscript=no",
		"-device", "virtio-net-pci,netdev=net0,mac=02:00:00:00:00:02",
		"-device", "virtio-rng-pci",
	}
	if p.Spec.GuestAgent {
		args = append(args,
			"-uuid", p.Session.Head.VMID,
			"-chardev", "socket,id=qga,path="+filepath.Join(p.Dir, "guest-agent.sock")+",server=on,wait=off",
			"-device", "virtio-serial-pci",
			"-device", "virtserialport,chardev=qga,name=org.qemu.guest_agent.0")
	}
	if p.Spec.Hugepages != "" {
		pages := resource.MustParse(p.Spec.Hugepages)
		args = append(args, "-object", "memory-backend-memfd,id=ram,size="+strconv.FormatInt(memory.Value(), 10)+",hugetlb=on,hugetlbsize="+strconv.FormatInt(pages.Value(), 10)+",prealloc=on,share=on", "-machine", "memory-backend=ram")
	}
	if p.ExternalRAM {
		ramSize := ((memory.Value() + (1 << 20) - 1) >> 20) << 20
		if p.Hibernation == nil {
			f, err := os.OpenFile(filepath.Join(p.Dir, hibernate.RAM), os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600)
			if err != nil {
				return nil, err
			}
			if err = fileio.SyncClose(f, f.Truncate(ramSize)); err != nil {
				return nil, err
			}
		}
		sharing := "share=on,readonly=off"
		if privateRAM(p) {
			// Guest writes become anonymous CoW pages, never filesystem writeback.
			sharing = "share=off,readonly=on"
		}
		args = append(args, "-object", "memory-backend-file,id=pc.ram,size="+strconv.FormatInt(ramSize, 10)+",mem-path="+storagePath(p, hibernate.RAM)+","+sharing+",rom=off,prealloc=off,discard-data=off,x-use-canonical-path-for-ramblock-id=off", "-machine", "memory-backend=pc.ram")
		if p.Hibernation != nil {
			args = append(args, "-incoming", "defer")
		}
	}
	if (p.Hibernation != nil && p.Spec.Hibernate) || privateRAM(p) {
		args = append(args, "-S")
	}
	file := func(path string, writable bool) map[string]any {
		node := map[string]any{"driver": "file", "filename": path}
		if p.PagerDir != "" && filepath.Dir(path) == p.PagerDir {
			// runner.lock and the ownership CAS already exclude another writer.
			// The fault-served file does not implement host advisory locks.
			node["locking"] = "off"
		}
		if writable {
			// RAM restore makes many small reads. Retain the node's evictable
			// page cache for hibernating roots, while honoring every guest flush.
			node["cache"] = map[string]any{"direct": !p.Spec.Hibernate, "no-flush": false}
		}
		return node
	}
	base := map[string]any{"driver": p.Base.Manifest.Format, "file": file(p.Base.Disk(), false), "read-only": true}
	if p.Base.Manifest.Format == "qcow2" {
		base["backing"] = nil
	}
	root := map[string]any{"driver": "qcow2", "node-name": "root", "file": file(storagePath(p, "overlay.qcow2"), true), "backing": base}
	addDisk := func(node map[string]any, name string) error {
		encoded, err := json.Marshal(node)
		if err != nil {
			return err
		}
		device := "virtio-blk-pci,drive=" + name
		if name == "root" {
			device += ",bootindex=1"
		}
		args = append(args, "-blockdev", string(encoded), "-device", device)
		return nil
	}
	if err = addDisk(root, "root"); err != nil {
		return nil, err
	}
	addRaw := func(path, name string, readonly bool) error {
		return addDisk(map[string]any{"driver": "raw", "node-name": name, "file": file(path, !readonly), "read-only": readonly}, name)
	}
	if _, err = os.Stat(filepath.Join(p.Base.Dir, "vmlinux")); err == nil && p.Spec.BootMode != "Disk" {
		cmdline := p.Base.Manifest.Cmdline
		if p.Spec.Hostname != "" {
			cmdline += " systemd.hostname=" + p.Spec.Hostname
		}
		args = append(args, "-kernel", filepath.Join(p.Base.Dir, "vmlinux"), "-append", cmdline)
		if _, err = os.Stat(filepath.Join(p.Base.Dir, "initrd")); err == nil {
			args = append(args, "-initrd", filepath.Join(p.Base.Dir, "initrd"))
		}
	} else {
		if _, err = os.Stat(filepath.Join(p.Base.Dir, "firmware")); err == nil {
			args = append(args, "-bios", filepath.Join(p.Base.Dir, "firmware"))
		}
		if p.Spec.Hostname != "" {
			args = append(args, "-smbios", "type=11,value=io.systemd.credential:system.hostname="+p.Spec.Hostname)
		}
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
	if p.Spec.GuestServiceAccountToken != nil {
		args = append(args,
			"-fsdev", "local,id=guest-token,path="+daemon.GuestTokenDirectory+",security_model=none,readonly=on",
			"-device", "virtio-9p-pci,fsdev=guest-token,mount_tag=roamvm-token")
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

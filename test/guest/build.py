#!/usr/bin/env python3
"""Build a small real ext4 VM disk using an existing Linux kernel and BusyBox.

Requires Go, mke2fs, modprobe, cpio, gzip, xz and Linux's extract-vmlinux script.
No mounts, loop devices, containers or production credentials are needed.
"""

import argparse
import json
import os
import shutil
import subprocess as sp
import tarfile
from pathlib import Path

p = argparse.ArgumentParser()
p.add_argument("--out", required=True)
p.add_argument("--kernel", required=True)
p.add_argument("--modules", required=True)
p.add_argument("--kernel-version", required=True)
p.add_argument("--busybox", required=True)
p.add_argument("--extract-vmlinux", required=True)
a = p.parse_args()
out = Path(a.out).resolve()
out.mkdir(parents=True, exist_ok=True)
root, initrd, disk = (out / n for n in ["root", "initrd", "disk"])
for d in [root, initrd]:
    for name in [
        "bin",
        "sbin",
        "etc",
        "proc",
        "sys",
        "dev",
        "tmp",
        "run",
        "root",
        "mnt",
    ]:
        (d / name).mkdir(parents=True, exist_ok=True)
    shutil.copyfile(a.busybox, d / "bin/busybox")
    (d / "bin/busybox").chmod(0o755)
    for applet in sp.check_output([a.busybox, "--list"], text=True).splitlines():
        target = d / "bin" / applet
        if not target.exists():
            target.symlink_to("busybox")
disk.mkdir(exist_ok=True)
if not (root / "sbin/init").is_symlink():
    (root / "sbin/init").symlink_to("/bin/busybox")
(root / "etc/inittab").write_text("""::sysinit:/etc/rcS
ttyS0::respawn:/bin/sh
::shutdown:/bin/sync
::shutdown:/bin/umount -a -r
""")
(root / "etc/rcS").write_text("""#!/bin/sh
export PATH=/bin:/sbin
mount -t proc proc /proc
mount -t sysfs sys /sys
mount -t devtmpfs dev /dev
mount -t tmpfs tmpfs /tmp
mount -t tmpfs tmpfs /run
hostname roamvm-test
ip link set lo up
ip link set eth0 up
udhcpc -i eth0 -s /etc/udhcpc -b
acpid -d -c /etc/acpi /dev/input/event0 &
mkdir -p /config /secondary /agent-config /tool-config
for d in /dev/vd?; do
 [ "$d" = /dev/vda ] && continue
 case "$(blkid "$d")" in
  *TEST_AGENT*) mount -t iso9660 -o ro "$d" /agent-config; continue ;;
  *TEST_TOOL*) mount -t iso9660 -o ro "$d" /tool-config; continue ;;
 esac
 mount -t iso9660 -o ro "$d" /config 2>/dev/null && continue
 mount -t ext4 "$d" /secondary 2>/dev/null || true
done
/bin/guest-test &
""")
(root / "etc/rcS").chmod(0o755)
(root / "etc/udhcpc").write_text("""#!/bin/sh
export PATH=/bin:/sbin
case "$1" in
 bound|renew)
  ip addr flush dev "$interface"
  ip addr add "$ip/30" dev "$interface"
  ip route replace default via "$router" dev "$interface"
  : > /etc/resolv.conf
  for server in $dns; do echo "nameserver $server" >> /etc/resolv.conf; done
  echo "search $search" >> /etc/resolv.conf
  ;;
esac
""")
(root / "etc/udhcpc").chmod(0o755)
(root / "etc/acpi/events").mkdir(parents=True, exist_ok=True)
(root / "etc/acpid.conf").write_text("PWRF poweroff\nPWRB poweroff\n")
(root / "etc/acpi/poweroff").write_text("#!/bin/sh\nexec /bin/poweroff\n")
(root / "etc/acpi/poweroff").chmod(0o755)
sp.run(
    ["go", "build", "-o", str(root / "bin/guest-test"), "./test/guest"],
    check=True,
    env={**os.environ, "CGO_ENABLED": "0"},
)
modules = []
for module in [
    "virtio_pci",
    "virtio_blk",
    "virtio_net",
    "ext4",
    "isofs",
    "af_packet",
    "button",
    "evdev",
]:
    deps = sp.check_output(
        ["modprobe", "--show-depends", "-d", a.modules, "-S", a.kernel_version, module],
        text=True,
    )
    for line in deps.splitlines():
        if not line.startswith("insmod "):
            continue
        path = Path(line.split()[1])
        name = path.name.removesuffix(".xz").removesuffix(".zst")
        if name in modules:
            continue
        modules.append(name)
        (initrd / "modules").mkdir(exist_ok=True)
        if path.suffix == ".xz":
            data = sp.check_output(["xz", "-dc", str(path)])
        elif path.suffix == ".zst":
            data = sp.check_output(["zstd", "-dc", str(path)])
        else:
            data = path.read_bytes()
        (initrd / "modules" / name).write_bytes(data)
load = "\n".join("insmod /modules/" + module for module in modules)
(initrd / "init").write_text(
    """#!/bin/sh
export PATH=/bin
mount -t proc proc /proc
mount -t sysfs sys /sys
mount -t devtmpfs dev /dev
"""
    + load
    + """
for i in 1 2 3 4 5; do [ -b /dev/vda ] && break; sleep 1; done
mount -t ext4 /dev/vda /mnt || exec sh
umount /proc /sys /dev
exec switch_root /mnt /sbin/init
"""
)
(initrd / "init").chmod(0o755)
with open(disk / "initrd", "wb") as f:
    sp.run(
        ["sh", "-c", "find . -print0 | cpio --null -o --format=newc --quiet | gzip -1"],
        cwd=initrd,
        stdout=f,
        check=True,
    )
with open(disk / "vmlinux", "wb") as f:
    sp.run(["sh", a.extract_vmlinux, a.kernel], stdout=f, check=True)
with open(disk / "root.raw", "wb") as f:
    f.truncate(256 << 20)
sp.run(
    ["mke2fs", "-q", "-t", "ext4", "-F", "-d", str(root), str(disk / "root.raw")],
    check=True,
)
(disk / "manifest.json").write_text(
    json.dumps(
        {
            "format": "raw",
            "cmdline": "console=ttyS0 root=/dev/vda rw panic=1 net.ifnames=0",
        }
    )
)
with tarfile.open(out / "guest.tar", "w:gz") as t:
    t.add(disk, arcname="disk")
print(out / "guest.tar")

{pkgs}: let
  kernel = pkgs.linuxPackages.kernel;
  moduleNames = ["virtio_pci" "virtio_blk" "virtio_net" "ext4" "isofs" "af_packet" "button" "evdev"];
  moduleClosure = pkgs.makeModulesClosure {
    kernel = kernel.modules;
    firmware = pkgs.emptyDirectory;
    rootModules = moduleNames;
    allowMissing = false;
  };
  # The initrd is already compressed. Avoid decompressing each module again
  # at boot, including BusyBox's failed direct-load attempt before its fallback.
  modules =
    pkgs.runCommand "guest-modules" {
      nativeBuildInputs = [pkgs.zstd pkgs.kmod];
    } ''
      mkdir -p $out
      cp -r ${moduleClosure}/lib $out/lib
      chmod -R u+w $out/lib
      find $out/lib -name '*.ko.zst' -exec zstd -d --rm {} \;
      depmod -b $out ${kernel.modDirVersion}
    '';
  busybox = pkgs.pkgsStatic.busybox;
  applets = pkgs.runCommand "guest-applets" {} ''
    mkdir -p $out/bin
    cp ${busybox}/bin/busybox $out/bin/
    for applet in $($out/bin/busybox --list); do
      [ "$applet" = busybox ] || ln -s busybox "$out/bin/$applet"
    done
  '';
  guest = import ./service.nix {inherit pkgs;};
  init = pkgs.writeScript "guest-init" ''
    #!/bin/sh
    export PATH=/bin
    mount -t proc proc /proc
    mount -t sysfs sys /sys
    mount -t devtmpfs dev /dev
    modprobe -a ${pkgs.lib.concatStringsSep " " moduleNames}
    for i in 1 2 3 4 5; do [ -b /dev/vda ] && break; sleep 1; done
    mkdir -p /mnt
    mount -t ext4 /dev/vda /mnt || exec sh
    umount /proc /sys /dev
    exec switch_root /mnt /sbin/init
  '';
  initrd = pkgs.makeInitrd {
    contents = [
      {
        object = "${applets}/bin";
        symlink = "/bin";
      }
      {
        object = "${modules}/lib";
        symlink = "/lib";
      }
      {
        object = init;
        symlink = "/init";
      }
    ];
  };
in
  pkgs.runCommand "roamvm-test-guest.tar.gz" {
    nativeBuildInputs = [pkgs.e2fsprogs pkgs.qemu-utils pkgs.linux-scripts pkgs.binutils];
  } ''
    mkdir -p root/{sbin,proc,sys,dev,tmp,run,mnt} disk
    cp -r ${applets}/bin root/bin
    cp -r ${./root}/etc root/etc
    chmod -R u+w root
    ln -s /bin/busybox root/sbin/init
    cp ${guest}/bin/guest root/bin/guest-test
    truncate -s 256M root.raw
    mke2fs -q -t ext4 -F -d root root.raw
    qemu-img convert -f raw -O qcow2 -c -o compression_type=zstd root.raw disk/root.qcow2
    qemu-img compare -f raw -F qcow2 root.raw disk/root.qcow2
    cp ${initrd}/initrd disk/initrd
    # Decompress once in the cached image build, rather than on every guest boot.
    # QEMU's PVH entry still uses the same Q35 devices, ACPI tables and initrd.
    extract-vmlinux ${kernel}/bzImage > disk/vmlinux
    strip --strip-debug disk/vmlinux
    readelf -n disk/vmlinux | grep -q 'Xen.*0x00000012'
    # Keep errors on the console, without serializing every boot message through the
    # nested emulated UART. The full kernel log remains in the guest ring buffer.
    # The test Q35 guest has one PCI root. Skip Linux's legacy peer-root sweep
    # (255 absent buses × 32 slots), retaining normal ACPI/PCI enumeration.
    echo '{"format":"qcow2","cmdline":"console=ttyS0,115200 quiet pci=lastbus=0 root=/dev/vda rw panic=1 net.ifnames=0"}' > disk/manifest.json
    tar -czf $out disk
  ''

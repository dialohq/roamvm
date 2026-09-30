{pkgs}: let
  kernel = pkgs.linuxPackages.kernel;
  extractVmlinux = pkgs.fetchurl {
    url = "https://raw.githubusercontent.com/torvalds/linux/v6.18/scripts/extract-vmlinux";
    hash = "sha256-qstrsJryJ6bPUI9Sg0WPABcio5q9nEz7AmvJfDFiSRA=";
  };
  moduleNames = ["virtio_pci" "virtio_blk" "virtio_net" "ext4" "isofs" "af_packet" "button" "evdev"];
  modules = pkgs.makeModulesClosure {
    kernel = kernel.modules;
    firmware = pkgs.emptyDirectory;
    rootModules = moduleNames;
    allowMissing = false;
  };
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
    nativeBuildInputs = [pkgs.e2fsprogs pkgs.gzip pkgs.xz pkgs.zstd pkgs.binutils];
  } ''
    mkdir -p root/{sbin,proc,sys,dev,tmp,run,mnt} disk
    cp -r ${applets}/bin root/bin
    cp -r ${./root}/etc root/etc
    chmod -R u+w root
    ln -s /bin/busybox root/sbin/init
    cp ${guest}/bin/guest root/bin/guest-test
    truncate -s 256M disk/root.raw
    mke2fs -q -t ext4 -F -d root disk/root.raw
    cp ${initrd}/initrd disk/initrd
    bash ${extractVmlinux} ${kernel}/bzImage > disk/vmlinux
    echo '{"format":"raw","cmdline":"console=ttyS0 root=/dev/vda rw panic=1 net.ifnames=0"}' > disk/manifest.json
    tar -czf $out disk
  ''

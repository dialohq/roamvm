{
  pkgs,
  nixpkgs,
}: let
  service = import ./service.nix {inherit pkgs;};
  guest = nixpkgs.lib.nixosSystem {
    system = pkgs.stdenv.hostPlatform.system;
    modules = [
      ../../nix/online-grow.nix
      ({modulesPath, ...}: {
        imports = [(modulesPath + "/profiles/minimal.nix")];
        system.stateVersion = "26.05";
        nix.enable = false;
        nixpkgs.flake.setNixPath = false;
        nixpkgs.flake.setFlakeRegistry = false;
        boot.loader.grub.enable = false;
        boot.initrd.availableKernelModules = ["virtio_pci" "virtio_blk" "virtio_net" "ext4"];
        boot.kernelParams = ["console=ttyS0,115200" "quiet" "net.ifnames=0"];
        fileSystems."/" = {
          device = "/dev/disk/by-label/nixos";
          fsType = "ext4";
        };
        networking = {
          hostName = "resize-test";
          useDHCP = false;
          useNetworkd = true;
          firewall.enable = false;
        };
        systemd.network.networks."10-ethernet" = {
          matchConfig.Name = "eth0";
          networkConfig.DHCP = "ipv4";
        };
        systemd.network.wait-online.enable = false;
        # Test-only gate after a growth pass: deliver another disk event while
        # the oneshot is still active, before allowing it to become inactive.
        systemd.services.roamvm-grow-root.serviceConfig.ExecStartPre = pkgs.writeShellScript "count-growth" ''
          count=$(cat /run/growth-passes 2>/dev/null || echo 0)
          echo $((count + 1)) > /run/growth-passes.next
          mv /run/growth-passes.next /run/growth-passes
        '';
        systemd.services.roamvm-grow-root.serviceConfig.ExecStartPost = pkgs.writeShellScript "hold-growth" ''
          if test -e /run/hold-growth; then
            touch /run/growth-held
            trap 'rm -f /run/growth-held' EXIT
            while test -e /run/hold-growth; do sleep 0.1; done
          fi
        '';
        systemd.services.guest-test = {
          wantedBy = ["multi-user.target"];
          after = ["local-fs.target"];
          serviceConfig.ExecStart = "${service}/bin/guest";
        };
      })
    ];
  };
  disk = import "${nixpkgs}/nixos/lib/make-disk-image.nix" {
    inherit pkgs;
    inherit (pkgs) lib;
    config = guest.config;
    format = "raw";
    diskSize = 1536;
    partitionTableType = "legacy";
    installBootLoader = false;
    copyChannel = false;
  };
in
  pkgs.runCommand "roamvm-nixos-resize-test.tar.gz" {
    nativeBuildInputs = [pkgs.qemu-utils];
  } ''
    mkdir disk
    qemu-img convert -f raw -O qcow2 -c -o compression_type=zstd ${disk}/nixos.img disk/root.qcow2
    qemu-img compare -f raw -F qcow2 ${disk}/nixos.img disk/root.qcow2
    ln -s ${guest.config.system.build.kernel}/bzImage disk/vmlinux
    ln -s ${guest.config.system.build.initialRamdisk}/initrd disk/initrd
    echo '{"format":"qcow2","cmdline":"init=${guest.config.system.build.toplevel}/init console=ttyS0,115200 quiet net.ifnames=0"}' > disk/manifest.json
    tar -chzf $out disk
  ''

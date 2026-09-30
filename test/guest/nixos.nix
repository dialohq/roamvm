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
        boot.kernelParams = ["console=ttyS0" "net.ifnames=0"];
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
    echo '{"format":"qcow2","cmdline":"init=${guest.config.system.build.toplevel}/init console=ttyS0 net.ifnames=0"}' > disk/manifest.json
    tar -chzf $out disk
  ''

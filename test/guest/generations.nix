{
  pkgs,
  nixpkgs,
  diskBoot ? false,
}: let
  service = import ./service.nix {inherit pkgs;};
  configuration = generation:
    nixpkgs.lib.nixosSystem {
      system = pkgs.stdenv.hostPlatform.system;
      modules = [
        ({modulesPath, ...}: {
          imports = [(modulesPath + "/profiles/minimal.nix")];
          system.stateVersion = "26.05";
          nix.settings.experimental-features = ["nix-command" "flakes"];
          nix.channel.enable = false;
          nixpkgs.flake.setNixPath = false;
          nixpkgs.flake.setFlakeRegistry = false;
          boot.loader.grub = {
            enable = true;
            device = "/dev/vda";
            font = null;
            splashImage = null;
          };
          boot.loader.timeout = 0;
          boot.initrd.systemd.enable = true;
          boot.initrd.availableKernelModules = ["virtio_pci" "virtio_blk" "virtio_net" "ext4"];
          boot.kernelParams = ["console=ttyS0" "net.ifnames=0" "generation=${generation}"];
          fileSystems."/" = {
            device = "/dev/disk/by-label/nixos";
            fsType = "ext4";
          };
          networking = {
            hostName = "";
            useDHCP = false;
            useNetworkd = true;
            firewall.enable = false;
          };
          systemd.network.networks."10-ethernet" = {
            matchConfig.Name = "eth0";
            networkConfig.DHCP = "ipv4";
          };
          systemd.network.wait-online.enable = false;
          environment.etc."generation-test".text = generation;
          systemd.services.guest-test = {
            wantedBy = ["multi-user.target"];
            after = ["local-fs.target"];
            serviceConfig.ExecStart = "${service}/bin/guest";
          };
        })
      ];
    };
  initial = configuration "initial";
  updated = configuration "updated";
  disk = import "${nixpkgs}/nixos/lib/make-disk-image.nix" {
    inherit pkgs;
    inherit (pkgs) lib;
    config = initial.config;
    format = "raw";
    diskSize = 3072;
    partitionTableType = "legacy";
    installBootLoader = true;
    copyChannel = false;
    additionalPaths = [updated.config.system.build.toplevel];
    contents = [
      {
        source = pkgs.writeText "generation-target" "${updated.config.system.build.toplevel}";
        target = "/generation-target";
      }
    ];
  };
in
  pkgs.runCommand "roamvm-generation-test.tar.gz" {
    nativeBuildInputs = [pkgs.qemu-utils];
  } ''
    mkdir disk
    qemu-img convert -f raw -O qcow2 -c -o compression_type=zstd ${disk}/nixos.img disk/root.qcow2
    qemu-img compare -f raw -F qcow2 ${disk}/nixos.img disk/root.qcow2
    ${pkgs.lib.optionalString (!diskBoot) ''
      ln -s ${initial.config.system.build.kernel}/bzImage disk/vmlinux
      ln -s ${initial.config.system.build.initialRamdisk}/initrd disk/initrd
    ''}
    ${pkgs.lib.optionalString diskBoot ''
      ln -s ${pkgs.qemu_kvm}/share/qemu/bios-256k.bin disk/firmware
    ''}
    echo '${builtins.toJSON ({format = "qcow2";}
      // pkgs.lib.optionalAttrs (!diskBoot) {
        cmdline = "init=${initial.config.system.build.toplevel}/init console=ttyS0 net.ifnames=0 generation=initial";
      })}' > disk/manifest.json
    tar -chzf $out disk
  ''

{
  pkgs,
  nixpkgs,
}: let
  amp = import ./amp.nix {inherit pkgs;};
  guest = nixpkgs.lib.nixosSystem {
    system = pkgs.stdenv.hostPlatform.system;
    modules = [
      ./online-grow.nix
      ./remote-builders.nix
      ({modulesPath, ...}: {
        imports = [(modulesPath + "/profiles/minimal.nix")];
        system.stateVersion = "26.05";
        nix.settings.experimental-features = ["nix-command" "flakes"];
        nix.channel.enable = false;
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
          hostName = "orb";
          useDHCP = false;
          useNetworkd = true;
          firewall.enable = false;
        };
        systemd.network.networks."10-ethernet" = {
          matchConfig.Name = "eth0";
          networkConfig.DHCP = "ipv4";
          dhcpV4Config.ClientIdentifier = "mac";
        };
        systemd.network.wait-online.enable = false;
        services.qemuGuest.enable = true;
        services.openssh.enable = true;
        # The VM UID is stable on resume and distinct for every disk clone.
        system.activationScripts.orb-identity = {
          deps = ["etc"];
          text = ''
            identity=$(${pkgs.coreutils}/bin/tr -d '-\n' </sys/class/dmi/id/product_uuid)
            if [ "$(cat /etc/machine-id 2>/dev/null)" != "$identity" ]; then
              printf '%s\n' "$identity" >/etc/machine-id
              rm -f /etc/ssh/ssh_host_*
            fi
          '';
        };
        users.groups.user = {};
        users.users.user = {
          isNormalUser = true;
          group = "user";
          extraGroups = ["wheel"];
        };
        security.sudo.wheelNeedsPassword = false;
        programs.nix-ld.enable = true;
        environment.systemPackages = with pkgs; [amp bash coreutils util-linux cacert git curl nodejs tmux ripgrep gh openssh python3 jq procps];
        systemd.tmpfiles.rules = [
          "L+ /usr/local/bin/amp - - - - ${amp}/bin/amp"
          "L+ /usr/bin/git - - - - ${pkgs.git}/bin/git"
          "L+ /bin/bash - - - - ${pkgs.bash}/bin/bash"
        ];
      })
    ];
  };
  disk = import "${nixpkgs}/nixos/lib/make-disk-image.nix" {
    inherit pkgs;
    inherit (pkgs) lib;
    config = guest.config;
    format = "raw";
    diskSize = "auto";
    additionalSpace = "512M";
    partitionTableType = "legacy";
    installBootLoader = false;
    copyChannel = false;
  };
in
  pkgs.runCommand "roamvm-orb.tar.gz" {nativeBuildInputs = [pkgs.qemu-utils];} ''
    mkdir disk
    qemu-img convert -f raw -O qcow2 -c -o compression_type=zstd ${disk}/nixos.img disk/root.qcow2
    qemu-img compare -f raw -F qcow2 ${disk}/nixos.img disk/root.qcow2
    ln -s ${guest.config.system.build.kernel}/bzImage disk/vmlinux
    ln -s ${guest.config.system.build.initialRamdisk}/initrd disk/initrd
    echo '{"format":"qcow2","cmdline":"init=${guest.config.system.build.toplevel}/init console=ttyS0,115200 quiet net.ifnames=0"}' > disk/manifest.json
    tar -chzf $out disk
  ''

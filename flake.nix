{
  description = "RoamVM development and real-KVM test fixture";
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/6aefcda9401be8acc2b74244fb3b37520ea1f0a8";
  outputs = {nixpkgs, ...}: let
    pkgs = import nixpkgs {
      system = "x86_64-linux";
      # Isolated compatibility fixture; never used by the runtime or deployment.
      config.permittedInsecurePackages = ["minio-2025-10-15T17-29-55Z"];
    };
  in {
    nixosModules.online-grow = import ./nix/online-grow.nix;
    nixosConfigurations = builtins.listToAttrs (map (name: {
      inherit name;
      value = nixpkgs.lib.nixosSystem {
        system = "x86_64-linux";
        specialArgs = {inherit name;};
        modules = [./test/libvirt/node.nix];
      };
    }) ["roamvm-libvirt-control-plane" "roamvm-libvirt-worker" "roamvm-libvirt-worker2"]);
    packages.x86_64-linux.test-firmware-guest = import ./test/guest/generations.nix {
      inherit pkgs nixpkgs;
      diskBoot = true;
    };
    packages.x86_64-linux.test-generation-guest = import ./test/guest/generations.nix {inherit pkgs nixpkgs;};
    packages.x86_64-linux.libvirt-runtime = import ./test/libvirt/runtime.nix {inherit pkgs;};
    packages.x86_64-linux.test-nixos-guest = import ./test/guest/nixos.nix {inherit pkgs nixpkgs;};
    packages.x86_64-linux.test-guest = import ./test/guest {inherit pkgs;};
    packages.x86_64-linux.test-store = pkgs.dockerTools.buildLayeredImage {
      name = "roamvm-test-store";
      tag = "dev";
      contents = [pkgs.minio pkgs.minio-client pkgs.busybox pkgs.cacert];
      config.Env = ["PATH=/bin"];
    };
    devShells.x86_64-linux.default = pkgs.mkShell {
      packages = with pkgs; [go_1_26 gofumpt gnumake docker-client docker-compose kind kubectl qemu_kvm qemu-utils alejandra shellcheck libvirt openssh iproute2 iptables e2fsprogs minio-client util-linux];
    };
  };
}

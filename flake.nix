{
  description = "RoamVM development and real-KVM test fixture";
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/6aefcda9401be8acc2b74244fb3b37520ea1f0a8";
  inputs.terranix = {
    url = "github:terranix/terranix/2.9.0";
    inputs.nixpkgs.follows = "nixpkgs";
  };
  outputs = {
    nixpkgs,
    terranix,
    ...
  }: let
    pkgs = import nixpkgs {
      system = "x86_64-linux";
      # Isolated compatibility fixture; never used by the runtime or deployment.
      config.permittedInsecurePackages = ["minio-2025-10-15T17-29-55Z"];
    };
    # Libvirt may have started without NixOS's user-namespace helpers on PATH.
    virtiofsd = pkgs.writeShellScriptBin "virtiofsd" ''
      export PATH=/run/wrappers/bin:$PATH
      exec ${pkgs.virtiofsd}/bin/virtiofsd "$@"
    '';
    runtime = import ./test/libvirt/runtime.nix {inherit pkgs;};
    mkLab = slot: let
      layout = import ./test/libvirt/config.nix {inherit slot;};
      nodes = builtins.mapAttrs (name: node:
        nixpkgs.lib.nixosSystem {
          system = "x86_64-linux";
          specialArgs = {
            inherit name node layout;
            roamvm = runtime.runtime;
          };
          modules = [./test/libvirt/node.nix];
        })
      layout.nodes;
      systems = builtins.mapAttrs (_: node: node.config.system.build.toplevel) nodes;
      terraform = terranix.lib.terranixConfiguration {
        inherit pkgs;
        extraArgs = {inherit layout systems virtiofsd;};
        modules = [./test/libvirt/terraform.nix];
      };
    in {
      inherit nodes;
      config =
        pkgs.runCommand "roamvm-lab-config-${toString slot}" {
          kustomization = builtins.toJSON (import ./test/libvirt/kustomization.nix {inherit layout;});
          volumes = builtins.toJSON (import ./test/libvirt/volumes.nix {inherit layout;});
          manifest = builtins.toJSON (layout
            // {
              inherit systems;
              binary = "${runtime.runtime}/bin/roamvm";
            });
          passAsFile = ["kustomization" "manifest" "volumes"];
        } ''
          mkdir -p "$out"
          cp ${terraform} "$out/main.tf.json"
          cp "$kustomizationPath" "$out/kustomization.yaml"
          cp "$manifestPath" "$out/manifest.json"
          cp "$volumesPath" "$out/warm-volumes.json"
          cp -r ${./config} "$out/config"
        '';
    };
  in {
    lib.mkLibvirtLab = slot: (mkLab slot).config;
    nixosModules.online-grow = import ./nix/online-grow.nix;
    nixosConfigurations = (mkLab 0).nodes;
    packages.x86_64-linux.libvirt-config = (mkLab 0).config;
    packages.x86_64-linux.test-firmware-guest = import ./test/guest/generations.nix {
      inherit pkgs nixpkgs;
      diskBoot = true;
    };
    packages.x86_64-linux.test-generation-guest = import ./test/guest/generations.nix {inherit pkgs nixpkgs;};
    packages.x86_64-linux.libvirt-runtime = runtime;
    packages.x86_64-linux.test-nixos-guest = import ./test/guest/nixos.nix {inherit pkgs nixpkgs;};
    packages.x86_64-linux.test-guest = import ./test/guest {inherit pkgs;};
    packages.x86_64-linux.test-store = pkgs.dockerTools.buildLayeredImage {
      name = "roamvm-test-store";
      tag = "dev";
      contents = [pkgs.minio pkgs.minio-client pkgs.busybox pkgs.cacert];
      config.Env = ["PATH=/bin"];
    };
    devShells.x86_64-linux.default = pkgs.mkShell {
      packages = with pkgs; [go_1_26 gofumpt gnumake docker-client docker-compose kind kubectl qemu_kvm qemu-utils alejandra shellcheck libvirt virtiofsd opentofu openssh openssl iproute2 iptables e2fsprogs minio-client util-linux curl jq];
    };
  };
}

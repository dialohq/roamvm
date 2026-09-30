{pkgs}: let
  hypervisor = pkgs.qemu_kvm;
in
  pkgs.buildGoModule {
    pname = "roamvm";
    version = "0.1.0";
    src = pkgs.lib.fileset.toSource {
      root = ../.;
      fileset = pkgs.lib.fileset.unions [../go.mod ../go.sum ../api ../cmd ../internal];
    };
    vendorHash = "sha256-P1t0NmWQAjtvR6t3K+2MyWHt8ymeua4eR78RjYF3GuA=";
    subPackages = ["cmd/roamvm"];
    nativeBuildInputs = [pkgs.makeWrapper];
    nativeCheckInputs = [hypervisor pkgs.qemu-utils];
    ldflags = ["-s" "-w"];
    doCheck = true;
    checkPhase = ''
      runHook preCheck
      go test ./...
      runHook postCheck
    '';
    postInstall = ''
      wrapProgram $out/bin/roamvm --prefix PATH : ${pkgs.lib.makeBinPath [hypervisor pkgs.qemu-utils pkgs.iproute2 pkgs.iptables pkgs.dnsmasq pkgs.cdrkit pkgs.coreutils]}
    '';
    passthru = {inherit hypervisor;};
    meta.platforms = ["x86_64-linux"];
  }

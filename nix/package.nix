{pkgs}: let
  hypervisor = pkgs.stdenvNoCC.mkDerivation {
    pname = "cloud-hypervisor";
    version = "53.0";
    src = pkgs.fetchurl {
      url = "https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v53.0/cloud-hypervisor-static";
      hash = "sha256-RIrz1OWbIsKYf335TCE61A+1OhDUN+QrXubE/OfCnsw=";
    };
    dontUnpack = true;
    installPhase = ''
      install -Dm755 $src $out/bin/cloud-hypervisor
    '';
  };
in
  pkgs.buildGoModule {
    pname = "roamvm";
    version = "0.1.0";
    src = pkgs.lib.fileset.toSource {
      root = ../.;
      fileset = pkgs.lib.fileset.unions [../go.mod ../go.sum ../api ../cmd ../internal];
    };
    vendorHash = "sha256-awYbt/g5JGU/k4YAP5dGxaBYh0p9mqVXEs+sX1DJVCk=";
    subPackages = ["cmd/roamvm"];
    nativeBuildInputs = [pkgs.makeWrapper];
    nativeCheckInputs = [pkgs.qemu-utils];
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

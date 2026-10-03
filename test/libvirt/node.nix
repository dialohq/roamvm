{
  lib,
  pkgs,
  modulesPath,
  name,
  ...
}: let
  server = name == "roamvm-libvirt-control-plane";
  ip =
    if server
    then "192.168.124.10"
    else if name == "roamvm-libvirt-worker"
    then "192.168.124.11"
    else "192.168.124.12";
in {
  imports = [(modulesPath + "/virtualisation/qemu-vm.nix")];
  system.stateVersion = "26.05";
  nixpkgs.config.permittedInsecurePackages = ["minio-2025-10-15T17-29-55Z"];
  virtualisation = {
    graphics = false;
    writableStore = false;
    rootDevice = "/dev/vda";
    sharedDirectories = lib.mkForce {
      nix-store = {
        source = "/nix/store";
        target = "/nix/.ro-store";
      };
      lab = {
        source = "lab";
        target = "/lab";
      };
    };
  };
  # qemu-vm.nix generates 9p mounts; the custom libvirt domains use migratable
  # virtiofs devices so a prepared cluster can be saved with its RAM intact.
  virtualisation.fileSystems."/nix/.ro-store" = {
    fsType = lib.mkForce "virtiofs";
    options = lib.mkForce ["ro"];
  };
  virtualisation.fileSystems."/lab" = {
    fsType = lib.mkForce "virtiofs";
    options = lib.mkForce ["ro"];
  };
  boot.kernelParams = ["console=ttyS0" "net.ifnames=0"];
  boot.kernel.sysctl."net.ipv4.ip_forward" = 1;
  networking = {
    hostName = name;
    useDHCP = false;
    firewall.enable = false;
    interfaces.eth0.ipv4.addresses = [
      {
        address = ip;
        prefixLength = 24;
      }
    ];
    defaultGateway = "192.168.124.1";
    nameservers = ["1.1.1.1"];
  };
  services.openssh = {
    enable = true;
    settings = {
      PermitRootLogin = "prohibit-password";
      PasswordAuthentication = false;
      AuthorizedKeysFile = "/lab/.lab/libvirt/id_ed25519.pub";
      # This test-only key is owned by the unprivileged host user on the share.
      StrictModes = false;
    };
  };
  services.qemuGuest.enable = true;
  # K3s also supplies crictl; prefer the small standalone client for probes.
  environment.systemPackages = [pkgs.curl pkgs.jq pkgs.k3s (lib.hiPrio pkgs.cri-tools) pkgs.util-linux];
  environment.etc."rancher/k3s/registries.yaml".text = ''
    mirrors:
      "192.168.124.10:5000":
        endpoint: ["http://192.168.124.10:5000"]
  '';
  # Writable solely so the CPU oversubscription test can change and restore it.
  systemd.tmpfiles.rules = [
    "d /var/lib/kubelet 0755 root root -"
    "C /var/lib/kubelet/config.yaml 0644 root root - ${pkgs.writeText "kubelet-lab.yaml" ''
      apiVersion: kubelet.config.k8s.io/v1beta1
      kind: KubeletConfiguration
      allowedUnsafeSysctls: [net.ipv4.ip_forward, net.ipv4.conf.all.route_localnet]
    ''}"
  ];
  services.k3s = {
    enable = true;
    role =
      if server
      then "server"
      else "agent";
    token = "roamvm-isolated-libvirt-lab-only";
    serverAddr =
      if server
      then ""
      else "https://192.168.124.10:6443";
    extraFlags =
      ["--node-ip=${ip}" "--kubelet-arg=config=/var/lib/kubelet/config.yaml"]
      ++ lib.optionals server ["--disable=traefik" "--disable=servicelb" "--disable=metrics-server" "--tls-san=192.168.124.10"];
  };
  # This small test cluster favors lower resident memory over GC throughput.
  # This is a heap-growth target, not a hard memory cap or memory overcommit.
  systemd.services.k3s.environment.GOGC = "50";
  systemd.services.roamvm-device-plugin = lib.mkIf (!server) {
    wantedBy = ["multi-user.target"];
    after = ["k3s.service"];
    serviceConfig = {
      ExecStart = "/lab/bin/roamvm device-plugin";
      Restart = "always";
      RestartSec = 2;
    };
  };
  systemd.services.minio = lib.mkIf server {
    wantedBy = ["multi-user.target"];
    path = [pkgs.getent];
    environment = {
      MINIO_ROOT_USER = "roamvm-local";
      MINIO_ROOT_PASSWORD = "roamvm-local-test-only";
    };
    serviceConfig = {
      ExecStart = "${pkgs.minio}/bin/minio server /var/lib/minio --address :9000";
      StateDirectory = "minio";
      Restart = "always";
      StandardOutput = "journal+console";
      StandardError = "journal+console";
    };
  };
  services.dockerRegistry = lib.mkIf server {
    enable = true;
    listenAddress = "0.0.0.0";
    port = 5000;
  };
}

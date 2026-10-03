{
  lib,
  pkgs,
  modulesPath,
  name,
  node,
  layout,
  roamvm,
  ...
}: let
  server = node.role == "server";
  ip = node.ip;
  control = layout.nodes."${layout.cluster}-control-plane".ip;
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
      lab-state = {
        source = "lab-state";
        target = "/lab-state";
      };
    };
  };
  # qemu-vm.nix generates 9p mounts; the custom libvirt domains use migratable
  # virtiofs devices so a prepared cluster can be saved with its RAM intact.
  virtualisation.fileSystems."/nix/.ro-store" = {
    fsType = lib.mkForce "virtiofs";
    options = lib.mkForce ["ro"];
  };
  virtualisation.fileSystems."/lab-state" = {
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
        prefixLength = layout.network.prefix;
      }
    ];
    defaultGateway = layout.network.gateway;
    nameservers = ["1.1.1.1"];
  };
  services.openssh = {
    enable = true;
    settings = {
      PermitRootLogin = "prohibit-password";
      PasswordAuthentication = false;
      AuthorizedKeysFile = "/lab-state/id_ed25519.pub";
      # This test-only key is owned by the unprivileged host user on the share.
      StrictModes = false;
    };
  };
  services.qemuGuest.enable = true;
  # K3s also supplies crictl; prefer the small standalone client for probes.
  environment.systemPackages = [pkgs.curl pkgs.jq pkgs.k3s (lib.hiPrio pkgs.cri-tools) pkgs.util-linux];
  environment.etc."rancher/k3s/registries.yaml".text = ''
    mirrors:
      "${control}:5000":
        endpoint: ["http://${control}:5000"]
  '';
  # Reapply at boot, but keep writable so the CPU oversubscription test can
  # change and restore it without changing the declarative node definition.
  environment.etc."roamvm/kubelet.yaml" = {
    mode = "0644";
    text = ''
      apiVersion: kubelet.config.k8s.io/v1beta1
      kind: KubeletConfiguration
      # Publish recovered device capacity promptly, without increasing the
      # API reporting rate when node status is unchanged.
      nodeStatusUpdateFrequency: 1s
      nodeStatusReportFrequency: 5m
      allowedUnsafeSysctls: [net.ipv4.ip_forward, net.ipv4.conf.all.route_localnet]
    '';
  };
  systemd.tmpfiles.rules = [
    "d /var/lib/kubelet 0755 root root -"
    "L+ /var/lib/kubelet/config.yaml - - - - /etc/roamvm/kubelet.yaml"
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
      else "https://${control}:6443";
    extraFlags =
      ["--node-ip=${ip}" "--kubelet-arg=config=/var/lib/kubelet/config.yaml"]
      ++ lib.optionals server ["--disable=traefik" "--disable=servicelb" "--disable=metrics-server" "--tls-san=${control}"];
  };
  # This small test cluster favors lower resident memory over GC throughput.
  # This is a heap-growth target, not a hard memory cap or memory overcommit.
  systemd.services.k3s.environment.GOGC = "50";
  systemd.services.roamvm-device-plugin = lib.mkIf (!server) {
    wantedBy = ["multi-user.target"];
    after = ["k3s.service"];
    serviceConfig = {
      ExecStart = "${roamvm}/bin/roamvm device-plugin";
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

# Import on a dedicated NixOS lab host: (import ./host.nix {labUser = "coder";})
{labUser}: {pkgs, ...}: {
  virtualisation.libvirtd = {
    enable = true;
    qemu = {
      package = pkgs.qemu_kvm;
      # Suppress the module's forced qemu-libvirtd user/group. The explicit
      # user below is authoritative; QEMU does NOT run as root.
      runAsRoot = true;
      # The workspace and resettable disks belong to the harness user. Avoid
      # ownership changes on every start; do not run the guest QEMU as root.
      verbatimConfig = ''
        namespaces = []
        user = "${labUser}"
        group = "users"
        dynamic_ownership = 0
        remember_owner = 0
      '';
    };
  };
  users.users.${labUser}.extraGroups = ["libvirtd"];
}

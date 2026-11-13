{pkgs, ...}: {
  # Bootstrap supplies this tmpfs file only when remote builders are configured.
  nix.extraOptions = "!include /run/roamvm-builder/nix.conf";
  environment.systemPackages = [pkgs.python3];
  systemd.services.nix-daemon.environment.NIX_SSHOPTS = "-oUserKnownHostsFile=/run/roamvm-builder/known_hosts -oStrictHostKeyChecking=yes -oIdentitiesOnly=yes -oBatchMode=yes";
}

# Import into a guest to grow a plain ext4/XFS root on a virtio disk in place.
{pkgs, ...}: {
  services.udev.extraRules = ''
    ACTION=="change", SUBSYSTEM=="block", KERNEL=="vd[a-z]", RUN+="${pkgs.systemd}/bin/systemctl --no-block start roamvm-grow-root.service"
  '';
  # Retry changes coalesced while the oneshot was already running.
  systemd.timers.roamvm-grow-root = {
    wantedBy = ["timers.target"];
    timerConfig.OnUnitInactiveSec = "30s";
  };
  systemd.services.roamvm-grow-root = {
    description = "Grow the root partition and filesystem to the current disk capacity";
    wantedBy = ["multi-user.target"];
    after = ["local-fs.target"];
    path = [pkgs.util-linux pkgs.coreutils pkgs.cloud-utils pkgs.e2fsprogs pkgs.xfsprogs];
    serviceConfig = {
      Type = "oneshot";
      PrivateTmp = true;
    };
    script = ''
      root=$(readlink -f "$(findmnt -n -o SOURCE /)")
      name=$(basename "$root")
      case "$name" in vd[a-z]|vd[a-z][0-9]*) ;; *) echo "Unsupported root device: $root" >&2; exit 1 ;; esac
      if [ -f "/sys/class/block/$name/partition" ]; then
        partition=$(cat "/sys/class/block/$name/partition")
        parent=$(basename "$(dirname "$(readlink -f "/sys/class/block/$name")")")
        if ! result=$(growpart "/dev/$parent" "$partition" 2>&1); then
          case "$result" in NOCHANGE:*) ;; *) echo "$result" >&2; exit 1 ;; esac
        fi
      fi
      case "$(findmnt -n -o FSTYPE /)" in
        ext4) resize2fs "$root" ;;
        xfs) xfs_growfs / ;;
        *) echo "Unsupported root filesystem" >&2; exit 1 ;;
      esac
    '';
  };
}

# Import into a guest to grow a plain ext4/XFS root on a virtio disk in place.
{pkgs, ...}: let
  pending = "/run/roamvm-grow-root.pending";
  notify = pkgs.writeShellScript "notify-root-growth" ''
    set -eu
    size=$(${pkgs.coreutils}/bin/cat "/sys/class/block/$1/size")
    observed="/run/roamvm-grow-root-$1.capacity"
    # growpart's writable close also emits change events, even on NOCHANGE or
    # failed growth. Filter by observed capacity, independently of success;
    # the timer must still be able to retry failures at the same capacity.
    if [ -f "$observed" ] && [ "$(${pkgs.coreutils}/bin/cat "$observed")" = "$size" ]; then
      exit 0
    fi
    echo "$size" > "$observed"
    ${pkgs.coreutils}/bin/touch ${pending}
  '';
in {
  services.udev.extraRules = ''
    ACTION=="change", SUBSYSTEM=="block", KERNEL=="vd[a-z]", RUN+="${notify} %k"
  '';
  # PathExists is rechecked when the service finishes. Unlike starting an
  # already-active oneshot, a notification received during growth is retained.
  systemd.paths.roamvm-grow-root = {
    wantedBy = ["multi-user.target"];
    pathConfig.PathExists = pending;
  };
  # Retain a fallback for a missed udev event or a transient growth failure.
  systemd.timers.roamvm-grow-root = {
    wantedBy = ["timers.target"];
    timerConfig.OnUnitInactiveSec = "30s";
  };
  systemd.services.roamvm-grow-root = {
    description = "Grow the root partition and filesystem to the current disk capacity";
    wantedBy = ["multi-user.target"];
    after = ["local-fs.target"];
    # Bursts can require successive passes. A service start-limit failure also
    # disables the path watcher; retain the path unit's own trigger limit instead.
    unitConfig.StartLimitIntervalSec = 0;
    path = [pkgs.util-linux pkgs.coreutils pkgs.cloud-utils pkgs.e2fsprogs pkgs.xfsprogs];
    serviceConfig = {
      Type = "oneshot";
      PrivateTmp = true;
    };
    script = ''
      # Consume only notifications preceding this pass; events arriving while
      # it runs leave another marker for the path unit to pick up afterwards.
      rm -f ${pending}
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

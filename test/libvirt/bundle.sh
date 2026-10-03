#!/usr/bin/env bash
# Portable prepared disks; RAM snapshots must be captured on the receiving host.
set -euo pipefail
export LIBVIRT_DEFAULT_URI=qemu:///session
umask 077
root=$(git rev-parse --show-toplevel)
test "$PWD" = "$root" || { echo 'Run from the repository root' >&2; exit 1; }
lab="$root/.lab/libvirt"
names=(roamvm-libvirt-control-plane roamvm-libvirt-worker roamvm-libvirt-worker2)
operation=${1:-}
test -n "${2:-}" || { echo "Usage: $0 export|import BUNDLE_DIRECTORY" >&2; exit 1; }
bundle=$(realpath -m "$2")
stage=""
trap 'if [[ -n "$stage" ]]; then rm -rf -- "$stage"; fi' EXIT

case "$operation" in
  export)
    test ! -e "$bundle" || { echo 'Bundle destination already exists' >&2; exit 1; }
    exec 9> "$lab/scenario.lock"
    flock -n 9 || { echo 'Another baseline/scenario operation is running' >&2; exit 1; }
    baseline="$lab/baseline"
    if [[ -f "$lab/warm/ready" ]]; then baseline="$lab/warm"; fi
    test -f "$baseline/ready" || { echo 'Freeze a baseline before exporting' >&2; exit 1; }
    mkdir -p "$(dirname "$bundle")"
    stage=$(mktemp -d "$bundle.partial.XXXXXX")
    echo roamvm-libvirt-disks-v1 > "$stage/format"
    git rev-parse HEAD > "$stage/revision"
    git diff HEAD > "$stage/worktree.patch"
    cp flake.lock "$stage/flake.lock"
    systems=()
    for name in "${names[@]}"; do
      grep -q "type='virtiofs'" "$baseline/$name.xml" || {
        echo 'Recreate legacy 9p baselines with the current lab before exporting' >&2; exit 1;
      }
      system=$(readlink -f "$baseline/$name-system")
      test -d "$system"
      systems+=("$system")
      printf '%s\n' "$system" > "$stage/$name-system"
      # Flatten all backing chains, including warm -> cold. Only immutable
      # baseline disks are read; running working overlays are never copied.
      qemu-img convert -f qcow2 -O qcow2 -c "$baseline/$name.qcow2" "$stage/$name.qcow2"
      qemu-img check "$stage/$name.qcow2"
    done
    cp "$baseline/"*-ref "$stage/"
    # Include the complete OS closures, not symlinks into the producing host's
    # store. A file binary cache is relocatable and deduplicates shared paths.
    nix copy --to "file://$stage/nix-cache" "${systems[@]}"
    (
      cd "$stage"
      # The output manifest is explicitly excluded from its own input.
      # shellcheck disable=SC2094
      find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum > SHA256SUMS
    )
    sync -f "$stage"
    mv -T "$stage" "$bundle"
    stage=""
    sync -f "$bundle"
    echo "Portable disk baseline: $bundle (contains private cluster state; share only with trusted runners)"
    ;;
  import)
    test ! -e "$lab" || { echo 'Refusing to replace an existing lab' >&2; exit 1; }
    # Do not take over another checkout's domains on this host.
    domains=$(virsh list --all --name)
    for name in "${names[@]}"; do
      if grep -Fxq "$name" <<< "$domains"; then echo "Existing lab domain: $name" >&2; exit 1; fi
    done
    test -d "$bundle"
    test -z "$(find "$bundle" -type l -print -quit)" || { echo 'Bundle must not contain symlinks' >&2; exit 1; }
    (
      cd "$bundle"
      # Compare the entire tree, so truncated manifests and additional files
      # are rejected as well as missing/corrupt payloads.
      find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum |
        cmp SHA256SUMS -
    )
    test "$(cat "$bundle/format")" = roamvm-libvirt-disks-v1
    cmp flake.lock "$bundle/flake.lock" || { echo 'Use the flake.lock from the bundle producer' >&2; exit 1; }
    systems=()
    for name in "${names[@]}"; do
      system=$(cat "$bundle/$name-system")
      [[ "$system" =~ ^/nix/store/[a-z0-9]{32}-nixos-system-$name-[^/[:space:]]+$ ]]
      systems+=("$system")
      qemu-img info --output=json "$bundle/$name.qcow2" |
        jq -e '.format == "qcow2" and (has("backing-filename") | not)' >/dev/null
      qemu-img check "$bundle/$name.qcow2"
    done
    test -s "$bundle/guest-ref"
    # This is a trusted executable VM artifact, not an untrusted download.
    # Nix verifies NAR hashes; the local exported cache has no signing key.
    nix copy --no-check-sigs --from "file://$bundle/nix-cache" "${systems[@]}"
    mkdir -p "$root/.lab"
    stage=$(mktemp -d "$root/.lab/import.XXXXXX")
    mkdir "$stage/baseline"
    for i in 0 1 2; do
      name=${names[$i]}
      cp --reflink=auto --sparse=always "$bundle/$name.qcow2" "$stage/baseline/"
      chmod a-w "$stage/baseline/$name.qcow2"
      nix-store --add-root "$stage/baseline/$name-system" --realise "${systems[$i]}" >/dev/null
      # Regenerate host paths and devices, never reuse source domain UUIDs,
      # MACs, CPU state, sockets, SSH keys or virtiofs inode handles.
      bash test/libvirt/lab.sh xml "$name" "${systems[$i]}" > "$stage/baseline/$name.xml"
      cp "$stage/baseline/$name.xml" "$stage/"
    done
    cp "$bundle/"*-ref "$stage/baseline/"
    touch "$stage/baseline/ready"
    sync -f "$stage"
    mv -T "$stage" "$lab"
    stage=""
    for i in 0 1 2; do
      nix-store --add-root "$lab/baseline/${names[$i]}-system" --realise "${systems[$i]}" >/dev/null
    done
    sync -f "$lab"
    echo 'Imported. Run make libvirt-reset-cold, then make libvirt-freeze-warm for fast local resets.'
    ;;
  *) echo "Usage: $0 export|import BUNDLE_DIRECTORY" >&2; exit 1 ;;
esac

#!/usr/bin/env bash
# Shared paths and locking. Nested harness commands inherit the same flock FD.
root=$(git rev-parse --show-toplevel)
export ROAMVM_LAB_SLOT="${ROAMVM_LAB_SLOT:-0}"
[[ "$ROAMVM_LAB_SLOT" =~ ^([0-9]|[1-9][0-9])$ ]] || { echo 'ROAMVM_LAB_SLOT must be 0..99' >&2; exit 1; }
lab="$root/.lab/libvirt"
if [[ "$ROAMVM_LAB_SLOT" != 0 ]]; then lab+="-$ROAMVM_LAB_SLOT"; fi
export PATH=/run/wrappers/bin:$PATH
export KUBECONFIG="$lab/kubeconfig"
umask 077

build_lab_config() {
  ROAMVM_FLAKE_ROOT="$root" nix build --impure --expr '
    (builtins.getFlake ("git+file://" + builtins.getEnv "ROAMVM_FLAKE_ROOT")).lib.mkLibvirtLab
      (builtins.fromJSON (builtins.getEnv "ROAMVM_LAB_SLOT"))
  ' --out-link "$1"
}

lock_lab() {
  # Share the slot lock across checkouts, before checking libvirt ownership.
  local lock="${XDG_RUNTIME_DIR:-$HOME/.cache}/roamvm-lab-$ROAMVM_LAB_SLOT.lock"
  mkdir -p "$(dirname "$lock")"
  if [[ $(readlink /proc/$$/fd/9 || true) != "$lock" ]]; then
    exec 9> "$lock"
  fi
  flock -n 9 || { echo 'Another lab operation is running' >&2; exit 1; }
}

load_lab() {
  test -f "$lab/config/manifest.json" || { echo 'Run make libvirt-up first' >&2; exit 1; }
  manifest="$lab/config/manifest.json"
  test "$(jq -r .slot "$manifest")" = "$ROAMVM_LAB_SLOT"
  cluster=$(jq -r .cluster "$manifest")
  binary=$(jq -r .binary "$manifest")
  export LIBVIRT_DEFAULT_URI="${LIBVIRT_DEFAULT_URI:-$(jq -r .uri "$manifest")}"
  if [[ -f "$lab/uri" ]]; then
    test "$LIBVIRT_DEFAULT_URI" = "$(cat "$lab/uri")" || { echo 'Lab connection differs from its Terraform state' >&2; exit 1; }
  fi
  mapfile -t names < <(jq -r '.nodes | keys[]' "$manifest")
  control=$(jq -r '.nodes | to_entries[] | select(.value.role == "server") | .key' "$manifest")
  control_ip=$(jq -r --arg name "$control" '.nodes[$name].ip' "$manifest")
}

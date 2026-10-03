#!/usr/bin/env bash
# No KVM or running libvirt required: validate declarations and harness locking.
set -euo pipefail
source "$(dirname "$0")/common.sh"
mkdir -p "$root/.lab"
work=$(mktemp -d "$root/.lab/check.XXXXXX")
trap 'rm -rf "$work"' EXIT
for slot in 0 1; do
  ROAMVM_LAB_SLOT=$slot build_lab_config "$work/config-$slot"
  mkdir "$work/tf-$slot"
  install -m600 "$work/config-$slot/main.tf.json" "$work/tf-$slot/main.tf.json"
  install -m600 test/libvirt/terraform.lock.hcl "$work/tf-$slot/.terraform.lock.hcl"
  tofu -chdir="$work/tf-$slot" init -backend=false -input=false -lockfile=readonly
  tofu -chdir="$work/tf-$slot" validate
  kubectl kustomize "$work/config-$slot" >/dev/null
  jq -e '.resource.libvirt_domain | length == 3 and all(.[]; has("running") | not)' "$work/config-$slot/main.tf.json" >/dev/null
  jq -e '.resource.libvirt_domain | all(.[]; .cpu.topology.sockets == 1 and .cpu.topology.threads == 1 and .cpu.topology.cores == .vcpu)' "$work/config-$slot/main.tf.json" >/dev/null
  jq -e '.resource.libvirt_network.lab.bridge | .stp == "off" and .delay == "0"' "$work/config-$slot/main.tf.json" >/dev/null
  # Terraform JSON can silently discard unknown nested object attributes.
  # In particular, the provider's kernel_args example drops the boot command.
  jq -e '.resource.libvirt_domain | all(.[]; .os.cmdline == ("init=" + (.os.kernel | rtrimstr("/kernel")) + "/init console=ttyS0 net.ifnames=0"))' "$work/config-$slot/main.tf.json" >/dev/null
  tofu -chdir="$work/tf-$slot" providers schema -json |
    jq -e '.provider_schemas[].resource_schemas.libvirt_domain.block.attributes.os.nested_type.attributes.cmdline.type == "string"' >/dev/null
done
jq -es '
  .[0] as $a | .[1] as $b |
  $a.slot == 0 and $b.slot == 1 and
  $a.nodes[$a.cluster + "-worker2"].cpus == 2 and
  $b.nodes[$b.cluster + "-worker2"].cpus == 2 and
  $a.network.gateway == "192.168.124.1" and $b.network.gateway == "192.168.125.1" and
  $a.network.name != $b.network.name and $a.network.bridge != $b.network.bridge and
  ([($a.nodes | keys[]) as $n | $b.nodes | has($n)] | any | not) and
  ([($a.nodes[].mac) as $mac | $b.nodes[] | .mac == $mac] | any | not) and
  $a.scenarios == $b.scenarios
' "$work/config-0/manifest.json" "$work/config-1/manifest.json" >/dev/null
common="$root/test/libvirt/common.sh"
(
  export XDG_RUNTIME_DIR="$work" ROAMVM_LAB_SLOT=0
  lock_lab
  # Nested calls reuse the lock. Independent callers of that lab must fail.
  bash -c 'source "$1"; lock_lab' bash "$common"
  if bash -c 'source "$1"; lock_lab' bash "$common" 9>&- >"$work/blocked.log" 2>&1; then
    echo 'Concurrent caller acquired the same lab' >&2; exit 1
  fi
  grep -q 'Another lab operation' "$work/blocked.log"
  ROAMVM_LAB_SLOT=1 bash -c 'source "$1"; lock_lab' bash "$common" 9>&-
)
if ROAMVM_LAB_SLOT=100 bash -c 'source "$1"' bash "$common" >"$work/invalid.log" 2>&1; then
  echo 'Accepted an out-of-range lab slot' >&2; exit 1
fi
grep -q 'must be 0..99' "$work/invalid.log"
echo 'Lab declarations, provider schemas and lock isolation passed.'

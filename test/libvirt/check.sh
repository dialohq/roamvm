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
  jq -e '.cluster as $cluster | .addons.items |
    map(.metadata.name) == ["coredns", "local-path-provisioner"] and
    all(.[]; .metadata.namespace == "kube-system" and
      .spec.template.spec.nodeSelector["kubernetes.io/hostname"] == ($cluster + "-control-plane")) and
    .[1].spec.template.spec.containers == [{name:"local-path-provisioner",image:"rancher/local-path-provisioner:v0.0.37"}]
  ' "$work/config-$slot/manifest.json" >/dev/null
  jq -e --slurpfile layout "$work/config-$slot/manifest.json" '
    .items as $volumes |
    ($volumes | length) == 16 and
    ([$volumes[] | [.spec.nodeAffinity.required.nodeSelectorTerms[0].matchExpressions[0].values[0], .spec.hostPath.path]] | unique | length) == 16 and
    all($volumes[];
      .metadata.annotations["pv.kubernetes.io/provisioned-by"] == "rancher.io/local-path" and
      .spec.persistentVolumeReclaimPolicy == "Delete" and .spec.storageClassName == "local-path" and
      .spec.hostPath.type == "DirectoryOrCreate" and (.spec | has("claimRef") | not) and
      (.spec.nodeAffinity.required.nodeSelectorTerms[0].matchExpressions[0] |
        .key == "kubernetes.io/hostname" and .operator == "In" and (.values | length) == 1 and
        ($layout[0].nodes[.values[0]].role == "agent")))
  ' "$work/config-$slot/warm-volumes.json" >/dev/null
  jq -e '.resource.libvirt_domain | length == 3 and all(.[]; has("running") | not)' "$work/config-$slot/main.tf.json" >/dev/null
  jq -e '.resource.libvirt_domain | all(.[]; .cpu.topology.sockets == 1 and .cpu.topology.threads == 1 and .cpu.topology.cores == .vcpu)' "$work/config-$slot/main.tf.json" >/dev/null
  jq -e '.resource.libvirt_domain | all(.[]; .memory == 2048 and .memory_backing.memory_source.type == "memfd" and .memory_backing.memory_access.mode == "shared" and .memory_backing.memory_huge_pages.hugepages == [{size:2048,unit:"KiB"}])' "$work/config-$slot/main.tf.json" >/dev/null
  jq -e '.resource.libvirt_network.lab.bridge | .stp == "off" and .delay == "0"' "$work/config-$slot/main.tf.json" >/dev/null
  # Terraform JSON can silently discard unknown nested object attributes.
  # In particular, the provider's kernel_args example drops the boot command.
  jq -e '.resource.libvirt_domain | all(.[]; .os.cmdline == ("init=" + (.os.kernel | rtrimstr("/kernel")) + "/init console=ttyS0 net.ifnames=0"))' "$work/config-$slot/main.tf.json" >/dev/null
  tofu -chdir="$work/tf-$slot" providers schema -json |
    jq -e '.provider_schemas[].resource_schemas.libvirt_domain.block.attributes.os.nested_type.attributes.cmdline.type == "string"' >/dev/null
done
nix eval --impure --json --expr '
  map (args: ((import ./test/libvirt/host.nix args) {pkgs = {};}).boot.kernelParams)
    [{labUser = "test";} {labUser = "test"; labInstances = 2;}]
' | jq -e '. == [["hugepagesz=2M","hugepages=3072"],["hugepagesz=2M","hugepages=6144"]]' >/dev/null
jq -es '[.[].items[].metadata.name] | length == (unique | length)' "$work/config-0/warm-volumes.json" "$work/config-1/warm-volumes.json" >/dev/null
jq -es '
  .[0] as $a | .[1] as $b |
  $a.slot == 0 and $b.slot == 1 and
  $a.network.gateway == "192.168.124.1" and $b.network.gateway == "192.168.125.1" and
  $a.network.name != $b.network.name and $a.network.bridge != $b.network.bridge and
  ([($a.nodes | keys[]) as $n | $b.nodes | has($n)] | any | not) and
  ([($a.nodes[].mac) as $mac | $b.nodes[] | .mac == $mac] | any | not)
' "$work/config-0/manifest.json" "$work/config-1/manifest.json" >/dev/null
# Scenario YAML, not the lab manifest, owns the lab reference and fixtures.
for file in "$root"/test/scenarios/*/scenario.yaml; do
  scenario_spec "$(basename "$(dirname "$file")")" > "$work/scenario.json"
  jq -e '.lab == "../../libvirt/config.nix"' "$work/scenario.json" >/dev/null
done
scenario_spec resize | jq -e '.fixtures == {ROAMVM_TEST_RESIZE_IMAGE:"nixos"}' >/dev/null
scenario_spec generations | jq -e '.fixtures == {ROAMVM_TEST_GENERATION_IMAGE:"generation",ROAMVM_TEST_FIRMWARE_IMAGE:"firmware"} and (.cases[1].steps == .cases[0].steps)' >/dev/null
(
  # Intentionally isolate invalid fixtures from the real checkout.
  # shellcheck disable=SC2030
  root="$work"
  mkdir -p "$root/test/scenarios/invalid" "$root/test/libvirt"
  touch "$root/test/libvirt/config.nix" "$root/test/libvirt/other.nix"
  for definition in '' 'lab: ../../libvirt/missing.nix' 'lab: ../../libvirt/other.nix'; do
    printf '%s\nsteps: [{ready: {}}]\n' "$definition" > "$root/test/scenarios/invalid/scenario.yaml"
    if scenario_spec invalid; then echo 'Accepted invalid lab reference' >&2; exit 1; fi
  done
)
# shellcheck disable=SC2031
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
# Require an explicit success for every case, without duplicates or failures.
printf '%s\n' '{"event":"case","status":"passed","name":"TestOne"}' '{"event":"case","status":"passed","name":"TestTwo"}' > "$work/events.json"
check_scenario_result '["TestOne","TestTwo"]' "$work/events.json"
if check_scenario_result '["TestOne","TestMissing"]' "$work/events.json"; then exit 1; fi
for action in skipped failed; do
  printf '{"event":"case","status":"%s","name":"TestOne/subtest"}\n' "$action" > "$work/events.json"
  printf '%s\n' '{"event":"case","status":"passed","name":"TestOne"}' >> "$work/events.json"
  if check_scenario_result '["TestOne"]' "$work/events.json"; then exit 1; fi
done
printf '%s\n' '{"status":"passed"}' > "$work/events.json"
if check_scenario_result '["TestOne"]' "$work/events.json"; then exit 1; fi
echo 'Lab declarations, provider schemas, lock isolation and scenario results passed.'

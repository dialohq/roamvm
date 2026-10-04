#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/common.sh"
operation=${1:-}
if [[ "$operation" != ssh ]]; then lock_lab; fi

configure() {
  mkdir -p "$lab/terraform"
  build_lab_config "$lab/config"
  load_lab
  printf '%s\n' "$LIBVIRT_DEFAULT_URI" > "$lab/uri"
  install -m600 "$lab/config/main.tf.json" "$lab/terraform/main.tf.json"
  install -m600 "$root/test/libvirt/terraform.lock.hcl" "$lab/terraform/.terraform.lock.hcl"
  export TF_VAR_lab="$lab" TF_VAR_uri="$LIBVIRT_DEFAULT_URI"
  tofu -chdir="$lab/terraform" init -input=false -lockfile=readonly
  for name in "${names[@]}"; do
    system=$(jq -r --arg name "$name" '.systems[$name]' "$manifest")
    nix-store --add-root "$lab/$name-system" --realise "$system" >/dev/null
  done
}

start_nodes() {
  for name in "${names[@]}"; do
    if [[ $(virsh domstate "$name") == 'shut off' ]]; then virsh start "$name"; fi
  done
  for name in "${names[@]}"; do
    deadline=$((SECONDS + 180))
    until bash "$0" ssh "$name" true </dev/null; do
      if (( SECONDS >= deadline )); then echo "$name: SSH did not become ready" >&2; exit 1; fi
      sleep 2
    done
  done
  # SSH can be ready before the first K3s bootstrap publishes its kubeconfig.
  bash "$0" ssh "$control" 'timeout 180 bash -c "until test -s /etc/rancher/k3s/k3s.yaml; do sleep 0.5; done"' </dev/null
  bash "$0" ssh "$control" cat /etc/rancher/k3s/k3s.yaml </dev/null |
    sed -e "s/127.0.0.1/$control_ip/g" -e "s/default/$cluster/g" > "$lab/kubeconfig"
  chmod 600 "$lab/kubeconfig"
  deadline=$((SECONDS + 180))
  until kubectl --request-timeout=2s get --raw=/readyz >/dev/null 2>&1; do
    if (( SECONDS >= deadline )); then echo 'Kubernetes API did not become ready' >&2; exit 1; fi
    sleep 0.5
  done
  deadline=$((SECONDS + 180))
  until kubectl --request-timeout=2s get node "${names[@]}" >/dev/null 2>&1; do
    if (( SECONDS >= deadline )); then echo 'Nodes did not register' >&2; exit 1; fi
    sleep 0.5
  done
  kubectl wait node "${names[@]}" --for=condition=Ready --timeout=180s
}

case "$operation" in
  up|plan)
    test -c /dev/kvm
    configure
    if [[ "$operation" == plan ]]; then
      exec tofu -chdir="$lab/terraform" plan -input=false -detailed-exitcode
    fi
    if [[ ! -f "$lab/id_ed25519" ]]; then ssh-keygen -q -t ed25519 -N '' -f "$lab/id_ed25519"; fi
    network=$(jq -r .network.name "$manifest")
    if virsh net-info "$network" >/dev/null 2>&1 &&
      ! tofu -chdir="$lab/terraform" state show libvirt_network.lab >/dev/null 2>&1; then
      echo "Refusing unmanaged network $network" >&2; exit 1
    fi
    for name in "${names[@]}"; do
      # Never take over domains belonging to another checkout or the old harness.
      if virsh dominfo "$name" >/dev/null 2>&1 &&
        ! tofu -chdir="$lab/terraform" state show "libvirt_domain.$name" >/dev/null 2>&1; then
        echo "Refusing unmanaged domain $name" >&2; exit 1
      fi
      if [[ -f "$lab/baseline/imported" ]]; then
        test "$(readlink -f "$lab/baseline/$name-system")" = "$(readlink -f "$lab/$name-system")" || {
          echo "$name: imported NixOS system does not match this checkout" >&2; exit 1;
        }
      fi
      if [[ ! -f "$lab/$name.qcow2" ]]; then
        if [[ -f "$lab/baseline/$name.qcow2" ]]; then
          qemu-img create -f qcow2 -F qcow2 -b "$lab/baseline/$name.qcow2" "$lab/$name.qcow2"
        else
          truncate -s 16G "$lab/$name.raw.partial"
          mkfs.ext4 -q -F "$lab/$name.raw.partial"
          qemu-img convert -f raw -O qcow2 -c "$lab/$name.raw.partial" "$lab/$name.qcow2"
          rm "$lab/$name.raw.partial"
        fi
      fi
    done
    tofu -chdir="$lab/terraform" apply -input=false -auto-approve
    for name in "${names[@]}"; do
      virsh dumpxml "$name" --inactive > "$lab/$name.xml"
      if [[ -f "$lab/baseline/imported" ]]; then cp "$lab/$name.xml" "$lab/baseline/"; fi
    done
    rm -f "$lab/baseline/imported"
    start_nodes
    ;;
  fixtures)
    load_lab
    specifications=$(for file in "$root"/test/scenarios/*/scenario.yaml; do
      scenario_spec "$(basename "$(dirname "$file")")" || exit 1
    done)
    while read -r fixture; do
      nix build "$root#test-$fixture-guest" --out-link "$lab/$fixture.tar.gz"
      "$binary" image-push --plain-http --tag "$control_ip:5000/$fixture:local" --tar "$lab/$fixture.tar.gz" > "$lab/$fixture-ref"
    done < <(jq -rs '[.[] | (.fixtures // {})[]] | unique[]' <<< "$specifications")
    ;;
  start)
    load_lab
    start_nodes
    ;;
  ssh)
    load_lab
    ip=$(jq -er --arg name "${2:-}" '.nodes[$name].ip' "$manifest")
    shift 2
    exec ssh -i "$lab/id_ed25519" -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile="$lab/known_hosts" -o ConnectTimeout=5 -o ServerAliveInterval=5 -o ServerAliveCountMax=3 "root@$ip" "$@"
    ;;
  down)
    load_lab
    for name in "${names[@]}"; do
      if [[ $(virsh domstate "$name") != 'shut off' ]]; then virsh destroy "$name"; fi
    done
    echo 'Stopped nodes; Terraform definitions and all disks retained.'
    ;;
  destroy)
    load_lab
    export TF_VAR_lab="$lab" TF_VAR_uri="$LIBVIRT_DEFAULT_URI"
    # Use the applied configuration, not a new checkout's desired resources.
    exec tofu -chdir="$lab/terraform" destroy -input=false -auto-approve
    ;;
  *) echo "Usage: $0 up|plan|start|down|destroy|fixtures|ssh NODE COMMAND..." >&2; exit 1 ;;
esac

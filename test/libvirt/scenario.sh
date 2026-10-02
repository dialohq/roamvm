#!/usr/bin/env bash
set -euo pipefail
export LIBVIRT_DEFAULT_URI=qemu:///session
root=$(git rev-parse --show-toplevel)
test "$PWD" = "$root" || { echo 'Run from the repository root' >&2; exit 1; }
lab="$root/.lab/libvirt"
baseline="$lab/baseline"
names=(roamvm-libvirt-control-plane roamvm-libvirt-worker roamvm-libvirt-worker2)
mkdir -p "$lab"
# A reset and its test must own the whole cluster, not just one domain.
exec 9> "$lab/scenario.lock"
flock -n 9 || { echo 'Another baseline/scenario operation is running' >&2; exit 1; }

stop_domains() {
  local domains
  domains=$(virsh list --all --name)
  for name in "${names[@]}"; do
    test -f "$lab/$name.xml"
    if grep -Fxq "$name" <<< "$domains"; then
      if [[ $(virsh domstate "$name") != 'shut off' ]]; then virsh destroy "$name"; fi
      virsh undefine "$name"
    fi
  done
}

reset_disks() {
  test -f "$baseline/ready" || { echo 'Create a baseline first' >&2; exit 1; }
  # Validate every backing file before discarding any working disk.
  for name in "${names[@]}"; do
    test -f "$baseline/$name.qcow2"
    test -f "$baseline/$name.xml"
  done
  stop_domains
  for name in "${names[@]}"; do
    qemu-img create -f qcow2 -F qcow2 -b "$baseline/$name.qcow2" "$lab/$name.qcow2.next"
    mv -T "$lab/$name.qcow2.next" "$lab/$name.qcow2"
    cp "$baseline/$name.xml" "$lab/$name.xml"
    # Define the captured systems rather than rebuilding the node OS on reset.
    virsh define "$lab/$name.xml"
  done
  rm -f "$lab/guest-ref" "$lab/nixos-ref" "$lab/generation-ref" "$lab/firmware-ref"
  cp "$baseline/"*-ref "$lab/"
}

case "${1:-}" in
  freeze)
    test ! -e "$baseline" || { echo 'Baseline already exists (or an incomplete freeze needs inspection)' >&2; exit 1; }
    export KUBECONFIG="$lab/kubeconfig"
    test "$(kubectl config current-context)" = roamvm-libvirt
    vms=$(kubectl get virtualmachines -A -o name)
    test -z "$vms" || { echo 'Remove test VMs before freezing the lab' >&2; exit 1; }
    test -s "$lab/guest-ref"
    for name in "${names[@]}"; do
      test -f "$lab/$name.xml"
      test -f "$lab/$name.qcow2"
      test ! -L "$lab/$name.qcow2"
      test "$(virsh domstate "$name")" = running
    done
    mkdir "$baseline"
    for name in "${names[@]}"; do
      cp "$lab/$name.xml" "$baseline/"
      nix-store --add-root "$baseline/$name-system" --realise "$(readlink -f "$lab/$name-system")" >/dev/null
      bash test/libvirt/lab.sh ssh "$name" 'systemctl stop k3s && sync'
    done
    bash test/libvirt/lab.sh ssh roamvm-libvirt-control-plane 'systemctl stop minio docker-registry && sync'
    stop_domains
    for name in "${names[@]}"; do
      mv "$lab/$name.qcow2" "$baseline/$name.qcow2"
      chmod a-w "$baseline/$name.qcow2"
    done
    for fixture in guest nixos generation firmware; do
      if [[ -f "$lab/$fixture-ref" ]]; then cp "$lab/$fixture-ref" "$baseline/"; fi
    done
    touch "$baseline/ready"
    reset_disks
    echo 'Frozen disk baseline created; run a scenario or reset to boot it.'
    ;;
  reset)
    reset_disks
    bash test/libvirt/lab.sh up
    bash test/libvirt/install.sh
    ;;
  run)
    scenario=${2:-}
    case "$scenario" in
      crash) pattern=TestLocalCrashRecovery ;;
      lifecycle) pattern=TestLifecycle ;;
      network) pattern=TestKubernetes ;;
      cpu) pattern=TestOversubscription ;;
      resize) pattern='Test(OnlineResize|ResizeWithoutExpandableStorage)' ;;
      generations) pattern=TestNixOSGenerations ;;
      *) echo 'Scenarios: crash lifecycle network cpu resize generations' >&2; exit 1 ;;
    esac
    # Missing optional fixtures must fail here, not silently skip a scenario.
    if [[ "$scenario" == resize ]]; then test -s "$baseline/nixos-ref"; fi
    if [[ "$scenario" == generations ]]; then
      test -s "$baseline/generation-ref"
      test -s "$baseline/firmware-ref"
    fi
    run="$lab/runs/$(date -u +%Y%m%dT%H%M%S)-$scenario"
    mkdir -p "$run"
    git rev-parse HEAD > "$run/revision"
    git diff HEAD > "$run/worktree.patch"
    # Keep setup failures and test failures, including exit status, in one log.
    set +e
    (
      set -e
      reset_disks
      bash test/libvirt/lab.sh up
      bash test/libvirt/install.sh
      # shellcheck disable=SC1091
      source test/libvirt/env
      export ROAMVM_TEST_NETWORK_POLICY=1
      unset ROAMVM_TEST_RESIZE_IMAGE ROAMVM_TEST_GENERATION_IMAGE ROAMVM_TEST_FIRMWARE_IMAGE
      if [[ "$scenario" == resize ]]; then export ROAMVM_TEST_RESIZE_IMAGE; ROAMVM_TEST_RESIZE_IMAGE=$(cat "$baseline/nixos-ref"); fi
      if [[ "$scenario" == generations ]]; then
        export ROAMVM_TEST_GENERATION_IMAGE ROAMVM_TEST_FIRMWARE_IMAGE
        ROAMVM_TEST_GENERATION_IMAGE=$(cat "$baseline/generation-ref")
        ROAMVM_TEST_FIRMWARE_IMAGE=$(cat "$baseline/firmware-ref")
      fi
      go test -tags=integration -race -count=1 -timeout=30m -v -run "^$pattern$" ./test/integration
    ) 2>&1 | tee "$run/output.log"
    statuses=("${PIPESTATUS[@]}")
    status=${statuses[0]}
    if [[ "$status" == 0 ]]; then status=${statuses[1]}; fi
    set -e
    echo "$status" > "$run/exit-status"
    echo "Scenario output: $run (exit $status); working disks retained until the next reset."
    exit "$status"
    ;;
  *) echo "Usage: $0 freeze|reset|run SCENARIO" >&2; exit 1 ;;
esac

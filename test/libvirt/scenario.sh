#!/usr/bin/env bash
set -euo pipefail
export LIBVIRT_DEFAULT_URI=qemu:///session
root=$(git rev-parse --show-toplevel)
test "$PWD" = "$root" || { echo 'Run from the repository root' >&2; exit 1; }
lab="$root/.lab/libvirt"
baseline="$lab/baseline"
warm="$lab/warm"
if [[ -f "$warm/ready" && ${1:-} != freeze && ${1:-} != freeze-warm && ${1:-} != reset-cold ]]; then
  baseline="$warm"
fi
export KUBECONFIG="$lab/kubeconfig"
names=(roamvm-libvirt-control-plane roamvm-libvirt-worker roamvm-libvirt-worker2)
mkdir -p "$lab"
# A reset and its test must own the whole cluster, not just one domain.
exec 9> "$lab/scenario.lock"
flock -n 9 || { echo 'Another baseline/scenario operation is running' >&2; exit 1; }

stop_domains() {
  local domains name pids=()
  domains=$(virsh list --all --name)
  for name in "${names[@]}"; do test -f "$lab/$name.xml"; done
  for name in "${names[@]}"; do
    if grep -Fxq "$name" <<< "$domains"; then
      (
        if [[ $(virsh domstate "$name") != 'shut off' ]]; then virsh destroy "$name"; fi
        virsh undefine "$name"
      ) &
      pids+=("$!")
    fi
  done
  wait_jobs "${pids[@]}"
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

wait_jobs() {
  local pid status=0
  for pid in "$@"; do wait "$pid" || status=1; done
  return "$status"
}

fingerprint() {
  # A RAM snapshot contains running binaries and mounted host files. Never
  # silently replay it against a different runtime, configuration or key.
  {
    git ls-files --cached --others --exclude-standard -z -- api cmd internal config go.mod go.sum flake.nix flake.lock test/libvirt/node.nix test/libvirt/runtime.nix test/libvirt/install.sh test/libvirt/lab.sh test/libvirt/kustomization.yaml |
      sort -z | xargs -0 sha256sum
    sha256sum bin/roamvm "$lab/id_ed25519.pub"
    stat -c '%d:%i' bin/roamvm "$lab/id_ed25519.pub"
  } | sha256sum
}

warm_ready() {
  local node pids=()
  for node in "${names[@]}"; do
    bash test/libvirt/lab.sh ssh "$node" 'timeout 60 bash -o pipefail -c '\''until k3s crictl info 2>/dev/null | jq -e ".status.conditions | length > 0 and all(.status == true)" >/dev/null; do sleep 0.1; done'\''' </dev/null &
    pids+=("$!")
  done
  wait_jobs "${pids[@]}"
  kubectl --request-timeout=10s get --raw=/readyz
  kubectl -n roamvm-system rollout status deployment/controller --timeout=60s
  curl --fail --silent --show-error --max-time 10 http://192.168.124.10:9000/minio/health/ready
  curl --fail --silent --show-error --max-time 10 http://192.168.124.10:5000/v2/ >/dev/null
}

start_lab() {
  local node pids=()
  if [[ "$baseline" == "$warm" ]]; then
    test "$(fingerprint)" = "$(cat "$warm/fingerprint")" || {
      echo 'Warm baseline does not match this checkout. Use reset-cold and prepare a new warm baseline.' >&2
      return 1
    }
    for node in "${names[@]}"; do test -s "$warm/$node.save"; done
    reset_disks
    bash test/libvirt/lab.sh network
    for node in "${names[@]}"; do
      virsh restore "$warm/$node.save" --paused &
      pids+=("$!")
    done
    wait_jobs "${pids[@]}"
    # No node runs against peers whose memory/disks have not yet been restored.
    pids=()
    for node in "${names[@]}"; do virsh resume "$node" & pids+=("$!"); done
    wait_jobs "${pids[@]}"
    warm_ready
  else
    reset_disks
    bash test/libvirt/lab.sh up
    bash test/libvirt/install.sh
  fi
}

case "${1:-}" in
  freeze-warm)
    test ! -e "$warm" || { echo 'Warm baseline already exists (or an incomplete freeze needs inspection)' >&2; exit 1; }
    test "$(kubectl config current-context)" = roamvm-libvirt
    vms=$(kubectl get virtualmachines -A -o name)
    pods=$(kubectl get pods -A -l vm.roamvm.io/name -o name)
    test -z "$vms$pods" || { echo 'Remove test VMs and wait for their pods before freezing' >&2; exit 1; }
    test -s "$lab/guest-ref"
    for name in "${names[@]}"; do
      test -f "$lab/$name.xml"
      test -f "$lab/$name.qcow2" && test ! -L "$lab/$name.qcow2"
      test "$(virsh domstate "$name")" = running
      grep -q "type='virtiofs'" "$lab/$name.xml" || { echo 'Recreate domains with virtiofs before saving RAM' >&2; exit 1; }
      nix build ".#nixosConfigurations.$name.config.system.build.toplevel" --out-link "$lab/$name-system"
      system=$(bash test/libvirt/lab.sh ssh "$name" readlink -f /run/current-system </dev/null)
      test "$system" = "$(readlink -f "$lab/$name-system")" || { echo "Recreate $name with the current NixOS configuration" >&2; exit 1; }
    done
    # Capture only a deployment of the current source, not merely a fingerprint
    # of edited files beside stale running processes.
    make build
    bash test/libvirt/install.sh
    for name in "${names[@]:1}"; do
      bash test/libvirt/lab.sh ssh "$name" systemctl restart roamvm-device-plugin </dev/null
    done
    warm_ready
    mkdir "$warm"
    fingerprint > "$warm/fingerprint"
    for name in "${names[@]}"; do
      virsh dumpxml "$name" --inactive > "$warm/$name.xml"
      nix-store --add-root "$warm/$name-system" --realise "$(readlink -f "$lab/$name-system")" >/dev/null
      bash test/libvirt/lab.sh ssh "$name" 'sync; echo 3 > /proc/sys/vm/drop_caches' </dev/null
    done
    # Let free-page reporting discard released cache pages before saving RAM.
    sleep 2
    for name in "${names[@]}"; do virsh suspend "$name"; done
    pids=()
    for name in "${names[@]}"; do
      virsh save "$name" "$warm/$name.save" --paused --image-format gzip &
      pids+=("$!")
    done
    wait_jobs "${pids[@]}"
    for name in "${names[@]}"; do
      mv "$lab/$name.qcow2" "$warm/$name.qcow2"
      chmod a-w "$warm/$name.qcow2" "$warm/$name.save"
    done
    for fixture in guest nixos generation firmware; do
      if [[ -f "$lab/$fixture-ref" ]]; then cp "$lab/$fixture-ref" "$warm/"; fi
    done
    touch "$warm/ready"
    baseline="$warm"
    reset_disks
    echo 'Prepared cluster saved with RAM; resets and scenarios now use this warm baseline.'
    ;;
  freeze)
    test ! -e "$baseline" || { echo 'Baseline already exists (or an incomplete freeze needs inspection)' >&2; exit 1; }
    test ! -e "$warm" || { echo 'Create the cold baseline before the warm baseline, not from disks backed by it' >&2; exit 1; }
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
  reset|reset-cold)
    start_lab
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
      start_lab
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
  *) echo "Usage: $0 freeze|freeze-warm|reset|reset-cold|run SCENARIO" >&2; exit 1 ;;
esac

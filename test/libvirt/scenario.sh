#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/common.sh"
lock_lab
load_lab
test "$PWD" = "$root" || { echo 'Run from the repository root' >&2; exit 1; }
baseline="$lab/baseline"
warm="$lab/warm"
if [[ -f "$warm/ready" && ${1:-} != freeze && ${1:-} != freeze-warm && ${1:-} != reset-cold ]]; then
  baseline="$warm"
fi
export KUBECONFIG="$lab/kubeconfig"
mkdir -p "$lab"

stop_domains() {
  local domains name pids=()
  local targets=("${names[@]}")
  if (( $# )); then targets=("$@"); fi
  domains=$(virsh list --all --name)
  for name in "${targets[@]}"; do test -f "$lab/$name.xml"; done
  for name in "${targets[@]}"; do
    if grep -Fxq "$name" <<< "$domains"; then
      (
        if [[ $(virsh domstate "$name") != 'shut off' ]]; then virsh destroy "$name"; fi
      ) &
      pids+=("$!")
    fi
  done
  wait_jobs "${pids[@]}"
}

reset_disks() {
  local name pids=()
  test -f "$baseline/ready" || { echo 'Create a baseline first' >&2; exit 1; }
  # Validate every backing file before discarding any working disk.
  for name in "${names[@]}"; do
    test -f "$baseline/$name.qcow2"
    test -f "$baseline/$name.xml"
    test -f "$lab/$name.xml"
    # Terraform owns definitions and UUIDs. Never restore old RAM into changed
    # hardware or silently redefine a domain to match an obsolete snapshot.
    virsh dumpxml "$name" --inactive | cmp "$baseline/$name.xml" - || {
      echo "$name: baseline domain differs; prepare a new baseline" >&2; return 1;
    }
  done
  # Pipeline each node's teardown, overlay reset and optional paused restore.
  # The caller must wait for every restore before resuming any node.
  for name in "${names[@]}"; do
    (
      stop_domains "$name"
      qemu-img create -f qcow2 -F qcow2 -b "$baseline/$name.qcow2" "$lab/$name.qcow2.next"
      mv -T "$lab/$name.qcow2.next" "$lab/$name.qcow2"
      if [[ ${1:-} == restore ]]; then
        virsh restore "$baseline/$name.save" --paused --parallel-channels 4
      fi
    ) &
    pids+=("$!")
  done
  wait_jobs "${pids[@]}"
  rm -f "$lab/guest-ref" "$lab/nixos-ref" "$lab/generation-ref" "$lab/firmware-ref"
  cp "$baseline/"*-ref "$lab/"
}

wait_jobs() {
  local pid status=0
  for pid in "$@"; do wait "$pid" || status=1; done
  return "$status"
}

require_persistent() {
  local filesystem
  filesystem=$(findmnt -n -o FSTYPE -T "$(realpath "$1")") || return 1
  case "$filesystem" in
    tmpfs|ramfs)
      echo "Saved baselines require persistent storage: $1" >&2
      return 1 ;;
  esac
}

fingerprint() {
  # A RAM snapshot contains running binaries and mounted host files. Never
  # silently replay it against a different runtime, configuration or key.
  {
    git ls-files --cached --others --exclude-standard -z -- api cmd internal config go.mod go.sum flake.nix flake.lock test/guest test/libvirt/scenario.sh test/libvirt/node.nix test/libvirt/runtime.nix test/libvirt/install.sh test/libvirt/lab.sh test/libvirt/common.sh test/libvirt/config.nix test/libvirt/terraform.nix test/libvirt/terraform.lock.hcl test/libvirt/kustomization.nix |
      sort -z | xargs -0 sha256sum
    # OpenSSL uses hardware SHA acceleration for the large runtime binary;
    # still hash the entire content on every reset, not just file metadata.
    openssl dgst -sha256 -r "$manifest" "$lab/id_ed25519.pub"
    stat -c '%d:%i' "$lab/id_ed25519.pub"
  } | sha256sum
}

guest_ready() {
  local node=$1 pid result request deadline=$((SECONDS + 60))
  # RAM restore also rewinds the guest wall clock. Correct it before probing
  # live CRI/CNI state through the local guest-agent channel, without SSH/PAM.
  # QEMU's host-clock RTC already advances while saved. Set CLOCK_REALTIME
  # only: guest-set-time unnecessarily waits for a synchronous hwclock write.
  while (( SECONDS < deadline )); do
    request=$(jq -nc --arg now "$EPOCHREALTIME" '{execute:"guest-exec",arguments:{path:"/run/current-system/sw/bin/bash",arg:["-ec",("/run/current-system/sw/bin/date --set @" + $now + " >/dev/null; exec /run/current-system/sw/bin/crictl --runtime-endpoint unix:///run/k3s/containerd/containerd.sock info")],"capture-output":true}}')
    pid=$(virsh qemu-agent-command "$node" --timeout 2 "$request" | jq -er '.return.pid')
    while (( SECONDS < deadline )); do
      result=$(virsh qemu-agent-command "$node" --timeout 2 "{\"execute\":\"guest-exec-status\",\"arguments\":{\"pid\":$pid}}")
      if jq -e '.return.exited' <<< "$result" >/dev/null; then
        if jq -e '.return.exitcode == 0' <<< "$result" >/dev/null &&
          jq -r '.return["out-data"]' <<< "$result" | base64 -d | jq -e '.status.conditions | length > 0 and all(.status == true)' >/dev/null; then
          return 0
        fi
        break
      fi
      sleep 0.02
    done
  done
  echo "$node: runtime did not become ready" >&2
  return 1
}

warm_ready() {
  local node pids=()
  for node in "${names[@]}"; do
    guest_ready "$node" &
    pids+=("$!")
  done
  wait_jobs "${pids[@]}"
  pids=()
  kubectl --request-timeout=10s get --raw=/readyz & pids+=("$!")
  kubectl --request-timeout=10s get --raw="/api/v1/namespaces/roamvm-system/pods/$controller:8081/proxy/readyz" & pids+=("$!")
  curl --fail --silent --show-error --max-time 10 "http://$control_ip:9000/minio/health/ready" & pids+=("$!")
  curl --fail --silent --show-error --max-time 10 "http://$control_ip:5000/v2/" >/dev/null & pids+=("$!")
  wait_jobs "${pids[@]}"
}

start_lab() {
  local node pids=()
  if [[ "$baseline" == "$warm" ]]; then
    test "$(fingerprint)" = "$(cat "$warm/fingerprint")" || {
      echo 'Warm baseline does not match this checkout. Use reset-cold and prepare a new warm baseline.' >&2
      return 1
    }
    for node in "${names[@]}"; do
      test -s "$warm/$node.save"
      require_persistent "$warm/$node.save"
    done
    controller=$(cat "$warm/controller-pod")
    reset_disks restore
    # No node runs against peers whose memory/disks have not yet been restored.
    pids=()
    for node in "${names[@]}"; do virsh resume "$node" & pids+=("$!"); done
    wait_jobs "${pids[@]}"
    warm_ready
  else
    reset_disks
    bash test/libvirt/lab.sh start
    bash test/libvirt/install.sh
  fi
}

case "${1:-}" in
  freeze-warm)
    test ! -e "$warm" || { echo 'Warm baseline already exists (or an incomplete freeze needs inspection)' >&2; exit 1; }
    require_persistent "$lab"
    test -z "${ROAMVM_WARM_MEMORY_DIR:-}" || { echo 'RAM images are stored persistently under .lab/libvirt/warm; ROAMVM_WARM_MEMORY_DIR is no longer supported' >&2; exit 1; }
    test "$(kubectl config current-context)" = "$cluster"
    vms=$(kubectl get virtualmachines -A -o name)
    pods=$(kubectl get pods -A -l vm.roamvm.io/name -o name)
    test -z "$vms$pods" || { echo 'Remove test VMs and wait for their pods before freezing' >&2; exit 1; }
    test -s "$lab/guest-ref"
    build_lab_config "$lab/config-check"
    test "$(readlink -f "$lab/config")" = "$(readlink -f "$lab/config-check")" || {
      echo 'Apply the current lab configuration with make libvirt-up before freezing' >&2; exit 1;
    }
    for name in "${names[@]}"; do
      test -f "$lab/$name.xml"
      test -f "$lab/$name.qcow2" && test ! -L "$lab/$name.qcow2"
      test "$(virsh domstate "$name")" = running
      grep -q "type='virtiofs'" "$lab/$name.xml" || { echo 'Recreate domains with virtiofs before saving RAM' >&2; exit 1; }
      system=$(bash test/libvirt/lab.sh ssh "$name" readlink -f /run/current-system </dev/null)
      test "$system" = "$(readlink -f "$lab/$name-system")" || { echo "Recreate $name with the current NixOS configuration" >&2; exit 1; }
    done
    # Capture only a deployment of the current source, not merely a fingerprint
    # of edited files beside stale running processes.
    bash test/libvirt/install.sh
    for name in "${names[@]:1}"; do
      bash test/libvirt/lab.sh ssh "$name" systemctl restart roamvm-device-plugin </dev/null
    done
    # Freeze node-local unpacked images, not just a registry cache. Otherwise
    # every reset repeats public pulls for the probe and PVC helper Pods.
    # Keep this probe image aligned with networkClient in integration/lab_test.go.
    probe_image=curlimages/curl:8.17.0
    helper_image=$(kubectl -n kube-system get configmap local-path-config -o jsonpath='{.data.helperPod\.yaml}' |
      kubectl create --dry-run=client -f - -o jsonpath='{.spec.containers[0].image}')
    test -n "$helper_image"
    # Large optional NixOS fixtures remain registry-only.
    image=$(cat "$lab/guest-ref")
    pids=()
    bash test/libvirt/lab.sh ssh "$control" timeout 180 crictl --runtime-endpoint unix:///run/k3s/containerd/containerd.sock pull "$probe_image" </dev/null &
    pids+=("$!")
    for name in "${names[@]:1}"; do
      bash test/libvirt/lab.sh ssh "$name" bash -s -- "$image" "$helper_image" <<'PULL' &
set -euo pipefail
for image; do
  timeout 180 crictl --runtime-endpoint unix:///run/k3s/containerd/containerd.sock pull "$image"
done
PULL
      pids+=("$!")
    done
    wait_jobs "${pids[@]}"
    controller=$(kubectl -n roamvm-system get pods -l app=roamvm-controller -o json | jq -er '[.items[] | select(.metadata.deletionTimestamp == null) | .metadata.name] | if length == 1 then .[0] else error("Expected one controller pod") end')
    warm_ready
    # Installation leaves large, reclaimable image/file caches in guest RAM.
    # Keep sparse restore fast without persisting those preparation-only pages.
    pids=()
    for name in "${names[@]}"; do
      bash test/libvirt/lab.sh ssh "$name" bash -s <<'RECLAIM' &
set -euo pipefail
reporting=/sys/module/page_reporting/parameters/page_reporting_order
order=$(cat "$reporting")
trap 'echo "$order" > "$reporting"' EXIT
# Report small free blocks too; the normal 2 MiB threshold misses fragmented
# pages. Restore the original threshold so tests do not pay this overhead.
echo 0 > "$reporting"
sync
echo 3 > /proc/sys/vm/drop_caches
# Unlike drop_caches, proactive reclaim also evicts mapped clean file pages.
# This is best-effort: the kernel may reclaim less and return EAGAIN. Do not
# swap or kill services to meet a size target; keep all live anonymous memory.
if ! echo '256M swappiness=0' > /sys/fs/cgroup/system.slice/memory.reclaim; then
  echo 'File-cache reclaim was partial; retaining the remaining pages.' >&2
fi
echo 1 > /proc/sys/vm/compact_memory
# Free-page reporting is asynchronous. This delay is paid only at capture.
sleep 5
RECLAIM
      pids+=("$!")
    done
    wait_jobs "${pids[@]}"
    # Warm the real guest path after reclaiming installation caches. Merely
    # pulling images leaves QEMU, the kernel and provisioning cold on every reset.
    # This also proves controller reconciliation, not just process readiness.
    for node in "${names[@]:1}"; do
      jq -nc --arg image "$(cat "$lab/guest-ref")" --arg node "$node" '{apiVersion:"vm.roamvm.io/v1alpha1",kind:"VirtualMachine",metadata:{generateName:"warm-probe-",namespace:"default"},spec:{powerState:"Running",image:$image,cpus:2,memory:"512Mi",readinessPort:8080,nodeSelector:{"kubernetes.io/hostname":$node}}}' |
        kubectl create -f - -o name > "$lab/warm-probe"
      probe=$(cat "$lab/warm-probe")
      kubectl -n default wait "$probe" --for=condition=Ready --timeout=180s
      kubectl -n default delete "$probe" --wait=true --timeout=180s
      rm "$lab/warm-probe"
    done
    warm_ready
    mkdir "$warm"
    printf '%s\n' "$controller" > "$warm/controller-pod"
    fingerprint > "$warm/fingerprint"
    for name in "${names[@]}"; do
      virsh dumpxml "$name" --inactive > "$warm/$name.xml"
      nix-store --add-root "$warm/$name-system" --realise "$(readlink -f "$lab/$name-system")" >/dev/null
      bash test/libvirt/lab.sh ssh "$name" sync </dev/null
    done
    for name in "${names[@]}"; do virsh suspend "$name"; done
    pids=()
    for name in "${names[@]}"; do
      virsh save "$name" "$warm/$name.save" --paused --image-format sparse --parallel-channels 2 &
      pids+=("$!")
    done
    wait_jobs "${pids[@]}"
    for name in "${names[@]}"; do
      mv "$lab/$name.qcow2" "$warm/$name.qcow2"
      chmod a-w "$warm/$name.qcow2"
      # System libvirt creates root-owned save files; only the daemon reads
      # them during restore. Harden user-owned files on session connections.
      if [[ -O "$warm/$name.save" ]]; then chmod a-w "$warm/$name.save"; fi
    done
    for fixture in guest nixos generation firmware; do
      if [[ -f "$lab/$fixture-ref" ]]; then cp "$lab/$fixture-ref" "$warm/"; fi
    done
    # Publish readiness only after the images, disk renames and metadata are
    # durable. A host crash during capture must leave an incomplete baseline.
    sync -f "$warm"
    touch "$warm/ready"
    sync -f "$warm"
    baseline="$warm"
    reset_disks
    echo 'Prepared cluster saved with RAM; resets and scenarios now use this warm baseline.'
    ;;
  freeze)
    test ! -e "$baseline" || { echo 'Baseline already exists (or an incomplete freeze needs inspection)' >&2; exit 1; }
    test ! -e "$warm" || { echo 'Create the cold baseline before the warm baseline, not from disks backed by it' >&2; exit 1; }
    export KUBECONFIG="$lab/kubeconfig"
    test "$(kubectl config current-context)" = "$cluster"
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
      virsh dumpxml "$name" --inactive > "$baseline/$name.xml"
      nix-store --add-root "$baseline/$name-system" --realise "$(readlink -f "$lab/$name-system")" >/dev/null
      bash test/libvirt/lab.sh ssh "$name" 'systemctl stop k3s && sync'
    done
    bash test/libvirt/lab.sh ssh "$control" 'systemctl stop minio docker-registry && sync'
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
  run-all)
    while read -r scenario; do
      bash "$0" run "$scenario"
    done < <(jq -r '.scenarios | keys[]' "$manifest")
    ;;
  run)
    scenario=${2:-}
    specification=$(jq -ec --arg name "$scenario" '.scenarios[$name] // error("Unknown scenario: " + $name)' "$manifest")
    pattern=$(jq -r '.tests | "(" + join("|") + ")"' <<< "$specification")
    # Missing optional fixtures must fail here, not silently skip a scenario.
    while read -r fixture; do test -s "$baseline/$fixture-ref"; done < <(jq -r '(.fixtures // {})[]' <<< "$specification")
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
      while IFS=$'\t' read -r variable fixture; do
        export "$variable=$(cat "$baseline/$fixture-ref")"
      done < <(jq -r '(.fixtures // {}) | to_entries[] | [.key, .value] | @tsv' <<< "$specification")
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
  *) echo "Usage: $0 freeze|freeze-warm|reset|reset-cold|run-all|run SCENARIO" >&2; exit 1 ;;
esac

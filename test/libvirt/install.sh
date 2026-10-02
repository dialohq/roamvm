#!/usr/bin/env bash
set -euo pipefail
export KUBECONFIG="$PWD/.lab/libvirt/kubeconfig"
test "$(kubectl config current-context)" = roamvm-libvirt
# Take over the already-provisioned addon. Otherwise K3s can race our image pin
# during startup. A .skip file retains resources but stops manifest reapplication.
kubectl -n kube-system get deployment/local-path-provisioner >/dev/null
bash test/libvirt/lab.sh ssh roamvm-libvirt-control-plane 'set -e; skip=/var/lib/rancher/k3s/server/manifests/local-storage.yaml.skip; if ! test -f "$skip"; then touch "$skip"; systemctl restart k3s; fi' </dev/null
kubectl -n kube-system set image deployment/local-path-provisioner local-path-provisioner=rancher/local-path-provisioner:v0.0.37
# Keep cluster services off workers that the failure tests deliberately power off.
for deployment in coredns local-path-provisioner metrics-server; do
  kubectl -n kube-system patch deployment "$deployment" --type=merge -p '{"spec":{"template":{"spec":{"nodeSelector":{"kubernetes.io/hostname":"roamvm-libvirt-control-plane"}}}}}'
  kubectl -n kube-system rollout status deployment/"$deployment" --timeout=180s
done
nix build .#libvirt-runtime --out-link .lab/libvirt/runtime.tar.gz
nix build .#test-guest --out-link .lab/libvirt/guest.tar.gz
runtime=$(readlink -f .lab/libvirt/runtime.tar.gz)
# Nix store paths are immutable. Keep the imported manifest digest on the host,
# outside the resettable disks, but check each node's actual image on every run.
mkdir -p .lab/libvirt/runtime-digests
digest_file=".lab/libvirt/runtime-digests/$(basename "$runtime")"
expected=""
if [[ -f "$digest_file" ]]; then
  expected=$(cat "$digest_file")
  [[ $expected =~ ^sha256:[0-9a-f]{64}$ ]]
fi
install_runtime() {
  bash test/libvirt/lab.sh ssh "$1" bash -s -- "$runtime" "$expected" <<'REMOTE'
set -euo pipefail
runtime=$1
expected=${2:-}
image=docker.io/library/roamvm:libvirt-lab
# Restored Kubernetes Ready conditions can predate this boot. Wait for the live
# containerd server; the old, slow imports used to hide this startup race.
timeout 180 bash -c 'until k3s ctr version >/dev/null 2>&1; do sleep 0.5; done' </dev/null
check=$(k3s ctr images check "name==$image")
if [[ -n "$expected" ]] && awk -v digest="$expected" '$3 == digest && $4 == "complete" && $NF == "true" {found=1} END {exit !found}' <<< "$check"; then
  echo "$(hostname): reusing verified runtime $expected" >&2
else
  echo "$(hostname): importing runtime $runtime" >&2
  k3s ctr images import "$runtime" >&2
  sync
  fstrim / >&2
fi
actual=$(k3s ctr images ls "name==$image" | awk 'NR == 2 {print $3}')
[[ $actual =~ ^sha256:[0-9a-f]{64}$ ]]
[[ -z "$expected" || "$actual" == "$expected" ]]
k3s ctr run --rm "$image" verify-runtime /bin/sh -ec 'ip -V; qemu-img --version; id root' </dev/null >&2
printf '%s\n' "$actual"
REMOTE
}
nodes=(roamvm-libvirt-control-plane roamvm-libvirt-worker roamvm-libvirt-worker2)
if [[ -z "$expected" ]]; then
  # Learn a new archive's digest from a successful import and smoke test, never
  # from a tag that may still refer to the previous checkout's runtime.
  expected=$(install_runtime "${nodes[0]}")
  [[ $expected =~ ^sha256:[0-9a-f]{64}$ ]]
  printf '%s\n' "$expected" > "$digest_file.next"
  mv "$digest_file.next" "$digest_file"
  nodes=("${nodes[@]:1}")
fi
pids=()
for node in "${nodes[@]}"; do
  install_runtime "$node" >/dev/null &
  pids+=("$!")
done
status=0
for pid in "${pids[@]}"; do
  wait "$pid" || status=1
done
test "$status" = 0
mc alias set roamvm-libvirt http://192.168.124.10:9000 roamvm-local roamvm-local-test-only
mc mb --ignore-existing roamvm-libvirt/roamvm
bin/roamvm image-push --plain-http --tag 192.168.124.10:5000/test-guest:local --tar .lab/libvirt/guest.tar.gz > .lab/libvirt/guest-ref
kubectl apply --server-side -k test/libvirt
kubectl -n roamvm-system rollout restart deployment/controller
kubectl -n roamvm-system rollout status deployment/controller --timeout=180s

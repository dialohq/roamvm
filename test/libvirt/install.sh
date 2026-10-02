#!/usr/bin/env bash
set -euo pipefail
export KUBECONFIG="$PWD/.lab/libvirt/kubeconfig"
test "$(kubectl config current-context)" = roamvm-libvirt
# Take over the already-provisioned addon. Otherwise K3s can race our image pin
# during startup. A .skip file retains resources but stops manifest reapplication.
kubectl -n kube-system get deployment/local-path-provisioner >/dev/null
bash test/libvirt/lab.sh ssh roamvm-libvirt-control-plane 'set -e; skip=/var/lib/rancher/k3s/server/manifests/local-storage.yaml.skip; if ! test -f "$skip"; then touch "$skip"; systemctl restart k3s; fi'
kubectl -n kube-system set image deployment/local-path-provisioner local-path-provisioner=rancher/local-path-provisioner:v0.0.37
# Keep cluster services off workers that the failure tests deliberately power off.
for deployment in coredns local-path-provisioner metrics-server; do
  kubectl -n kube-system patch deployment "$deployment" --type=merge -p '{"spec":{"template":{"spec":{"nodeSelector":{"kubernetes.io/hostname":"roamvm-libvirt-control-plane"}}}}}'
  kubectl -n kube-system rollout status deployment/"$deployment" --timeout=180s
done
nix build .#libvirt-runtime --out-link .lab/libvirt/runtime.tar.gz
nix build .#test-guest --out-link .lab/libvirt/guest.tar.gz
for node in roamvm-libvirt-control-plane roamvm-libvirt-worker roamvm-libvirt-worker2; do
  bash test/libvirt/lab.sh ssh "$node" 'k3s ctr images import /lab/.lab/libvirt/runtime.tar.gz'
  bash test/libvirt/lab.sh ssh "$node" 'k3s ctr run --rm docker.io/library/roamvm:libvirt-lab verify-runtime /bin/sh -ec "ip -V; qemu-img --version; id root" && sync && fstrim /'
done
mc alias set roamvm-libvirt http://192.168.124.10:9000 roamvm-local roamvm-local-test-only
mc mb --ignore-existing roamvm-libvirt/roamvm
bin/roamvm image-push --plain-http --tag 192.168.124.10:5000/test-guest:local --tar .lab/libvirt/guest.tar.gz > .lab/libvirt/guest-ref
kubectl apply --server-side -k test/libvirt
kubectl -n roamvm-system rollout restart deployment/controller
kubectl -n roamvm-system rollout status deployment/controller --timeout=180s

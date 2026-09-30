#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."
export KUBECONFIG="$PWD/.lab/kubeconfig"
test "$(kubectl config current-context)" = kind-roamvm-test
compose=(docker compose -f test/lab/compose.yaml)
s3_ip=$(docker inspect -f '{{(index .NetworkSettings.Networks "kind").IPAddress}}' "$("${compose[@]}" ps -q s3)")
trap 'rm -f test/lab/store.env' EXIT
cat > test/lab/store.env <<ENV
S3_ENDPOINT=http://$s3_ip:9000
S3_BUCKET=roamvm
AWS_ACCESS_KEY_ID=roamvm-local
AWS_SECRET_ACCESS_KEY=roamvm-local-test-only
ENV
kubectl apply --server-side -k test/lab
for node in roamvm-test-worker roamvm-test-worker2; do
  docker exec "$node" test -c /dev/kvm
  docker cp -L bin/roamvm "$node:/usr/local/bin/roamvm.new"
  docker exec "$node" mv /usr/local/bin/roamvm.new /usr/local/bin/roamvm
  docker cp config/roamvm-device-plugin.service "$node:/etc/systemd/system/"
  docker exec "$node" systemctl daemon-reload
  docker exec "$node" systemctl enable roamvm-device-plugin
  docker exec "$node" systemctl restart roamvm-device-plugin
  docker exec "$node" mkdir -p /etc/containerd/certs.d/localhost:15001
  docker exec -i "$node" sh -c 'cat > /etc/containerd/certs.d/localhost:15001/hosts.toml' <<'HOSTS'
[host."http://roamvm-test-registry-1:5000"]
  capabilities = ["pull", "resolve"]
HOSTS
done
kubectl -n roamvm-system rollout restart deployment/controller
kubectl -n roamvm-system rollout status deployment/controller --timeout=120s

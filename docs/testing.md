# Local validation

The test lab is a dedicated `kind-roamvm` cluster: one control plane and two worker
containers on one physical Linux machine. Workers receive real `/dev/kvm`; their
runtime disks are bind-mounted directories on the host's NVMe-backed ext4
filesystem. They are separate scheduling targets, not separate physical servers.
The registry and MinIO are separate containers on the kind Docker network, with
host ports bound only to loopback.

## Repeat the tests

Prerequisites: Docker, kind 0.31.0, kubectl, Go 1.26.7, Python 3, make, qemu-img,
mke2fs, modprobe, cpio, gzip, xz/zstd, and a working KVM device. Supply a Linux
x86-64 MinIO binary supporting conditional multipart completion; the tested lab
used 2025-10-15T17-29-55Z. This is a test dependency, not the prescribed production
object store. Registry and object-store credentials generated here are local
fixtures only.

```sh
python3 test/lab/up.py --minio-binary /path/to/minio
# Existing lab: append --reuse. Nothing is deleted automatically.
source .lab/env
export PATH="$PWD/bin:$PATH"
make test
```

`up.py` builds the actual multi-stage Dockerfile, loads it into kind, creates the
bucket/credentials, installs the generated CRD and controllers, and checks their
rollouts. `--skip-build` uses an already built `roamvm:dev` image. 

Build the small, deliberately unauthenticated **test-only** VM fixture using a
matching Linux kernel and module tree, a static BusyBox, and Linux's
`scripts/extract-vmlinux` script:

```sh
python3 test/guest/build.py --out .lab/guest \
  --kernel /path/to/bzImage --modules /path/to/module-root \
  --kernel-version YOUR_KERNEL_VERSION \
  --busybox /path/to/static/busybox \
  --extract-vmlinux /path/to/linux/scripts/extract-vmlinux
roamvm image-push --plain-http --tag "$LAB_REGISTRY/test-guest:local" \
  --tar .lab/guest/guest.tar > .lab/guest-ref

go test -race ./internal/state -run TestS3 -count=1
python3 test/e2e.py --image "$(cat .lab/guest-ref)" \
  --lab-tool bin/lab-tool --node-failure
python3 test/kubernetes.py --image "$(cat .lab/guest-ref)" \
  --lab-tool bin/lab-tool
# After creating an SSH-enabled VM from your own image, initially Stopped:
python3 test/existing-vm.py --vm YOUR_VM_NAME
```

The integration scripts refuse unrelated Kubernetes contexts. The Kubernetes
suite also accepts `kind-roamvm-cilium` for CNI policy testing.
`--node-failure` kills one **kind worker container**, verifies fenced recovery,
and restarts it. The scripts create uniquely named fixtures and leave stopped VM
records/checkpoints for inspection. The configuration/PVC suite deletes its
successful fixtures. Failed fixtures remain for debugging. JSON results are in
`test-results/` and intentionally ignored by Git.

To remove the lab explicitly, first stop any VM whose state you care about, then
`kind delete cluster --name roamvm` and `docker rm -f roamvm-registry roamvm-s3`.
Keep `.lab/s3` and `.lab-disks` until their contents are no longer needed.

## Coverage

- Race-enabled Go tests: competing ownership acquisitions, immutable generations,
  failed/corrupt uploads, lost commit response, corrupt restore, old-epoch fencing,
  Pod-bound authentication and Kubernetes resource accounting.
- Actual qemu-img tests: writable overlay compaction/rebase preserves both data
  and zero-overwrites, and leaves its base unchanged.
- Registry tests: private authentication, eight concurrent pulls, atomic cache
  publication, offline cache reuse, unsafe tar entries and QCOW2 host-file references.
- Real MinIO: conditional create/replace, stale ETag rejection, read-after-write,
  conditional multipart upload and overwrite rejection.
- Real KVM: create, Service access, DNS, repeated shutdown/restore, cross-node
  scheduling and byte-exact payload persistence, daemon restart, S3 outage during
  stop, hypervisor kill, corrupt object refusal and explicit fenced recovery.
- Worker-container failure: last checkpoint retained, ownership never expires,
  explicit recovery runs elsewhere and discards uncommitted changes as specified.
- Kubernetes integration: queued cancellation, immutable-base CRD validation,
  configuration ISO refresh at next boot, secondary PVC writes/persistence,
  port forwarding, and durable VM deletion.
- Existing NixOS 26.05 disk: actual boot to SSH, ACPI shutdown, checkpoint commit,
  restart and identical generated SSH host key after restore.

The Kubernetes suite also passed on Cilium **1.20.1** with kube-proxy replacement
and `socketLB.hostNamespaceOnly=true`: Service/DNS reachability, port forwarding,
deny/allow NetworkPolicy enforcement, projected configuration disks, secondary
PVCs and durable deletion. Run it with `--check-network-policy` on a Cilium kind
cluster named `roamvm-cilium`; disable kind's default CNI and kube-proxy before
installing the Cilium chart. The default kind CNI does not enforce NetworkPolicy.

Garage **2.3.0** passed the lifecycle/failure and Kubernetes integration suites
using `STATE_BACKEND=kubernetes`. The real Kubernetes API ownership test uses
`TEST_KUBERNETES_NAMESPACE=roamvm-system go test -race ./internal/state`: 32
concurrent acquisitions admit exactly one owner, and an ownership change during
upload prevents the old owner from committing its checkpoint.

GPU/VFIO passthrough, reserved hugepages, block-mode PVCs, UEFI,
production S3 latency/durability and physical-machine failure have **not** been
qualified. CI runs unit/race/disk tests, schema regeneration and container build;
it does not pretend ordinary hosted runners run these KVM integration tests.

## Measurements

The small fixture has a 256 MiB virtual root. With cached bases and no reserved
VMs, repeated completed runs measured request-to-Service-ready starts of
5.0–6.1 seconds, and graceful durable stops of 4.0–4.9 seconds. The first request
immediately following the final controller rollout took 21.6 seconds: the
controller logs show it was waiting to acquire the Kubernetes leader lease for
most of the extra time. Do not count that deployment transition as a 6-second
start. About 1 MiB of
random guest data produced checkpoints of 1.9–3.1 MiB after filesystem metadata
and repeated writes. These include scheduling, Pod startup, disk restore, guest
boot and readiness—not just hypervisor launch time.

The existing 16 GiB virtual NixOS image (about 2.1 GiB base data) started to SSH
readiness in **6.82 seconds** in each of two final runs. Durable stops took
**2.96 and 2.99 seconds**, producing 13.9 and 15.3 MiB checkpoints. The base was
cached; these are cold VM starts, not cold registry downloads. SSH host keys
matched across the two runs. The NixOS test restored on the same worker; the
smaller guest separately exercised cross-worker placement and failure recovery.

The local object store was RAM-backed for this run. These numbers do not predict
WAN/object-store performance or power-loss durability, and are not a controlled
comparison with KubeVirt. Larger changed disks require proportionally more
upload/download work. See the per-operation results in `test-results/` for the
latest run; no warm VM pool or preallocated guest RAM was used.

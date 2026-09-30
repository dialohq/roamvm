# Local validation

The test lab is a dedicated `kind-roamvm` cluster: one control plane and two worker
containers on one physical Linux machine. Kind's privileged nodes expose real
`/dev/kvm`; the device plugin runs as a node systemd service. VM Pods use native
read-only image volumes and generic ephemeral PVCs from kind's local-path storage
class. Production uses the existing OpenEBS thin CSI storage class. The registry
and MinIO are separate containers, with host ports bound only to loopback.

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
bucket/credentials, installs the generated CRD, controller and node device-plugin service, and checks
their rollouts. `--skip-build` uses an already built `roamvm:dev` image.

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
# Use an idle worker; this temporarily restricts its CPUs and kubelet reservation.
python3 test/oversubscription.py --image "$(cat .lab/guest-ref)" \
  --node roamvm-worker2
# After creating an SSH-enabled VM from your own image, initially Stopped:
python3 test/existing-vm.py --vm YOUR_VM_NAME
# Controlled create/restore benchmark, timed through an actual guest response:
python3 test/startup.py --image "$(cat .lab/guest-ref)" \
  --node roamvm-worker --runs 5 --output test-results/startup.json
# SSH-enabled images: add --port 22 --cpus 4 --memory 4Gi.
# --cold-cache evicts only this image on the idle test node before each start.
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
Keep `.lab/s3` until its checkpoints are no longer needed.

## Coverage

- Race-enabled Go tests: competing ownership acquisitions, checkpoint replacement,
  failed/corrupt uploads, lost commit response, cleanup failures and delayed cleanup
  racing with a newer stop, corrupt restore, old-epoch fencing,
  Pod-bound authentication and Kubernetes resource accounting.
- Actual qemu-img tests: writable overlay compaction/rebase preserves both data
  and zero-overwrites, and leaves its base unchanged.
- Mounted-image validation: artifact symlinks, unexpected files, invalid boot
  manifests, oversized/empty disks and QCOW2 backing references are rejected.
  Kubelet/containerd owns registry authentication, unpacking and cache publication.
- Real MinIO: conditional create/replace, stale ETag rejection, read-after-write,
  conditional multipart upload and overwrite rejection. Checkpoint replacement
  also checks physical deletion in a temporary versioned bucket; its test requires
  bucket creation/versioning permissions. Garage replacement uses Kubernetes metadata.
- Real KVM: create, Service access, DNS, repeated shutdown/restore, cross-node
  scheduling and byte-exact payload persistence, runtime sidecar restart, S3 outage during
  stop, hypervisor kill, corrupt object refusal and explicit fenced recovery.
  Every successful stop asserts that exactly the current checkpoint remains in S3.
- Worker-container failure: last checkpoint retained, ownership never expires,
  explicit recovery runs elsewhere and discards uncommitted changes as specified.
- Kubernetes integration: queued cancellation, immutable-base CRD validation,
  configuration ISO refresh at next boot, secondary PVC writes/persistence,
  port forwarding, and durable VM deletion.
- Existing NixOS 26.05 disk: actual boot to SSH, ACPI shutdown, checkpoint commit,
  restart and identical generated SSH host key after restore.
- CPU oversubscription: two four-vCPU guests on a worker restricted to two CPU
  threads and two allocatable CPUs, each requesting 500m. Checks idle bursting,
  concurrent CPU work, enforcement of an explicit 250m limit, rejection of an
  excessive scheduler request, and checkpoint/restore after contention. The test
  restores the worker's cpuset and kubelet configuration on exit. RAM stays fully
  reserved; this does not test memory overcommit.

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

## Startup measurements

The PVC/image-volume implementation initially measured **7.139 s** median for
three small-guest HTTP starts on kind's local-path provisioner (7.067–7.598 s).
About four seconds precede Pod scheduling while that provisioner starts a helper
Pod and creates/binds the PVC. Production OpenEBS provisioning latency has not
been measured. Base caching uses containerd and does not require a new download
on every restart. Storage provisioning and native sidecar startup add costs that
must be included in end-to-end measurements.

The Nix-built PVC runtime also restored an existing Coder/NixOS workspace from
its pre-PVC checkpoint, preserving its saved project and SSH host key. Three
subsequent stop/start cycles took **14.035 s** median from Coder start to reading
the project over SSH (12.005–14.161 s), versus the earlier 9.243 s hostPath median.
These PVC measurements use kind's local-path storage inside container nodes;
they are not a controlled comparison of identical storage backends.

The PVC run passed 44 lifecycle/failure checks, 17 Cilium integration checks and
20 CPU oversubscription checks, plus race-enabled unit tests and the real
Kubernetes/MinIO state tests. The working claim is checked for Pod ownership and
garbage collection after durable stop. Cached base distribution is delegated to
kubelet; the runtime's custom registry-pull and cache-lock implementation is gone.

The following numbers are historical: they describe `fd742d3`, before switching
from hostPath to PVCs. They are not the startup claim for the PVC implementation.

Measure from the Kubernetes create/start request to an HTTP response or SSH
banner through a NodePort Service. `test/startup.py` records scheduling, runner,
and VM readiness observations separately, saves startup logs, and durably stops
between boots. No VM pool, paused guests, or reserved guest RAM is used. A cached
base is still a cold VM boot; `--cold-cache` includes pulling the base again.

The controlled September 2026 comparison uses baseline `4221e6b` and the optimized
Nix-built runtime on the same worker. The Nix guest has four vCPUs and 4 GiB RAM;
the small test guest has two vCPUs and 512 MiB. Medians include the first create
and subsequent checkpoint restores:

| Actual guest response | Before | After | Runs per version |
| --- | ---: | ---: | ---: |
| Small guest, cached base | 5.814 s | 2.104 s | 5 |
| NixOS SSH, cached base | 7.880 s | 4.021 s | 3 |
| NixOS SSH, uncached base | 23.518 s | 19.085 s | 3 |

The small guest's final range was 2.076–3.151 seconds; report the slower initial
Service setup along with the median. The runtime changes disable dnsmasq's
redundant address-conflict ping on the one-guest TAP, probe startup readiness at
100 ms while retaining one-second ownership heartbeats, and use buffered root
disk I/O. Guest flushes and checkpoint integrity/commit rules remain unchanged.
Startup logs expose base download, prepare, network, hypervisor and guest-ready
stages. The tested full Nix guest also passed a memory-pressure workload holding
about 3.4 GB, reading 851 MB, and writing/fsyncing 32 MiB with no cgroup OOM.

Base download remains bandwidth-bound: this worker uses Amazon EBS exposed as
NVMe, not physical local NVMe. The registry and object store are local; the
object store is RAM-backed. These measurements do not predict WAN performance,
production storage durability, or physical-host failure, and are not a controlled
comparison with KubeVirt. Larger writable checkpoints add transfer work. Wait for
controller leadership after a rollout before benchmarking; lease handover is a
deployment transition, separate from steady-state VM startup.

# Local validation

The lab uses Docker Compose for the registry and S3 fixture, kind for a three-node
Kubernetes cluster, Kustomize for installation, Nix for the guest image, and Go's
standard test and benchmark runners. It requires a Linux x86-64 host with real
`/dev/kvm`; VM execution is never mocked.

VM Pods use read-only image volumes and generic ephemeral PVCs from kind's
local-path storage class. Production uses OpenEBS thin CSI storage. The device
plugin runs as a systemd service inside each kind worker, matching its node-level
installation in production. The small install script only connects those pieces;
it does not implement a provisioner or test runner.

## Run the lab

Install Nix with flakes enabled and a working Docker daemon, then:

```sh
nix develop
make lab-up
make lab-guest
source test/lab/env
make test
make integration
```

`lab-up` creates **kind-roamvm-test** and refuses to replace an existing cluster.
To update an existing lab, use `make image lab-install`. Compose health checks and
its bucket initialization service handle dependency ordering. `make lab-guest`
builds a fixed kernel/module/BusyBox fixture and publishes its OCI digest; no
manual kernel paths or host mounts are required.

Registry and S3 ports bind to loopback (`15001` and `19001`). Their Compose named
volumes survive `make lab-down`; `docker compose -f test/lab/compose.yaml down -v`
explicitly discards them. Credentials in `test/lab` are public test fixtures.
The pinned MinIO package is an unmaintained **test-only compatibility fixture**
with known vulnerabilities; the Nix allowance is confined to the development
flake. Do not expose it or deploy it as your object store. Production Garage and
Kubernetes metadata remain supported independently of this fixture.

The default suite includes lifecycle/failure, Kubernetes integration, and CPU
oversubscription tests. `test/lab/env` selects an idle worker for the CPU test.
The QEMU unit tests also boot a paused TCG machine to exercise real QMP commands.
Optional cases are explicit:

```sh
# Partitioned NixOS root: grow online, preserve processes, stop and move.
make lab-nixos-guest
ROAMVM_TEST_RESIZE_IMAGE="$(cat .lab/nixos-guest-ref)" go test -tags=integration -race -run TestOnlineResize -v ./test/integration

# Kills a kind worker container, proves fencing, and restores it afterward.
ROAMVM_TEST_NODE_FAILURE=1 make integration

# On the separate kind-roamvm-cilium lab with an enforcing CNI:
ROAMVM_TEST_NETWORK_POLICY=1 go test -tags=integration -run TestKubernetes -v ./test/integration

# An existing stopped SSH-enabled VM; the test leaves it stopped.
ROAMVM_TEST_EXISTING_VM=my-vm go test -tags=integration -run TestExistingVM -v ./test/integration

# Standard machine-readable test output, usable by Go test reporters.
go test -json -tags=integration -count=1 -timeout=30m ./test/integration > test-results.json

# Actual guest response, including scheduling and storage provisioning.
make benchmark
ROAMVM_TEST_COLD_CACHE=1 make benchmark
# For your own SSH image, also set ROAMVM_TEST_IMAGE, ROAMVM_TEST_PORT=22,
# ROAMVM_TEST_CPUS=4 and ROAMVM_TEST_MEMORY=4Gi.
```

Tests refuse unrelated contexts. Node failure and CPU tests also reject workers
with other VMs. CPU limits/kubelet configuration and fault injections are restored
with `t.Cleanup`; successful fixtures are removed, and failed fixtures are retained
for debugging. Stop or delete failed VM fixtures before repeating CPU tests.
Checkpoints remain in the test bucket after VM deletion, as in the runtime's
normal retention model. Tests run sequentially; do not run multiple suites against
the same lab concurrently.

S3 state tests remain native Go tests:

```sh
go test -race -count=1 ./internal/state -run TestS3
TEST_KUBERNETES_NAMESPACE=roamvm-system go test -race -count=1 ./internal/state
```

For a Garage lab, provide its fixture credentials and set
`STATE_BACKEND=kubernetes STATE_NAMESPACE=roamvm-system`, matching the runtime's
object-store Secret. Go tests use the Kubernetes and S3 clients directly instead
of shelling out to a separate state-inspection program.

`make fmt` / `make fmt-check` use gofumpt. Format Nix with
`alejandra flake.nix nix test/guest/default.nix`. CI runs unit/race/disk tests,
compiles the real-VM suites without executing them, checks generated schemas, and
builds the container. KVM tests require the local lab.

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
PVCs and durable deletion. Run it with `ROAMVM_TEST_NETWORK_POLICY=1` on a Cilium kind
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
banner through a NodePort Service. `BenchmarkStartup` records scheduling, runner,
and VM readiness observations separately, reports startup logs, and durably stops
between boots. No VM pool, paused guests, or reserved guest RAM is used. A cached
base is still a cold VM boot; `ROAMVM_TEST_COLD_CACHE=1` includes pulling the base again.

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

# Local validation

The lab uses Docker Compose for the registry and S3 fixture, kind for a three-node
Kubernetes cluster, Kustomize for installation, Nix for the guest image, and Go's
standard test and benchmark runners. It requires a Linux x86-64 host with real
`/dev/kvm`; VM execution is never mocked.

VM Pods use read-only image volumes and retained VM-owned PVCs from kind's
local-path storage class. Production uses OpenEBS thin CSI storage. The device
plugin runs as a systemd service inside each kind worker, matching its node-level
installation in production. The small install script only connects those pieces;
it does not implement a provisioner or test runner.

## Run native NixOS nodes under libvirt

`test/libvirt/node.nix` declares three NixOS machines: a K3s control plane with
registry/MinIO, and two workers running the device plugin as a systemd service.
`lab.sh` defines their libvirt domains, disks and isolated bridge; Kubernetes runs
directly on NixOS, not inside kind or Docker. The runtime image is also built with
Nix and imported into containerd. Dependencies are pinned by `flake.lock`.
DNS, storage provisioning, metrics and the RoamVM controller stay on the control
plane so deliberately powering off a worker does not remove cluster services.

Use a Linux x86-64 host with nested KVM, Nix, sudo, at least 16 GiB RAM and ample
disk space (64 GiB recommended for all fixtures). The domains reserve 8 GiB RAM
in total, use sparse 16 GiB root disks and share the host Nix store read-only via
virtiofs. Do not put all three disks in tmpfs: image unpacking competes with guest
RAM. Session libvirt needs working `newuidmap`/`newgidmap` helpers and subordinate
UID/GID ranges. The Nix wrapper includes NixOS's `/run/wrappers/bin` on their PATH.

```sh
nix develop
make libvirt-up libvirt-install
source test/libvirt/env
make test
make libvirt-fixtures
export ROAMVM_TEST_RESIZE_IMAGE=$(cat .lab/libvirt/nixos-ref)
export ROAMVM_TEST_GENERATION_IMAGE=$(cat .lab/libvirt/generation-ref)
export ROAMVM_TEST_FIRMWARE_IMAGE=$(cat .lab/libvirt/firmware-ref)
ROAMVM_TEST_NETWORK_POLICY=1 make integration
```

Omit `libvirt-fixtures` and those three exports for a smaller run; resize and
generation tests will explicitly skip. The full run needs all three fixtures.

The environment enables the destructive **disposable worker** failure test.
The CPU test pins the worker's vCPU threads and restores their original affinity.
Lab configuration lives in `test/libvirt`; generated XML, disks, unique SSH keys,
kubeconfig and fixture references live in `.lab/libvirt`. Inspect a node with
`bash test/libvirt/lab.sh ssh roamvm-libvirt-worker systemctl --failed`.

`make libvirt-down` powers off and undefines only these lab domains and removes
their TAPs/bridge/firewall rules; it retains disks and keys. `make libvirt-up`
reuses them. Run `make libvirt-install` after each startup to reapply the pinned
Kubernetes settings. For NixOS configuration changes, take the lab down and up again;
for runtime changes, run `make build libvirt-install`. Do not run two suites
against the same lab concurrently.

This is a trusted local test fixture, not a production cluster. It uses public
test credentials, password-disabled root SSH with a generated key, and the
private subnet `192.168.124.0/24`. Reserve that subnet and the `rvm-lab` bridge
for this lab. The VMs can read this checkout and the host Nix store. Do not expose
its unauthenticated registry or test MinIO outside the isolated bridge.

### Run isolated scenarios from a frozen baseline

After installing the lab and publishing the desired fixtures, remove any test
VMs and freeze the prepared cluster once:

```sh
make libvirt-freeze              # Optional cold fallback; leaves nodes stopped
make libvirt-reset-cold
make libvirt-freeze-warm         # Capture a fully prepared, running cluster
make libvirt-scenario SCENARIO=lifecycle
make libvirt-scenario SCENARIO=network
# Or run every scenario, resetting the entire cluster before each:
make libvirt-scenarios
```

Scenarios are `crash`, `lifecycle`, `network`, `cpu`, `resize`, and `generations`.
The last two require `make libvirt-fixtures` **before freezing**; missing fixtures
cause an error rather than a silently skipped test. These run the existing Go
E2E tests, including real node failure and NetworkPolicy enforcement.

**Warm snapshots save RAM as well as disks.** `libvirt-freeze-warm` verifies the
running NixOS configurations, builds/installs current RoamVM, restarts the device
plugins and checks service readiness. It pauses all three nodes before saving
their memory in parallel, retains read-only disks under `.lab/libvirt/warm`, and
creates disposable working overlays. Free-page reporting and dropping guest page
caches reduce the saved memory size; save files are gzip-compressed. Existing
cold baselines remain unchanged, including any backing files used by warm disks.

`libvirt-reset` and scenarios prefer the warm baseline when one exists. They
restore all RAM images paused, then resume the nodes together and check live
containerd/CNI, Kubernetes API, controller rollout, MinIO and registry readiness.
There is no boot, build, image import or controller restart on this path. The
snapshot is tied to its host, QEMU/virtiofs configuration, runtime source and
mounted binary/key files. Changes are rejected **before discarding working
disks**, rather than silently testing stale processes. Host files shared through
virtiofs are not snapshotted; do not modify them during a capture or restore.

Use `make libvirt-reset-cold` to bypass a warm snapshot. To refresh it after code
changes, restore the cold baseline first, take the lab down, remove only
`.lab/libvirt/warm`, then run `make libvirt-up libvirt-freeze-warm`. Never remove a
baseline while working disks still reference it. Without a cold baseline,
recreate the disposable lab instead. Existing 9p lab domains must be recreated
with `make libvirt-down libvirt-up` before capturing RAM.

`libvirt-freeze` creates a **cold disk snapshot**. It stops K3s,
MinIO and the registry, flushes the disks, and stops all three domains. It moves
their QCOW2 files into `.lab/libvirt/baseline` and marks them read-only; fresh
copy-on-write overlays avoid another full disk copy. Filesystems may replay their
journals on boot. Each reset restores all three disks together, including the
Kubernetes database, object store, registry, kubelet settings and local PVCs.
Captured domain definitions and NixOS store roots keep the node systems fixed.
With a cold baseline, each scenario builds/installs the **current RoamVM** before
testing it against that infrastructure baseline.

Freeze after `make build libvirt-install` and fixture publication have completed,
so the baseline already contains unpacked runtime images and the pinned K3s
configuration. Installation caches the image manifest digest by immutable Nix
archive path under `.lab/libvirt/runtime-digests`, outside the resettable disks.
On every node it checks the actual digest, content completeness and unpacked
state before skipping an import, and still smoke-tests the runtime. Missing or
different images are imported; node checks/imports run concurrently. A new
archive requires one import to establish its digest. These caches do not skip
building the current source or restarting the controller.

On the 6-vCPU nested-KVM runner, reset-to-installed time with an unchanged,
already-built runtime fell from 79 seconds (one run) to 44 seconds median
(39, 44, 45 seconds across three resets). This includes cold boot, live
containerd readiness, image smoke tests and controller rollout, but not source
recompilation or the scenario itself; it is not a guest-VM startup benchmark.

With RAM snapshots, three full resets on that runner took **7.73, 7.74 and
8.06 seconds**, including the live readiness checks. Recreating the bridge/TAPs
after `libvirt-down` took 7.21 seconds. A saved-memory page-cache eviction run
took 9.14 seconds before the concurrent-shutdown optimization. The three
compressed RAM files occupy about 963 MiB in addition to the disk baseline.
These timings require an already-captured snapshot matching the checkout;
building code and capturing a new baseline are separate preparation steps.

`make libvirt-reset` restores the preferred baseline without running a test.
**Reset discards all changes in the working lab**, including failed-test VMs.
A scenario leaves its working disks available for debugging until the next
reset; logs, revision, tracked diff and exit status remain under
`.lab/libvirt/runs`. The scenario loop stops on failure. Baseline/scenario
operations are locked against each other; do not run manual lab commands or
other tests concurrently.

Keep the baseline at its original path: overlays contain absolute backing paths.
It is local to this checkout/host, including its SSH keys and private network,
not a portable VM artifact. Freezing refuses to overwrite an existing baseline.
To change the node OS or fixture set, take the lab down, discard the disposable
`.lab/libvirt` directory, and prepare a new lab and baseline. Only scripts and
scenario definitions belong in Git; generated VM disks and run logs do not.

## Run the kind lab

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

# GRUB generation selection, existing-image migration, and persistent rollback.
make lab-generation-guest
ROAMVM_TEST_GENERATION_IMAGE="$(cat .lab/generation-guest-ref)" \
ROAMVM_TEST_FIRMWARE_IMAGE="$(cat .lab/firmware-guest-ref)" \
  go test -tags=integration -race -run TestNixOSGenerations -v ./test/integration

# Kills a kind worker container, proves fencing, and restores it afterward.
ROAMVM_TEST_NODE_FAILURE=1 make integration

# Root, KVM, util-linux and e2fsprogs; uses only a disposable loopback filesystem.
# Extract the archive produced by make lab-guest, then point at its disk directory.
ROAMVM_TEST_DISK_FULL_BASE=/absolute/path/to/disk \
  go test -tags=integration -run TestDiskFullRecovery -v ./internal/runner

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
- Async stop tests: stop without an object store, independently retried worker,
  cancellation and exclusive disk locking, zero-download local resume, stale
  worker fencing, durable-only remote fallback and deletion. The local-resume
  tests use real QCOW2 files and verify data plus explicit zero-overwrites.
- Actual qemu-img tests: writable overlay validation/rebase preserves both data
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
- Host filesystem exhaustion: a real KVM guest fills a disposable 64 MiB ext4
  filesystem, pauses with `io-error`/`nospace`, resumes after online expansion,
  and verifies a 96 MiB payload before and after clean shutdown and reboot.
  The test also checks the effective writable file cache mode through QMP. It
  does not emulate thin-pool metadata exhaustion or physical device failure.
- Worker-container failure: last checkpoint retained, ownership never expires,
  explicit recovery runs elsewhere and discards uncommitted changes as specified.
- Kubernetes integration: queued cancellation, immutable-base CRD validation,
  configuration ISO refresh at next boot, secondary PVC writes/persistence,
  port forwarding, and durable VM deletion.
- NixOS generations: `nixos-rebuild test`, `switch`, and `switch --rollback`
  using prebuilt system closures through the native `--store-path` interface.
  Checks the active and booted system, kernel command line, runtime hostname,
  unchanged image/VM identity, user data, and checkpoint restore on another node.
  Covers migration from direct kernel boot and new SeaBIOS/GRUB images. This
  isolates activation and boot persistence; it does not rebuild packages in-guest.
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
100 ms while retaining one-second ownership heartbeats. Those historical runs
used buffered root disk I/O; writable disks now use direct I/O with guest flushes
enabled. The timings above have not been remeasured for the new cache mode.
Checkpoint integrity/commit rules remain unchanged.
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

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

`test/libvirt/config.nix` is one lab definition, parameterized by an instance slot:
a K3s control plane with registry/MinIO, two workers, and the scenario selections.
Nix builds the node systems and runtime; Terranix generates the Terraform JSON.
The runtime image uses Nixpkgs' headless `qemu_test` build, retaining KVM, virtio,
QCOW2 and firmware boot without loading GUI/audio libraries for every guest.
OpenTofu (the Terraform-compatible CLI pinned in the dev shell) owns domain
definitions and a NAT network through `dmacvicar/libvirt` 0.9.9. The harness owns
working disk contents, snapshot capture/restore and the existing Go test runs.
It never undefines or recreates domains during reset. Dependencies are pinned by
`flake.lock` and `test/libvirt/terraform.lock.hcl`. No handwritten domain XML or
host TAP/iptables setup is needed. Kubernetes runs directly on NixOS.
DNS, storage provisioning and the RoamVM controller stay on the control
plane so deliberately powering off a worker does not remove cluster services.
The lab omits metrics-server (the scenarios read CPU cgroup counters directly)
and runs K3s with `GOGC=50` to favor lower resident memory over GC throughput.
Guest memory reservations and hypervisor overhead remain unchanged; this does
not enable memory overcommit. The intentional OOM test uses a 1.25 GiB container
limit, above the 512 MiB guest's reservation but below node capacity, and checks
that recovery did not rely on a node-wide OOM kill.

Use a Linux x86-64 host with nested KVM, Nix, sudo, at least 16 GiB RAM and ample
disk space (64 GiB recommended for all fixtures). Each domain has 2 GiB RAM,
totaling 6 GiB, and four vCPUs. The CPU test pins a worker to two host CPUs and
runs two four-vCPU guests, including explicit quota and checkpoint checks.
Expose vCPUs as cores in one socket, not separate sockets. On AMD hosts without
an exposed invariant TSC, multiple sockets make Linux mark TSC unsynchronized.
This does not guarantee stable clocks across nested save/restore; do not force
`tsc=reliable` to suppress a watchdog failure.
They use sparse 16 GiB root disks and share the host Nix store read-only via
virtiofs. Do not put all three disks in tmpfs: image unpacking competes with guest
RAM. Provisioning requires an accessible system libvirt connection, including
its network driver, and a QEMU user that can access the lab directory. On a
dedicated NixOS host import `(import ./test/libvirt/host.nix {labUser = "coder";})`
into the host configuration, substituting your user. This enables libvirt and
runs QEMU as that unprivileged user without changing disk ownership. Host changes
are an administrator step, not a hidden side effect of `make libvirt-up`.
Virtiofs also needs working user-namespace helpers/subordinate UID/GID ranges.

The domains use 2 MiB huge pages for their shared RAM. Ordinary shared `memfd`
RAM can remain backed by 4 KiB pages even when anonymous transparent huge pages
are enabled, substantially slowing nested KVM. The host module reserves 3,072
huge pages (6 GiB) at boot; set its `labInstances` argument to the number of
concurrent labs. This pool is unavailable to ordinary host allocations even
while labs are stopped. Guest memory limits stay unchanged; snapshots remain
persistent on disk.
On other Linux hosts, reserve the same pool as an administrator before starting
the lab, for example `sudo sysctl -w vm.nr_hugepages=3072` for one instance.
Check `HugePages_Total` in `/proc/meminfo`: runtime allocation can fall short on
a fragmented host. Boot parameters `hugepagesz=2M hugepages=3072` reserve it early.
Budget additional pages for any other huge-page users; never shrink their pool.

Leave several GiB free beyond the frozen baseline for migration and generation
tests, which hold images on both workers. A full host filesystem makes libvirt
pause nodes with `I/O error`; that is distinct from guest memory exhaustion.

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

`make libvirt-plan` builds the declaration and shows the Terraform plan (exit 2
means changes). `make libvirt-up` applies it and boots the nodes. `make
libvirt-down` only powers them off; `make libvirt-destroy` removes Terraform-owned
domains and networking, retaining disks and baselines. Keep the Terraform state
under the instance directory until destruction completes. Do not delete state to
work around a failed apply. `up` refuses to take over untracked domain/network names.

All mutating entry points take the same per-instance lock. Terraform intentionally
does not manage running/stopped state, so test power cycles and snapshot restores
do not need an apply. Run `make libvirt-down` before applying infrastructure
changes: replacing a network does not reconnect already-running guest TAPs.
Definition changes invalidate existing baselines; prepare new baselines after
applying infrastructure or node-system changes. Runtime
changes are node-system changes too: the device plugin runs an immutable Nix
binary, not a shared mutable `bin/roamvm` from the checkout.

### Run multiple instances of the same lab

`ROAMVM_LAB_SLOT` selects an instance (0 by default, 0–99 supported). Slot 0 uses
`.lab/libvirt`; other slots use `.lab/libvirt-N`. Domain names, subnets, bridges,
MACs, Terraform state, SSH keys, snapshots, kubeconfig and locks are slot-specific.
All instances share one libvirt daemon; libvirt allocates their VM sockets.
Select a free slot across all checkouts on the host. The fixed three-node topology
and test code are reused; there are no per-scenario lab definitions.

```sh
# Prepare once per instance (repeat for slot 2).
ROAMVM_LAB_SLOT=1 make libvirt-up libvirt-install libvirt-fixtures libvirt-freeze-warm
# Independent workers may run concurrently; each restores its own baseline.
ROAMVM_LAB_SLOT=1 make libvirt-scenario SCENARIO=lifecycle &
ROAMVM_LAB_SLOT=2 make libvirt-scenario SCENARIO=network &
wait
```

Sequential scenarios reuse an instance; destructive scenarios must not share a
running cluster concurrently. Budget 6 GiB assigned node RAM per instance, plus
host overhead, persistent snapshots and disk headroom. Prepared bundles retain
their slot identity and can be moved to another host using that same slot.

This is a trusted local test fixture, not a production cluster. It uses public
test credentials, password-disabled root SSH with a generated key, and the
private subnet `192.168.(124 + slot).0/24`. Reserve that subnet and the `rvm-labN`
bridge for the instance. VMs can read their lab directory and the host Nix store. Do not expose
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
running NixOS configurations against the current declaration, installs RoamVM, restarts the device
plugins, and pulls/unpacks the small guest shared by most scenarios on both
workers before checking service readiness. It also caches the curl probe on the
control plane and the provisioner's configured PVC helper image on both workers.
Image transfer and unpacking are paid at capture, rather than repeated after
every reset; a registry mirror alone would not eliminate unpacking. Large optional NixOS
fixtures stay in the registry until their scenarios pull them: preloading all of
them added roughly 6 GiB of worker image caches to the baseline. PVC creation and
guest boot still happen normally inside each scenario.
The declaration also supplies eight empty, node-affine local-path volumes per
worker. These avoid launching a provisioning Pod for each new VM. Consumed
volumes use the provisioner's normal deletion path and are never rebound with
old guest data; exhaustion falls back to dynamic provisioning. Reset restores
the empty capacity along with the cluster. This caches test storage preparation,
not guest boot, checkpoint upload, restore, or PVC binding.
Before capture it creates a stopped probe VM, waits for reconciliation, and
deletes it; a process health check alone does not prove the controller has
acquired leadership. It pauses all nodes before
saving their memory in parallel, retains read-only disks under `.lab/libvirt/warm`, and
creates disposable working overlays. Before capture it flushes/reclaims file
caches, compacts guest memory, and briefly lowers the free-page reporting
threshold so fragmented free blocks can be omitted too. It restores the normal
threshold and warms the live readiness path before saving. File-cache reclaim is
best-effort, does not swap anonymous memory, and does not kill services to meet
a size target. The five-second reporting wait is capture-only, never a reset
delay. Sparse mapped-RAM files and four parallel restore channels per node
avoid serial decompression. Existing cold disk baselines and fixtures remain
unchanged, including any backing files used by warm disks.

RAM images are stored on persistent disk under `.lab/libvirt/warm`, alongside
the captured disks and metadata. Capture flushes the filesystem before publishing
the ready marker. Allow several GiB of disk space for the sparse RAM files.
Tmpfs/ramfs-backed images are rejected before touching running nodes; the former
`ROAMVM_WARM_MEMORY_DIR` option is no longer supported. A missing image also
rejects reset; use `libvirt-reset-cold` to recover or recapture the baseline.
The host page cache may accelerate repeated restores, but it is not the source
of truth: snapshots survive a host reboot with no recapture required, provided
the pinned host configuration and shared files remain unchanged.

`libvirt-reset` and scenarios prefer the warm baseline when one exists. They
restore all RAM images paused, then resume the nodes together and check live
containerd/CNI, Kubernetes API, controller, MinIO and registry readiness.
There is no boot, build, image import or controller restart on this path. The
guest agent sets the restored wall clock from host time and runs the standalone
CRI client on every node. QEMU's host-clock RTC already advances while saved;
avoiding a redundant hardware-clock write reduces the readiness overhead.
The controller check calls its live readiness endpoint through the API proxy,
not the restored Kubernetes pod status.
The snapshot is tied to its host, QEMU/virtiofs configuration, runtime source and
mounted binary/key files. Changes are rejected **before discarding working
disks**, rather than silently testing stale processes. Host files shared through
virtiofs are not snapshotted; do not modify them during a capture or restore.

Use `make libvirt-reset-cold` to bypass a warm snapshot. To refresh it after code
changes, restore the cold baseline first, take the lab down, remove only
`.lab/libvirt/warm`, then run `make libvirt-up libvirt-install libvirt-fixtures
libvirt-freeze-warm` (omit `libvirt-fixtures` when not using optional fixtures).
Never remove a baseline while working disks still reference it. Without a cold
baseline, recreate the disposable lab instead. Old session-libvirt/XML-managed
labs are not adopted automatically: stop/remove those domains with the old
harness before provisioning this version. Their old RAM snapshots cannot be reused.

Cold startup waits for all three named nodes to exist and become Ready, and
installation waits for the storage/DNS deployments to be created. Waiting for
`nodes --all` alone can return before workers register on a fresh cluster.

`libvirt-freeze` creates a **cold disk snapshot**. It stops K3s,
MinIO and the registry, flushes the disks, and stops all three domains. It moves
their QCOW2 files into `.lab/libvirt/baseline` and marks them read-only; fresh
copy-on-write overlays avoid another full disk copy. Filesystems may replay their
journals on boot. Each reset restores all three disks together, including the
Kubernetes database, object store, registry, kubelet settings and local PVCs.
Captured domain definitions and NixOS store roots keep the node systems fixed.
Cold resets reinstall the runtime. A baseline's node-system definitions must
still match the Terraform-owned domains; reset never rolls infrastructure back.

Freeze after `make libvirt-up libvirt-install` and fixture publication have completed,
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

With gzip RAM snapshots, three full resets on that runner took **7.73, 7.74 and
8.06 seconds**. Persistent sparse RAM snapshots on ext4 initially reached a
1.985-second cached median. Reclaiming preparation caches/free pages before
capture reduced allocated RAM-image storage from **3.44 GB to 2.44 GB (29%)**
and cached full resets to a 1.806-second median. Reducing the workers to 2 GiB,
omitting metrics-server and lowering K3s's heap-growth target reduced the saved
images further to **2.16 GB** (another 11.5%). Cached full resets then took
**1.700, 1.809, 1.600, 1.733 and 1.550 seconds** (1.700-second median).
This includes stopping running domains, resetting disks, restoring all three
nodes, full source/binary fingerprint verification, clock correction and live
readiness checks. Node teardown/reset/restore operations overlap, but every
restored node remains paused until all restores succeed. Independent service
probes run concurrently after the guest clocks and CRI checks complete.

Evicting the saved-memory files with `POSIX_FADV_DONTNEED` before each reset
produced **2.097, 2.000 and 2.001 seconds**. `/proc/diskstats` recorded about
**2.16 GB read from vda per reset**. This tests reads beyond the runner's page
cache, not eviction of the underlying hypervisor/storage caches. The three
save files occupy about 2.01 GiB on disk; their logical lengths still reflect
the nodes' configured RAM. Preserve holes when copying them, for example with
`cp --sparse=always`. The cold QCOW2 backing files are not included in these
RAM-image sizes. These results meet sub-2 seconds in the five cached runs,
not disk-cold reads or a latency guarantee. CPU/process startup and
readiness overhead still exist; this is not an I/O-only latency claim.
The earlier `/dev/shm` results are superseded by these persistent measurements.
All timings require an already-captured snapshot matching the checkout;
building code and capturing/flushing a new baseline are separate preparation
steps. They measure lab reset, not startup of a guest VM inside the lab.

`make libvirt-reset` restores the preferred baseline without running a test.
**Reset discards all changes in the working lab**, including failed-test VMs.
A scenario leaves its working disks available for debugging until the next
reset; logs, revision, tracked diff and exit status remain under
`.lab/libvirt/runs`. The scenario loop stops on failure. Baseline/scenario
operations are locked against each other; do not run manual lab commands or
other tests concurrently.

For development, keep the installed cluster running between scenarios:

```sh
make libvirt-reset                     # Once, if the lab is not already running
make libvirt-dev SCENARIO=network
make libvirt-dev SCENARIO=lifecycle
```

`libvirt-dev` runs the same tests in an owned `<cluster>-dev` namespace. Before
each run it deletes that namespace's VMs, waits for their finalizers, then deletes
the namespace and waits for its PVs to be reclaimed. The local-path provisioner
removes each deleted claim's directory, not the worker's entire storage tree.
Other namespaces, image caches and object-store checkpoints remain untouched.
Consumed warm PVs are not rebound: after the pool is exhausted, ordinary
local-path provisioning creates fresh directories. Failed tests retain their
resources until the next development run; logs use the usual `runs` directory.

This mode requires healthy, running, uncordoned nodes. It refuses unowned
namespaces and non-`Delete` PVs, and never forces finalizers or deletes PVs
directly. If cleanup cannot finish, inspect the retained lab or use
`make libvirt-reset` to discard its working state. It is not a pristine reset:
cluster-wide mutations or arbitrary host writes need the normal reset path.
Scenario fault injection still stops/restarts components where the test requires
it. The same slot lock prevents concurrent runs on one lab; use separate
`ROAMVM_LAB_SLOT` values for parallel labs.

Development runs use the currently installed runtime and live fixture references;
they do not rebuild or deploy changes. Run `make libvirt-install` after runtime
code edits. Node OS/configuration changes still require normal reprovisioning and
a new baseline. `libvirt-scenario` and `libvirt-scenarios` keep their existing
fresh-reset behavior and remain the isolation checks for CI.

Keep an installed baseline at its original path: overlays contain absolute
backing paths. Use the export/import workflow below to move it. Freezing refuses
to overwrite an existing baseline. To change the node OS or fixture set, take
the lab down, discard the disposable `.lab/libvirt` directory, and prepare a new
lab and baseline. Only scripts and scenario definitions belong in Git; generated
VM disks and run logs do not.

### Scenario execution time

The test images use `console=ttyS0,115200 quiet` for direct-kernel and GRUB boots,
including switched and rolled-back generations. This avoids sending verbose
boot output through the nested emulated UART; errors remain on the console and
the full kernel log remains in the guest ring buffer. Production guest images
and runtime defaults are unchanged. Readiness-only HTTP connections time out
after one second and retry under the existing 180-second readiness deadline;
ordinary requests, including the 10-second CPU loads, retain their old timeout.
Waits lasting at least one second are logged by the test helper.
After a worker power cycle, the lifecycle test requires a new kubelet heartbeat
and positive healthy KVM capacity before uncordoning it; cached Node Ready
status alone can admit a VM before the device plugin has re-registered.
Corrupt-checkpoint refusal checks the runner's hypervisor-start marker as well
as the kernel banner, so quiet console output cannot conceal an attempted boot.

All six race-enabled scenarios passed serially on the same 6-vCPU runner:

| Scenario | Original run | Quiet boots/readiness | Cache/growth/recovery fixes |
| --- | ---: | ---: | ---: |
| Crash recovery | 2m52s | 2m25s | 2m28s |
| Lifecycle | 4m11s | 3m17s | 3m48s |
| Networking | 1m20s | 1m07s | 1m12s |
| CPU oversubscription | 5m45s | 4m41s | 4m11s |
| Disk resize | 5m36s | 3m00s | 2m42s |
| NixOS generations | 6m32s | 6m07s | 6m02s |
| Total Go package execution | 26m17s | 20m37s | 20m24s |

The latest full `make libvirt-scenarios` command took **20m44s**, versus 20m57s
after quiet boots/readiness changes: only about 1% less overall. Both include
resets and Go invocation overhead, but exclude fixture builds, transfers and
baseline capture. The first iteration reduced package execution about 22%.
The latest run saves time in resize and CPU scenarios but pays a real 35-second
worker-rejoin wait that the old lifecycle test could incorrectly skip. The three
held-service filesystem expansions took 2.69s, 2.89s and 1.81s; a previous lost
notification had taken 32.9s. No fixed CPU-load duration or fault was removed.
A fresh generation-only before/after comparison took 7m03s and 6m02s (14% less).
These are individual runs, not latency guarantees: image caches, provisioning
and the guest resize retry timer contribute variation, particularly to resize.
No scenarios, assertions, durability checks or CPU-load durations were removed.
Rebuild/publish the fixtures and recapture the baseline to adopt these settings;
an older frozen baseline still references the old guest image digests.

The next iteration preloads the shared small guest on both workers and fixes
online growth notifications arriving while the growth service is already active.
The growth module queues a marker for another pass, rather than waiting for the
unchanged 30-second fallback timer. The udev notification helper records each
device's last observed capacity: even `growpart` returning `NOCHANGE` can generate
another udev event, so repeating it for unchanged capacity would form a feedback
loop. Filtering notifications independently of growth success prevents that loop
on failures too. The timer still retries failures at unchanged capacity, and a
concurrent expansion queues another pass.
The module disables service-level start limiting, which can permanently fail
the path watcher during a burst; the path unit keeps its own trigger limit.
The resize test disables the timer, deliberately holds the service across each
disk change, and checks three successive expansions, the live watcher, absence
of repeated growth passes, unchanged guest identity and data after cross-node
restore.
The old module failed the held-service test, and the queued version with the
default service start limit failed the watcher check on repeated growth.
With start limiting disabled but without the capacity guard, the idle-pass
regression also failed: the service ran another two passes in two seconds.
After moving capacity filtering into udev, the resize scenario passed again in
2m53s, with expansions taking 2.58s, 2.60s and 3.08s. The lifecycle scenario also
passed again with the stricter no-hypervisor-start corruption assertion. These
follow-up runs are separate from the full-suite timing above.

Three independent fresh-reset startup samples with preloading took **22.01,
23.01 and 21.21 seconds**, versus **23.47, 22.23 and 24.50 seconds** before it
(median 22.01s versus 23.47s, about 6% less). Each sample creates a fresh VM/PVC
and measures through its first HTTP response; these are not checkpoint-restart
or lab-reset times. A further reset/start with the registry stopped passed in
22.01s, proving the worker could use the cached fixture without contacting it.
These compare prepared baselines on this runner, not a production-storage
benchmark or an isolated estimate of network transfer time.

### Transfer a prepared lab to another checkout or host

The **disk baseline is portable; the RAM baseline is host-local**. Export flattens
and compresses the frozen QCOW2 backing chains into three self-contained disks,
and includes the node NixOS closures as a relocatable Nix binary cache. Registry
fixtures, containerd images, Kubernetes state and MinIO objects are already on
those disks. Export prefers the warm baseline's frozen disks when available;
it does not copy running working disks or change the running lab. Disks from a
warm snapshot recover as after a power loss on their first cold boot. Legacy
9p baselines must be recreated with the current virtiofs configuration first.

On the producing host, inside `nix develop`:

```sh
make libvirt-export BUNDLE=/path/to/prepared-lab
# Copy this whole directory, or archive it for your trusted artifact cache:
tar -C /path/to -cf prepared-lab.tar prepared-lab
```

On a receiving Linux x86-64 host with the prerequisites above, use a fresh
checkout with the same `flake.lock`, enter `nix develop`, then:

```sh
make libvirt-import BUNDLE=/path/to/prepared-lab
make libvirt-up libvirt-install  # Terraform creates new identities and boots the disks
make libvirt-freeze-warm         # Capture a host-local RAM baseline once
make libvirt-scenario SCENARIO=network
```

Import checks SHA-256 hashes, disk structure, absence of external backing files
and the Nix pin before creating the lab. Use the producer's `ROAMVM_LAB_SLOT`.
It refuses an existing instance directory
or any lab domain already defined on that host. It imports/roots the bundled OS
closures; the first Terraform apply generates domain definitions for the new
host and checks that the declared node systems match the bundled systems.
The first boot generates a new client SSH key and reads kubeconfig
from the cluster. No source-host paths, client keys or RAM files are required.
The source revision and tracked diff are included for provenance; cold setup
requires matching node systems rather than silently testing stale source.
Normal Go/Nix dependency caches are still needed to avoid rebuilds and
downloads; the bundle is not a fully offline development environment.

Only accept bundles from trusted producers. They contain executable VM disks,
cluster certificates, host SSH keys and private cluster state. Checksums detect
corruption, not malicious producers; the local Nix cache is unsigned, so import
uses `--no-check-sigs` while Nix still checks content hashes. Do not publish these
bundles publicly. Each concurrent instance reserves its own subnet and domain names.

The first cold boot/install and local RAM capture are preparation costs, **not
the approximately two-second reset**. Subsequent local scenarios use the same
fast RAM-restore path. RAM files themselves cannot safely be moved across hosts:
they contain host-passthrough CPU state and live virtiofs references to host files.

The portability round trip was tested in a fresh checkout at a different path
on the same runner: all three flattened disks compared identical to their backing
chains, and the bundled cache contained all 668 OS-closure paths. Import took
18.2 seconds with those paths already in the host store; cold boot/install took
67.9 seconds and local RAM capture 45.6 seconds. The Kubernetes scenario then
passed after RAM restore, including NetworkPolicy deny/allow enforcement.
The full bundle with optional fixtures occupied about 6.3 GiB. These are not
cross-hardware or empty-Nix-store timings; a second physical host was not tested.

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

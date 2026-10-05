# RoamVM

This branch is the [QEMU / online resizing spike](QEMU-SPIKE.md).

A Kubernetes VM runtime for development machines. The root disk follows compute:
OCI stores the immutable base, S3 stores verified checkpoints, and a retained
node-local PVC holds the working copy and enables fast same-node restarts.

This is an experimental implementation with real KVM integration tests, not a
production-qualified replacement for KubeVirt. It is independent of Coder.

```text
VirtualMachine → controller → runner Pod → kube-scheduler
                                  │
                      runtime sidecar prepares disks
                         OCI base + S3 checkpoint
                                  │
                         QEMU/KVM inside Pod
                                  │
                    graceful shutdown → Stopped
                                  │
                     background worker → S3 commit
```

The hypervisor runs **inside the runner container's cgroup and network namespace**.
The daemon prepares storage; it does not put the VMM outside Kubernetes resource
accounting. A small independent Pod uploads stopped disks. Restarts first try the
cached node; other placement waits for the latest verified checkpoint.

## Start and stop

Install the runtime and publish an image as described below. Put the returned
image digest in [examples/vm.yaml](examples/vm.yaml), then:

```sh
kubectl apply -f examples/vm.yaml
roamvm start devbox
kubectl wait rvm/devbox --for=condition=Ready --timeout=180s
kubectl get rvm
kubectl port-forward service/devbox 2222:22
# In another terminal, using credentials configured in your image:
ssh -p 2222 localhost

roamvm stop devbox
kubectl wait rvm/devbox --for=condition=Stopped --timeout=600s
```

`start`/`stop` patch `spec.powerState`; tools can use the Kubernetes API directly.
A local runner or hypervisor crash is checkpointed after Kubernetes confirms the
runner has terminated. The VM stops instead of rebooting in a loop; its next start
restores the crash-consistent disk. A filesystem may replay its journal, and
unflushed application data is not guaranteed. If the disk cannot be validated or
uploaded, it stays retained with `CheckpointReady=False`; there is no fallback to an
older checkpoint. See [recovery](docs/recovery.md) for existing failed Pods.

A stopped VM retains the entire changed root disk, including packages, project
files and machine credentials. RAM and running processes are not retained.

`Stopped` means the guest has exited and the local disk has been flushed. Upload,
verification and cleanup run independently; `CheckpointReady=True` means the
latest stopped state is verified in S3. Until then, losing the node can lose the
latest changes. A same-node restart cancels an unfinished upload before making
the disk writable and defers backup to the next stop. It never downloads the
cached disk. Ownership metadata must still be available (S3 or Kubernetes).

For a planned move, stop and wait for `CheckpointReady`, change placement constraints or
cordon the old node, and start again. The default scheduler selects the destination.
Use Services for stable identity; the Pod IP can change. A normal `kubectl delete
rvm` also waits for a checkpoint, retaining the latest stopped state for recovery.

Each successful stop replaces the previous checkpoint. The runtime uploads and
verifies a temporary replacement, commits it as current, then deletes superseded
checkpoints before reporting `CheckpointReady`. There is no stop-history or rollback
catalogue. Old and new objects coexist only while replacement/cleanup is pending.

## Install

Requirements: Linux x86-64 KVM nodes, `/dev/kvm`, `/dev/net/tun`, Kubernetes 1.35+
with image-volume support (tested with containerd 2.2), an IPv4 CNI with interface
`eth0`, and a local storage class using `WaitForFirstConsumer` and `Delete`.
Set the controller's `WORKING_STORAGE_CLASS` and `WORKING_STORAGE_SIZE` (64Gi by
default). This is a minimum: with `rootDiskSize` set, the reservation is at least
one disk capacity plus 1 GiB for filesystem/QCOW2 metadata. Checkpoints stream
directly from the stopped overlay to S3 without a second local copy; the base is
separate. The working PVC belongs to the VM and survives runner deletion. A local
restart reuses it; durable remote fallback allocates a fresh claim and removes
the old cache. VM deletion waits for durability before releasing retained storage.

The runtime uses per-container `OnFailure` restart policy (the
`ContainerRestartRules` feature, enabled by default in Kubernetes 1.35+). It stays
alive after the runner exits to record the local stop. A separate `checkpoint-worker`
Pod retries uploads independently and releases guest CPU/RAM reservations.

The immutable base uses a read-only Kubernetes `image` volume. Kubelet/containerd
handle pulling, authentication, caching and garbage collection. The runtime socket
uses an `emptyDir` shared only inside the VM Pod. RoamVM defines no hostPath volumes.
Install `roamvm device-plugin` as a node OS service (example:
`config/roamvm-device-plugin.service`). It registers KVM/TUN with kubelet; the
hypervisor and storage runtime remain in the Pod. Dialo's NixOS module manages
this service declaratively. A host-installed hypervisor is not required.

To upgrade the earlier hostPath prototype, first stop every VM and wait for its
durable `Stopped` condition. Remove the old daemon DaemonSet before starting the
node device-plugin service, provide runtime credentials in each VM namespace,
then update the controller. Existing object-store checkpoints remain compatible.

Allow these **namespaced** sysctls in kubelet configuration on VM nodes:

```yaml
allowedUnsafeSysctls:
- net.ipv4.ip_forward
- net.ipv4.conf.all.route_localnet
```

The first enables guest forwarding. The second lets Kubernetes port forwarding
reach the guest through the Pod loopback address. Pods keep CNI's original IP,
interface and routes. Guest virtio-net uses DHCP, TAP, and NAT inside the Pod. Its private transit
subnet is `192.168.127.0/30`; do not use those addresses for external dependencies.
The runtime grants NET_ADMIN/NET_RAW to the runner; it is not a privileged Pod.
Namespaces admitting VMs must allow these capabilities and sysctls.

Build with Go 1.26.7 (`make build`) and publish `make image IMAGE=YOUR_REGISTRY/roamvm:VERSION`.
In `config/install.yaml`, replace the controller image
and its `RUNNER_IMAGE` value with the same published digest.

Create a dedicated S3 bucket. With the default `STATE_BACKEND=s3`, the store must implement strongly consistent reads,
conditional `PutObject`, and conditional `CompleteMultipartUpload` with `If-Match`
and `If-None-Match`. Checkpoint workers probe basic conditional semantics on startup;
`TEST_S3_ENDPOINT=... TEST_S3_BUCKET=... go test ./internal/state -run TestS3 -count=1`
also checks multipart behavior against your backend.

For object stores without conditional writes (including Garage 2.3.0), set
`STATE_BACKEND=kubernetes`. Ownership and the current checkpoint pointer then
use a retained ConfigMap in `STATE_NAMESPACE` (default `roamvm-system`); only
immutable disk objects go to S3. Back up these ConfigMaps along with the bucket.
Checkpoint workers verify object upload/readback at startup. Configure every daemon
and recovery command with the same backend; changing it requires an offline
metadata migration, not an environment-variable rollout.

```sh
kubectl apply --server-side -f config/crd
kubectl create namespace roamvm-system
# Create default/roamvm-object-store through Vault or your usual secret manager.
# Required: S3_BUCKET, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY.
# Optional: S3_ENDPOINT (S3-compatible URL), AWS_REGION, AWS_SESSION_TOKEN.
# With AWS workload identity, use the SDK credential chain instead of static keys.
kubectl apply -f config/install.yaml
```

The example provisions the runtime ServiceAccount and namespace-scoped RoleBinding
in `default`. For another VM namespace, create the same `roamvm-runtime` account
and RoleBinding there, authorize it in the state RoleBinding in `roamvm-system`,
and provide `roamvm-object-store`. Optional non-secret settings can live in that
namespace's `roamvm-runtime` ConfigMap. The runtime sidecar alone receives the
object-store environment and projected Kubernetes token.

The SDK uses normal HTTPS validation. Configure registry authentication through
VM `imagePullSecrets`; local plaintext registries use containerd's registry config. Runtime AWS permissions cover GetObject/PutObject under `vm/` and
`runtime-probes/`, multipart upload/abort, ListBucket scoped to `vm/*/overlay/`,
and DeleteObject on overlay objects and `runtime-probes/`. Versioned buckets also
require DeleteObjectVersion: superseded checkpoint versions are physically removed,
not hidden behind delete markers. Keep this bucket
private and enable your usual encryption/access controls; overlays contain the
VM's private files. S3 bucket versioning is supported but not required: checkpoint
keys are unique during replacement, and head replacement uses ETag compare-and-swap.
Bucket policies must permit cleanup; otherwise the Pod stays `Checkpointing`
and retries for legacy ephemeral runners. Retained-disk workers retry with
`CheckpointReady=False` while the VM remains stopped. The next successful upload
also removes history from older releases. Configure S3 lifecycle cleanup for
abandoned multipart uploads in case a worker is forcibly killed during cancellation.

The controller and daemon are trusted cluster components. VM creation is comparable
to Pod creation: it can reference namespace-local Secrets and PVCs. Do not grant
untrusted users Pod creation/exec/mutation or runtime Secret access in VM namespaces,
or access to the runtime S3 prefix. The runtime accepts requests only through its
mode-0600 Unix socket, mounted into its own Pod's two containers. It selects the
Pod from its downward API identity and verifies the Pod UID, assigned node and VM
owner on every request; callers cannot select another VM. Guest configuration rejects
token and Pod-certificate projections and the reserved `roamvm-object-store` Secret.
The runner container has neither the Kubernetes token nor the object-store environment.
New Pods need no per-runner authentication Secret. The runtime Role retains
Secret reads for older Pods until their next cold start. Their existing token
Secrets remain owned by the VM and are garbage-collected when that VM is deleted.

## Package a NixOS or other Linux image

An ordinary OCI image layer contains files, **not** a container rootfs to execute:

```text
disk/
  manifest.json       {"format":"qcow2","cmdline":"console=ttyS0 ..."}
  root.qcow2          standalone immutable disk; root.raw also supported
  vmlinux             Linux bzImage or ELF kernel, for direct boot
  initrd              optional initramfs
```

Alternatively put `firmware` in the directory instead of `vmlinux`, with a guest
bootloader on the disk. Direct Linux boot and SeaBIOS/GRUB disk boot are covered
by the real-KVM tests; UEFI is not.

`spec.bootMode: Disk` boots the root disk's bootloader even when the original
image contains `vmlinux`. It uses the image's `firmware` when present, otherwise
QEMU's default BIOS. This lets an existing BIOS-bootable NixOS workspace use its
selected system generation after `nixos-rebuild switch`, without replacing its
immutable base or checkpoints. Stop the VM, set `bootMode: Disk`, then start it.
The original disk must already contain a working BIOS bootloader; this option
does not install one. Omitted `bootMode` (or `Image`) preserves image-selected
booting, with `vmlinux` taking precedence over `firmware`.

For disk boot, `spec.hostname` is supplied through the standard systemd
`system.hostname` SMBIOS credential. Leave NixOS `networking.hostName = ""` to
accept the runtime hostname. Other distributions must support that credential
or configure their hostname themselves.
The image needs virtio PCI/block/net support, DHCP, ACPI power-button shutdown,
and the service named by `readinessPort`. There is no required guest agent.
NixOS images should have guest firewall/SSH/authentication configured intentionally.

Copy files **dereferencing Nix store symlinks**, preserve the Nix image's kernel
command line, and package them:

```sh
# Recommended immutable base for the QEMU runner:
qemu-img convert -f raw -O qcow2 -c -o compression_type=zstd \
  /path/to/root.raw disk/root.qcow2
qemu-img compare -f raw -F qcow2 /path/to/root.raw disk/root.qcow2
# Supply disk/manifest.json, vmlinux and initrd from the same NixOS build.
# Copy the kernel's bzImage directly to disk/vmlinux; no extraction is needed.
tar -C . -czf vm-image.tar.gz disk
roamvm image-push --tag registry.example.com/vm-images/devbox:BUILD \
  --tar vm-image.tar.gz
```

The push command uses standard Docker credentials and prints the immutable digest.
Private pulls use `spec.imagePullSecrets` in the VM namespace. A base with its own
backing dependency is rejected. The base digest cannot change on an existing VM:
create a new VM to change the base, or install packages into its current overlay.

QCOW2 keeps unallocated disk space out of the OCI payload and the node's unpacked
image. Gzip around a raw disk saves transfer bytes, but an ordinary OCI layer
can still unpack to the disk's full virtual capacity. Zstd-compressed QCOW2 also
reduces the cached base's size; QEMU decompresses clusters as they are read.
Uncompressed QCOW2 avoids that read CPU cost at the expense of more node storage.
Raw, uncompressed QCOW2 and zlib-compressed QCOW2 remain supported. The writable
overlay stays uncompressed, and its checkpoint format does not change. This
recommendation requires the QEMU runner; do not publish Zstd bases to the older
Cloud Hypervisor deployment.

## Kubernetes integration

- `cpus` defines guest vCPUs, independently of `resources.requests.cpu` (scheduler
  reservation and CPU weight) and `resources.limits.cpu` (optional CPU ceiling).
  Requests default to the vCPU count. Set a lower request to oversubscribe CPUs;
  the sum of guest vCPUs/limits may exceed the node's CPUs. Requests still have
  to fit. KVM slots are shared access tokens, not dedicated physical CPUs.
  Memory reserves guest RAM plus 512 MiB and an additional 1/32 of ordinary guest
  RAM for host overhead. Hugepage guests reserve the 512 MiB overhead separately.
  The runtime sidecar additionally requests 100m CPU and 128 MiB RAM, with a
  512 MiB memory limit. Adjust upward if your workload/devices need more host memory.
- Affinity, node selectors, tolerations and topology spread pass to the runner Pod.
  Each incarnation captures its boot configuration when the Pod is created;
  later spec edits apply at the next start, keeping boot RAM/CPU and reservations
  consistent even while the Pod is queued.
- Services, DNS, port-forwarding and CNI policy operate on the Pod endpoint.
- `config` accepts Kubernetes projected volume sources, converted to a read-only
  ISO labelled `ROAMVM_CONFIG`. Your guest mounts/consumes it. Changes appear next
  boot. `configDisks` provides multiple separately labelled projected ISOs, for
  guests that already consume bootstrap disks. The runtime does not mutate the
  guest's root filesystem to inject settings.
- `guestServiceAccountToken: {name: guest-account, audience: external-service}`
  selects an existing ServiceAccount in the VM's namespace. Grant the runtime
  `create` on `serviceaccounts/token`, restricted by `resourceNames` to that
  exact account. RoamVM does not grant this permission automatically, and rejects
  selecting `roamvm-runtime` itself. The runtime requests a one-hour token before
  boot and refreshes at 80% of the returned lifetime. Failed requests retry once
  per minute, preserve a still-valid token, and remove it at expiry. Token refresh
  failure does not stop an already-running guest or block graceful shutdown.
  A runtime restart reacquires the token. The token is unbound to a Pod because
  the guest and runtime ServiceAccounts differ; offline OIDC verifiers do not
  observe object deletion before JWT expiry.
  Mount `roamvm-token` as a read-only 9p filesystem with
  `trans=virtio,version=9p2000.L,cache=none,ro`; read its `token` file afresh when
  renewing external credentials. Only this Pod-local memory directory is shared,
  not the runtime Kubernetes credentials, socket or storage. The token is readable
  by guest users; the VM, not each guest process, is the isolation boundary.
  TokenRequest uses the host's Kubernetes credentials, so the guest needs no
  Kubernetes credentials or API connectivity. The shared runtime identity can
  request tokens for every account explicitly authorized through additive RBAC.
- Secondary PVCs use native Kubernetes attachment/mounting. Block PVCs are exposed
  as raw virtio disks; filesystem PVCs must contain `disk.img`. Their own storage
  topology/access-mode restrictions still apply. Only the root is portable via S3.
- Hugepage accounting (`hugepages: 2Mi` or `1Gi`) and VFIO device-plugin plumbing are
  implemented. `devices` requires resource limits and an allocated PCI address;
  ordinary CUDA-container GPU plugins are insufficient. These paths have **not**
  been exercised with physical GPUs/hugepages and need hardware qualification.
- Controller metrics are on port 8080, health probes on 8081. Pod/cgroup resource
  usage uses ordinary Kubernetes metrics. There are no guest OS metrics yet.

See [configuration fields](api/v1alpha1/types.go), [state/failure model](docs/design.md),
and [local testing](docs/testing.md).

For example, this guest sees four vCPUs while Kubernetes reserves half a CPU.
It can burst up to four CPUs when capacity is available; contending VMs share
host CPU time through their runner cgroups. Omitting the CPU limit removes that
quota without changing the guest's vCPU count.

```yaml
spec:
  cpus: 4
  memory: 4Gi
  resources:
    requests:
      cpu: 500m
    limits:
      cpu: "4"
```

The runner reserves and limits 4288 MiB of RAM for this example. CPU sharing does
not make guest RAM shareable. Hugepages and passed-through devices retain native
Kubernetes reservations.

## Current limits

IPv4 and x86-64 only; no live migration, automatic lost-node takeover, memory snapshots,
online disk resizing, image upgrades, or periodic checkpoints. The latest checkpoint
outlives VM deletion; deleting that final recovery copy is an operator decision.
Never delete the current checkpoint or a base used by
a running VM. Do not apply age-only S3 expiration to the entire overlay prefix.

Stopping waits for guest shutdown and local flush, not S3. Background checkpointing
validates, uploads and verifies the **entire current overlay**, not only changes
since the previous stop. Each generation
is independently usable against its pinned base. The overlay is not compacted,
so unused QCOW2 space can increase upload size. Running-node failure can lose
all changes since the last committed stop, as intended by this model.

# QEMU / online root growth spike

This branch replaces Cloud Hypervisor with QEMU/KVM. It is a local spike, not a
production rollout. The VM API, scheduler placement, networking, generic ephemeral
working PVC, OCI base and S3 checkpoint model are retained. Existing Cloud
Hypervisor QCOW2 checkpoints restore directly.

Increase a running VM's minimum root capacity:

```sh
kubectl patch rvm devbox --type=merge -p '{"spec":{"rootDiskSize":"64Gi"}}'
kubectl wait rvm/devbox --for=jsonpath='{.status.rootDiskSize}'=68719476736 --timeout=120s
kubectl get rvm devbox -o jsonpath='{.status.rootDiskSize}'
```

`rootDiskSize` is a Kubernetes quantity; omitting it preserves the image/checkpoint
capacity. Explicit sizes can only increase. `status.rootDiskSize` reports bytes;
`DiskReady` describes the virtual block device, not the guest filesystem.

The controller expands the running Pod's working PVC when necessary. It requests
at least the configured `WORKING_STORAGE_SIZE` (64Gi by default) or twice the
requested root size plus 1Gi, whichever is larger. This leaves room for the live
QCOW2 overlay and its compacted checkpoint. QEMU's `block_resize` runs only after
PVC capacity is sufficient and Kubernetes reports no pending filesystem expansion.
An expansion failure reports `DiskReady=False`; the guest keeps running. The
StorageClass must permit and implement online expansion when the PVC needs to grow.

QMP calls are idempotent on retry, never shrink, and operate on the explicit `root`
block node. The base remains read-only. QEMU starts with `-no-shutdown`: only an
observed guest shutdown followed by QMP `quit` permits checkpoint publication.
SIGTERM (which QEMU can treat as a successful process exit) is not a durable stop.

## Guest filesystem

Import the optional NixOS module into the image:

```nix
imports = [ inputs.roamvm.nixosModules.online-grow ];
```

It uses udev, a systemd oneshot, `growpart` and `resize2fs` / `xfs_growfs`. There is
no custom guest agent. It supports a plain virtio root disk, with or without a
partition, using ext4 or XFS. The root partition must have free space after it;
encrypted disks, LVM, Btrfs and arbitrary layouts need their own guest-side policy.
The module also runs at boot, so stopped/restarted guests catch up with capacity.
A 30-second timer retries changes coalesced while a growth job was already running.
Only partitioned ext4 is exercised by the NixOS integration fixture in this spike.

## Reproduce

The ordinary Compose + kind + Go test workflow in [docs/testing.md](docs/testing.md) still applies.
The unit suite now exercises QMP against a real, paused QEMU using TCG, so it does
not require nested KVM. The Kubernetes tests use actual KVM guests.

```sh
make fmt-check test generate
make image lab-install
make lab-guest
make lab-nixos-guest
source test/lab/env
export ROAMVM_TEST_RESIZE_IMAGE="$(cat .lab/nixos-guest-ref)"
ROAMVM_TEST_NODE_FAILURE=1 make integration
make benchmark
```

`TestOnlineResize` grows a 1.5Gi partitioned NixOS root twice, verifies unchanged
boot/process identity, allocates more blocks than the original filesystem could
hold, writes and fsyncs a random marker, then stops, cordons the original node and
restores on the other worker. It verifies capacity, allocation and marker bytes.
The lifecycle suite checks both SIGKILL and SIGTERM, sidecar restart, S3 outage,
node loss with explicit fencing, corruption rejection and single-checkpoint retention.

The local kind StorageClass does not implement expansion. Live root growth is
exercised within an already sufficient working PVC; rejection by a non-expandable
class is tested against Kubernetes, and expansion/capacity gating has controller
and daemon unit tests. A successful CSI-backed PVC expansion must still be checked
against the target cluster's driver before production adoption. VFIO/hugepages,
firmware-only images and XFS also need hardware/image-specific validation.

No Coder template or production deployment is switched by this spike. Exposing
`rootDiskSize` in Coder is a follow-up once the runtime and storage-driver checks
are accepted.

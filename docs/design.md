# Design and failure semantics

## Choice of runtime

We inspected [Virtink](https://github.com/smartxworks/virtink) at commit
`a0cbe18c496b8634163916c28d94e038df489690`. Its controller → Pod → node daemon
architecture fits. Its existing disk/lifecycle APIs would still need substantial
changes for a stopped-state object store, so RoamVM is a fresh implementation of
that architecture, without copied Virtink code or a compatibility layer.

Cloud Hypervisor [v53.0](https://github.com/cloud-hypervisor/cloud-hypervisor/releases/tag/v53.0)
has native QCOW2 backing-file support. This avoids a separate NBD/ublk storage daemon.
The pinned binary checksum is in the Dockerfile. Explicit `image_type` and
`backing_files=on`, standalone-base validation and backing-path replacement follow
its [QCOW2 hardening guidance](https://github.com/cloud-hypervisor/cloud-hypervisor/security/advisories/GHSA-jmr4-g2hv-mjj6).

## Components and trust

- Controller: creates one named runner incarnation, mirrors status, requests stop,
  and removes resources after durable completion. It has no S3 credentials.
- Node daemon: resolves OCI credentials in the VM namespace, caches bases,
  authorizes the runner by token/Pod UID/node/VM ownership, and manages checkpoints.
- Runner: holds a local file lock, sets up TAP/NAT/DHCP, runs the hypervisor inside
  its Pod, sends heartbeats and reports guest readiness. It cannot access the S3
  credentials or other VMs' writable disks through its normal mounts.
- Device plugin: allocates shared KVM/TUN access using native extended resources.
  KVM slots only advertise healthy while the local daemon socket is reachable.

The daemon and Kubernetes administrators are trusted. A runner receives read-only
access to the node's base cache, so use a dedicated trusted cluster/node pool when
base-image confidentiality matters. There is no hardened multi-tenant isolation
claim. The VM API does not expose host command execution or arbitrary host paths.

## Durable state

`vm/<VM UID>/head.json` is authoritative. Kubernetes status is a projection. The
head identifies the base digest, owner Pod UID, node, epoch and current checkpoint.
Each acquisition uses S3 `If-Match` compare-and-swap; initial creation uses
`If-None-Match: *`. There is no expiring lease that permits a second writer.

Checkpoints are immutable objects named by generation, epoch and SHA-256. The
runtime uploads the compact QCOW2 overlay, reads it back and verifies its full
hash and size, then conditionally replaces the head. Only that last write commits
Stopped. A lost response is reconciled by reading the head. Multipart completion
also uses conditional headers. These operations rely on the backend's
[conditional write semantics](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html).

Restore checks size, hash and object version, rewrites the backing reference to
the locally cached pinned base, validates QCOW2 metadata, and then starts the VMM.
Incoming embedded backing paths are never trusted. New checkpoints contain the
full current delta against the base; they do not depend on previous generations.

Local layout:

```text
/var/lib/roamvm/
  images/<digest>/          disposable, shared immutable base cache
  running/<VM UID>/         overlay, compact checkpoint, VMM socket and lock
  sessions/<Pod UID>.json   daemon-only reconciliation metadata
```

The local disk is not a PV. Successful commit removes its writable disk files.
Failed uploads retain the working disk and runner. Interruption before commit
must not erase local files; the node may still hold the only newest copy.

## Failures and recovery

| Failure | Behavior |
| --- | --- |
| Missing OCI base | Pull on the chosen node; no scheduling dependency |
| Duplicate start | One runner name plus conditional S3 ownership; second owner rejected |
| S3 unavailable during stop | Guest exits, Pod stays Checkpointing, local state retained, upload retried |
| Daemon restarts | Same runner/epoch resumes using saved metadata |
| Control contact lost for 30 seconds | Runner requests guest shutdown; ownership stays held |
| Guest ignores ACPI timeout | VMM killed, no new checkpoint, RecoveryRequired |
| Hypervisor crashes / OOM / node disappears | No automatic takeover; last committed generation remains valid |
| Corrupt downloaded checkpoint | Hash failure before VMM launch |
| Stale owner uploads after recovery | Head CAS fails; it cannot publish a new current generation |
| Unscheduled Pod cancelled | Resource-version checked deletion, no ownership acquired |
| Graceful VM deletion | Finalizer waits for checkpoint; object retention is separate |

Deleting a Pod starts its Kubernetes termination grace period (one hour in the
initial configuration). Finalizers do **not** prevent kubelet killing containers
when that grace period expires. Prefer setting VM powerState to Stopped before
maintenance; wait for Stopped before draining/shutting down the node. Forced
Pod deletion, eviction or node loss follows the stated crash model.

If recovery is necessary, first prove the old VMM has exited, or fence/power off
its old node. Kubernetes NotReady and timeouts are not proof. Record the VM UID
and old Pod UID, then run the operator command with S3 and Kubernetes credentials:

```sh
roamvm recover --vm-id VM_UID --owner OLD_POD_UID --fenced
roamvm start --namespace NAMESPACE VM_NAME
```

This is an explicit decision to discard uncommitted changes and restore the last
checkpoint. It never runs automatically. It first requests Stopped, removes the
old incarnation, advances the S3 epoch, and resets Kubernetes status. Retained
local failed disks are not automatically adopted; preserve them for investigation
before recovery if the uncommitted work matters.

## Deliberate boundaries

No distributed storage, continuous replication, root CSI/PVs, local disk inventory
service, live migration, VM pool or memory hibernation. Coder can create/patch a
VirtualMachine and use its Service like any other client. There is no Coder API
or guest-agent dependency in this repository.

# Design and failure semantics

## Choice of runtime

We inspected [Virtink](https://github.com/smartxworks/virtink) at commit
`a0cbe18c496b8634163916c28d94e038df489690`. Its controller → Pod → node daemon
architecture was the starting point. RoamVM uses a runtime sidecar with each VM
Pod so it can use PVCs and image volumes without hostPath mounts. No Virtink code
or compatibility layer is included.

QEMU runs guests with KVM acceleration and a QCOW2 backing chain. The runner
provides the validated base and working overlay explicitly through `-blockdev`;
QMP controls shutdown and online root-disk growth.

## Components and trust

- Controller: creates one named runner incarnation, mirrors status, requests stop,
  retains its local PVC and schedules independent checkpoint workers. It has no S3 credentials.
- Runtime sidecar: serves a private Pod-local Unix socket, verifies its downward API
  Pod identity and VM ownership,
  validates the mounted base disk, restores or resumes local state and records local stops.
- Checkpoint worker: locks the stopped disk, uploads and verifies it, commits the
  durable checkpoint and prunes old objects. It is independently restartable.
- Runner: holds a local file lock, sets up TAP/NAT/DHCP, runs the hypervisor,
  sends heartbeats and reports guest readiness. It cannot access the sidecar's
  Kubernetes token or S3 environment through its mounts.
- Device plugin: a node OS service allocates shared KVM/TUN access using native
  extended resources. It requires no Kubernetes hostPath volume.

Kubelet owns OCI pulling, registry credentials, caching and read-only image-volume
mounts. Initial placement uses a VM-owned local PVC with WaitForFirstConsumer.
The controller retains it after stop and tries it first on restart. The runtime
is a regular container with per-container `OnFailure` restart policy.
It exits after observing the runner exit and persisting a local stop.
A runtime failure restarts that container with the same Pod/PVC and
ownership epoch; the runner has `Never` restart policy.

The runtime and Kubernetes administrators are trusted. VM configuration cannot
project the reserved runtime Secret, service account tokens or Pod certificates. Pod creation,
exec or modification in a VM namespace is privileged access to runtime credentials;
VM-only users should receive VM-resource permissions, not those Pod permissions.
There is no hardened multi-tenant isolation claim.

## Memory accounting

`spec.memory` controls the guest's RAM. The runner requests that RAM plus
`512 MiB + guest RAM / 32` for host overhead. This allowance is a conservative
scheduling estimate, not a calibrated maximum. Hugepage-backed guests reserve
their guest RAM through native hugepage resources and 512 MiB of ordinary RAM.

The runner has no memory limit by default, so host overhead can exceed its
reservation without hitting a per-container memory ceiling. Guest RAM remains
bounded by the hypervisor configuration. An explicit `spec.resources.limits.memory`
is preserved and must cover the memory request. Admission policies can still
inject limits; inspect the admitted Pod when verifying this behavior.

Node memory pressure can still evict or kill a VM. Reserve capacity for node
services and monitor aggregate memory usage; removing the runner's limit does
not guarantee unlimited physical memory. The checkpoint runtime is a separate
container and retains its own 128 MiB request and 512 MiB limit.

## Durable state

The head is authoritative; VM status is a projection. The head identifies the
base digest, owner Pod UID, node, epoch and current checkpoint. Acquisition and
checkpoint publication both require compare-and-swap on that same record.
There is no expiring lease that permits a second writer.

`STATE_BACKEND=s3` stores the head at `vm/<VM UID>/head.json` with S3 `If-Match`;
initial creation uses `If-None-Match: *`. `STATE_BACKEND=kubernetes` stores it in
the `roamvm-<VM UID>` ConfigMap in `STATE_NAMESPACE` (default `roamvm-system`),
using Kubernetes resourceVersion preconditions. Disk objects stay in S3 in both
modes. The Kubernetes mode supports stores such as Garage 2.3.0 that do not
implement conditional S3 writes. These ConfigMaps deliberately outlive the VM;
include them in control-plane backups. Neither VM status nor a bucket listing
can replace a lost head. Do not switch backends, namespaces or buckets for an
existing deployment without an offline metadata migration with all VMs stopped.

Checkpoints are immutable objects named by generation, epoch and SHA-256. The
checkpoint worker validates and hashes the stopped QCOW2 overlay, then streams that same
file to S3 (large files use multipart upload with file-backed, retryable parts).
There is no local checkpoint copy or compaction. It reads the upload back and
verifies its full hash and size, then conditionally replaces the head. That write
commits the new durable state. Cleanup removes previous checkpoints before
`CheckpointReady=True`. `Stopped` is reported earlier, after local flush and
stop-metadata persistence; it is not a remote durability guarantee. A lost response is reconciled by reading
the head. In S3 mode these
operations rely on the backend's
[conditional write semantics](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html).
In Kubernetes mode, S3 conditional headers are only an additional safeguard:
epoch and content hash distinguish checkpoint keys, and retrying a key writes
the same bytes. Only the Kubernetes head determines which object is committed.

Only the latest stopped checkpoint is retained. Unique keys are temporary commit
identities, not a snapshot history. After publication, cleanup lists this VM's
overlay prefix and deletes superseded objects at or below the committed checkpoint's
epoch. It preserves the committed key and every newer epoch, so delayed cleanup
cannot remove a subsequent owner's upload or checkpoint. No rollback operation
can republish an older key. Unknown key formats are left alone.

Cleanup errors keep the worker retrying with `CheckpointReady=False`. A restarted worker reads the committed
head and retries cleanup without uploading again. The next successful stop also
removes history left by older releases. On versioned S3, deletion specifies the
obsolete object's version ID to remove its bytes instead of adding a delete marker.

Ownership can change while an upload is in flight. The check before upload is
not sufficient: the final head update must compare the exact revision acquired
by that runner, without retrying against a newly read revision. A stale upload
can leave an unreferenced object, but cannot change the current checkpoint. Such
objects are removed by a later successful stop once their epoch is obsolete.
Compare-and-swap fences publication, not execution. Recovery still requires
proof that the previous VMM has terminated or its node has been fenced.

Restore checks size, hash and object version, rewrites the backing reference to
the read-only image-volume base, validates QCOW2 metadata, and then starts the VMM.
Incoming embedded backing paths are never trusted. New checkpoints contain the
full current delta against the base; they do not depend on previous generations.

Same-node restart acquires the local disk lock, checks the persisted stopped
owner/VM/base/node identity and transfers ownership with a new epoch. It does not
read the checkpoint object. An in-flight upload is canceled before the new VMM
can write; the latest successful S3 checkpoint stays the recovery point until a
later stop finishes uploading. No additional disk-sized snapshot is created.

The scheduler still admits cached starts, intersecting user placement constraints
with the cache node. The worker continues until a cached runner is assigned.
An unavailable/cordoned node or unschedulable cached start can fall back to a fresh
PVC only after the latest local stop is durable. No automatic fallback discards
unuploaded changes. Loss of that node before durability requires explicit fenced
recovery and can lose those changes. Even local resume requires ownership metadata.

Pod-local layout:

```text
/base/disk/                         read-only OCI image volume, cached by containerd
/var/lib/roamvm/                    retained VM-owned local PVC
  running/<VM UID>/                overlay, VMM socket and lock
  sessions/<Pod UID>.json           runtime reconciliation metadata
/run/roamvm/                       shared emptyDir for the runtime API socket
```

Successful commit retains the local overlay. Runner deletion frees guest resources
without deleting the VM-owned claim. Workers can be reconstructed from
`status.local` after controller/worker restarts. VM deletion waits for durability;
remote fallback and explicit recovery invalidate the local cache. Legacy
ephemeral-PVC runners still save synchronously until their next incarnation.

## Failures and recovery

| Failure | Behavior |
| --- | --- |
| Missing OCI base | Pull on the chosen node; no scheduling dependency |
| Duplicate start | One runner name plus conditional head ownership; second owner rejected |
| S3 unavailable during stop | Guest reports Stopped; worker retries; CheckpointReady remains false |
| Old-checkpoint deletion fails | Worker retries cleanup without uploading again |
| Runtime sidecar restarts | Same runner/epoch resumes using saved metadata |
| Control contact lost for 30 seconds | Runner requests guest shutdown; ownership stays held |
| Guest ignores ACPI timeout | VMM killed; validate and checkpoint the crash-consistent disk |
| Hypervisor / runner crashes or is OOM-killed | Surviving runtime validates and checkpoints the working disk, then stops the VM |
| Runtime crashes / OOM | Container restarts independently; saved session resumes |
| Node disappears / Pod cannot finish | No automatic takeover; ownership and local disk retained |
| Invalid local QCOW2 or failed upload after a crash | Local disk retained; worker retries; no silent restore of older data |
| Corrupt downloaded checkpoint | Hash failure before VMM launch |
| Stale owner uploads after recovery | Head CAS fails; it cannot publish a new current generation |
| Unscheduled Pod cancelled | Resource-version checked deletion, no ownership acquired |
| Graceful VM deletion | Finalizer waits for replacement and cleanup; latest recovery checkpoint retained |

Deleting a Pod starts its Kubernetes termination grace period (one hour in the
initial configuration). Finalizers do **not** prevent kubelet killing containers
when that grace period expires. Prefer setting VM powerState to Stopped before
maintenance; wait for CheckpointReady before draining/shutting down the node. Forced
Pod deletion, eviction or node loss follows the stated crash model.

For local process failures, the runtime requires the exact Pod UID and node, a
terminated runner with no restart policy, VM owner identity, the saved session,
and an exclusive working-directory lock. Normal QCOW2 validation and state CAS
still gate publication. It never infers termination from a timeout or NodeReady.
A stopped incarnation cannot overwrite a newer start/configuration generation.

For an older failed Pod, prefer [preserving its working disk](recovery.md).

If local recovery is impossible, first prove the old VMM has exited, or fence/power off
its old node. Kubernetes NotReady and timeouts are not proof. Record the VM UID
and old Pod UID, then run the operator command with S3 and Kubernetes credentials:

```sh
roamvm recover --vm-id VM_UID --owner OLD_POD_UID --fenced
roamvm start --namespace NAMESPACE VM_NAME
```

This is an explicit decision to discard uncommitted changes and restore the last
checkpoint. It never runs automatically. It first requests Stopped, removes the
old incarnation, advances the head's epoch, and resets Kubernetes status. Retained
local failed disks are not automatically adopted; preserve them for investigation
before recovery if the uncommitted work matters.

## Deliberate boundaries

No distributed storage, continuous replication, root CSI/PVs, local disk inventory
service, live migration, VM pool or memory hibernation. Coder can create/patch a
VirtualMachine and use its Service like any other client. There is no Coder API
or guest-agent dependency in this repository.

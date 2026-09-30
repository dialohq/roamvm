# Design and failure semantics

## Choice of runtime

We inspected [Virtink](https://github.com/smartxworks/virtink) at commit
`a0cbe18c496b8634163916c28d94e038df489690`. Its controller → Pod → node daemon
architecture was the starting point. RoamVM uses a runtime sidecar with each VM
Pod so it can use PVCs and image volumes without hostPath mounts. No Virtink code
or compatibility layer is included.

Cloud Hypervisor [v53.0](https://github.com/cloud-hypervisor/cloud-hypervisor/releases/tag/v53.0)
has native QCOW2 backing-file support. This avoids a separate NBD/ublk storage daemon.
The pinned binary checksum is in the Dockerfile. Explicit `image_type` and
`backing_files=on`, standalone-base validation and backing-path replacement follow
its [QCOW2 hardening guidance](https://github.com/cloud-hypervisor/cloud-hypervisor/security/advisories/GHSA-jmr4-g2hv-mjj6).

## Components and trust

- Controller: creates one named runner incarnation, mirrors status, requests stop,
  and removes resources after durable completion. It has no S3 credentials.
- Runtime sidecar: authorizes its own runner by token/Pod UID/node/VM ownership,
  validates the mounted base disk, restores state and commits checkpoints.
- Runner: holds a local file lock, sets up TAP/NAT/DHCP, runs the hypervisor,
  sends heartbeats and reports guest readiness. It cannot access the sidecar's
  Kubernetes token or S3 environment through its mounts.
- Device plugin: a node OS service allocates shared KVM/TUN access using native
  extended resources. It requires no Kubernetes hostPath volume.

Kubelet owns OCI pulling, registry credentials, caching and read-only image-volume
mounts. Each incarnation receives a generic ephemeral PVC on the node selected
by the scheduler. WaitForFirstConsumer preserves normal placement; the old claim
is not reused on restart. The runtime is a native sidecar, so Kubernetes stops it
after the runner exits, including after a checkpoint upload finishes. Its restart
retains the same Pod/PVC and ownership epoch.

The runtime and Kubernetes administrators are trusted. VM configuration cannot
project the reserved runtime Secret, service account tokens or Pod certificates. Pod creation,
exec or modification in a VM namespace is privileged access to runtime credentials;
VM-only users should receive VM-resource permissions, not those Pod permissions.
There is no hardened multi-tenant isolation claim.

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
runtime uploads the compact QCOW2 overlay, reads it back and verifies its full
hash and size, then conditionally replaces the head. Only that last write commits
Stopped. A lost response is reconciled by reading the head. In S3 mode these
operations rely on the backend's
[conditional write semantics](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html).
In Kubernetes mode, S3 conditional headers are only an additional safeguard:
epoch and content hash distinguish checkpoint keys, and retrying a key writes
the same bytes. Only the Kubernetes head determines which object is committed.

Ownership can change while an upload is in flight. The check before upload is
not sufficient: the final head update must compare the exact revision acquired
by that runner, without retrying against a newly read revision. A stale upload
can leave an unreferenced object, but cannot change the current checkpoint.
Compare-and-swap fences publication, not execution. Recovery still requires
proof that the previous VMM has terminated or its node has been fenced.

Restore checks size, hash and object version, rewrites the backing reference to
the read-only image-volume base, validates QCOW2 metadata, and then starts the VMM.
Incoming embedded backing paths are never trusted. New checkpoints contain the
full current delta against the base; they do not depend on previous generations.

Pod-local layout:

```text
/base/disk/                         read-only OCI image volume, cached by containerd
/var/lib/roamvm/                    fresh generic ephemeral PVC
  running/<VM UID>/                overlay, compact checkpoint, VMM socket and lock
  sessions/<Pod UID>.json           runtime reconciliation metadata
/run/roamvm/                       shared emptyDir for the runtime API socket
```

Successful commit removes writable files; Pod deletion then garbage-collects the
PVC. Failed uploads retain both the Pod and PVC. The PVC's node affinity applies
only to that incarnation; future starts create a new Pod and claim. No persistent
root claim, custom CSI driver, or manual local-disk inventory is involved.

## Failures and recovery

| Failure | Behavior |
| --- | --- |
| Missing OCI base | Pull on the chosen node; no scheduling dependency |
| Duplicate start | One runner name plus conditional head ownership; second owner rejected |
| S3 unavailable during stop | Guest exits, Pod stays Checkpointing, local state retained, upload retried |
| Runtime sidecar restarts | Same runner/epoch resumes using saved metadata |
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
old incarnation, advances the head's epoch, and resets Kubernetes status. Retained
local failed disks are not automatically adopted; preserve them for investigation
before recovery if the uncommitted work matters.

## Deliberate boundaries

No distributed storage, continuous replication, root CSI/PVs, local disk inventory
service, live migration, VM pool or memory hibernation. Coder can create/patch a
VirtualMachine and use its Service like any other client. There is no Coder API
or guest-agent dependency in this repository.

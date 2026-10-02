# Recover a retained working disk

New runner Pods checkpoint local process crashes automatically. Existing Pods
keep their original container layout until restarted. This procedure preserves
their working disk; `roamvm recover --fenced` instead **discards** uncommitted work.

1. Inspect the VM and its Pod. Record both UIDs, the node, immutable base image,
   working PVC and runtime image. Confirm the runner is terminated. A missing
   Pod, a disconnected agent or a NotReady node is not proof of termination.
2. Request Stop through the normal client. For Coder, run `coder stop WORKSPACE`
   in one terminal and let its stop build wait. Check that the VM's
   `spec.powerState` is `Stopped`. Do not delete the VM, Pod, PVC or finalizers.
3. Make a storage snapshot or an independent backup of the retained PVC before
   repair. Keep the original base image too. Treat these as workspace secrets.
4. Create a one-shot rescue Pod on the original node, mounting that PVC and the
   **original** base image. Use a RoamVM image containing the `checkpoint`
   command. Copy the original runtime's ConfigMap/Secret references and service
   account; never substitute another object store, bucket or state namespace.
5. Run `roamvm checkpoint` with the original Pod identity. For legacy ephemeral
   claims it validates QCOW2, uploads and verifies the overlay, and commits using
   the original ownership epoch. For retained VM-owned claims it flushes the
   local stop; the controller creates a background worker to finish that upload.
6. Wait for `Stopped`, delete the completed rescue Pod, and wait for
   `CheckpointReady=True` before removing the backup or maintaining the node.
   Start through the normal client and verify project files and agent connectivity.

A rescue Pod has this shape (replace every capitalized placeholder):

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: recover-working-disk
  namespace: WORKSPACE_NAMESPACE
spec:
  restartPolicy: Never
  serviceAccountName: roamvm-runtime
  nodeSelector:
    kubernetes.io/hostname: ORIGINAL_NODE
  imagePullSecrets:
    - name: ghcr
  containers:
    - name: checkpoint
      image: FIXED_ROAMVM_IMAGE
      args: [checkpoint]
      env:
        - name: POD_NAME
          value: ORIGINAL_POD_NAME
        - name: POD_NAMESPACE
          value: WORKSPACE_NAMESPACE
        - name: POD_UID
          value: ORIGINAL_POD_UID
        - name: VM_UID
          value: ORIGINAL_VM_UID
        - name: NODE_NAME
          valueFrom:
            fieldRef:
              fieldPath: spec.nodeName
        - name: GOMEMLIMIT
          value: 256MiB
      envFrom:
        - configMapRef:
            name: roamvm-runtime
        - secretRef:
            name: roamvm-object-store
      resources:
        requests: {cpu: 100m, memory: 128Mi}
        limits: {memory: 1Gi}
      volumeMounts:
        - {name: working, mountPath: /var/lib/roamvm}
        - {name: base, mountPath: /base, readOnly: true}
  volumes:
    - name: working
      persistentVolumeClaim:
        claimName: ORIGINAL_WORKING_PVC
    - name: base
      image:
        reference: ORIGINAL_VM_IMAGE_DIGEST
        pullPolicy: IfNotPresent
```

Legacy failed Pods remain in place until commit. Their PVC may remain Terminating
while the rescue Pod mounts it; PVC protection releases it after the rescue Pod
is deleted. VM-owned claims remain available for local restart after completion.
Do not force-remove PVC protection.

If validation fails, keep the backup and working PVC. Inspect `qemu-img check`
on a copy with the same backing image. Repair only a copy after understanding the
error; the checkpoint command deliberately does not run destructive repairs or
fall back to an older generation. A crash-consistent disk can require guest
filesystem journal replay and does not preserve RAM or unflushed writes.

For a lost node, fence it before using the explicit `recover --fenced` command.
That operation restores the last durable checkpoint and may lose newer work;
it cannot recover a workspace that never produced a checkpoint.

## Host storage exhaustion

Writable root overlays and secondary disks use direct I/O with flushes enabled.
Read-only base images and configuration disks may use the host page cache. This
avoids QEMU permanently retaining a failed buffered `fdatasync` error after the
host page cache becomes inconsistent. Storage must support direct I/O; startup
fails if it cannot, rather than silently falling back to buffered writes.

QEMU pauses on ENOSPC using its native write-error policy. Inspect `query-status`
and `query-block` on the runner's `qmp.sock`; `io-error` and `nospace` identify this
case. Keep the runner alive, restore physical capacity and confirm the storage
layer is healthy before issuing QMP `cont`. Verify the guest responds and writes
succeed. Direct I/O does not add capacity or repair filesystem corruption, and
RoamVM does not automatically resume guests after arbitrary I/O errors.

Existing running Pods retain their original cache mode. Update the runtime, then
stop and start normally to pick up the change. A buffered runner whose flushes
already fail permanently can still require the retained-disk procedure above;
changing the runtime image cannot repair its in-memory state.

Monitor thin-pool data and metadata usage, automatic-extension monitoring, and
free space in the containing volume group. Allow space for the working overlay
and filesystem/QCOW2 metadata; Stop streams it to S3 without a second local copy.
Virtual disk capacity is not a physical reservation when thin provisioning is
enabled. Keep independent backups; a checkpoint or snapshot on the same storage
is not protection against
physical storage loss.

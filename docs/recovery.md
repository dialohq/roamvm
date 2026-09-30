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
5. Run `roamvm checkpoint` with the original Pod identity. It validates QCOW2,
   compacts and verifies the upload, commits the checkpoint using the original
   ownership epoch, then annotates the original Pod as durably stopped.
6. Wait for the VM and the client stop build to report Stopped. Delete the rescue
   Pod after successful completion. Start through the normal client; verify
   project files and agent connectivity before removing the backup.

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

The original failed Pod remains in place until commit. Its PVC may remain
Terminating while the rescue Pod mounts it; PVC protection releases it after the
rescue Pod is deleted. Do not force-remove that protection.

If validation fails, keep the backup and working PVC. Inspect `qemu-img check`
on a copy with the same backing image. Repair only a copy after understanding the
error; the checkpoint command deliberately does not run destructive repairs or
fall back to an older generation. A crash-consistent disk can require guest
filesystem journal replay and does not preserve RAM or unflushed writes.

For a lost node, fence it before using the explicit `recover --fenced` command.
That operation restores the last durable checkpoint and may lose newer work;
it cannot recover a workspace that never produced a checkpoint.

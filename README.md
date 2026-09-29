# RoamVM

A Kubernetes VM runtime for development machines. The root disk follows compute:
OCI stores the immutable base, S3 stores stopped-VM changes, and local filesystem
storage is the working copy. There is no root PVC, CSI driver or replica set.

This is an experimental implementation with real KVM integration tests, not a
production-qualified replacement for KubeVirt. It is independent of Coder.

```text
VirtualMachine → controller → runner Pod → kube-scheduler
                                  │
                      node daemon prepares disks
                         OCI base + S3 checkpoint
                                  │
                       Cloud Hypervisor inside Pod
                                  │
                    graceful shutdown → S3 commit
```

The hypervisor runs **inside the runner container's cgroup and network namespace**.
The daemon prepares and commits storage; it does not put the VMM outside Kubernetes
resource accounting. The root overlay never participates in scheduling.

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
A stopped VM retains the entire changed root disk, including packages, project
files and machine credentials. RAM and running processes are not retained.

For a planned move, stop and wait for `Stopped`, change placement constraints or
cordon the old node, and start again. The default scheduler selects the destination.
Use Services for stable identity; the Pod IP can change. A normal `kubectl delete
rvm` also waits for a checkpoint, retaining the S3 objects for recovery/retention.

## Install

Requirements: Linux x86-64 KVM nodes, `/dev/kvm`, `/dev/net/tun`, Kubernetes 1.35+
(the tested version), an IPv4 CNI with interface `eth0`, and ext4/XFS scratch space
at `/var/lib/roamvm`. Mount your NVMe filesystem there. Only the DaemonSet and
runner Pods use this host path. No host-installed hypervisor is required.

Allow these **namespaced** sysctls in kubelet configuration on VM nodes:

```yaml
allowedUnsafeSysctls:
- net.ipv4.ip_forward
- net.ipv4.conf.all.route_localnet
```

The first enables guest forwarding. The second lets Kubernetes port forwarding
reach the guest through the Pod loopback address. Pods keep CNI's original IP,
interface and routes. Guest virtio-net uses DHCP, TAP, and NAT inside the Pod.
The runtime grants NET_ADMIN/NET_RAW to the runner; it is not a privileged Pod.
Namespaces admitting VMs must allow its capabilities, sysctls and hostPath mounts.

Build with Go 1.26.7 (`make build`) and publish `make image IMAGE=YOUR_REGISTRY/roamvm:VERSION`.
In `config/install.yaml`, replace all three `roamvm:dev` container image references
and the controller's `RUNNER_IMAGE` value with the same published digest.

Create a dedicated S3 bucket. The store must implement strongly consistent reads,
conditional `PutObject`, and conditional `CompleteMultipartUpload` with `If-Match`
and `If-None-Match`. The daemon probes basic conditional semantics on startup;
`TEST_S3_ENDPOINT=... TEST_S3_BUCKET=... go test ./internal/state -run TestS3 -count=1`
also checks multipart behavior against your backend.

```sh
kubectl apply --server-side -f config/crd
kubectl create namespace roamvm-system
# Create roamvm-system/object-store through Vault or your usual secret manager.
# Required: S3_BUCKET, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY.
# Optional: S3_ENDPOINT (S3-compatible URL), AWS_REGION, AWS_SESSION_TOKEN.
# With AWS workload identity, use the SDK credential chain instead of static keys.
kubectl apply -f config/install.yaml
kubectl label node NODE_NAME vm.roamvm.io/enabled=true
```

The SDK uses normal HTTPS validation. `REGISTRY_PLAIN_HTTP=true` is for the local
lab only. Runtime AWS permissions cover GetObject/PutObject under `vm/` and
`runtime-probes/`, multipart upload/abort, and DeleteObject on `runtime-probes/`
for probe cleanup. The runtime does not delete checkpoints. Keep this bucket
private and enable your usual encryption/access controls; overlays contain the
VM's private files. S3 bucket versioning is supported but not required: checkpoint
keys are already immutable, and head replacement uses ETag compare-and-swap.

The controller and daemon are trusted cluster components. VM creation is comparable
to Pod creation: it can reference namespace-local Secrets and PVCs. Do not grant
untrusted users direct access to daemon credentials, runner mutation, host paths,
or the runtime S3 prefix. Per-incarnation runner tokens are bound to VM, Pod UID
and assigned node; the guest receives no Kubernetes or S3 credentials by default.

## Package a NixOS or other Linux image

An ordinary OCI image layer contains files, **not** a container rootfs to execute:

```text
disk/
  manifest.json       {"format":"qcow2","cmdline":"console=ttyS0 ..."}
  root.qcow2          standalone immutable disk; root.raw also supported
  vmlinux             uncompressed ELF Linux kernel, for direct boot
  initrd              optional initramfs
```

Alternatively put `firmware` in the directory instead of `vmlinux`, with a guest
bootloader on the disk. Direct Linux boot was integration-tested; UEFI was not.
The image needs virtio PCI/block/net support, DHCP, ACPI power-button shutdown,
and the service named by `readinessPort`. There is no required guest agent.
NixOS images should have guest firewall/SSH/authentication configured intentionally.

Copy files **dereferencing Nix store symlinks**, preserve the Nix image's kernel
command line, and package them:

```sh
# To keep OCI transfers small, optionally convert an existing raw disk first:
qemu-img convert -f raw -O qcow2 /path/to/root.raw disk/root.qcow2
# Supply disk/manifest.json, vmlinux and initrd from the same NixOS build.
# Linux scripts/extract-vmlinux can extract an ELF kernel from bzImage.
tar -C . -czf vm-image.tar.gz disk
roamvm image-push --tag registry.example.com/vm-images/devbox:BUILD \
  --tar vm-image.tar.gz
```

The push command uses standard Docker credentials and prints the immutable digest.
Private pulls use `spec.imagePullSecrets` in the VM namespace. A base with its own
backing dependency is rejected. The base digest cannot change on an existing VM:
create a new VM to change the base, or install packages into its current overlay.

## Kubernetes integration

- `cpus` defines vCPUs. CPU requests default to that count; lower requests allow
  overcommit. CPU limits are optional. Memory reserves guest RAM plus 192 MiB VMM
  overhead. Adjust upward if your workload/devices need more host memory.
- Affinity, node selectors, tolerations and topology spread pass to the runner Pod.
  Each incarnation captures its boot configuration when the Pod is created;
  later spec edits apply at the next start, keeping boot RAM/CPU and reservations
  consistent even while the Pod is queued.
- Services, DNS, port-forwarding and CNI policy operate on the Pod endpoint.
- `config` accepts Kubernetes projected volume sources, converted to a read-only
  ISO labelled `ROAMVM_CONFIG`. Your guest mounts/consumes it. Changes appear next
  boot. The runtime does not mutate the guest's root filesystem to inject settings.
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

## Current limits

IPv4 and x86-64 only; no live migration, automatic crash recovery, memory snapshots,
online disk resizing, image upgrades, periodic checkpoints, or automatic cache /
checkpoint garbage collection. Old generations and bases accumulate until an
operator applies retention. Never delete the current checkpoint or a base used by
a running VM. Do not apply age-only S3 expiration to the entire overlay prefix.

Stopping costs guest shutdown plus compaction, upload and verification of the
**entire current overlay**, not only changes since the previous stop. Each generation
is independently usable against its pinned base. Running-node failure can lose
all changes since the last committed stop, as intended by this model.

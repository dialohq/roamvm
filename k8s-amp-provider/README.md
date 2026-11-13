# Kubernetes Amp provider

VM-backed, self-hosted Amp orbs using RoamVM's experimental Kubernetes operator.
The operator already implements VM lifecycle, hibernation, checkpoint storage,
cloning and warm-cache retention. This folder packages the Amp-specific setup;
it does not introduce another operator or replace the legacy RoamVM installation.

The implementation is lab-tested, not production-qualified. Full startup latency
and a usable session against Amp's live service remain to be validated.

Run the commands below from the RoamVM repository root.

## Install the operator and provider permissions

Follow the [runtime prerequisites and object-store setup](../docs/v2.md), including
the node device plugin, FUSE support, storage class and namespace credentials.
Build and publish the opt-in runtime with `docker build --target v2 ...`; it
includes the pinned QEMU extension required for private-RAM hibernation.

Use a Kustomize overlay over this folder to pin both the controller image and its
`RUNNER_IMAGE` environment variable to the published digest. The base still uses
the development tag `roamvm:dev` and is not a production image selection.

Render the combined installation before applying your configured overlay:

```sh
kubectl kustomize k8s-amp-provider
```

The [Kustomization](kustomization.yaml) includes the existing v2 operator/CRD and
[provider RBAC](rbac.yaml). By default, the operator runs in `roamvm-v2-system`
and orb VMs run in `default`. The namespace-scoped `roamvm-orb-provider` account
can dispatch v2 VMs and exec bootstrap into runner Pods; it does not receive the
object-store secret or access to the state namespace. Use a kubeconfig
authenticated as that account for the provider process.

**These manifests do not start the `amp orb provider` process.** Run that process
separately as described below. Shared remote builders are also a separate
deployment, not installed by this Kustomization.

## Run the provider

Build the supplied NixOS guest with `nix build .#orb-image`, then publish the
archive with `roamvm image-push`. It includes QGA, Amp, Node, tmux, sudo and the
standard command-line tools; root growth is enabled. VM UIDs seed distinct guest
machine identities and SSH host keys on cold clone boot. The image supports the
command-line orb environment; Amp Desktop's Wayland bundle is not included.
Amp's ordinary container `ORB_IMAGE` is **not** a bootable disk and is not
automatically converted; set `ORB_VM_IMAGE` explicitly. Keep both CLIs updated
and validate a new pinned guest before rolling it out.

Load `AMP_ORB_PROVIDER_TOKEN` from your secret manager and provide the restricted
`KUBECONFIG`. Put the built `roamvm` binary and Amp CLI on `PATH`, then run exactly
one provider process per provider token:

```sh
export ORB_NAMESPACE=default
export ORB_VM_IMAGE='YOUR_REGISTRY/orb-guest@sha256:YOUR_GUEST_DIGEST'
amp orb provider ./k8s-amp-provider/dispatch.sh
```

[dispatch.sh](dispatch.sh) adapts Amp's documented `spawn`, `pause`, `delete`
environment contract through `roamvm --experimental-v2 orb-dispatch`.
The dispatch implementation lives in [internal/orbs](../internal/orbs) and
[cmd/roamvm/orbs.go](../cmd/roamvm/orbs.go), sharing the existing runtime rather
than duplicating it here.

Register these exact orb types in Amp settings, with `medium` as the default:

| Type | vCPUs | Guest RAM | Guest root disk |
| --- | ---: | ---: | ---: |
| small | 2 | 4 GiB | 64 GiB |
| medium | 4 | 8 GiB | 64 GiB |
| large | 8 | 16 GiB | 64 GiB |

Memory/working-volume requests also include hypervisor and checkpoint headroom.
The Kubernetes scheduler admits runners using those requests; it does not resize
a VM when the cluster is full. An existing orb's type cannot change.

On spawn, dispatch resumes the existing VM, otherwise clones the named prepared
project snapshot if present, otherwise creates from the base. Every spawn sends
the fresh `ORB_TOKEN` through Kubernetes exec stdin and the private QEMU guest
agent, then launches detached `amp orb bootstrap` as `user`. Tokens are not Pod
arguments, annotations or Kubernetes Secrets. Dispatch completion means the
guest launcher succeeded, not that Amp's remote setup/model loop succeeded.

A pause carrying `ORB_SNAPSHOT_ID` also creates an independent disk-only snapshot
VM. Repeated requests reuse that snapshot ID. Deleting the source orb does not
delete the project snapshot. These stopped snapshot resources need explicit
deletion when no longer used; the public dispatch contract has no separate
snapshot-deletion job, so Amp-side invalidation alone does not purge their bytes.

## Shared remote Nix builders

The supplied orb image imports `nix/remote-builders.nix`; custom NixOS images can
import `nixosModules.remote-builders`. Native builds use stock Nix `ssh-ng`, not
the builder pool's gRPC administration API. The pool needs its optional native
SSH endpoint enabled, public client keys authorized, and network access from
runner Pods to each builder. Use individual builder DNS names, not a load balancer
that can route successive connections to different stores.

Mount a private JSON file into the **provider process** and set
`ORB_NIX_BUILDERS_FILE` to its path. Its fields are:

* `Machines`: the standard Nix builders string, with one semicolon-separated
  entry per builder. For example:
  `ssh-ng://root@HOST:2222?ssh-key=/run/roamvm-builder/id_ed25519 x86_64-linux - 2 1`.
  Advertise only features the builder actually supports.
* `SSHPrivateKey`: the client private key whose public half the pool authorizes.
* `KnownHosts`: pinned public host keys, with entries such as
  `[HOST]:2222 ssh-ed25519 BASE64_PUBLIC_HOST_KEY`. Match `HOST` to the URI exactly.

Do not put this file in Git, a Nix derivation, an image, a VM spec or a Pod
argument. It contains neither the pool's admin token nor its private host key.
Bootstrap delivers it through private Kubernetes exec/QGA stdin and writes
the root-only private key under guest tmpfs `/run/roamvm-builder`. Public Nix
settings and host keys are readable by the normal guest user. Disk-only project
clones do not retain those files. **RAM checkpoints do contain live credentials**;
protect checkpoint access and revoke or rotate client keys when required.

When configured, `max-jobs = 0` prevents local build fallback and
`builders-use-substitutes = true` lets builders fetch cached inputs. Authentication,
host-key or connectivity failures fail builds rather than using tiny guest CPUs.
The pool must enable trusted signed substitutes for image inputs. NixOS disk
image registration can omit their cache signatures; stock Nix can substitute
those inputs remotely instead of importing the unsigned guest copies.
Enable pool output signing as well: `signing.existingSecret` contains a Nix
binary-cache private key under `key`, mounted only on builders, and
`signing.publicKeys` installs its public key on every builder and origin/store.
Otherwise a later build can fail after pool GC when stock Nix first tries to
upload a previous, unsigned output from the guest. Guests receive no signing
key and the SSH endpoint remains untrusted.
Nix output paths and their normal GC roots remain in the guest; builder-side
connection leases protect active remote build/copy roots. Refreshing an unchanged
configuration does not restart the guest daemon or interrupt live builds.
Changing/removing it restarts an active daemon, so do that between builds.
Removing `ORB_NIX_BUILDERS_FILE` removes the old guest configuration on next spawn.

Suspension is not a durable remote-build session. Server keepalives eventually
retire disconnected SSH workers and release their leases for pool GC. Completed
outputs already copied into the guest survive hibernation; an interrupted remote
build/copy may need retrying after resume. A fresh connection can reuse outputs
the shared pool has retained. Production pool enablement and credential/network
policy provisioning are separate deployment steps.

## Verification

Run `make test` and render `kubectl kustomize k8s-amp-provider`.
The [runtime integration checks](../docs/v2.md#verification) cover hibernation,
lazy recovery, eviction, cloning and deletion.

For the real Amp CLI against the mock provider API in [test](test), install the
new runtime in the disposable **slot-zero libvirt lab**, publish `orb-image` to
its registry, and source `test/libvirt/env`. Build `bin/roamvm`, set `RUNTIME_BIN`
to its absolute path, `AMP_BIN` to the pinned Amp CLI, and
`ROAMVM_TEST_ORB_IMAGE` to the published guest digest, then run:

```sh
ROAMVM_TEST_ORBS=1 python3 k8s-amp-provider/test/prove.py
```

This exercises restricted provider RBAC, real guest-agent token delivery,
snapshot copying, warm/cold resume and deletion. It uses 512 MiB fixtures because
lab nodes have 2 GiB; production size mapping is covered by unit tests, not this
run. The mock deliberately returns 401 after checking bootstrap discovery:
repository setup, model/tool execution, Terminal and portals against Amp's live
service are not proven by it. Never use real credentials with the plaintext mock.

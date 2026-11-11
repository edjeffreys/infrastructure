# buildkit

A single rootless `buildkitd` that builds container images for CI. Today its
only caller is `.github/workflows/build-claude-agent.yaml`, which runs the
matching `buildctl` client as its ARC job container and connects over gRPC.

## Why a daemon here rather than a builder in the runner

The previous setup ran Kaniko as the job container in `arc-runners`. Kaniko was
archived upstream in June 2025 with `v1.24.0` as its final release, and it
publishes to the deprecated `gcr.io` mirror, so there was nothing left to track
or update.

BuildKit is the replacement, but *where* it runs is not a free choice.
`arc-runners` carries no `pod-security.kubernetes.io/enforce` label, so it
inherits the Talos cluster default of **baseline** — and rootless `buildkitd`
requires `seccompProfile: Unconfined`, which baseline forbids. Running it in the
job container would mean labelling `arc-runners` privileged: the namespace that
executes workflow-defined code, in a public repository, on self-hosted runners.

Putting the daemon in its own namespace confines that exemption to one
long-lived pod that runs no workflow code, and leaves `arc-runners` at baseline.

Two things fall out of the split that are worth knowing:

- **The registry token never leaves the runner.** `buildctl` resolves
  `~/.docker/config.json` client-side and forwards the credential over the build
  session, so `GITHUB_TOKEN` is not written to buildkitd's config and is not
  retained in its cache.
- **The build's own network traffic egresses from this namespace.** Every `RUN`
  in a Dockerfile — `apt-get`, `npm`, `curl` — is this pod's egress, not the
  runner's. That is why `allow-world-egress` here is as broad as the one in
  `arc-runners`, and why tightening the runner's egress does not constrain what
  a build can reach.

## Cache

The `buildkit-cache-single` PVC (`longhorn-single`, 10Gi) is mounted at
`/home/user/.local/share/buildkit`. Kaniko had no cache at all, so every push
re-downloaded the whole `claude-agent` base layer, `apt` set and global `npm`
install; those are now reused between builds.

GC is set explicitly in `buildkitd.toml`: `maxUsedSpace = "6GB"`,
`minFreeSpace = "3GB"`, `reservedSpace = "2GB"`. The caps are absolute, so if you
resize the PVC, change them too.

The defaults were not enough. On a 10Gi volume BuildKit's default caps the cache
at 80% of the filesystem with 20% kept free. GC only prunes records that no
build is using, and it runs between builds, so concurrent CI builds kept writing
past the cap. On 2026-11-11 the filesystem reached 99%. That alone is not an
outage. The damage came from what Longhorn did with it:

- A replica rebuild took a system snapshot of the ~10 GB of data. New writes then
  landed on top of the snapshot, and the single replica on `talos-worker-2` grew
  to 138% of the volume's size.
- Merging the snapshot back needs more free space, and the purge failed with
  `no space left on device`.
- `/var/lib/longhorn` shares a filesystem with the kubelet's images and ephemeral
  storage, so the node went into `DiskPressure`. The kubelet started evicting
  pods and Longhorn marked the node down.

Leaving the gap between 6 GB and 10Gi is what absorbs builds that are running
while GC waits. Bear in mind too that all worker VMs sit on the one Proxmox
physical disk.

It is single-replica because everything in it can be rebuilt: losing the
replica's node costs one cold build, while a second replica doubled every
cache write onto the same disk. Storage class is immutable on a bound PVC,
which is why the claim was renamed from `buildkit-cache` when it moved off
`longhorn` — Flux prunes the old one.

## The gRPC endpoint is unauthenticated

`buildkitd` listens on `tcp://0.0.0.0:1234` with no TLS and no client
authentication. Anything that can reach that port can build an arbitrary
Dockerfile and push with whatever credentials it supplies.

The `allow-arc-runners-ingress` `CiliumNetworkPolicy` is the entire access
control. That is proportionate while the only client is CI in a single
namespace, and it is the same trust boundary either way — but it means the
policy is load-bearing in a way a network policy usually is not. If a second
namespace ever needs to build, add mTLS (`--tlscacert`/`--tlscert`/`--tlskey`
with a cert-manager `Certificate`) rather than widening the selector.

## Gotchas

- **The daemon depends on a Talos setting, not just this directory.** Talos
  defaults `user.max_user_namespaces` to 0; the worker patch in
  `talos/talconfig.yaml` raises it. A worker rebuilt without it crashloops, and
  CI then blames the *network* — Cilium's socket LB answers `operation not
  permitted` for a ClusterIP whose only backend is unready.
- **`--oci-worker-no-process-sandbox` is required.** Without it the daemon tries
  to unshare a nested PID namespace, which needs privileges the pod does not
  have. Build steps lose their own process sandbox; the pod's is unaffected.
- **No AppArmor annotation.** Upstream's example sets one alongside the seccomp
  profile, but Talos ships no AppArmor LSM, so there is nothing to unconfine.
- **`fsGroup: 1000`.** Longhorn presents the volume root-owned and the daemon
  runs as uid 1000, so without it buildkitd cannot write its own cache. The
  same failure the ARC work volume had.
- **Client and daemon versions are pinned in two files** and read by two
  different Renovate managers. A `groupName` in `renovate.json` keeps them in
  one PR; without it the client can merge a release ahead of the daemon.

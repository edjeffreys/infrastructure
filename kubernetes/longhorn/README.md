# Longhorn

Replicated block storage on the worker VMs. Most of what is non-obvious about it
is in the comments in `longhorn-values.yaml` and `recurring-job.yaml`; this file
covers backups.

## Backups

Longhorn replicas only survive one *VM* failing. Every worker VM sits on the one
Proxmox NVMe, so until backups there was no copy of any volume off that disk.

- **Target**: NFS on TrueNAS, `nfs://10.60.0.233:/mnt/main0/longhorn-backups`,
  set through `defaultBackupStore` in `longhorn-values.yaml`. Separate hardware
  from the Proxmox host. The chart writes it to the `longhorn-default-resource`
  ConfigMap, and longhorn-manager copies that onto the `default` BackupTarget.
- **Schedule**: `backup-job.yaml`, RecurringJob `backup-daily`, 04:30 UTC, keeps
  14, one volume at a time. It runs after the 04:00 trim, so blocks that were
  just freed are not copied, and stays clear of the 02:00–03:30 Europe/London
  media restarts in GMT and BST alike. Backups are incremental (2 MiB blocks),
  so only the first run is a full copy.
- **History on the NAS**: a daily ZFS snapshot of the dataset, kept 14 days.
  A backup deleted from the cluster side (by hand, by retention, or by a bad
  `auto-cleanup` setting) can be recovered from that.

### Which volumes, and how they opt in

`backup-daily` targets the group `backup`, not `default`. A volume joins it
through labels on its **PVC**:

```yaml
labels:
  recurring-job.longhorn.io/source: enabled
  recurring-job-group.longhorn.io/default: enabled
  recurring-job-group.longhorn.io/backup: enabled
```

**All three are required, `default` included.** With the `source` label set,
Longhorn treats the PVC as the source of truth and *replaces* the volume's
recurring-job labels with the PVC's: anything missing from the PVC is removed
from the volume (`syncRecurringJobLabelsToTargetResource` in longhorn-manager).
Longhorn only adds `default` itself when a volume has no recurring-job labels at
all. So a PVC labelled with `backup` alone would quietly leave the `default`
group, and `filesystem-trim-daily` would stop trimming it.

| Volume | Where the labels live |
|---|---|
| `actual-budget-data`, `claude-agent-home`, `emby-config`, `mealie-data`, `plex-config`, `radarr-config`, `sabnzbd-config`, `seerr-config`, `sonarr-config` | the app's `pvc.yaml` |
| `paperless-paperless-ngx-data` | `persistence.data.labels` in `kubernetes/paperless/paperless-values.yaml` |
| alertmanager | `kubernetes/monitoring/alertmanager-pvc.yaml` (see below) |
| `arr-postgres-*` | `spec.inheritedMetadata` on the CNPG `Cluster` |

**Alertmanager.** The PVC comes from the StatefulSet's `volumeClaimTemplate`,
and Kubernetes never applies template changes to a PVC that already exists. So
`alertmanager-pvc.yaml` is a partial patch that carries labels only, with the
`ssa: merge` + `prune: disabled` recipe from `kubernetes/kube-system/coredns.yaml`. The
PVC is in the Kustomization's inventory, so without `prune: disabled`, removing
the file would delete Alertmanager's data rather than just its labels. On a
fresh cluster the patch fails until the operator has created the PVC. That
failure is loud and fixes itself.

**CNPG: both instances, not one.** The plan was to back up one instance per
cluster. There is no declarative way to pick one. `inheritedMetadata` is applied
to every PVC in the Cluster, and CNPG reconciles it onto existing PVCs too.
`storage.pvcTemplate` is a bare spec with no metadata. Hand-labelling
`arr-postgres-1` would last only until the instance is rebuilt as
`arr-postgres-3`. A CNPG-native backup (barman to object storage, or
`volumeSnapshot`) would need S3 or the external-snapshotter CRDs, and neither
is installed. Backing up both costs about another 0.9 GB per cluster, for the
first full copy only. The upside is that whichever instance is primary is
always covered. A Longhorn backup is a block-level point-in-time snapshot. With
WAL inside PGDATA (neither Cluster sets `walStorage`), that is crash-consistent,
and Postgres replays WAL on restore exactly as it would after a power cut.

**Deliberately not backed up**: `pinchflat-config` and Grafana (low value, the
user's choice), the Prometheus TSDB (compaction rewrites blocks, so every
incremental would be close to a full copy), NATS JetStream and
`buildkit-cache-single` (rebuildable), ARC runner work volumes (ephemeral, no
PVC), and every `nfs`-class PVC (already on TrueNAS).

Recurring jobs skip detached volumes
(`allow-recurring-job-while-volume-detached: false`). A volume whose workload
is scaled to 0 keeps its last backups but takes no new ones.

Nominal size is about 5 GiB. Expect 10–20 GiB on TrueNAS once 14 incrementals
and the ZFS snapshots have built up.

### Network

No policy change was needed. `allow-longhorn-intra` in `network-policy.yaml`
already grants every `longhorn-system` pod egress to `world`, and
`10.60.0.233` falls under it. Both longhorn-manager (it mounts the store to list
and sync backups) and the instance-managers (they stream the blocks) talk to the
NAS. Longhorn's NFS driver only mounts `nfs4`. It steps down through 4.2, 4.1
and 4.0. That means only TCP 2049, with no rpcbind or mountd. **If that `world`
grant is ever narrowed, add `10.60.0.233/32` on TCP 2049**, or backups fail
while every workload keeps working.

Pod traffic is masqueraded, so TrueNAS sees **node** IPs. Those are the
standard workers, `10.11.0.10-12`, and the PIA workers, `10.200.0.10-11`. The
PIA workers matter because `arr-postgres` has its replicas there. The existing
`/mnt/main0/k3s` export already serves both VLANs, since arr's media PVC mounts
from PIA nodes. Copy its network list.

**Masquerading also rewrites the source *port*, and TrueNAS minds.** Longhorn
mounts from inside its pods on a privileged port (below 1024), and Cilium's BPF
SNAT maps it to an unprivileged one on the node IP
(`10.52.7.173:962 -> 10.200.0.11:34502` in `cilium-dbg bpf nat list`). By
default TrueNAS only accepts NFS from ports below 1024, so every mount fails
with `mount.nfs4: Operation not permitted`, the same error a bad network list
gives. The `nfs` StorageClass never hits this, because the node kernel makes
those mounts itself, without SNAT. The fix is **Allow non-root mount** on the
TrueNAS NFS service (step 2 below). It is service-wide, so the export network
lists are what still restrict access.

**That toggle opens NFS to every pod, so `deny-nfs-egress` closes it again.**
Before the toggle, no pod could speak NFS to the NAS at all, because SNAT
always delivered a high port. Afterwards, any pod with `world` egress could,
from a user-space client (libnfs needs no mount privilege) claiming any UID
under `AUTH_SYS`: read paperless documents and media, or delete Longhorn
backups. `kubernetes/cilium/policies/deny-nfs-egress.yaml` denies
`10.60.0.233:2049` to every pod outside `longhorn-system`. Cilium denies beat
allows, so it holds whatever `world` grants sit next to it. The CSI NFS pods
are `hostNetwork`, so endpoint policy never sees them and the `nfs`
StorageClass is unaffected. The policy sets `enableDefaultDeny: false`.
Without it, a deny-only policy puts every pod it selects into egress
default-deny, which would cut off the kube-system pods that `default-deny`
deliberately excludes.

A new workload that needs NFS from inside its pod (not through a PV) has to be
added to that policy's exclusions. Also keep the export network lists to the
node `/32`s, not whole VLANs.

### TrueNAS setup (by hand, once)

1. **Datasets → `main0` → Add Dataset**: name `longhorn-backups`, preset
   *Generic*. Leave compression at the inherited default (lz4) and dedup off.
   Longhorn already lz4-compresses each block and reuses unchanged blocks across
   a volume's backups, so ZFS dedup would cost RAM and save almost nothing.
2. **Shares → NFS → Add**: path `/mnt/main0/longhorn-backups`.
   - *Networks*: the node IPs above as `/32`s (`10.11.0.10-12`,
     `10.200.0.10-11`). Not the whole `/24`s: with non-root mounts allowed,
     any host in a listed network can mount with any UID.
   - *Advanced → Maproot User* `root`, *Maproot Group* `wheel`. Longhorn writes
     as root, and the default root-squash fails the mount check with
     `permission denied`.
   - Under **System → Services → NFS**, tick **Allow non-root mount** (see
     Network above). NFSv4 is already on, because the `nfs` StorageClass mounts
     with `nfsvers=4.1`.
   - If saving fails with `bindip.0: Cannot use 10.60.0.233`, clear **Bind IP
     Addresses**. TrueNAS only accepts statically configured addresses there,
     and the NAS's interface is on DHCP, so any edit of the NFS service fails
     until the field is empty. Saving restarts NFS, which briefly stalls the
     `nfs`-class mounts.
3. **Data Protection → Periodic Snapshot Tasks → Add**: dataset
   `main0/longhorn-backups`, not recursive, schedule *Daily* at 07:00 (after
   the 04:30 UTC run has finished in both GMT and BST), lifetime **14 days**.

### Verifying after the push

```bash
kubectl -n longhorn-system get backuptarget default        # AVAILABLE=true within ~5 min
kubectl -n longhorn-system get recurringjob backup-daily
kubectl -n longhorn-system get volumes.longhorn.io -L recurring-job-group.longhorn.io/backup,recurring-job-group.longhorn.io/default
```

Every opted-in volume should show `enabled` in **both** columns, and every other
volume `enabled` under `default` only. A volume with `backup` but no `default` means
a PVC lost its `default` label.

If the BackupTarget stays unavailable, its `.status.conditions` give the mount
error. `Operation not permitted` means TrueNAS refused the mount: check
*Allow non-root mount* first, then the export's network list. A
`permission denied` after mounting means maproot.

Do not wait for 04:30 to find out whether it works. Back up one small volume by
hand from the UI (Volume → *Create Backup*, e.g. `seerr-config`), then check
that it appears under **Backup** and as a directory tree under
`/mnt/main0/longhorn-backups/backupstore/` on the NAS.

### Restoring

From the UI: **Backup** → pick the volume → *Restore Latest Backup*, into a new
volume. Then create a PV/PVC for it (*Create PV/PVC*) under the original PVC
name, after scaling the workload to 0 **in git** and deleting the old PVC.
Flux reverts a `kubectl scale`. A restored CNPG volume is a plain PGDATA. The
least fiddly path is to mount it in a throwaway `postgres:18` pod, `pg_dump`
the databases, and load them into the live Cluster. Re-adopting the volume as
an instance means matching CNPG's PVC names and labels exactly.

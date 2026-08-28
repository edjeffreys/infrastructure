Here's the compressed version:

---

# Infrastructure Project Context

Homelab IaC repo. GitOps-managed. User handles all git commits — don't commit for them.

---

## Physical Host

Single Proxmox node: `proxmox-mini-1` — 16 CPU cores, 64GB RAM.

---

## Kubernetes Cluster (Talos)

- **OS**: Talos Linux v1.13.0
- **Kubernetes**: v1.36.1
- **Config**: `talos/talconfig.yaml` (managed via `talhelper`)
- **Generated configs**: `talos/clusterconfig/` — apply with `talosctl apply-config --nodes <ip> --file clusterconfig/<node>.yaml`

### Node layout

| Pool | Count | CPU | RAM | Taint | Purpose |
|------|-------|-----|-----|-------|---------|
| talos-cp-{0,1,2} | 3 | — | — | `control-plane:NoSchedule` | Control plane / etcd only |
| talos-worker-{0,1,2} | 3 | 8 cores | 8GB | — | General workloads |
| talos-worker-pia-{0,1} | 2 | 4 cores | 8GB | `node-type=pia:NoExecute` | arr stack + PIA exit node |

**Node IPs:**
- Control plane: `10.0.0.10-12`
- Workers: `10.11.0.10-12`
- PIA workers: `10.200.0.10-11`

**Node selectors:**
- Standard workloads: `nodeSelector: node-type: standard`
- arr/PIA workloads: `nodeSelector: node-type: pia` + toleration for `node-type=pia:NoExecute`

**Talos gotcha**: Labels/taints in `talconfig.yaml` only fully apply on reboot. Labels apply immediately on `talosctl apply-config`; taints may not. Apply taints via kubectl if needed — Talos reconciles on next reboot.

### CNI: Cilium

- Routing mode: VXLAN tunnel
- MTU `1450` in `cilium-config` (overrides auto-detect — `tailscale0` on nodes has MTU 1280; Cilium would use it for all pods, breaking large downloads)
- `CiliumClusterwideNetworkPolicy` `default-deny`: cluster-wide deny in both directions, excludes `kube-system`, `kube-public`, `kube-node-lease`, `cilium-secrets` — **applied**, managed by `flux/apps/cilium.yaml`
- Every namespace needs `CiliumNetworkPolicy` — see **Network Policy Conventions**
- `coredns-custom` ConfigMap in `kube-system` blocks AAAA responses — no IPv6 outbound
- Hubble UI exposed via Tailscale at `hubble.tail5f17e.ts.net`

---

## Repository conventions

ArgoCD was removed in wave 14 — `argocd/`, `kubernetes/argocd/` and the two
Talos `extraManifests` that installed it are all gone. Flux is the only GitOps
controller. These conventions predate the migration and still hold.

### Adding a new application

1. Create `kubernetes/<app>/` with manifests + `network-policy.yaml` + `<app>-values.yaml` (if Helm)
2. Create `flux/apps/<app>.yaml` — a `Kustomization`, plus a `HelmRepository`
   (in `flux-system`) and a `HelmRelease` (in the app's own namespace) if it's a chart
3. Add `kubernetes/<app>/kustomization.yaml` if the directory holds a
   `-values.yaml`, with an explicit `resources:` list and a `configMapGenerator`
4. Push — the `root` Kustomization picks it up

### Naming convention

**Kustomization name == directory name**, always: `flux/apps/<app>.yaml` manages
`kubernetes/<app>/`. Nothing is prefixed (no `workloads-*`) and no directory
holds two apps.

**Namespaces are left at whatever upstream calls them** and are deliberately
*not* forced to match: `longhorn` → `longhorn-system`, `cnpg` → `cnpg-system`,
`onepassword` → `onepassword-operator`, and `metrics-server`/`storage` →
`kube-system`. Longhorn in particular hardcodes `longhorn-system` internally and
will not start elsewhere. `cilium` and `storage` are cluster-scoped only and own
no namespace.

### Filenames inside `kubernetes/<app>/`

| File | Rule |
|------|------|
| `network-policy.yaml` | always, never prefixed |
| `tailscale-ingress.yaml` / `traefik-ingress.yaml` | named for the ingress path it actually uses — never bare `ingress.yaml` |
| `<name>-secret.yaml` | named after the `OnePasswordItem`/Secret it declares — never bare `secret.yaml` |
| `<name>-values.yaml` | Helm values. **Do not rename to `values.yaml`** — Renovate's runner-image `regexManager` and kubeconform's `-ignore-filename-pattern` both key off this suffix |
| `<component>/` | subdirectory per **workload** once a directory holds more than one — `arr/{sonarr,radarr,…}`, `tdarr/{server,node,node-cpu}`, `plex/postgres`. Resources shared between components stay at the top level (`tdarr/cache-pvc.yaml`, mounted by both server and node) |

`monitoring/` is deliberately flat despite holding several files: they are all
supplements to a *single* Helm release (`kube-prometheus-stack`), not separate
workloads.

> **Check for a `kustomization.yaml` before adding any file to an app.** The
> recursion trap inverted when ArgoCD went. Flux recurses by default, so
> `directory.recurse` is gone and a new subdirectory is picked up on its own —
> *unless* the directory has a `kustomization.yaml`, in which case only what its
> `resources:` list names is applied and anything else is silently ignored. Every
> Helm app has one (it is how `-values.yaml` is kept out of autodetect); the
> plain-manifest apps do not. Silently ignored is the better failure of the two
> — under ArgoCD the same mistake meant `prune: true` deleting the resources the
> dropped files declared.

`ingress.yaml` was ambiguous — it meant a Tailscale Ingress in four places, a
Traefik one in three, and a bare `IngressRoute` in three more. The two ingress
paths behave very differently (Tailscale creates a tailnet device and is subject
to Tailnet Lock; Traefik needs a cert-manager `Certificate`), so the filename
says which.

### Bootstrap secrets (not managed by Flux)

Contain `op://` refs, apply manually with `op inject`:

```bash
op inject -i kubernetes/flux-system/flux-git-auth-secret.yaml | kubectl apply -f -
```

ArgoCD needed two more — `repo-secret.yaml` for git and `oci-repos.yaml` for
ghcr.io. Both retired with it: Flux needs only the git auth Secret, because the
ARC charts it pulls from ghcr.io are public and its `HelmRepository` carries no
`secretRef`.

### Commit and PR conventions

Conventional Commits: `<type>(<scope>): <subject>`, imperative mood, lower case
after the colon. Types in use are `feat`, `fix`, `chore`, `docs`, `refactor` and
`ci`. Scope is the app or subsystem the change belongs to and matches the
directory name where there is one — `feat(cluster-access)`, `fix(kube-system)`,
`chore(flux)`. PR titles follow the same form, because a squash merge makes the
PR title the commit message.

**This is written down because the evidence for it was destroyed.** The
convention used to be self-documenting: 950 commits demonstrated it, and anyone
— human or tool — could infer it from `git log`. The repo was recreated from a
single squashed commit in Aug 2026, so `git log` now shows exactly one
`Initial commit` and teaches nothing. Renovate inferred it too, which is how the
loss first surfaced: see the `semanticCommits` note in `renovate.json`.

The old history is not gone, only moved — `edjeffreys/infrastructure-archive`,
private, and it must stay private (it carries credentials in its history).
Reach for it when you need to know *why* something is the way it is; this repo
cannot answer that question any more.

### Comment conventions

Comments explain **why**, not what — a hidden constraint, a non-obvious
consequence, a workaround, something that would surprise the next reader. If
removing a comment wouldn't confuse someone reading the code, don't write it;
never restate what a well-named field or the line right above it already
shows.

This matters more here than in most repos: the squashed history means
`git log`/`git blame` can't explain a decision (see "Commit and PR
conventions" above), so comments are one of the few places that can — and a
comment that just restates its own line is noise crowding out the ones that
carry real information.

### Renovate

`renovate.json` at root. The `flux` manager watches `flux/**`, raises PRs for
chart bumps. Pin chart `version` to an explicit value (never `*`). The `argocd`
manager was removed in wave 14.

`semanticCommits` is pinned to `enabled` rather than left at its default of
`auto`. Auto-detection reads recent commits to decide whether to prefix PR
titles, so the squash left it nothing to detect and `chore(deps):` quietly
disappeared. Nothing broke — there is no commitlint and no release automation —
but the titles stopped matching the convention above. Pin settings that
Renovate would otherwise infer: this is the same class of silent failure as the
ARC runner image going five releases stale.


---

## GitOps: Flux CD

Flux **v2.9.4**, the only GitOps controller since wave 14.
`kubernetes/flux-system/README.md` is the reference — install steps,
conventions, the per-app migration history, and the ArgoCD uninstall procedure.

```
flux/bootstrap/   GitRepository `infrastructure` + Kustomization `root`; applied by hand
flux/apps/        one Kustomization (plus HelmRelease) per app
```

APIs, all GA: `source.toolkit.fluxcd.io/v1` (GitRepository, HelmRepository,
OCIRepository), `kustomize.toolkit.fluxcd.io/v1` (Kustomization),
`helm.toolkit.fluxcd.io/v2` (HelmRelease).

### Conventions that differ from ArgoCD

| ArgoCD | Flux |
|---|---|
| `$values/kubernetes/<app>/<app>-values.yaml` | `configMapGenerator` + `valuesFrom` — there is no `$values` equivalent |
| `directory.exclude` | an explicit `resources:` list in `kustomization.yaml` |
| `directory.recurse: true` | the default; drop it |
| `ServerSideApply=true` | redundant — Flux always applies server-side |
| `CreateNamespace=true` | a `namespace.yaml`, or `install.createNamespace` on Helm-only apps |
| `destination.namespace` filling in blanks | `targetNamespace` **overrides** — so do not set it on Kustomizations |
| implicit retry until dependencies exist | explicit `dependsOn` |
| the web UI | `flux get all -A` + Grafana + a `gotk_resource_info` alert |

### Flux gotchas

- **Renovate will not infer a namespace.** Its `flux` manager requires an
  explicit `metadata.namespace` on the `HelmRelease` *and* on the
  `HelmRepository`/`OCIRepository` it references — it does not read the parent
  Kustomization. Omit it and chart updates stop arriving, silently. Same class
  of failure as the ARC runner image going five releases stale.
  `scripts/check-flux-refs.py` fails CI on this.
- **`deletionPolicy` defaults to `MirrorPrune`**, so with `prune: true`,
  deleting a Kustomization deletes the workload. Set `deletionPolicy: Orphan`
  while a migration wave is in flight, or a rollback becomes an outage.
- **Helm apps adopt without pre-annotation, but only because Flux does it.**
  ArgoCD renders charts with `helm template`, so resources carry the chart's own
  `app.kubernetes.io/managed-by: Helm` label but *not* the `meta.helm.sh/release-*`
  annotations Helm needs to claim them — a plain `helm install` refuses with
  `invalid ownership metadata`. helm-controller takes ownership by default;
  `install.disableTakeOwnership` and `upgrade.disableTakeOwnership` must stay
  `false`.
- **`storageNamespace` follows the HelmRelease's namespace, not
  `targetNamespace`** — and changing either on a live release *uninstalls and
  reinstalls* it. Get both right the first time.
- **A values change waits for the reconcile interval** unless the generated
  ConfigMap carries `reconcile.fluxcd.io/watch: Enabled`.
- **`configMapGenerator` needs `disableNameSuffixHash: true`** — a hashed name
  would not match `valuesFrom`.
- **Flux resource metrics come from kube-state-metrics, not the controllers.**
  `gotk_resource_info` is produced by the `customResourceState` config in
  `kube-prometheus-stack-values.yaml`. KSM builds its informers at startup and
  never notices a CRD added later, so **restart it after installing Flux** or
  the dashboards and `FluxResourceNotReady` stay silently empty. It does not
  crash, and it logs nothing useful — the metrics are just absent. Do not copy
  upstream's `collectors: []` / `--custom-resource-state-only=true`: that would
  switch off every `kube_pod_*` series in the cluster.
- **Flux's own network policies are self-managed** (`flux/apps/flux-system.yaml`),
  deliberately unlike ArgoCD's own `kubernetes/argocd/network-policy.yaml`,
  which was referenced by no Application and drifted unreconciled from bootstrap
  until it was deleted with ArgoCD in wave 14.
  If a bad policy ever locks the controllers out of GitHub, recover with
  `kubectl -n flux-system delete cnp --all`.

---

## Secrets Management: 1Password

### In-cluster secrets (runtime)

**1Password Connect operator** in `onepassword-operator`. Use `OnePasswordItem` CRDs to create K8s secrets:

```yaml
apiVersion: onepassword.com/v1
kind: OnePasswordItem
metadata:
  name: my-secret
  namespace: my-app
spec:
  itemPath: "vaults/Secrets/items/My Item Name"
```

Resulting Secret keys match 1Password item field names.

### Bootstrap secrets (one-time apply)

For secrets needing specific labels or a type `OnePasswordItem` can't set — it
only takes `spec.itemPath` and always produces `Opaque`:

```bash
op inject -i kubernetes/flux-system/flux-git-auth-secret.yaml | kubectl apply -f -
```

---

## Storage

Two storage classes — pick per workload:

- **`nfs`** (TrueNAS, CSI driver `csi-driver-nfs`): bulk/shared data. `ReadWriteMany`, pods
  reschedule freely (no `ReadWriteOnce` lock-in). Used for large media PVCs (`*-media-nfs`,
  10Ti) and shared/consume dirs (loki, paperless consume/export/media).
- **`longhorn`** (replicated block on worker-node local disk): app config + databases.
  Mostly `ReadWriteOnce`. Used for prometheus, grafana, alertmanager, plex-config (25Gi),
  emby-config, mealie, paperless data, and all arr-stack `-config` volumes.

**Longhorn layout/gotcha**: Longhorn data lives at `/var/lib/longhorn/` on each *worker* VM
(standard + PIA), ~62 GiB per VM, XFS, ~5.7 GiB reserved. All worker VMs sit on the **single
Proxmox physical disk**, and Longhorn keeps 2 replicas per volume across VMs → every write
hits the same physical disk ~2×. This is the main source of I/O contention under load;
keep heavy-write volumes (prometheus retention) modest. Check disk headroom via the
`longhorn.io/v1beta2 Node` CRD `.status.diskStatus` (`storageAvailable`/`storageMaximum`).

---

## Databases: CloudNativePG

CNPG operator in `cnpg-system` (`kubernetes/cnpg/`, chart `cloudnative-pg`). Runs
cluster-wide, so `Cluster` resources live **in the namespace of the workload that uses
them**, not in a central database namespace.

### Why per-namespace clusters

CNPG generates the credentials Secret `<cluster>-app` (type `kubernetes.io/basic-auth`)
in the Cluster's own namespace, so apps consume it directly via `secretKeyRef` — no
1Password plumbing. A shared cluster would need `spec.managed.roles`, whose
`passwordSecret` **must** be `kubernetes.io/basic-auth`, and the `OnePasswordItem` CRD
exposes only `spec.itemPath` and always produces `Opaque` — so every role password would
become a manual `op inject` bootstrap step. It also keeps all DB traffic inside one
namespace under the cluster-wide default-deny.

### Adding a database to an existing cluster

Declare a `postgresql.cnpg.io/v1` `Database` (see `kubernetes/arr/postgres/databases.yaml`)
with `owner` set to the role created by the Cluster's `bootstrap.initdb.owner`. Do not
create roles per app unless you are prepared to hand-manage their basic-auth secrets.

### Current clusters

| Cluster | Namespace | Nodes | Databases |
|---------|-----------|-------|-----------|
| `arr-postgres` | `arr` | pia | `sonarr-main`, `sonarr-log`, `radarr-main`, `radarr-log`, `seerr` |

Sonarr and Radarr each need **two** databases (main + log). The apps still keep their
`-config` PVC — it holds `config.xml`, logs, MediaCover and Backups.

### Migrating an app off SQLite

`scripts/cnpg-migration/` (deliberately outside `kubernetes/`, which Flux prunes).
The app must create its own schema first, the schema must then be emptied, and sequences
must be reset afterwards — see the README there. Pointing pgloader at a fresh database,
as the upstream Servarr guide says, fails on PostgreSQL 15+.

### Not migrated

sabnzbd has no database; plex, emby and pinchflat have no upstream Postgres support.
mealie, paperless and grafana all support it and are still on SQLite.

actual-budget is permanent, not pending: its sync protocol makes each budget a
SQLite file the server ships to clients whole, so there is nothing to migrate.

**Backups**: none yet beyond Longhorn snapshots. A logical `pg_dump` CronJob to the `nfs`
storage class is the outstanding action.

---

## Network Policy Conventions

Every namespace needs policies. `default-deny` is live, so a namespace without
them is fully isolated rather than fully open — the failure is loud, but it is
still a failure.

> **`default-deny` is applied** (`kubernetes/cilium/policies/default-deny.yaml`,
> managed by `flux/apps/cilium.yaml`). It denies both directions for every
> endpoint outside `kube-system`, `kube-public`, `kube-node-lease` and
> `cilium-secrets`.
>
> Turning it on changed nothing for any running workload — all 129 live pods
> were already deny-by-default in both directions. Ingress was covered
> cluster-wide by `allow-kubelet-probes` and `allow-proxy-identity`, which each
> select every pod outside the exclusions; egress was covered because every
> namespace ships at least an `allow-dns`. Its value is as the catch-all for the
> *next* namespace, not as a tightening of the current one.
>
> Two consequences worth knowing before touching any of this:
>
> - **`allow-health-checks` carries an egress rule that exists only because of
>   `default-deny`.** The Cilium health endpoint has no
>   `k8s:io.kubernetes.pod.namespace` label, and a missing label satisfies
>   `NotIn`, so `default-deny` selects it too. Remove that egress rule and
>   node-to-node `cilium-health` breaks with no workload symptom.
> - **A namespace that fetches its own policies from git cannot bootstrap under
>   `default-deny`** — it needs the policy to reach git and git to get the
>   policy. `flux-system` is that case; its `network-policy.yaml` is applied by
>   hand before `flux/bootstrap/`.
>
> Re-check coverage with `scripts/check-policy-coverage.py` before adding a
> namespace. It reads the live cluster, not the repo, and exits 1 if any pod is
> unrestricted. Note that "has a policy file" is not "is constrained": Cilium is
> deny-by-default *per direction*, so a namespace with only an egress `allow-dns`
> had unrestricted ingress until `default-deny` landed.

### Standard boilerplate

```yaml
# Always needed
- name: allow-dns              # UDP/TCP 53 to kube-system/kube-dns
- name: allow-intra-namespace  # if multiple pods in namespace talk to each other
- name: allow-kube-apiserver   # if pod talks to Kubernetes API
```

### Kube-apiserver access

Use **both** (covers node connections + ClusterIP):

```yaml
egress:
  - toEntities:
      - kube-apiserver
  - toServices:
      - k8sService:
          serviceName: kubernetes
          namespace: default
```

### Tailscale ingress

Services exposed via Tailscale need:
1. Ingress policy in target namespace from `tailscale` namespace
2. Entry in `kubernetes/tailscale/network-policy.yaml` `allow-backend-egress` for target namespace

```yaml
ingress:
  - fromEndpoints:
      - matchLabels:
          k8s:io.kubernetes.pod.namespace: tailscale
```

---

## Networking: Tailscale

- Tailscale operator in `tailscale` namespace
- All services via `tailnet: tail5f17e.ts.net` (no public internet)
- Ingress resources use `ingressClassName: tailscale` and `tailscale.com/proxy-class: "standard"`
- ProxyClasses: `standard` (standard nodes), `pia-exit` (PIA nodes, for exit node connector)
- Exit nodes: `vpn-home` (standard nodes) and `vpn-pia` (PIA nodes) via `Connector` CRDs
- Operator **not** configured with oauth in `operator-values.yaml` — reads pre-existing `operator-oauth` Secret automatically

---

## Cluster Access (kubeconfig for other devices)

The Kubernetes API is reachable only over the tailnet, at
`https://talos-cp-{0,1,2}.tail5f17e.ts.net:6443`.

`additionalApiServerCertSans` in `talconfig.yaml` puts those MagicDNS names in
the API server cert. Before that the cert had `certSANs: []` — only
`talos-cp-0` and the node/VIP IPs — so any client using a tailnet hostname had
to carry `tls-server-name: talos-cp-0` to pass verification. Older kubeconfigs
still have that field; it's harmless but no longer needed.

Second-device credentials are a ServiceAccount, not a Talos x509 admin cert: a
cert can't be revoked short of rotating the Kubernetes CA. See
`kubernetes/cluster-access/`, managed by `flux/apps/cluster-access.yaml` — SA
`remote-admin` in namespace `cluster-access`, bound to `cluster-admin`, with a
long-lived `kubernetes.io/service-account-token` Secret `remote-admin-token`.

These were live in the cluster but absent from the repo until they were adopted
into Flux; nothing reconciled them, and this file described a directory that did
not exist.

**Revoke from git**, since the Kustomization has `prune: true`:

- delete `clusterrolebinding.yaml` — the token survives but can do nothing
- delete `remote-admin-token-secret.yaml` — kills the token outright

**The token Secret must never declare `.data`.** The token controller populates
it after creation. Under ArgoCD this needed an `ignoreDifferences` block or
selfHeal wiped the token on every sync, silently killing the credential; the
manifest now carries `kustomize.toolkit.fluxcd.io/ssa: merge` so Flux cannot
take ownership of the field.

> `scripts/gen-remote-kubeconfig.sh` is referenced by older notes but is not in
> the repo. Build the kubeconfig by hand from the `remote-admin-token` Secret
> and one of the `talos-cp-{0,1,2}.tail5f17e.ts.net:6443` endpoints.

**Applying certSANs changes reboots the node.** Do the control plane one at a
time, waiting for `Ready` in between — three nodes means etcd tolerates exactly
one down.

---

## Terraform

- **Provider**: Proxmox (`bpg/proxmox`)
- **Backend**: Terraform Cloud — org `Skaal`, workspace `proxmox`
- **Parallelism**: `TF_CLI_ARGS_apply=-parallelism=1` — prevents simultaneous VM changes
- VMs provisioned via cloud-init; no Packer templates

---

## GitHub Actions: ARC

See `kubernetes/arc-systems/README.md` for the full setup and a diagnosis runbook.

- Self-hosted runners via ARC in `arc-systems` / `arc-runners`
- Runner scale set: **`arc-runners`** — so `runs-on: arc-runners`
- Helm charts from `oci://ghcr.io/actions/actions-runner-controller-charts/`
  - Controller chart: `gha-runner-scale-set-controller`
  - Scale set chart: `gha-runner-scale-set`
- Runners are on standard (non-PIA) nodes, `minRunners: 0` / `maxRunners: 4`
- No registry credentials needed: the ARC charts on ghcr.io are public, so the `HelmRepository` has no `secretRef`. ArgoCD needed a `repo-ghcr-arc` bootstrap Secret; it retired in wave 14

---

## Workloads

| App | Namespace | Nodes | Notes |
|-----|-----------|-------|-------|
| plex | plex | worker-0 only | QuickSync via passed-through iGPU (`gpu: intel`) |
| intel-gpu-plugin | intel-gpu | worker-0 only | Advertises `gpu.intel.com/i915` |
| tdarr-server | tdarr | standard | UI 8265, node protocol 8266, `internalNode: false` |
| tdarr-node | tdarr | worker-0 only | Holds the second `i915` device; cache on `nfs` |
| tdarr-node-cpu | tdarr | standard, non-GPU | No `/dev/dri` — CPU-only work only |
| radarr | arr | pia | Postgres (`radarr-main`/`radarr-log` on `arr-postgres`) |
| sonarr | arr | pia | Postgres (`sonarr-main`/`sonarr-log` on `arr-postgres`) |
| sabnzbd | arr | pia | CPU limit 3000m for par2/unpack |
| seerr | arr | pia | Postgres (`seerr` on `arr-postgres`) |
| arr-postgres | arr | pia | CNPG cluster for the arr stack |
| mealie | mealie | standard | |
| actual-budget | actual-budget | standard | SQLite only — see "Not migrated" above |
| paperless | paperless | standard | Helm chart, Tailscale ingress |
| homeassistant | homeassistant | external | ExternalName proxy → `homeassistant.mng.skaal.lan:8123`, Traefik ingress only |

All workloads via Tailscale ingress. Traefik installed, internal only (+ homeassistant proxy via `home.skaal.dev`).

---

## Key Gotchas

- **Cilium MTU**: `1450` in `cilium-config`. Don't remove — `tailscale0` at MTU 1280 would be auto-detected, breaking large downloads.
- **CoreDNS AAAA block**: `coredns-custom` in `kube-system` returns NOERROR/empty for AAAA. No IPv6 outbound.
- **CoreDNS spread**: `kubernetes/kube-system/coredns.yaml` patches CoreDNS with topology spread + anti-affinity (excludes PIA nodes). Managed by the `kube-system` Flux Kustomization. It is a *partial* Deployment, so it carries `kustomize.toolkit.fluxcd.io/ssa: merge` plus a selector and an empty `containers: []` — without those Flux treats it as the full desired state and the Kustomization goes NotReady. `prune: false` on the Kustomization stops a removal deleting cluster DNS.
- **ARC CRDs**: `ServerSideApply=true` in `arc-systems` syncOptions — CRDs too large for client-side apply.
- **ARC `Outdated` is a terminal phase, and a stale runner image causes it.** GitHub deprecates old runner versions; an ephemeral runner can't self-update, so it exits 7, ARC marks it `Outdated`, and that propagates EphemeralRunner → EphemeralRunnerSet → AutoscalingRunnerSet. Every rebuild path in the controller is gated behind `!outdated` and nothing ever clears the phase, so the scale set deregisters itself from GitHub, re-registers, fails again — churning scale set IDs while jobs queue. Looks like a network or credentials fault; it is neither. Fix is **both** bumping `image:` in `runner-scale-set-values.yaml` *and* deleting the AutoscalingRunnerSet so Flux recreates it with an empty status.
- **Renovate can't see images in `*-values.yaml`**: the `kubernetes` manager only reads manifest-shaped files, and Helm values have no `apiVersion`/`kind`. That is how the ARC runner image went five releases stale unnoticed. `runner-scale-set-values.yaml` is the only values file in the repo with an image pin, and a `regexManager` now covers it — but check this before assuming Renovate is watching any new values file.
- **ARC runner label is the Helm release name** — today `arc-runners`, pinned explicitly as `releaseName` on the HelmRelease in `flux/apps/arc-runners.yaml` for exactly this reason. Change it and every `runs-on:` in `.github/workflows/` is silently invalidated: jobs queue forever against a label nothing offers, with no error anywhere. Under ArgoCD the name came implicitly from the Application name, so a file rename was enough to break it.
- **ARC's `githubConfigSecret` resolves in the AutoscalingRunnerSet's own namespace**, not the controller's — hence the same `OnePasswordItem` declared in both `arc-systems` and `arc-runners`.
- **ARC needs all three GitHub App fields**: `github_app_id`, `github_app_installation_id`, `github_app_private_key`. `hasGitHubAppAuth()` is all-or-nothing and `FromSecret` doesn't error on an absent key, so omitting *one* reports `no credentials provided ...` as though the Secret were empty. The scale set then sits at `status.phase: Pending` with no `runner-scale-set-id` annotation, no listener pod is created, jobs queue with no explanation in the GitHub UI — **and the Flux HelmRelease still reports Ready**, because that only means the chart installed, not that the scale set registered. Check `runner-scale-set-id` first; empty means it never registered.
- **The 1Password operator does not reliably re-poll**: after editing an item, `kubectl -n onepassword-operator rollout restart deploy/onepassword-connect-operator`. Then restart the consuming controller too — ARC's reconcile backoff reaches ~16 minutes, so a fixed Secret can appear not to work for a quarter of an hour.
- **CNPG CRDs**: same — `ServerSideApply=true` in the `cnpg` app's syncOptions.
- **CNPG webhooks**: the chart installs mutating + validating webhooks with `failurePolicy: Fail`. `cnpg-system` needs a `CiliumNetworkPolicy` allowing ingress from `kube-apiserver`/`remote-node` on **9443**, or every apply of a CNPG resource is rejected with an opaque webhook timeout — including Flux's. First thing to check if the `cnpg` HelmRelease stops going Ready.
- **CNPG role secrets**: `spec.managed.roles` requires a `kubernetes.io/basic-auth` Secret, which `OnePasswordItem` cannot produce. Prefer one Cluster per namespace and reuse the auto-generated `<cluster>-app` Secret.
- **New CNPG namespace ⇒ update `kubernetes/cnpg/network-policy.yaml`**: `allow-instance-egress` lists the namespaces the operator may reach its instances in. Omit one and it fails *quietly* — Postgres serves queries normally, but the Cluster hangs at `Instance Status Extraction Error: HTTP communication issue` and never builds its replica.
- **CNPG single-instance blocks node drains**: CNPG gives every Cluster a PDB, and for `instances: 1` that PDB deliberately prevents draining the node the instance is on — there is nowhere to switch over to, so a Talos upgrade drain hangs indefinitely rather than failing. Use `instances: 2` with `podAntiAffinityType: required`, or set `enablePDB: false` and accept the outage. Applies to any new Cluster, not just `arr-postgres`.
- **Descheduler vs PVC pods**: `LowNodeUtilization` evicts via the Eviction API, so PDBs are respected — CNPG instances are safe without extra config. Do *not* reach for `ignorePvcPods: true`: nearly every workload here has an RWO Longhorn PVC, so it would switch the descheduler off cluster-wide.
- **arr migration to Postgres**: scale the Deployment to 0 **in git** — `kubectl scale` is reverted by `selfHeal` mid-migration. See `scripts/cnpg-migration/README.md`.
- **Seerr's `migrations` table is dialect-specific**: SQLite and Postgres have separate migration sets, so it must NOT be copied during a migration or Seerr crashloops replaying `InitialMigration...` over an existing schema. The arr apps' `VersionInfo` is dialect-neutral and *should* be copied. Handled by `EXCLUDE_TABLES` in `migrate.sh`.
- **Tailscale StatefulSets**: created by the Tailscale operator, not from git. ArgoCD needed `resourceExclusions` to stop diffing them; Flux never sees them, since they are not in any Kustomization inventory.
- **Tailscale ProxyClass**: all ingresses use `tailscale.com/proxy-class: "standard"` (not `"default"` — renamed).
- **iGPU passthrough is exclusive**: the MS-01's single Alder Lake iGPU (`8086:46a6`, `0000:00:02.0`) is passed to `talos-worker-0` alone, so `gpu: intel` labels exactly one node and plex is pinned to it. Sharing it more widely means SR-IOV (out-of-tree DKMS on the Proxmox host, breaks on kernel bumps) — don't. The host must keep `i915` blacklisted and `softdep i915 pre: vfio-pci`, or the VM won't start.
- **GPU devices are fully allocated**: the plugin advertises `gpu.intel.com/i915: 2` (`sharedDevNum`), and plex + tdarr-node hold one each. Anything else requesting the resource — emby, a second tdarr node — sits `Pending` until `sharedDevNum` is raised in `kubernetes/intel-gpu/daemonset.yaml`. It's an accounting limit, not a hardware one.
- **Tdarr UI actions fail over the Tailscale Ingress (known, accepted)**: the UI is served on 8265 but the browser calls the server API on 8266 directly, and an Ingress carries one backend port — so the page renders while Start Scan / delete library / delete past jobs fail silently. `tailscale.com/expose` on the Service fixes it (all ports on one tailnet host) but is layer-4, so it drops HTTPS and changes the URL to `http://tdarr.tail5f17e.ts.net:8265`; tried and reverted. Workaround: `kubectl -n tdarr port-forward svc/tdarr-server 8265:8265 8266:8266`.
- **Tailnet Lock signs devices, not services**: swapping a Tailscale Ingress for an exposed Service (or vice versa) destroys one tailnet device and creates another, and the new one is **locked out** until signed — no connectivity, and nothing in the app logs explains it. Check `tailscale lock status` inside the `ts-<name>-0` pod in `tailscale`, then run the printed `tailscale lock sign ...` on a node holding a trusted signing key. Also: the old hostname served HTTPS, so browsers may hold HSTS for it and silently upgrade `http://` back to `https://`.
- **Tdarr worker type is only a queue label**: "GPU worker" vs "CPU worker" controls which pool claims a job, *not* which encoder ffmpeg uses — that comes from the flow. So a flow specifying `hevc_qsv` that lands on `tdarr-node-cpu` fails outright: no `/dev/dri` there. Any video-encode step must be preceded by a `tagsWorkerType: GPU` node once both nodes exist.
- **Tdarr cache must not go on longhorn**: the node writes a full working copy of every file it processes. On longhorn that's 2 replicas onto the one Proxmox physical disk. It uses the `nfs` class deliberately.
- **Talos extensions need `upgrade`, not `apply-config`**: adding `siderolabs/i915` to `talconfig.yaml` changes the *installer image*. Existing nodes only pick it up via `talosctl upgrade --image factory.talos.dev/metal-installer/<id>:<ver>`. The schematic hash covers list **order**, so read it from `talhelper genurl installer` rather than recomputing — and keep `talos_worker_schematic_id` in `terraform/proxmox/talos.tf` in step, or a rebuilt node comes back without extensions.
- **PIA node taint**: `node-type=pia:NoExecute` only — `NoExecute` prevents scheduling; `NoSchedule` redundant, don't add.
- **PSA privileged namespaces**: `tailscale`, `monitoring`, `loki`, `longhorn-system` need `pod-security.kubernetes.io/enforce: privileged` — enforced via namespace manifests in git.
- **Bootstrap order**: 1Password operator healthy before `OnePasswordItem` creates secrets. cert-manager healthy before issuers work.

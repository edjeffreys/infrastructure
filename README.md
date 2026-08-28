# Infrastructure

Homelab infrastructure-as-code. A single Proxmox host running a Talos Linux
Kubernetes cluster, reconciled from this repository by Flux CD.

Nothing here is exposed to the public internet: every service is reached over a
Tailscale tailnet, and the Kubernetes API itself is only reachable that way.

## Stack

| Layer | Tool |
|-------|------|
| Hypervisor | Proxmox |
| VM provisioning | Terraform (Terraform Cloud) |
| Kubernetes | Talos Linux (managed with `talhelper`) |
| CNI | Cilium (VXLAN, cluster-wide default-deny) |
| GitOps | Flux CD v2 |
| Ingress | Tailscale operator (private), Traefik (internal LAN) |
| Certificates | cert-manager, Cloudflare DNS-01 |
| Secrets | 1Password Connect operator |
| Storage | Longhorn (replicated block), TrueNAS (NFS) |
| Databases | CloudNativePG |
| Observability | kube-prometheus-stack, Loki, Hubble |
| CI/CD | GitHub Actions (ARC self-hosted runners) |
| Dependency updates | Renovate |

## Cluster layout

```
proxmox-mini-1 (16 CPU, 64GB RAM)
├── talos-cp-{0,1,2}          — control plane / etcd (10.0.0.10-12)
├── talos-worker-{0,1,2}      — general workloads (10.11.0.10-12, 8 cores, 8GB)
└── talos-worker-pia-{0,1}    — arr stack + PIA exit node (10.200.0.10-11, 4 cores, 8GB)
```

PIA nodes are tainted `node-type=pia:NoExecute` — only workloads with an
explicit toleration schedule there. `talos-worker-0` additionally holds the
passed-through Intel iGPU (`gpu: intel`), which is why Plex and the GPU Tdarr
node are pinned to it.

## Repository structure

```
.github/workflows/       — manifest validation, Talos upgrade, agent image build
flux/
  bootstrap/             — GitRepository + root Kustomization; applied by hand
  apps/                  — one Kustomization (± HelmRelease) per app
kubernetes/            — one directory per app, named after its Kustomization
  arc-runners/           — Actions Runner Controller scale set
  arc-systems/           — Actions Runner Controller controller
  arr/                   — radarr, sonarr, sabnzbd, seerr + their CNPG cluster
  cert-manager/          — ClusterIssuer + Cloudflare DNS-01 solver
  cilium/                — Cilium values (bootstrap) + policies/; see its README
  claude-agent/          — in-cluster read-only diagnostic agent; see its README
  cluster-access/        — ServiceAccount + token for second-device kubeconfigs
  cnpg/                  — CloudNativePG operator
  descheduler/           — node rebalancing
  emby/                  — media server
  flux-system/           — Cilium policies for Flux's own namespace; see its README
  goldilocks/            — VPA recommendation dashboard
  homeassistant/         — ExternalName proxy to Home Assistant on the LAN
  intel-gpu/             — Intel GPU device plugin
  kube-system/           — CoreDNS patches, Cilium metrics, Hubble UI ingress
  loki/                  — log aggregation
  longhorn/              — replicated block storage
  mealie/                — recipe manager
  metrics-server/        — Metrics API (kubectl top, VPA)
  monitoring/            — kube-prometheus-stack (Prometheus + Grafana + Alertmanager)
  onepassword/           — 1Password Connect operator
  paperless/             — document management
  pinchflat/             — YouTube archiver
  plex/                  — media server
  reloader/              — auto-restart pods on ConfigMap/Secret changes
  storage/               — NFS + Longhorn StorageClasses
  storj/                 — Storj node ingress
  tailscale/             — Tailscale operator, ProxyClasses, exit node Connectors
  tdarr/                 — transcoding server + GPU and CPU nodes
  traefik/               — Traefik ingress controller + its Cilium LB IP pool
  truenas/               — TrueNAS UI ingress
images/claude-agent/     — container image built by GitHub Actions via Kaniko
scripts/                 — validation checks, migrations, operational helpers
talos/                   — Talos node configuration only
  talconfig.yaml         — talhelper cluster config (versions, nodes, patches)
  manifests/             — bootstrap manifests (Cilium, CSI driver, RBAC)
terraform/proxmox/       — Proxmox VM definitions
```

Two conventions worth knowing before adding anything:

- **A Kustomization is named after its directory.** `flux/apps/<app>.yaml`
  manages `kubernetes/<app>/`. Namespaces are deliberately *not* forced to
  match — they stay at whatever upstream calls them.
- **Check for a `kustomization.yaml` first.** Flux recurses by default, so a new
  file is picked up automatically — unless the directory has one, in which case
  only what its `resources:` list names is applied and anything else is silently
  ignored. Every Helm app has one; that is how `-values.yaml` files are kept out
  of manifest autodetection.

`CLAUDE.md` carries the long-form version of these, along with the accumulated
gotchas behind most of the non-obvious decisions here.

## Services

All private, over the tailnet:

| Service | Host |
|---------|------|
| Grafana | `grafana.tail5f17e.ts.net` |
| Alertmanager | `alertmanager.tail5f17e.ts.net` |
| Hubble UI | `hubble.tail5f17e.ts.net` |
| Longhorn | `longhorn.tail5f17e.ts.net` |
| Goldilocks | `goldilocks.tail5f17e.ts.net` |
| Plex | `plex.tail5f17e.ts.net` |
| Emby | `emby.tail5f17e.ts.net` |
| Tdarr | `tdarr.tail5f17e.ts.net` |
| Radarr / Sonarr | `radarr.` / `sonarr.tail5f17e.ts.net` |
| SABnzbd | `sabnzbd.tail5f17e.ts.net` |
| Seerr | `seerr.tail5f17e.ts.net` |
| Mealie | `mealie.tail5f17e.ts.net` |
| Paperless | `paperless.tail5f17e.ts.net` |
| Pinchflat | `pinchflat.tail5f17e.ts.net` |

A few also carry a Traefik ingress on `*.skaal.dev`, resolvable on the LAN
only — Home Assistant is Traefik-only, since it runs outside the cluster.

## Secrets

**There are no credentials in this repository, and there never should be.**

Runtime secrets are declared as `OnePasswordItem` CRDs carrying only a
`vaults/.../items/...` path; the 1Password Connect operator resolves them into
Kubernetes Secrets in-cluster. A handful of bootstrap secrets that the CRD
cannot express are checked in as manifests templated with `op://` references and
applied once by hand:

```bash
op inject -i kubernetes/flux-system/flux-git-auth-secret.yaml | kubectl apply -f -
```

The Talos cluster PKI (`talos/talsecret.yaml`), the generated node configs and
the `talosconfig` admin certificate are all gitignored and live only in
1Password.

## Bootstrap

A freshly provisioned cluster needs a few one-time steps before GitOps takes
over.

### 1. Provision VMs

```bash
cd terraform/proxmox
terraform apply
```

### 2. Generate and apply Talos configs

```bash
cd talos
TAILSCALE_AUTH_KEY=$(op read "op://Secrets/Tailscale Talos/credential") \
GITHUB_TOKEN=$(op read "op://Secrets/Github ArgoCD Token/credential") \
  talhelper genconfig
talosctl apply-config --insecure --nodes <ip> --file clusterconfig/<node>.yaml
talosctl bootstrap --nodes 10.0.0.10
```

Cilium and the NFS CSI driver come up from `cluster.extraManifests` in
`talconfig.yaml`. No GitOps controller is bootstrapped that way — see below.

### 3. Install the 1Password operator

Flux needs a git credential, and that credential comes from 1Password:

```bash
op inject -i kubernetes/onepassword/1password-token-secret.yaml | kubectl apply -f -
```

### 4. Install Flux

```bash
kubectl apply -f https://github.com/fluxcd/flux2/releases/download/v2.9.4/install.yaml
kubectl apply -f kubernetes/flux-system/network-policy.yaml
op inject -i kubernetes/flux-system/flux-git-auth-secret.yaml | kubectl apply -f -
kubectl apply -f flux/bootstrap/
```

The order matters. The cluster-wide `default-deny` policy means a namespace
without a `CiliumNetworkPolicy` can reach nothing — and Flux fetches its own
policies from GitHub, so without applying that policy by hand first the
controllers cannot reach GitHub to fetch the policy that would let them reach
GitHub. This is also why Flux is not installed from `extraManifests`: Talos
applies those verbatim and can neither run `op inject` nor sequence around the
deadlock.

Flux reconciles everything else from git. `kubernetes/flux-system/README.md` has
the full detail, including the kube-state-metrics restart that the Flux
dashboards need.

## Operations

### Adding an application

1. Create `kubernetes/<app>/` with manifests and a `network-policy.yaml`
2. Create `flux/apps/<app>.yaml` — a `Kustomization`, plus a `HelmRepository`
   and `HelmRelease` if it is a chart
3. Add a `kustomization.yaml` with an explicit `resources:` list if the
   directory holds a `-values.yaml`
4. Push — the `root` Kustomization picks it up

Every namespace needs Cilium policies, at minimum `allow-dns` plus whatever
egress the app actually needs; `scripts/check-policy-coverage.py` checks the
live cluster for pods that have none. Services exposed over Tailscale also need
ingress from the `tailscale` namespace and a matching entry in
`kubernetes/tailscale/network-policy.yaml`.

### Validation

`.github/workflows/validate-manifests.yaml` runs on every pull request:
yamllint, `kubeconform` against the Kubernetes version read from
`talconfig.yaml`, a reference check (`scripts/check-flux-refs.py`) for
Kustomization paths, `sourceRef`s and the `metadata.namespace` that Renovate
requires, and `flux build --dry-run` over the whole tree so a Kustomization that
would fail to reconcile fails the PR instead.

### Upgrading Talos / Kubernetes

Renovate raises PRs against `talosVersion` and `kubernetesVersion` in
`talos/talconfig.yaml`. After merging, run the **Talos Upgrade** workflow — it
reads both versions from that file and rolls control plane, workers and PIA
workers in turn, then upgrades the Kubernetes components.

Upgrades must use the Image Factory installer carrying each pool's schematic;
the stock installer ships no system extensions and would silently strip
iscsi-tools, nfs-utils and tailscale, taking Longhorn and NFS down cluster-wide.
Both schematic IDs are pinned in the workflow.

## License

MIT — see [LICENSE](LICENSE).

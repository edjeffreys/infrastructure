# Infrastructure

Homelab infrastructure-as-code. Single Proxmox host running a Talos Kubernetes cluster, managed via GitOps.

## Stack

| Layer | Tool |
|-------|------|
| Hypervisor | Proxmox |
| VM provisioning | Terraform (Terraform Cloud) |
| Kubernetes | Talos Linux |
| CNI | Cilium (VXLAN) |
| GitOps | ArgoCD |
| Ingress | Tailscale operator |
| Secrets | 1Password Connect operator |
| Storage | TrueNAS (NFS) |
| CI/CD | GitHub Actions (ARC self-hosted runners) |
| Dependency updates | Renovate |

## Cluster Layout

```
proxmox-mini-1 (16 CPU, 64GB RAM)
├── talos-cp-{0,1,2}          — control plane / etcd (10.0.0.10-12)
├── talos-worker-{0,1,2}      — general workloads (10.11.0.10-12, 8 cores, 8GB)
└── talos-worker-pia-{0,1}    — arr stack + PIA exit node (10.200.0.10-11, 4 cores, 8GB)
```

PIA nodes are tainted `node-type=pia:NoExecute` — only workloads with explicit tolerations schedule there.

## Repository Structure

```
.github/workflows/       — Talos upgrade workflow
argocd/
  apps/                  — one Application manifest per workload
  root-app.yaml          — bootstrapped once; watches argocd/apps/
flux/                  — Flux CD, replacing ArgoCD; both run during the migration
  apps/                  — one Kustomization (plus HelmRelease) per workload
  bootstrap/             — GitRepository + root Kustomization; applied by hand
kubernetes/            — one directory per application, named after its Application
  arc-runners/           — Actions Runner Controller scale set
  arc-systems/           — Actions Runner Controller controller
  arr/                   — radarr, sonarr, sabnzbd, seerr
  cert-manager/          — ClusterIssuer + Cloudflare DNS-01 solver
  cilium/                — Cilium values (bootstrap) + policies/ (GitOps); see its README
  descheduler/           — node rebalancing
  emby/                  — media server
  flux-system/           — Cilium policies for Flux's own namespace; see its README
  goldilocks/            — VPA recommendation dashboard
  homeassistant/         — Home Assistant
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
  storage/               — NFS StorageClass
  tailscale/             — Tailscale operator, ProxyClasses, exit node Connectors
  traefik/               — Traefik ingress controller + its Cilium LB IP pool
  truenas/               — TrueNAS UI ingress
talos/                 — Talos node configuration only
  talconfig.yaml         — talhelper cluster config (versions, nodes, patches)
  clusterconfig/         — generated node configs (apply with talosctl apply-config)
  manifests/             — bootstrap manifests (Cilium, ArgoCD, CSI driver)
terraform/proxmox/       — Proxmox VM definitions
```

## Workloads

All services are exposed privately via Tailscale (`*.tail5f17e.ts.net`) — nothing is public.

| Service | URL |
|---------|-----|
| ArgoCD | argocd.tail5f17e.ts.net |
| Grafana | grafana.tail5f17e.ts.net |
| Hubble UI | hubble.tail5f17e.ts.net |
| Goldilocks | goldilocks.tail5f17e.ts.net |
| Plex | plex.tail5f17e.ts.net |
| Radarr | radarr.tail5f17e.ts.net |
| Sonarr | sonarr.tail5f17e.ts.net |
| SABnzbd | sabnzbd.tail5f17e.ts.net |
| Seerr | seerr.tail5f17e.ts.net |
| Mealie | mealie.tail5f17e.ts.net |
| Home Assistant | homeassistant.tail5f17e.ts.net |

## Bootstrap

A freshly provisioned cluster requires a few one-time steps before GitOps takes over.

### 1. Generate and apply Talos configs

```bash
cd talos
talhelper gen config
talosctl apply-config --insecure --nodes <ip> --file clusterconfig/<node>.yaml
talosctl bootstrap --nodes 10.0.0.10
```

### 2. Install ArgoCD

```bash
helm repo add argo https://argoproj.github.io/argo-helm
helm install argocd argo/argo-cd -n argocd --create-namespace -f kubernetes/argocd/argocd-values.yaml
```

### 3. Apply bootstrap secrets

```bash
# Git repo credentials for ArgoCD
op inject -i kubernetes/argocd/repo-secret.yaml | kubectl apply -f -

# ghcr.io credentials for ARC Helm charts
op inject -i kubernetes/argocd/oci-repos.yaml | kubectl apply -f -
```

### 4. Apply root app

```bash
kubectl apply -f argocd/root-app.yaml
```

ArgoCD will reconcile everything else from git.

## Operations

### Adding a new application

1. Create `kubernetes/<app>/` with manifests and a `network-policy.yaml`
2. Create `argocd/apps/<app>.yaml` using the multi-source Application pattern
3. Push — ArgoCD deploys automatically

Every namespace needs Cilium network policies or pods will be blocked by the cluster-wide default deny. At minimum: `allow-dns` + whatever egress the app needs. Services exposed via Tailscale also need ingress from the `tailscale` namespace and a backend egress entry in `kubernetes/tailscale/network-policy.yaml`.

### Upgrading Talos / Kubernetes

Renovate raises PRs when new versions are available (tracking `talosVersion` and `kubernetesVersion` in `talos/talconfig.yaml`). After merging:

1. Actions → **Talos Upgrade** → Run workflow

The workflow reads versions from `talconfig.yaml` and performs a rolling upgrade across control plane, standard workers, and PIA workers, followed by a Kubernetes component upgrade.

### Secrets

Runtime secrets are managed via `OnePasswordItem` CRDs in each namespace. The 1Password Connect operator syncs them into standard Kubernetes secrets automatically.

Bootstrap secrets (ArgoCD repo credentials, ARC ghcr.io credentials) use `op inject` and are applied once — they are excluded from ArgoCD selfHeal.

# Infrastructure Project Context

This is a homelab infrastructure-as-code repository. Everything is managed via GitOps where possible. The user handles all git commits — do not commit on their behalf.

---

## Physical Host

Single Proxmox node: `proxmox-mini-1` — 16 CPU cores, 64GB RAM.

---

## Kubernetes Cluster (Talos)

- **OS**: Talos Linux v1.12.6
- **Kubernetes**: v1.35.2
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

**Talos gotcha**: node labels and taints are defined in `talconfig.yaml` but only fully apply on reboot. Labels apply immediately on `talosctl apply-config`; taints may not. Apply taints via kubectl if needed immediately — Talos will reconcile on next reboot.

### CNI: Cilium

- Routing mode: VXLAN tunnel
- MTU explicitly set to `1450` in `cilium-config` (overrides auto-detection — the `tailscale0` interface on nodes has MTU 1280 which Cilium would otherwise use for all pods, breaking large downloads)
- `CiliumClusterwideNetworkPolicy` named `default-deny` applies a cluster-wide default deny, excluding `kube-system`, `kube-public`, `kube-node-lease`, `cilium-secrets`
- Every namespace needs its own `CiliumNetworkPolicy` resources — see **Network Policy Conventions** below
- CoreDNS has a custom ConfigMap (`coredns-custom` in `kube-system`) blocking AAAA responses — cluster has no IPv6 outbound path
- Hubble UI exposed via Tailscale at `hubble.tail5f17e.ts.net`

---

## GitOps: ArgoCD

### App-of-apps pattern

- **Root app** (`argocd/root-app.yaml`) — bootstrapped once manually, watches `argocd/apps/`
- **Child apps** live in `argocd/apps/*.yaml` — one file per application
- **kube-system app** — manages `talos/kube-system/` with `ServerSideApply=true` and `prune: false`

### Adding a new application

1. Create `kubernetes/<app>/` with manifests + `network-policy.yaml` + `<app>-values.yaml` (if Helm)
2. Create `argocd/apps/<app>.yaml` — use multi-source pattern (Helm chart + `$values` ref + manifest path)
3. Push — ArgoCD picks it up automatically

### Multi-source app pattern

```yaml
sources:
  - repoURL: https://charts.example.com
    chart: my-chart
    targetRevision: "1.2.3"
    helm:
      valueFiles:
        - $values/kubernetes/my-app/values.yaml
  - repoURL: https://github.com/edjeffreys/infrastructure
    targetRevision: HEAD
    ref: values
  - repoURL: https://github.com/edjeffreys/infrastructure
    targetRevision: HEAD
    path: kubernetes/my-app
    directory:
      exclude: "*-values.yaml"
```

### Bootstrap secrets (not managed by ArgoCD)

These files contain `op://` references and must be applied manually with `op inject`:

```bash
op inject -i kubernetes/argocd/repo-secret.yaml | kubectl apply -f -
op inject -i kubernetes/argocd/oci-repos.yaml | kubectl apply -f -
```

`oci-repos.yaml` provides ghcr.io credentials for ARC Helm charts. Both files are excluded from ArgoCD selfHeal.

### argocd-values.yaml

This file is **not** applied by ArgoCD. It was used at bootstrap (`helm install argocd`) and changes must be applied by patching `argocd-cmd-params-cm` directly or re-running helm upgrade. Key settings:
- `server.insecure: true` (Tailscale handles TLS termination)
- `reposerver.exec.timeout: 300`
- `server.repo.server.timeout.seconds: 300`
- `resourceExclusions` for Tailscale StatefulSets

### Renovate

`renovate.json` at repo root. The `argocd` manager watches `argocd/apps/*.yaml` and raises PRs for Helm chart version bumps. Always pin `targetRevision` to an explicit version (never `*`).

---

## Secrets Management: 1Password

### In-cluster secrets (runtime)

The **1Password Connect operator** runs in `onepassword-operator` namespace. Use `OnePasswordItem` CRDs to create Kubernetes secrets from 1Password:

```yaml
apiVersion: onepassword.com/v1
kind: OnePasswordItem
metadata:
  name: my-secret
  namespace: my-app
spec:
  itemPath: "vaults/Secrets/items/My Item Name"
```

The resulting Secret has keys matching the 1Password item field names.

### Bootstrap secrets (one-time apply)

For secrets that need specific labels (e.g. ArgoCD repo credentials) which `OnePasswordItem` can't set:

```bash
op inject -i kubernetes/argocd/repo-secret.yaml | kubectl apply -f -
```

---

## Storage: TrueNAS NFS

- All PVCs are NFS-backed via TrueNAS
- All access modes: `ReadWriteMany` — pods can reschedule freely across nodes
- Storage class: `nfs`
- PVCs are safe to evict/reschedule (no `ReadWriteOnce` volume lock-in)

---

## Network Policy Conventions

Every namespace needs policies or pods will be blocked by the clusterwide default-deny.

### Standard boilerplate

```yaml
# Always needed
- name: allow-dns              # UDP/TCP 53 to kube-system/kube-dns
- name: allow-intra-namespace  # if multiple pods in namespace talk to each other
- name: allow-kube-apiserver   # if pod talks to Kubernetes API
```

### Kube-apiserver access

Use **both** (covers direct node connections AND the ClusterIP):

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
1. Ingress policy in the target namespace from the `tailscale` namespace
2. An entry in `kubernetes/tailscale/network-policy.yaml` `allow-backend-egress` for the target namespace

```yaml
ingress:
  - fromEndpoints:
      - matchLabels:
          k8s:io.kubernetes.pod.namespace: tailscale
```

---

## Networking: Tailscale

- Tailscale operator installed in `tailscale` namespace
- All services exposed via `tailnet: tail5f17e.ts.net` (no public internet exposure)
- Ingress resources use `ingressClassName: tailscale` and `tailscale.com/proxy-class: "standard"`
- ProxyClasses: `standard` (standard nodes), `pia-exit` (PIA nodes, for exit node connector)
- Exit nodes: `vpn-home` (standard nodes) and `vpn-pia` (PIA nodes) via `Connector` CRDs
- The operator is **not** configured with oauth credentials in `operator-values.yaml` — it reads a pre-existing `operator-oauth` Secret automatically

---

## Terraform

- **Provider**: Proxmox (`bpg/proxmox`)
- **Backend**: Terraform Cloud — org `Skaal`, workspace `proxmox`
- **Parallelism**: workspace has `TF_CLI_ARGS_apply=-parallelism=1` to prevent simultaneous VM changes
- VMs provisioned via cloud-init; no Packer templates

---

## GitHub Actions: ARC

- Self-hosted runners via Actions Runner Controller in `arc-systems` / `arc-runners` namespaces
- Runner scale set: `arc-runner-set`, runs on standard (non-PIA) nodes
- Helm charts from `oci://ghcr.io/actions/actions-runner-controller-charts/`
  - Controller chart: `gha-runner-scale-set-controller`
  - Scale set chart: `gha-runner-scale-set`
- ghcr.io credentials in `repo-ghcr-arc` Secret (bootstrap, not ArgoCD-managed)

---

## Workloads

| App | Namespace | Nodes | Notes |
|-----|-----------|-------|-------|
| plex | plex | standard | High CPU — limit 4000m |
| radarr | arr | pia | |
| sonarr | arr | pia | |
| sabnzbd | arr | pia | CPU limit 3000m for par2/unpack |
| seerr | arr | pia | |
| mealie | mealie | standard | |
| homeassistant | homeassistant | standard | |

All workloads exposed via Tailscale ingress. Traefik is installed but only used internally.

---

## Key Gotchas

- **Cilium MTU**: explicitly `1450` in `cilium-config`. Do not remove — nodes have a `tailscale0` interface at MTU 1280 which Cilium would auto-detect and apply to all pods, breaking large downloads.
- **CoreDNS AAAA block**: `coredns-custom` ConfigMap in `kube-system` returns NOERROR with no answer for all AAAA queries. Cluster has no IPv6 outbound path.
- **CoreDNS spread**: `talos/kube-system/coredns.yaml` patches the CoreDNS Deployment with topology spread and node anti-affinity (excludes PIA nodes). Managed by kube-system ArgoCD app with `ServerSideApply=true`.
- **ArgoCD self-management**: changes to `argocd-values.yaml` do NOT apply automatically — patch `argocd-cmd-params-cm` directly and restart affected deployments.
- **ARC CRDs**: use `ServerSideApply=true` in `arc-systems` syncOptions — the ARC CRDs are too large for client-side apply annotations.
- **Tailscale StatefulSets**: excluded from ArgoCD resource management via `resourceExclusions` — the Tailscale operator manages these directly.
- **Tailscale ProxyClass**: all ingresses must use `tailscale.com/proxy-class: "standard"` (not `"default"` — that ProxyClass was renamed).
- **PIA node taint**: `node-type=pia:NoExecute` only — `NoExecute` already prevents scheduling, `NoSchedule` is redundant and should not be added.
- **PSA privileged namespaces**: `tailscale`, `monitoring`, and `loki` namespaces need `pod-security.kubernetes.io/enforce: privileged` label — enforced via namespace manifests in git.
- **Bootstrap order**: OnePassword operator must be healthy before any `OnePasswordItem` resources can create secrets. cert-manager must be healthy before issuers work.
- **Root app path**: `argocd/root-app.yaml` points to `argocd/apps/`. The in-cluster Application resource must also reflect this — it was previously `talos/argocd/apps` and may need manual patching if re-bootstrapped.

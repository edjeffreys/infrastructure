# flux-system

Flux CD v2 — the cluster's GitOps controller, and since wave 14 the only one.
ArgoCD ran alongside it for waves 7–13, owning each app until that app moved;
it is now removed from the repo entirely. References to it below are kept where
they explain *why* something is shaped the way it is.

## What is where

| Path | Managed by | Contents |
|---|---|---|
| `flux/bootstrap/` | applied by hand | `GitRepository` + the `root` Kustomization |
| `flux/apps/<app>.yaml` | the `root` Kustomization | one `Kustomization` (± `HelmRelease`) per app |
| `kubernetes/flux-system/` | the `flux-system` Kustomization | Cilium policies for Flux's own namespace |

The namespace itself is not declared here. It comes from the install manifest,
the same way `argocd` comes from `helm install` — see `kubernetes/argocd/`,
which likewise has no `namespace.yaml`.

## Install

Pinned to **v2.9.4**. Kubernetes `>= 1.35.0` is required for v1.35 and later;
the cluster runs v1.36.1.

```bash
kubectl apply -f https://github.com/fluxcd/flux2/releases/download/v2.9.4/install.yaml
```

Then three bootstrap steps, **in this order**:

```bash
kubectl apply -f kubernetes/flux-system/network-policy.yaml
op inject -i kubernetes/flux-system/flux-git-auth-secret.yaml | kubectl apply -f -
kubectl apply -f flux/bootstrap/
```

The order is not cosmetic:

1. **Policies first, or Flux deadlocks.** The clusterwide `default-deny` makes a
   namespace with no `CiliumNetworkPolicy` unable to reach anything, and Flux
   fetches its own policies from GitHub. Without this step the controllers
   cannot reach GitHub to fetch the policy that would let them reach GitHub.
   Applying it by hand breaks the cycle; Flux then adopts and reconciles it
   like any other resource.
2. **Secret before the GitRepository**, or the first clone fails and the source
   backs off.

Confirm:

```bash
flux get sources git -A
flux get kustomizations -A
```

Finally, restart kube-state-metrics. It builds its informers at startup and does
not notice a CRD that appeared afterwards
(kubernetes/kube-state-metrics#2296), so until it restarts the `gotk_resource_info`
series behind the Flux dashboards and the `FluxResourceNotReady` alert stay
empty — with no error anywhere to say so:

```bash
kubectl -n monitoring rollout restart deploy/kube-prometheus-stack-kube-state-metrics
```

## Why no UI

Flux ships none. The replacement is `flux get all -A`, the three Grafana
dashboards in `kubernetes/monitoring/flux-dashboards.yaml`, and the alerts in
`kubernetes/monitoring/flux-monitoring.yaml` — chiefly `FluxResourceNotReady`,
which fires on `gotk_resource_info{ready="False"}`. Those alerts are not
optional: without them a Kustomization can sit failing indefinitely with nothing
to notice, which is the one thing the ArgoCD UI gave for free.

(The older `gotk_reconcile_condition` metric belongs to the monitoring setup Flux
deprecated in v2.1 and removed in v2.2. `gotk_resource_info` replaces it and is
produced by kube-state-metrics, not by the controllers.)

## Conventions

- **No `spec.targetNamespace` on Kustomizations.** Every manifest under
  `kubernetes/` already carries an explicit `metadata.namespace`, and
  `targetNamespace` *overrides* rather than filling in blanks the way ArgoCD's
  `destination.namespace` did.
- **`metadata.namespace` is mandatory on every `HelmRelease` and every
  `HelmRepository`/`OCIRepository`.** Renovate's `flux` manager will not infer
  it from the parent Kustomization; omit it and the chart silently stops being
  updated.
- **Helm values stay in `<app>-values.yaml`** and reach the chart through a
  `configMapGenerator` with `disableNameSuffixHash: true`. Label the generated
  ConfigMap `reconcile.fluxcd.io/watch: Enabled` so a values change upgrades
  immediately instead of waiting for the reconcile interval.
- **`deletionPolicy` is left at its default**, `MirrorPrune`, on every app bar
  two. Every migrated app carried `Orphan` while its wave was in flight — with
  `prune: true` the default means deleting a Kustomization deletes the workload,
  which would have turned a rollback into an outage — and wave 15 flipped them
  all back once the migration settled.

  `longhorn` and `cilium` keep `Orphan` permanently, and that is deliberate
  rather than a wave-15 oversight. Deleting the longhorn Kustomization under
  `MirrorPrune` removes the storage layer beneath every PVC in the cluster;
  deleting the cilium one removes `default-deny` and returns the cluster to
  default-allow **without breaking anything**, so it would page nobody and show
  up nowhere. Both are unrecoverable-by-accident in a way a single workload is
  not, and both cost only a manual cleanup when removal is genuinely intended.
- **`wait: true` on every Kustomization.** Without it `Ready` means only "the
  manifests applied", so `FluxResourceNotReady` could never catch a workload
  that applied fine and then failed to come up — which is the failure that
  actually happens.

## Migrating an app off ArgoCD

One PR: delete `argocd/apps/<name>.yaml` and add `flux/apps/<name>.yaml`.

That works because **none of these Applications carries the
`resources-finalizer.argocd.argoproj.io` finalizer**. Without it, deleting an
Application is a non-cascading delete: the Application object goes, the
workloads stay. The root app prunes the Application, the workloads are briefly
unmanaged, and Flux adopts them. Check before relying on it:

```bash
kubectl -n argocd get app <name> -o jsonpath='{.metadata.finalizers}'
```

**`longhorn` was the exception**, and this is what actually happened when it was
migrated in wave 12b — recorded here because the mechanism is not what the first
draft of this section claimed.

Longhorn's chart ships its uninstall job as a `helm.sh/hook: pre-delete`. Argo CD
v3.3 added PreDelete hooks and maps Helm's onto its own, so the
**`argocd-application-controller` added `pre-delete-finalizer.argocd.argoproj.io`
(plus `…/cleanup`) to the Application by itself**. Those finalizers were never in
`argocd/apps/longhorn.yaml` — `managedFields` shows the controller as their
owner, so no change to git could have removed them.

Merging the deletion without clearing them first is survivable, and was survived:
Argo CD ran the hook, and Longhorn's uninstaller refused it.

```
level=fatal msg="cannot uninstall Longhorn because deleting-confirmation-flag is set to `false`"
```

`deleting-confirmation-flag` is a Longhorn setting, `false` here and by default.
The uninstall job checks it before touching anything, so the failure mode is a
**blocked deletion, not data loss**: `backoffLimit: 1` means two attempts and it
stops, the Application sits in `deleting` with `DeletionError: PreDelete hook(s)
failed`, and every volume stays attached and healthy. Flux had already adopted
longhorn by then, so the workloads were never unmanaged.

> **Never set `deleting-confirmation-flag` to `true` to clear that error.** The
> job's own message, and the Longhorn UI, both advise exactly that — correct for
> someone genuinely uninstalling Longhorn, catastrophic here. It is the guard
> that stops the hook deleting every volume in the cluster. The error is fixed by
> removing the finalizer, never by satisfying the uninstaller.

Clearing the finalizer is safe before or after the merge — it just changes
whether you get a clean prune or a stuck Application to tidy up:

```bash
kubectl -n argocd get app longhorn -o jsonpath='{.metadata.finalizers}'
kubectl -n argocd patch app longhorn --type merge -p '{"metadata":{"finalizers":null}}'
```

The Application carries no `resources-finalizer`, so its deletion is
non-cascading either way: the object goes, the workloads stay. Whether the
controller re-adds a pre-delete finalizer after removal was never established —
if it does, clearing it in advance buys nothing, which is a further reason not to
treat the pre-merge patch as load-bearing.

Overlap is harmless in the other direction too: if Flux applies before ArgoCD
prunes, both are writing byte-identical manifests, so neither fights the other.

Helm apps adopt cleanly without any pre-annotation: ArgoCD renders charts with
`helm template`, so the resources carry the chart's own
`app.kubernetes.io/managed-by: Helm` label but *not* the `meta.helm.sh/release-*`
annotations Helm needs to claim them. A plain `helm install` would refuse them
with `invalid ownership metadata`. helm-controller does not — it takes ownership
of pre-existing resources by default (`install.disableTakeOwnership` and
`upgrade.disableTakeOwnership` both default to `false`). Do not set either.

## Pre-merge checks

`.github/workflows/validate-manifests.yaml` renders the whole Flux tree before
anything reaches `master`:

```bash
flux build kustomization root \
  --path ./flux/apps \
  --kustomization-file ./flux/bootstrap/root.yaml \
  --dry-run --recursive \
  --local-sources GitRepository/flux-system/infrastructure=.
```

No cluster needed, and `--recursive` walks from the root Kustomization into
every child, so it builds the same tree kustomize-controller would. CI then runs
kubeconform over the *rendered* output — the only check that sees the result of a
kustomize transformation rather than the files in git.

It catches, with a non-zero exit:

- a `spec.path` pointing at a directory that does not exist;
- a YAML under that path that is not a Kubernetes manifest (`missing Resource
  metadata`) — the trap waiting for every Helm app, whose `*-values.yaml` must be
  kept out with an explicit `resources:` list rather than left to autodetect;
- two manifests declaring the same resource (`already registered id`).

It does **not** render Helm charts. A `HelmRelease` is validated as a manifest;
what the chart produces is helm-controller's business and needs the chart pulled.

`scripts/check-flux-refs.py` still runs alongside it, because it checks things a
successful build cannot see — `dependsOn` targets, `valuesFrom` ConfigMaps, and
the explicit `metadata.namespace` Renovate needs.

## Helm apps

ArgoCD's multi-source Application collapsed three things into one manifest. Flux
splits them, and `flux/apps/<app>.yaml` holds all three:

- a `HelmRepository` (in `flux-system`) for the chart repo;
- a `Kustomization` for everything the chart does *not* ship — namespace,
  policies, secrets, ingress — plus the ConfigMap generated from
  `<app>-values.yaml`;
- a `HelmRelease` **in the app's own namespace**, not `flux-system`.

That last point is the one with a reason behind it. `valuesFrom` resolves in the
HelmRelease's own namespace, and the values ConfigMap is generated by the
Kustomization alongside the rest of the app — so putting the HelmRelease in the
app namespace is what lets the two find each other. It also makes
`targetNamespace` and `storageNamespace` unnecessary, since both default to it.

The cost is bootstrap ordering: on a cold cluster the root Kustomization applies
the HelmRelease before the app's Kustomization has created the namespace, so the
first pass fails and the second succeeds. Flux retries on its own; it converges
without help, it just looks noisy in the logs once.

**Always pin `releaseName`.** Charts name resources after the release, and
`Deployment.spec.selector` is immutable — so a release name that drifts forces a
recreation rather than an update. `paperless` and `traefik` (`ingress-traefik`)
and `monitoring` (`kube-prometheus-stack`) all have names that do not match their
app directory.

## Migration progress

| Wave | Apps | State |
|---|---|---|
| 7 | Flux install, `flux-system` | done |
| 8 | `emby`, `pinchflat`, `storj`, `truenas`, `tdarr` | done |
| 9 | `mealie`, `paperless`, `plex`, `arr`, `claude-agent`, `homeassistant` | done |
| 10 | `monitoring`, `loki` (+promtail), `goldilocks`, `metrics-server`, `descheduler`, `reloader` | done |
| 11 | `tailscale`, `traefik`, `cert-manager` | done |
| 12a | `cnpg`, `onepassword`, `arc-systems`, `arc-runners` | done |
| 12b | `longhorn` | done — needed a manual finalizer removal, see above |
| 13 | `kube-system`, `storage`, `intel-gpu`, `cilium` | done |
| 14 | remove ArgoCD | git side done; uninstall is manual, below |

Waves 8 onward are ordered by blast radius: leaves first, then workloads, then
the things everything else depends on. Wave 11 is the one to be careful with —
losing the Tailscale operator loses cluster access, so have `talosctl kubeconfig`
to hand.

## Uninstalling ArgoCD

Wave 14 removes ArgoCD from git: `argocd/`, `kubernetes/argocd/`,
`scripts/check-argocd-refs.py`, its CI steps and Renovate manager, and the two
`extraManifests` entries that installed it at Talos bootstrap. None of that
touches the running install — ArgoCD was never self-managed from git — so it
keeps running until removed by hand.

**Order matters.** Delete the Applications first, then the release:

```bash
# 1. Confirm nothing is finalizer-blocked. Anything listed here must be cleared
#    first, or the namespace delete in step 3 hangs forever with no controller
#    left to resolve it.
kubectl -n argocd get applications \
  -o jsonpath='{range .items[*]}{.metadata.name} {.metadata.finalizers}{"\n"}{end}'

# 2. Delete the Applications. None carries resources-finalizer, so this is
#    non-cascading: the objects go, the workloads stay, and Flux already owns
#    every one of them.
kubectl -n argocd delete applications --all

# 3. Remove the release and the namespace.
helm -n argocd uninstall argocd
kubectl delete namespace argocd
```

The root Application stops pruning before this point and says so:

```
SyncError: Skipping sync attempt: auto-sync will wipe out all resources
```

That is ArgoCD refusing to auto-sync a change that would delete *every* resource
an app manages — its guard against an emptied or broken repo path, which is
exactly what wave 13 left behind when it emptied `argocd/apps/`. It is expected,
it is not a fault, and step 2 above is what clears it. `allowEmpty: true` on the
root Application's `syncPolicy.automated` would have suppressed it, but the root
Application is not managed from git, so that would itself have been a manual
`kubectl apply` — no better than deleting the thing outright.

Leftovers worth checking afterwards:

- `kubernetes/tailscale/network-policy.yaml` no longer lists `argocd` in
  `allow-backend-egress`, so the `ts-argocd-tailscale-*` device disappears from
  the tailnet. Tailnet Lock signs devices, not services — nothing to re-sign on
  removal, but the hostname will not come back without one.
- `longhorn-uninstall` in `longhorn-system` is a failed Job left by wave 12b's
  pre-delete hook. It has no owner and no TTL, so nothing collects it and it
  will keep `KubeJobFailed` firing: `kubectl -n longhorn-system delete job
  longhorn-uninstall`.

Wave 12 is split because `longhorn` is the one app whose Application carries a
pre-delete finalizer. The other four are ordinary non-cascading deletes and go
together in 12a; `longhorn` gets 12b to itself, so the manual finalizer removal
is not buried in a five-app diff and a rollback of it touches nothing else.

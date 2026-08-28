# Cilium

Cilium is the CNI. Everything here is split by whether a controller can safely
own it.

## `policies/` — managed by ArgoCD (`argocd/apps/cilium.yaml`)

Cluster-wide policies that are already live in the cluster. ArgoCD adopts them;
bringing them under GitOps means drift here is now visible and reverted, which
it previously was not.

| Policy | Purpose |
|--------|---------|
| `allow-health-checks` | Cilium inter-node health probes (TCP 4240 + ICMP echo) |
| `allow-kubelet-probes` | kubelet → pod liveness/readiness probes |
| `allow-proxy-identity` | Envoy proxy identity traffic |
| `default-deny` | clusterwide catch-all — see below |

### `default-deny`

Every endpoint outside `kube-system`, `kube-public`, `kube-node-lease` and
`cilium-secrets` is deny-by-default in both directions unless another policy
allows the traffic.

Applying it **changed nothing for any running workload**. All 129 live pods were
already deny-by-default in both directions:

- **ingress** was already covered cluster-wide, because `allow-kubelet-probes`
  and `allow-proxy-identity` each select every pod outside the exclusions and
  carry an ingress rule — in Cilium, any policy with an ingress section flips
  the endpoints it selects to deny-by-default for ingress;
- **egress** was already covered because every namespace ships at least an
  `allow-dns`.

So this is not a tightening of the cluster as it stands. It is the safety net
for the *next* namespace: one added without a `CiliumNetworkPolicy` is
completely unrestricted until this exists, and nothing reports that.

Re-check the picture before adding a namespace, or before changing this policy:

```bash
scripts/check-policy-coverage.py
```

It reads the live cluster rather than the repo — a manifest nobody applied
restricts nothing, and this repo has shipped that mistake twice. It exits 1 if
any pod is unrestricted in either direction.

**Two things that make this safe, and are easy to undo by accident:**

`allow-health-checks` carries an **egress** rule that exists only because of
this policy. The Cilium health endpoint has no
`k8s:io.kubernetes.pod.namespace` label, and a missing label satisfies `NotIn`,
so `default-deny` selects the health endpoint too. Drop that egress rule and
node-to-node `cilium-health` breaks — with no workload symptom to point at it.

A namespace that fetches its own policies from git cannot bootstrap under
`default-deny`: it needs the policy to reach git, and git to get the policy.
`flux-system` is the case in this repo, and the bootstrap in
`kubernetes/flux-system/README.md` applies its `network-policy.yaml` by hand
first for that reason.

**If it bites:**

```bash
kubectl delete ccnp default-deny        # instant, cluster-wide
kubectl -n kube-system exec ds/cilium -- hubble observe --verdict DROPPED -f
```

Deleting it by hand is reverted by ArgoCD's `selfHeal` within a sync, so remove
the file in git if the revert needs to stick.

## Not managed, deliberately

**`cilium-values.yaml`** — Helm values, applied by hand:

```bash
helm upgrade cilium cilium/cilium -n kube-system -f kubernetes/cilium/cilium-values.yaml
```

Cilium cannot be managed by a controller that needs Cilium running to reach the
API server. It is installed as a Talos `extraManifest` at bootstrap and upgraded
manually. Note MTU (1450) lives in the `cilium-config` ConfigMap, not here.

**`hubble-relay-tailscale.yaml`** — staged, never applied.

A `LoadBalancer` Service with `loadBalancerClass: tailscale`, to expose the
Hubble gRPC relay to the tailnet for the local `hubble` CLI. The Hubble **UI**
is already reachable at `hubble.tail5f17e.ts.net` via the Ingress in
`kubernetes/kube-system/`, so this is an unfinished extra rather than something
that regressed.

Applying it creates a **new tailnet device**, which under Tailnet Lock is locked
out until signed — with no explanation in any log. If you want it, apply it
deliberately and be ready to run `tailscale lock sign` (see the Tailnet Lock
gotcha in `CLAUDE.md`), rather than letting it arrive as a side effect of a
directory move.

## Hubble PKI — outstanding

`talos/manifests/cilium.yaml` no longer carries the `cilium-ca`,
`hubble-relay-client-certs` and `hubble-server-certs` Secrets. They were emitted
by `hubble.tls.auto.method: helm`, which bakes freshly generated RSA private
keys straight into the rendered output — so the committed bootstrap manifest
held real `ca.key` and `tls.key` material, in a repo that is going public.

The running cluster still holds all three in `kube-system`; Talos only applies
`extraManifests` at bootstrap, so removing them from git changed nothing live.
**A node bootstrapped from the current file will not bring up hubble-relay.**

Two things close this out:

1. Re-render with `hubble.tls.auto.method: cronJob` (or `certmanager`). Cilium
   then generates the certificates in-cluster via a certgen Job, and the render
   stops containing key material — the same mistake cannot recur on the next
   `helm template`. The re-render also emits the certgen ServiceAccount and RBAC
   that the stripped file is currently missing.

2. Rotate what was exposed, since the keys sat in git history and are not
   recoverable by deletion alone:

   ```bash
   kubectl -n kube-system delete secret \
     cilium-ca hubble-relay-client-certs hubble-server-certs
   kubectl -n kube-system rollout restart deployment/hubble-relay
   kubectl -n kube-system rollout restart daemonset/cilium
   ```

Until (1) lands, rebuilding a node means applying the three Secrets out of band
before Hubble will start.

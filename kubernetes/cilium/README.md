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

**`cilium-values.yaml`** — the complete input to the rendered manifest.

Cilium cannot be managed by a controller that needs Cilium running to reach the
API server. It is installed as a Talos `extraManifest` at bootstrap, and updated
by re-rendering `talos/manifests/cilium.yaml`:

```bash
helm repo add cilium https://helm.cilium.io && helm repo update
helm template cilium cilium/cilium --version <ver> -n kube-system \
  -f kubernetes/cilium/cilium-values.yaml > talos/manifests/cilium.yaml
```

**There is no Helm release.** This file used to say to run `helm upgrade cilium
cilium/cilium -f cilium-values.yaml`; that has never worked — it fails with
"release not found" — and it was worse than useless, because the values file did
not then capture the Talos-specific `--set` flags the original install used. A
render from it would have dropped `MTU: 1450`, turned off `kubeProxyReplacement`
and pointed `cgroup-root` at `/run/cilium/cgroupv2`, any one of which breaks the
datapath. Those settings are all in the values file now, and the file is
verified to reproduce the committed manifest exactly.

Because `extraManifests` are applied only at bootstrap, re-rendering does not
touch a running cluster. Updates reach it by applying the changed documents by
hand — for a settings-only change that is the `cilium-config` ConfigMap plus a
DaemonSet restart; for a version bump it is the whole manifest.

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

## Hubble PKI

`hubble.tls.auto.method` is `cronJob`, not the chart default of `helm`. The
default generates fresh RSA private keys into the rendered output on every
render, so the committed manifest carried real `ca.key` and `tls.key` material
in a repo that was about to go public. They were stripped by hand, which left a
file that could not bootstrap Hubble — and the next render would have put them
straight back.

`cronJob` makes certgen produce the certificates in-cluster instead, so the
render contains no key material and cannot regain any. The render also emits the
certgen ServiceAccount and RBAC the stripped file was missing.

The keys that were exposed sat in git history and are not recoverable by
deletion, so rotate them if that has not already been done:

```bash
kubectl -n kube-system delete secret \
  cilium-ca hubble-relay-client-certs hubble-server-certs
kubectl -n kube-system rollout restart deployment/hubble-relay
kubectl -n kube-system rollout restart daemonset/cilium
```

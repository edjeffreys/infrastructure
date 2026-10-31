# homelab-ca

Private PKI for in-cluster mTLS — NATS first, then whatever talks to it.

| Piece | Where | Lifetime |
|---|---|---|
| Root CA | **offline** — 1Password item `Homelab Root CA` only | 20y |
| Intermediate CA | 1Password item `Homelab Intermediate CA` → Secret `homelab-ca` in `cert-manager` → ClusterIssuer `homelab-ca` | 5y |
| Root, distributed | Bundle `homelab-ca` → ConfigMap `homelab-ca` (key `ca.crt`) in **every** namespace | follows the root |

The root key never enters the cluster. Rotating the intermediate therefore
changes nothing a client trusts; only a new root does, and that means
re-trusting everything outside the cluster by hand. That is why the root is not
a cert-manager `selfSigned` CA: a cluster rebuild would have minted a new one.

## Requesting a certificate

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: my-app-nats
  namespace: my-app
spec:
  secretName: my-app-nats
  duration: 2160h      # 90d
  renewBefore: 720h
  issuerRef:
    kind: ClusterIssuer
    name: homelab-ca
  dnsNames:
    - my-app.my-app.svc
  usages: [client auth]
```

Verify peers against the `homelab-ca` ConfigMap, **not** the `ca.crt` in the
issued Secret — that one is the issuing CA, i.e. the intermediate.

A renewed cert is only picked up by a client that re-reads it. Most NATS
clients load their cert at connect time, so consumers need a Reloader
annotation.

## Creating the CA (one-time)

Needs `step` (`brew install step`) and `op`. Work in a throwaway directory.

```bash
step certificate create "Skaal Homelab Root CA" root_ca.crt root_ca.key \
  --profile root-ca --not-after 175320h
```

That prompts for a password — the root key stays encrypted. Then the
intermediate, unencrypted because cert-manager has to read it:

```bash
step certificate create "Skaal Homelab Intermediate CA" intermediate_ca.crt intermediate_ca.key \
  --profile intermediate-ca --ca root_ca.crt --ca-key root_ca.key \
  --not-after 43830h --no-password --insecure
```

Store both. The root item is never referenced by the cluster:

```bash
op item create --vault Secrets --category "Secure Note" --title "Homelab Root CA" \
  'root_ca.crt[file]=root_ca.crt' 'root_ca.key[file]=root_ca.key' \
  'key password[password]=<the password step prompted for>'
```

```bash
op item create --vault Secrets --category "Secure Note" --title "Homelab Intermediate CA" \
  'tls.crt[file]=intermediate_ca.crt' 'tls.key[file]=intermediate_ca.key' 'ca.crt[file]=root_ca.crt'
```

The intermediate item is stored as **file attachments**, not text fields: the
operator keys an attachment by its file name, and PEM survives intact, where a
multi-line concealed field is not guaranteed to. Then delete the local copies:

```bash
rm -P root_ca.key intermediate_ca.key root_ca.crt intermediate_ca.crt
```

## Verifying

```bash
kubectl -n cert-manager get secret homelab-ca -o json | jq '.data | keys'
```

Must list exactly `ca.crt`, `tls.crt`, `tls.key`. Anything else means the
attachments were named differently — rename them in 1Password, then restart
the operator (it does not reliably re-poll):

```bash
kubectl -n onepassword-operator rollout restart deploy/onepassword-connect-operator
```

```bash
kubectl get clusterissuer homelab-ca
```

```bash
kubectl get bundle homelab-ca
```

```bash
kubectl -n default get configmap homelab-ca -o jsonpath='{.data.ca\.crt}' | step certificate inspect
```

## Ordering, and why there are two Flux apps

trust-manager's webhook is `failurePolicy: Fail`. If a Bundle shared a
Kustomization with the policy that admits traffic to that webhook, Flux's
server-side dry-run of the Bundle would time out against the not-yet-applied
policy and abort the whole apply — including the policy. Nothing would ever
converge. So:

- `trust-manager` holds the chart and its network policy only, and
  health-checks its HelmRelease
- `homelab-ca` holds the issuer, the Bundle and the OnePasswordItem, and
  `dependsOn` both `cert-manager` and `trust-manager`

Do not merge them, and do not add a Bundle anywhere else without the same
dependency.

`OnePasswordItem` always produces an `Opaque` Secret. The CA issuer is expected
to read only the `tls.crt`/`tls.key` keys and ignore the type, but that was
**not verified** before this landed — if `kubectl get clusterissuer homelab-ca`
does not go `Ready` on first apply, the fallback is an `op inject` bootstrap
Secret of type `kubernetes.io/tls`.

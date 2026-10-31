# nats

3-node NATS cluster with JetStream, mTLS on every port. Message bus for the
homelab: Home Assistant (via natsmqtt5), Alertmanager and the bin calendar
feed into it in later phases.

| Port | Listener | TLS |
|---|---|---|
| 4222 | clients | `verify_and_map` — the client cert *is* the identity |
| 6222 | cluster routes | `verify` against `homelab-ca` |
| 7422 | leafnodes | `verify_and_map`; only `nats-mqtt` may connect (policy) |
| 8222 | monitoring | plain HTTP, pod-local only (probes, exporter) |
| 7777 | prometheus-nats-exporter | scraped via PodMonitor |

Certificates come from the `homelab-ca` ClusterIssuer and the root from the
`homelab-ca` ConfigMap trust-manager puts in every namespace — see
`kubernetes/homelab-ca/README.md`. The config-reloader sidecar picks up
renewed server certs without a restart.

## Adding a client

Three changes, all required. Miss the policy and the connection times out
with nothing in the server log; miss the user and the server logs
`authorization violation`.

1. A `Certificate` in the client's namespace, `commonName` only and **no
   SANs** — `verify_and_map` matches email SANs, then DNS SANs, before the
   subject, so a SAN would become the identity instead:

   ```yaml
   apiVersion: cert-manager.io/v1
   kind: Certificate
   metadata:
     name: my-app-nats
     namespace: my-app
   spec:
     secretName: my-app-nats
     duration: 2160h
     renewBefore: 720h
     issuerRef:
       kind: ClusterIssuer
       name: homelab-ca
     commonName: my-app.my-app
     usages: [client auth]
   ```

2. A user under `config.merge.accounts.APP.users` in `nats-values.yaml`,
   `user: "CN=my-app.my-app"`, with `permissions` scoped to its subjects.
3. The namespace in `allow-client-ingress` here, plus an egress policy in the
   client's namespace to `nats` on 4222.

The client verifies the server against the `homelab-ca` ConfigMap and connects
to `tls://nats.nats.svc:4222`. Most client libraries load their cert once, at
connect time, so give the Deployment a Reloader annotation or it fails when
cert-manager renews.

## Operating

nats-box carries two contexts — `default` (account `APP`) and `sys` (system
account):

```bash
kubectl -n nats exec -it deploy/nats-box -- nats --context sys server report jetstream
```

```bash
kubectl -n nats exec -it deploy/nats-box -- nats stream ls
```

Health, including whether every stream has caught up:

```bash
kubectl -n nats exec nats-0 -c nats -- wget -qO- http://localhost:8222/healthz
```

`scripts/upgrade-workers.sh` gates on that for all six NATS pods — these three
and `nats-mqtt`'s — between nodes.
Talos ignores PodDisruptionBudgets on drain, so it is the only thing stopping
a second node going down while JetStream is still re-syncing from the first —
which, at R3, loses quorum.

## From your Mac, over Tailscale

The client port is on the tailnet as `nats.tail5f17e.ts.net:4222` — an L4
passthrough Service (`tailscale-ingress.yaml`), so mTLS is still end to end and
a device without a mapped cert gets `certificate required`. It is the only way
onto the hub from outside the cluster; the hub is never on a LAN VIP.

Two personal identities: `CN=ed.nats` (account `APP`) and `CN=ed-sys.nats`
(system account). Pull them down once, and again after each renewal — they are
90-day certs that cert-manager renews at 60 days, so the copies on the Mac go
stale:

```bash
D=~/.config/nats/homelab; mkdir -p "$D"; chmod 700 "$D"
kubectl -n nats get configmap homelab-ca -o jsonpath='{.data.ca\.crt}' > "$D/ca.crt"
for s in ed-nats ed-sys-nats; do
  kubectl -n nats get secret "$s" -o jsonpath='{.data.tls\.crt}' | base64 -d > "$D/$s.crt"
  kubectl -n nats get secret "$s" -o jsonpath='{.data.tls\.key}' | base64 -d > "$D/$s.key"
done
chmod 600 "$D"/*.key
```

```bash
nats context save homelab --server tls://nats.tail5f17e.ts.net:4222 \
  --tlsca ~/.config/nats/homelab/ca.crt \
  --tlscert ~/.config/nats/homelab/ed-nats.crt --tlskey ~/.config/nats/homelab/ed-nats.key --select
nats context save homelab-sys --server tls://nats.tail5f17e.ts.net:4222 \
  --tlsca ~/.config/nats/homelab/ca.crt \
  --tlscert ~/.config/nats/homelab/ed-sys-nats.crt --tlskey ~/.config/nats/homelab/ed-sys-nats.key
```

Then `nats sub 'shellies.>'`, or `nats --context homelab-sys server report jetstream`.

The tailnet device `nats` is subject to Tailnet Lock: on first deploy it has no
connectivity until signed (`tailscale lock status` in the `ts-nats-tailscale-…`
pod in `tailscale`, then the printed `tailscale lock sign …` on a trusted node).

## Gotchas

- **Route TLS trusts the CA, not a specific cert.** Any `homelab-ca` cert with
  `client auth` would be accepted on 6222 as a route. `allow-cluster-routes` is
  the control: only NATS server pods reach that port. Do not loosen it.
- **JetStream storage is `longhorn-single`.** JetStream already replicates at
  R3; the default class would add two Longhorn replicas on top, six writes to
  the one physical disk.

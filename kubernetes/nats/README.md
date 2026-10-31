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

`scripts/upgrade-workers.sh` gates on that for all three pods between nodes.
Talos ignores PodDisruptionBudgets on drain, so it is the only thing stopping
a second node going down while JetStream is still re-syncing from the first —
which, at R3, loses quorum.

## Gotchas

- **Route TLS trusts the CA, not a specific cert.** Any `homelab-ca` cert with
  `client auth` would be accepted on 6222 as a route. `allow-cluster-routes` is
  the control: only NATS server pods reach that port. Do not loosen it.
- **JetStream storage is `longhorn-single`.** JetStream already replicates at
  R3; the default class would add two Longhorn replicas on top, six writes to
  the one physical disk.

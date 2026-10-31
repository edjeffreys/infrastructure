# alert-relay

Alertmanager → NATS. Alertmanager has no NATS receiver, so a webhook receiver
posts to this [Redpanda Connect](https://docs.redpanda.com/redpanda-connect/)
relay, which publishes each alert to the `ALERTS` JetStream stream on the hub.
Pushover is unchanged; this is a second route with `continue: true`.

```
Alertmanager ──webhook──▶ alert-relay (×2) ──mTLS──▶ nats  ALERTS  alerts.>
```

## Messages

One message **per alert**, not per Alertmanager group. The payload is the
alert object from the webhook body (`status`, `labels`, `annotations`,
`startsAt`, `endsAt`, `generatorURL`, `fingerprint`), on:

```
alerts.<status>.<severity>.<alertname>      alerts.firing.critical.PodCrashLooping
```

`status` is `firing` or `resolved`. A missing severity is `none`, and any
character outside `[A-Za-z0-9_-]` in a token becomes `_`. Every severity is
sent, `none` included.

`Nats-Msg-Id` is `<fingerprint>-<status>-<startsAt>`. Alertmanager re-sends an
unresolved alert every `repeatInterval` (4h) and re-sends a whole group whenever
it changes; the stream's 5h `duplicateWindow` drops those, so each firing and
each resolution lands once. Keep the window above `repeatInterval`. The same
dedup means `Watchdog`, firing forever with one `startsAt`, lands once — it is
not a heartbeat here.

The HTTP request is answered only after JetStream acks every alert in it, so a
NATS outage shows up as failed webhook notifications in Alertmanager, which
retries.

## Stream

`ALERTS`: R3, file storage, 30 days or 256 MiB, whichever comes first. Declared
in `stream.yaml` and created by NACK — see `kubernetes/nats/README.md`.

## Checking it

```bash
kubectl -n nats exec -it deploy/nats-box -- nats stream info ALERTS
```

```bash
kubectl -n nats exec -it deploy/nats-box -- nats sub 'alerts.>' --last
```

Relay errors (rejected publish, TLS):

```bash
kubectl -n alert-relay logs deploy/alert-relay
```

## Identity

`CN=alert-relay.alert-relay`, publish `alerts.>` only. Reloader restarts the
relay when its ConfigMap or client cert changes, since neither is re-read.

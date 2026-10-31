# nats-mqtt

The LAN-facing MQTT tier: a 3-node NATS **leaf** cluster with its own
JetStream, joined to the hub (`kubernetes/nats/`) over mTLS. Shellies, the Glow
display and Home Assistant connect here; in-cluster consumers stay on the hub.

```
devices / HA ──MQTT 3.1.1, plaintext──▶ 10.0.0.211:1883 ──▶ nats-mqtt (leaf, ×3)
                                                                │ leafnode, mTLS
                                                                ▼
                                                      nats (hub, ×3) ◀── in-cluster clients
```

## Why a leaf, and why it is the only thing on a VIP

The hub is mTLS-only on every port and is never exposed on the LAN. This tier is
the opposite — plaintext, password auth, reachable from the IoT VLAN — so it is
kept off the hub entirely:

- The hub sees the whole tier as **one user**, `CN=nats-mqtt-leaf.nats-mqtt`,
  limited to `shellies.>`, `glow.>` and `homeassistant.>`
  (`kubernetes/nats/nats-values.yaml`). A leaked device password reaches those
  subjects and nothing else on the hub, whatever is configured here.
- MQTT sessions and retained messages live in this tier's own JetStream domain,
  `mqtt`, not the hub's meta group.

It is stateful because nats-server's MQTT needs JetStream; 3 replicas because
that is the smallest JetStream group that survives losing a node during a roll.

## MQTT, not MQTT 5

nats-server implements MQTT **3.1.1** only. Home Assistant's MQTT integration
must be set to protocol 3.1.1; on 5 the CONNECT is refused. HA uses MQTT 5 for
message expiry (unused here), subscription identifiers (a CPU optimisation) and
richer refusal reason codes — so a permission denial shows up only in this
server's log:

```bash
kubectl -n nats-mqtt logs nats-mqtt-0 -c nats | grep -i "permissions violation\|authorization"
```

Topics map to subjects: `/` → `.`, and a literal `.` in a topic → `//`.
`shellies/minig3-office/status/input:0` is `shellies.minig3-office.status.input:0`
on both tiers.

## Users

| User | Who | Publish / subscribe |
|---|---|---|
| `shelly` | every Shelly | `shellies.>` |
| `glow` | Glow IHD | `glow.>` |
| `homeassistant` | HA | publish `shellies.>`, `glow.>`, `homeassistant.>`; subscribe `>` |

HA subscribes to `>` because it opens a discovery subscription for every
integration that *can* use MQTT (Tasmota, ESPHome, DSMR, …); scoping it made
each HA reconnect log ~15 `Subscription Violation` errors. It widens nothing
that matters: the leaf only carries what the devices publish and what the hub
lets through its leaf user.

All are `allowed_connection_types: ["MQTT"]`, so a device password cannot be
used on the NATS client port. A new device class needs a user here **and** its
subjects added to the hub's leaf user, or its messages never leave this tier.

Passwords come from the 1Password item `MQTT Users` in vault `Secrets`, one
**password field per user, labelled exactly** `shelly`, `glow`,
`homeassistant`. Use letters and digits only — nats-server substitutes the env
var into its config and re-parses it, so punctuation can break the config.
Reloader restarts the StatefulSet when the item changes.

## Exposure

`10.0.0.211:1883`, announced from the control-plane nodes by the
`nats-mqtt` `CiliumL2AnnouncementPolicy` (Traefik's is separate). Cilium SNATs
at the announcing node, so pods see a node IP, never the device: **source
filtering happens at the router** — allow the IoT VLAN and HA to
`10.0.0.211` TCP 1883, nothing else.

## Checking it

```bash
kubectl -n nats-mqtt exec deploy/nats-mqtt-box -- nats server report jetstream
```

Device traffic reaching the hub:

```bash
kubectl -n nats exec -it deploy/nats-box -- nats sub 'shellies.>'
```

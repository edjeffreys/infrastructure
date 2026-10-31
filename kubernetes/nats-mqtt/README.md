# nats-mqtt

The LAN-facing MQTT tier: a 3-node NATS **leaf** cluster with its own
JetStream, joined to the hub (`kubernetes/nats/`) over mTLS. Shellies, the Glow
display and Home Assistant connect here; in-cluster consumers stay on the hub.

```
devices / HA ──MQTT 3.1.1 over TLS──▶ mqtt.skaal.dev:8883 ──▶ nats-mqtt (leaf, ×3)
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

`10.0.0.211` and `2a0e:cb01:ca:fe00::211` (dual-stack), announced from the
control-plane nodes by the `nats-mqtt` `CiliumL2AnnouncementPolicy` — ARP for
v4, NDP for v6 (Traefik's policy is separate). Cilium SNATs at the announcing
node, so pods see a node IP, never the device: **source filtering happens at
the router** — allow the IoT VLAN and HA to both addresses on TCP 8883,
nothing else.

The VIP serves **only 8883** (TLS, Let's Encrypt cert for **`mqtt.skaal.dev`**).
Every client — HA, the Shellies and the Glow (`mqtts://` in its host field) —
does TLS, so plaintext 1883 was taken off the VIP; it still listens inside each
pod as ghostunnel's backend on `127.0.0.1`. A future client that cannot do TLS
would mean putting 1883 back on the Service and the policy.

### TLS on 8883, with nothing to distribute

8883 presents a **publicly trusted** Let's Encrypt cert (`mqtt-public`, issued
by the existing `letsencrypt` ClusterIssuer over Cloudflare DNS-01, which works
for a LAN-only name). Clients verify it with the CA bundle they already ship —
no CA upload to any device:

- **Shelly Gen2/Gen3**: MQTT → connection type **Default TLS**, server
  `mqtt.skaal.dev:8883`.
- **Home Assistant**: broker `mqtt.skaal.dev`, port `8883`, broker certificate
  validation **Auto**.

Connect by **name**, not IP — the cert only covers `mqtt.skaal.dev`, so
`10.0.0.211:8883` fails verification. The name is a **local override on the
gateway**, A `10.0.0.211` **and** AAAA `2a0e:cb01:ca:fe00::211` — not in
Cloudflare. Without the AAAA the gateway forwards it upstream and the public
`*.skaal.dev` wildcard answers with Traefik's `::210`, so IPv6-first clients
land on the wrong VIP. Cloudflare is only used for the DNS-01 challenge.

nats-server has a single MQTT listener, so TLS cannot sit beside plaintext in
the server itself. A **ghostunnel** sidecar in each leaf pod terminates TLS on
8883 and forwards to `127.0.0.1:1883`, re-reading the cert hourly so renewals
need no restart. Passwords and per-user permissions are unchanged — ghostunnel
only does server-side TLS. Renovate tracks its image through a regex manager,
since it cannot see images in Helm values.

Let's Encrypt cannot do the rest: it stopped issuing client-auth certificates in
2025, so `homelab-ca` remains for every mTLS identity.

## MQTT 5 deadline

Home Assistant **2027.1.0 removes MQTT 3.1.1** (repair: "MQTT protocol migration
failed"), and nats-server's listener is 3.1.1-only — upstream will not add 5.
The intended replacement is [nberlee/mqtt-nats-broker](https://github.com/nberlee/mqtt-nats-broker):
3.1.1 and 5.0 in one broker, derived from nats-server's MQTT code, state in
JetStream, auth passed through to NATS users, native TLS (so ghostunnel would
go). Pre-release as of 2026-10-31; alpha expected mid-November. Its MQTT
identities must be denied `$JS.>`, `$KV.>`, `$O.>` and `$MQTT.>` — note
`homeassistant`'s `subscribe: [">"]`.

## Checking it

```bash
kubectl -n nats-mqtt exec deploy/nats-mqtt-box -- nats server report jetstream
```

Device traffic reaching the hub:

```bash
kubectl -n nats exec -it deploy/nats-box -- nats sub 'shellies.>'
```

# zigbee2mqtt

Runs the Zigbee network. The radio is an SMLIGHT **SLZB-06U** on the IoT VLAN
(`10.100.0.252`, TI CC2652P), set to **Coordinator** mode over LAN. zigbee2mqtt
drives it as a serial port over TCP and publishes to the `nats-mqtt` leaf, which
is where Home Assistant picks up its discovery messages.

```
SLZB-06U ◀──tcp 6638── zigbee2mqtt ──mqtts 8883──▶ mqtt.skaal.dev (nats-mqtt) ◀── HA
```

## The PVC is the network

`zigbee2mqtt-data` holds `configuration.yaml` with the **network key, PAN ID
and extended PAN ID**, plus `database.db` (every paired device) and
`coordinator_backup.json`. Lose it and every device has to be re-paired by hand.
That is why it opts in to Longhorn's daily backup.

The init container copies `configuration.yaml` from the `zigbee2mqtt-seed`
ConfigMap **only if no file exists**. The seed says `GENERATE`, and zigbee2mqtt
writes the real values back into the file on first start. Editing the
ConfigMap afterwards changes nothing, which is deliberate: re-seeding would
create a brand-new network.

Everything git owns is set through `ZIGBEE2MQTT_CONFIG_*` env vars in
`deployment.yaml`, which override the file at runtime without being written to
it. Settings changed in the frontend persist in the file. A setting that git also
sets through env won't stick.

The channel is fixed at **25**, clear of Wi-Fi channels 1/6/11. It can't be
changed later without re-pairing everything.

## Connectivity

- **Coordinator:** the router must allow the worker VLAN to `10.100.0.252` on
  TCP 6638. Pods SNAT to their node's IP when they leave the cluster.
- **MQTT:** user `zigbee2mqtt`, password in the 1Password item `MQTT zigbee2mqtt`
  (field `password`). The same item is mirrored into `nats-mqtt`, so there is
  one source. MQTT 3.1.1 until the broker swap in `kubernetes/nats-mqtt/README.md`
  ("MQTT 5 deadline"). Retained discovery messages don't cross brokers, so
  zigbee2mqtt has to move together with HA.
- **Frontend:** `https://zigbee2mqtt.tail5f17e.ts.net`, tailnet only, no auth of
  its own. Being a new tailnet device, it needs signing under Tailnet Lock.

## Naming devices

MQTT topics become NATS subjects (`/` → `.`), so give devices friendly names
with no spaces or dots, such as `nursery_temperature`. NATS subjects can't contain
whitespace, and a dot in a topic turns into `//`.

## Radio firmware

Coordinator firmware `20221226`, which is SMLIGHT's newest stable build for this
board as of 2026-11. Keep radio firmware auto-update off. Updating doesn't
touch pairings, but a build zigbee2mqtt can't talk to takes the whole network
down until it is flashed back.

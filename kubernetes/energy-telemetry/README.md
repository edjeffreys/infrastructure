# energy-telemetry

Smart-meter, boiler and homelab power readings off the NATS bus, into Prometheus and a
`ENERGY` stream. Telegraf subscribes on the hub and serves the readings as
gauges, and Grafana's **Home energy** dashboard (`home-energy-dashboard.yaml`)
draws them.

```
Glow IHD ──MQTT──▶ nats-mqtt ──leaf──▶ nats ──▶ energy-telemetry (Telegraf) ──▶ Prometheus ──▶ Grafana
HA (ViCare) ─statestream─┤                  └──▶ ENERGY stream
TrueNAS, Proxmox ─cron──┘
```

## Sources

| Subject | From | Payload |
|---|---|---|
| `glow.<mac>.SENSOR.electricitymeter` | Glow IHD, ~10s | JSON: power, cumulative import/export, tariff |
| `glow.<mac>.SENSOR.gasmeter` | Glow IHD, ~10s | JSON: cumulative kWh and m³, tariff |
| `homeassistant.statestream.<domain>.<entity>.state` | HA `mqtt_statestream` | the bare state, on change only |
| `truenas.power`, `proxmox.power` | `scripts/host-power-mqtt.py`, cron, every minute | JSON: `cpu_power`, and `gpu_power`/`gpu_pstate` where the host has an NVIDIA GPU |

The ViCare entities reach the bus through Home Assistant, not the Viessmann
API: HA already polls ViCare and the API's daily call quota does not stretch to
a second poller. `homeassistant/packages/statestream.yaml` picks which entities
are published. Adding one there is all it takes, as long as its state is numeric
or `on`/`off`.

## Metrics

| Metric | Unit |
|---|---|
| `glow_electricity_power_kw` | kW |
| `glow_electricity_import_kwh`, `glow_electricity_export_kwh` | kWh, cumulative |
| `glow_gas_import_kwh`, `glow_gas_import_m3` | kWh / m³, cumulative |
| `glow_{electricity,gas}_unit_rate_gbp_per_kwh`, `…_standing_charge_gbp_per_day` | GBP |
| `ha_entity_state{domain, entity}` | the entity's own unit; `on`/`off` become 1/0 |
| `host_power_cpu_watts{host}`, `host_power_gpu_watts{host}` | W; CPU package (RAPL) and GPU only, not the wall |
| `host_power_gpu_pstate{host}` | `P8` becomes 8 |

Text states (`eco`, `unavailable`) are dropped, with an error line in the
Telegraf log for each.

**Values never expire.** Statestream only publishes on change, so a steady
value like `boiler_burner_hours` would vanish under Telegraf's default 60s
expiry. The flip side is that a dead feed keeps showing its last value. After a
restart, each HA entity is missing until it next changes, because Telegraf reads
core NATS and does not see MQTT retained messages.

One replica only. The four NATS subscriptions use a queue group, so a second
pod would take half of each subject and both `/metrics` endpoints would be
partial.

## Stream

`ENERGY`: R3, file storage, capped at 1 GiB with no age limit. At about
12 MB a day, mostly the Glow, that holds roughly three months. Prometheus here
keeps only 7 days, so the stream is the longer raw record. Backfill a future
long-term store from it rather than from Prometheus. Declared in `stream.yaml`
and created by NACK. See `kubernetes/nats/README.md`.

```bash
kubectl -n nats exec -it deploy/nats-box -- nats stream info ENERGY
```

```bash
kubectl -n nats exec -it deploy/nats-box -- nats stream view ENERGY --subject 'homeassistant.statestream.>'
```

## Checking it

```bash
kubectl -n energy-telemetry port-forward deploy/energy-telemetry 9273 & curl -s localhost:9273/metrics | grep -E '^(glow|ha)_'
```

```bash
kubectl -n energy-telemetry logs deploy/energy-telemetry
```

## Identity

`CN=energy-telemetry.energy-telemetry`. It subscribes to the source subjects
only and may publish nothing. Reloader restarts the pod when its
config or client cert changes, since Telegraf re-reads neither.

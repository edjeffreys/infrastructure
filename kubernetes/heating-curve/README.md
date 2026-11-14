# heating-curve

Recommends the Vitodens heating curve's slope and shift from how the house
actually behaves. It does the same job as the HA curve advisor in
`homeassistant/packages/heating_curve.yaml`, but it keeps every hourly sample, so
the fit can be changed and recomputed instead of starting over. Go source is
in `images/heating-curve/`.

```
HA ─statestream─▶ nats-mqtt ─leaf─▶ nats ─▶ ENERGY stream ─replay─▶ heating-curve ─▶ HEATING stream (hourly samples)
                                                                          │
HA ◀── MQTT discovery sensors ◀── nats-mqtt ◀─leaf── homeassistant.heating_curve.* ◀─┘
```

## What it does

Every hour, on the hour, it takes the last hour of state from the `ENERGY`
stream and works out the flow temperature that would have held the setpoint.
It uses the curve's commanded flow, how warm the house actually got, and a
radiator exponent of 1.3. It skips hours with solar gain in the last 4 h,
outdoor above 12 °C, heating off, a setpoint or curve change in the last 3 h,
or too little heat demand to measure. Unlike the HA advisor, it keeps hours
with hot-water use: see below.

Every hour is stored on `heating.curve.sample` in `HEATING`, the skipped ones
too, with the reason. The recommendation is a weighted least-squares fit of
the Viessmann curve to the valid hours. It differs from the HA advisor in
four ways:

- **Weighting by outdoor temperature.** Each sample's weight is divided by the
  square root of its 2 °C bin's total. Mild hours outnumber cold ones by an
  order of magnitude. Without this, the cold end of the curve is extrapolated
  from mild weather. The HA advisor did that on 2026-11-14: it recommended
  1.4 / -4, which fell 1.7 °C short of what a 0 °C night needed.
- **Time decay instead of a sample count.** The half-life is 30 days
  (`-half-life`). The HA advisor's 0.995 per valid sample forgets cold
  hours after about 200 valid hours, however long ago those were.
- **Hot-water hours are ordinary hours.** The HA advisor skips any hour with
  DHW gas use. That dates from when it read the measured flow temperature,
  which DHW firing spikes because it is the boiler's common supply sensor. The
  calculation uses the commanded flow, so DHW doesn't touch it. Any heat the
  radiators miss during a cylinder reheat is part of normal operation, which
  the curve has to cover anyway. Each sample records the hour's `dhw_gas` (in the
  sensor's unit) as raw data only.
- **The raw shortfall by bin.** `bins` gives the mean of required minus
  commanded flow per 2 °C band. It shows whether the curve in use runs hot or
  cold at each outdoor temperature, independent of the fit.

Status is `collecting (n/24 hours)` until 24 weighted hours, then `shift only`
while the outdoor spread is too narrow to fit a slope, then `slope and shift`.

## Inputs

All from `homeassistant/packages/statestream.yaml`. A missing one makes every
hour `missing data`, except the supply temperature and DHW gas:

| Entity | Used for |
|---|---|
| `sensor.house_temperature` | indoor, 1 h time-weighted mean |
| `sensor.boiler_outside_temperature` | outdoor, 1 h mean |
| `sensor.boiler_supply_temperature` | recorded only; the calculation uses the commanded flow |
| `sensor.boiler_dhw_gas_consumption_today` | recorded per hour as `dhw_gas` |
| `sensor.heating_active_setpoint` | setpoint, and the 3 h change filter |
| `number.boiler_heating_curve_slope`, `…_shift` | commanded curve, and the 3 h change filter |
| `climate.boiler_heating` | must be `auto` |
| `binary_sensor.recent_solar_gain` | HA's own 4 h solar flag |
| `input_number.heating_target_normal` | setpoint for the dashboard's flow table |

Statestream publishes on change only. On start the service reads each
entity's last message from `ENERGY`, because curve settings can go weeks
without changing. It then replays the 7 hours before the first hour it still
has to evaluate. A restart therefore loses nothing as long as `ENERGY` still
holds the gap, which is about three months.

## Outputs

HA MQTT discovery, published on the hub and carried to HA by the leaf
(`homeassistant.>` is in the leaf user's subjects). The device is
**Heating curve advisor**:

| Entity | State |
|---|---|
| `sensor.heating_curve_advisor_status` | status; attributes hold `flow_table`, `bins`, `last_sample`, the current curve |
| `sensor.heating_curve_advisor_recommended_slope`, `…_recommended_shift` | the recommendation, or unknown while collecting |
| `sensor.heating_curve_advisor_samples` | weighted sample count |
| `sensor.heating_curve_advisor_last_sample` | last hour's reason (`ok`, `solar gain`, …) |
| `sensor.heating_curve_advisor_required_flow` | last valid hour's required flow |

nats-server's MQTT bridge cannot retain a message published from core NATS.
So the service re-sends discovery whenever HA publishes `online` on
`homeassistant/status`, and every sensor has `expire_after` of 2 h 10 m: if
the service stops, they go unavailable rather than stale. The dashboard card
**Curve advisor (heating-curve service)** in `homeassistant/dashboards/heating.yaml`
sits under the HA advisor's card so the two can be compared.

## First start

With an empty `HEATING`, it computes the last 7 days (`-backfill`) from
`ENERGY`. Hours from before the statestream inputs above existed are stored as
`missing data`. Delete the stream's messages to recompute from scratch, after
changing the sample logic for example; anything older than `ENERGY`'s
retention is lost for good:

```bash
kubectl -n nats exec -it deploy/nats-box -- nats stream purge HEATING
kubectl -n heating-curve delete pod -l app=heating-curve
```

## Importing from HA history

When an input was missing from the bus but HA recorded it, as before the
statestream deploy on 2026-11-14, `import-ha` recomputes those hours from HA's
recorder instead. It writes one sample per line to stdout. Publish them to
`HEATING`, then restart the pod, which loads them: a later sample for the same
hour replaces the earlier one.

It runs wherever both HA and the NATS hub are reachable. In practice that's a
laptop with an HA long-lived token and a NATS context as `CN=ed.nats`; the
service's own identity can do neither.

```bash
export HA_TOKEN=…   # HA profile → Security → Long-lived access tokens
docker run --rm -e HA_TOKEN ghcr.io/edjeffreys/heating-curve:latest import-ha -days 7 > samples.jsonl
nats pub heating.curve.sample --send-on newline --jetstream < samples.jsonl
kubectl -n heating-curve delete pod -l app=heating-curve
```

`-ha-url` defaults to `https://home.skaal.dev`. `go run ./images/heating-curve
import-ha` works instead of Docker. The import reports the hour range and the
valid count on stderr, plus a warning for any entity HA has no history for.

## Checking it

```bash
kubectl -n heating-curve logs deploy/heating-curve
```

```bash
kubectl -n nats exec -it deploy/nats-box -- nats stream view HEATING
```

## Identity

`CN=heating-curve.heating-curve` may read `ENERGY` and `HEATING` through
ordered consumers, publish to `heating.curve.>`, and publish only its own HA
discovery and state subjects. It subscribes to `homeassistant.status` only.
`claude-agent` may read `HEATING` and subscribe to statestream and the
advisor's state.

## Image

Built by `.github/workflows/build-heating-curve.yaml` on push to master, and
tested on pull requests. The Dockerfile runs `go vet` and `go test` too, so a
failing test cannot ship. Go module updates arrive through Renovate's `gomod`
manager. As with `cups`, the package must be made public once, by hand, after
the first build, at
<https://github.com/users/edjeffreys/packages/container/heating-curve/settings>.
Until then the pod sits in `ImagePullBackOff`. Delete the pod to roll out a
new `:latest`.

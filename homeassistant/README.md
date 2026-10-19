# Home Assistant

Home Assistant runs on its own HAOS VM, outside the cluster, at
`home.skaal.dev`. This directory holds the parts of its config that are
managed from git. Flux never sees it — `flux/bootstrap/root.yaml` only reads
`flux/apps` — and the manifest CI ignores it.

```
packages/     → /config/packages/     loaded via `homeassistant: packages: !include_dir_named packages`
dashboards/   → /config/dashboards/   YAML-mode dashboards, declared in packages/dashboards.yaml
deploy.sh     sync, check, reload
```

## Ownership

`/config/packages/` and `/config/dashboards/` on HA are **owned by this
directory**. `deploy.sh` replaces both wholesale, so a file added on HA by
hand is deleted on the next deploy. Everything else in `/config` — including
`configuration.yaml`, `secrets.yaml`, `.storage/` and every UI-created helper
or dashboard — is untouched and not in git.

YAML-mode dashboards cannot be edited in the UI. Change `dashboards/*.yaml`
and deploy.

## Deploying

```bash
homeassistant/deploy.sh            # packages and dashboard edits
homeassistant/deploy.sh --restart  # new integrations, new dashboards, anything reload can't pick up
```

The script uploads, verifies sha256 on the far side, backs up the current
directories to `/config/.deploy-backup/<timestamp>/` (last five kept), runs
`ha core check` and restores the backup if it fails. Only then does it reload
`input_number`, `input_boolean`, `template`, `statistics` and `automation`, or
restart Core.

Dashboard edits need only a browser refresh. A brand-new dashboard entry in
`packages/dashboards.yaml` needs `--restart`.

## Access

`ssh homeassistant` — key `~/.ssh/homeassistant`, listed in the Advanced SSH
& Web Terminal add-on's `ssh.authorized_keys`, user `edj`. The host key is
pinned under the alias `homeassistant` in `~/.ssh/known_hosts`. Revoke by
removing the key from the add-on config.

A non-interactive SSH session does not inherit the add-on's Supervisor token.
`deploy.sh` reads it from `/run/s6/container_environment/SUPERVISOR_TOKEN`;
do the same for ad-hoc `ha` or API calls (`http://supervisor/core/api/`).

## Standing charges

`packages/standing_charges.yaml` gives the Energy dashboard the standing
charges it has no field for. Each fuel has a cumulative GBP sensor accruing
the Octopus `current_standing_charge` (a daily rate) by elapsed time, attached
as the cost of a constant 0 kWh sensor. The dashboard's energy sources are
`.storage` config, not git — they point at these four sensors.

## Heating

`packages/heating_curve.yaml` and `dashboards/heating.yaml` drive the Vitodens
100-W (ViCare integration):

- **Curve advisor** — hourly, fits the Viessmann curve formula to the flow
  temperature that *would* have held the setpoint, and recommends slope and
  shift. It uses the curve's commanded flow, not the measured one: the flow
  sensor also sees DHW firing (it is the boiler's common supply sensor) and
  burner-off coasting. Hours with sun in the last 4 h, DHW use, outdoor above
  12 °C, or a setpoint/curve change in the last 3 h are skipped.
- **Solar compensation** — lowers the boiler's *normal* temperature when the
  house overshoots, or pre-emptively in sun, and *reduced* by the same amount:
  weather compensation has no room feedback, so a setback period would
  otherwise keep heating an already-warm house.
- **Warm-weather cutoff** — parks normal and reduced at 10 °C above an outdoor threshold,
  overridden if the house drops more than 1 °C below target.

The automation owns `number.boiler_normal_temperature` and
`number.boiler_reduced_temperature`; set the targets on
`input_number.heating_target_normal` and `input_number.heating_target_reduced`.
The cutoff parks both. The schedule stays in ViCare — the HA integration
cannot edit schedules.

The outdoor sensor reads high in direct sun (June mean 23.5 °C). The cutoff's
house guard exists because of it.

The ViCare living-room climate sensor (`E3_RoomSensor`) is not usable: the
public API returns only its battery and signal strength, not temperature.

**The Shelly H&T entity IDs name the wrong rooms.** `sensor.bedroom_*` is in
the nursery and `sensor.bathroom_*` in the living room; only the display names
were changed, because HomeKit, the main dashboard and the `Bedroom Humidity`
automation reference the IDs and HA does not rewrite them on a rename. They
report on a 0.3 °C change and go `unavailable` after 24 h of silence
(`expire_after` in `configuration.yaml`), which is what the health alert keys on.

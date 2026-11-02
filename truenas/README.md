# TrueNAS

TrueNAS is a separate box, not managed by Flux or Terraform. This directory
holds the scripts it runs and the hand-made settings it carries.

## Power to Home Assistant

`scripts/host-power-mqtt.py` (shared with the Proxmox host) measures CPU
package power (RAPL, a 30 s average) and the GPU's power and P-state
(`nvidia-smi`), and publishes them to the nats-mqtt leaf at
`mqtt.skaal.dev:8883` as MQTT user `truenas`. HA finds them through MQTT
discovery as a device named after the hostname, with sensors
`<hostname>_cpu_power`, `_gpu_power` and `_gpu_p_state`. Pure stdlib Python,
since nothing can be pip-installed on TrueNAS.

These are components, not the wall figure: the four Exos HDDs (~5 W each at
idle), the board, fans and PSU losses are not measured.

### Setup

1. Add a `truenas` password field (letters and digits only) to the 1Password
   item `MQTT Users` **before** the nats-mqtt change reaches the cluster. The
   StatefulSet reads it through a `secretKeyRef`, so a missing field stalls the
   rolling restart on the first pod.
2. On TrueNAS, copy the script and a root-only password file onto a pool
   dataset — the boot filesystem is replaced on every upgrade:
   ```bash
   sudo install -m 700 host-power-mqtt.py /mnt/<pool>/scripts/host-power-mqtt.py
   ```
   ```bash
   sudo sh -c 'umask 077; cat > /mnt/<pool>/scripts/mqtt-password'
   ```
3. Run it once by hand; silence and no error is success:
   ```bash
   sudo python3 /mnt/<pool>/scripts/host-power-mqtt.py truenas /mnt/<pool>/scripts/mqtt-password
   ```
4. **System → Advanced Settings → Cron Jobs → Add**: that same command, user
   `root`, schedule every minute, with *Hide Standard Output* and
   *Hide Standard Error* left unticked so failures are emailed.

Sensors go `unavailable` five minutes after the last publish (`expire_after`).
If the GPU is removed, the script stops publishing its fields, but the retained
discovery configs remain. Clear them with an empty retained publish to
`homeassistant/sensor/truenas/gpu_power/config` and `.../gpu_pstate/config`.

## GPU persistence mode

The GTX 1080 Ti is unused. With persistence mode off, the driver left it in P0
at 60 W. `nvidia-smi -pm 1` drops it to P8 at 20 W. That does not survive a
reboot, so it is a **Post Init** command under System → Advanced Settings →
Init/Shutdown Scripts. Software cannot get it lower: removing it from the PCI
bus leaves it powered but unmanaged. Pulling the card saves the last 20 W.

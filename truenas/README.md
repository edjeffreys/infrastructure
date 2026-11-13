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

## Static routes: replies to cluster networks leave via Storage

TrueNAS has two legs: Management (10.0.0.94, default route) and Storage
(10.60.0.233). A client on another VLAN that mounts the Storage address would
otherwise get its replies out of the Management leg. The gateway then sees
Management → client-zone traffic that no automatic return rule covers, and
NFS hangs ("server 10.60.0.233 not responding", or `mount.nfs: Connection
timed out`).

So each cluster network has a static route via the Storage gateway, under
Network → Global Configuration → Static Routes:

| Destination | Gateway | For |
|---|---|---|
| `10.11.0.0/24` | `10.60.0.1` | DMZ workers |
| `10.200.0.0/24` | `10.60.0.1` | PIA workers (added 2026-11; until then a Management → PIA allow-all hid the asymmetry, and removing it broke PIA NFS) |

A new VLAN that mounts NFS or SMB from 10.60.0.233 needs its own row. Check
with `ip route get <client-ip>` in the TrueNAS shell: it should say
`via 10.60.0.1`.

The general fix would be source-based routing, so anything sent from
10.60.0.233 leaves via Storage whatever the destination. TrueNAS's UI has no
option for it; it would be a Post Init script:

```bash
ip route replace default via 10.60.0.1 dev <storage-iface> table 100
ip rule add from 10.60.0.233 lookup 100 priority 100
```

# Proxmox

Terraform here manages the VMs on `proxmox-mini-1`, not the host itself.
Host changes made by hand are listed below so they survive a reinstall.

## Host power

Measured with the package RAPL counter, 60 s averages, same workload:
`performance` 40.3 W, `powersave` 36.4 W. Proxmox defaults to `performance`.

`/etc/systemd/system/cpu-powersave.service` sets every core to `powersave` at
boot (EPP becomes `balance_performance` with it). intel_pstate still turbos on
demand; it only stops clocks sitting high between bursts.

```bash
a=$(cat /sys/class/powercap/intel-rapl:0/energy_uj); sleep 60; b=$(cat /sys/class/powercap/intel-rapl:0/energy_uj); awk "BEGIN{printf \"%.1f W\n\", ($b-$a)/60e6}"
```

Not done yet: most PCIe links have ASPM disabled by the BIOS (NVMe, both
ASM1164 SATA controllers, the I226 NICs), which keeps the package out of deep
C-states. Try NVMe and SATA first, in a maintenance window — I226 ASPM has a
history of link drops under `igc`, and this is the only node.

## Power to Home Assistant

`scripts/host-power-mqtt.py` publishes CPU package power (RAPL, a 30 s average)
to the nats-mqtt leaf as MQTT user `proxmox`; HA discovers it as
`sensor.<hostname>_cpu_power`. Setup is as in `truenas/README.md`, except:

- the 1Password field in `MQTT Users` is `proxmox`, added before the nats-mqtt
  change is pushed;
- the root filesystem persists, so the script lives in `/usr/local/bin` and the
  password in `/root/.mqtt-password` (mode 600);
- cron is a file, not a UI:
  ```bash
  echo '* * * * * root /usr/bin/python3 /usr/local/bin/host-power-mqtt.py proxmox /root/.mqtt-password' > /etc/cron.d/host-power-mqtt
  ```

There is no `nvidia-smi` here (the iGPU is passed through to `talos-worker-0`),
so the GPU fields are skipped.

## Changing control-plane VM hardware

The three `talos-cp-*` VMs set `reboot_after_update = false`. Without it, an
apply that changes memory or CPU restarts all three VMs one after another, and
doesn't wait for etcd to rejoin before moving to the next. With three members,
etcd can only lose one at a time.

After applying, Proxmox shows the change as pending. It only takes effect on a
**Proxmox-level** restart (UI → Reboot, or `qm reboot <vmid>`). `talosctl
reboot` won't do it: it restarts the guest without restarting the QEMU process.
Go one node at a time:

1. Reboot `talos-cp-0` (630) in Proxmox.
2. Wait for `kubectl get node talos-cp-0` to show `Ready`, and for
   `talosctl -n 10.0.0.10 etcd status` to list all three members healthy.
3. Repeat for `talos-cp-1` (633, `10.0.0.11`), then `talos-cp-2` (634,
   `10.0.0.12`).

Memory budget on the 64 GB host: the control planes take 3 × 6 GB and the
workers 5 × 8 GB, 58 GB in total.

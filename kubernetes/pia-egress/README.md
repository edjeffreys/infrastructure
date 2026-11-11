# pia-egress

Alerts when the PIA VLAN (`talos-worker-pia-*`, `10.200.0.0/24`) loses its
internet connection.

## Why it exists

All traffic from the PIA nodes leaves through the router's PIA OpenVPN client,
and the kill switch blocks the VLAN whenever the tunnel is down. Nothing in the
cluster watched the tunnel itself. On 2026-11-11 a network blip at 00:57 UTC
made the router reconnect, and PIA refused the old `.ovpn` config ("connection
was rejected by the server"). The VLAN had no internet for about six and a half
hours. The only alerts were side effects that came later: pinchflat stuck in
`ImagePullBackOff`, seerr failing its readiness check (its `/api/v1/status`
calls GitHub), and Flux health checks timing out. Nobody saw that the cause was
the VPN until the investigation got there. Regenerating the config on the
router fixed it.

## How it works

A `prometheus-blackbox-exporter` DaemonSet runs one pod on each PIA node. Every
60s Prometheus has each pod make an HTTPS request to two unrelated endpoints:

- `https://www.gstatic.com/generate_204`, which also exercises DNS
- `https://1.1.1.1/cdn-cgi/trace`, which needs no DNS lookup

The pods' traffic leaves through the node, so it takes the same route as the
arr stack.

| Alert | Fires when |
|-------|------------|
| `PIAEgressDown` | both probes from a node have failed for 5 minutes |
| `PIAEgressProbeMissing` | fewer than 2 probers have been scraped for 15 minutes, which would hide `PIAEgressDown` |

A single endpoint failing does not alert. It shows up as `probe_success == 0`
for that `target` in Prometheus.

## When `PIAEgressDown` fires

1. Check the PIA VPN client on the router. If it says "rejected by the server",
   download a fresh OpenVPN config from PIA and load it on the router. That was
   the fix on 2026-11-11.
2. If only one node is failing, the router is not the problem. Look at that
   node's network instead.
3. Once the tunnel is back, pods stuck in `ImagePullBackOff` on the PIA nodes
   recover on their next retry, which happens within about 5 minutes.

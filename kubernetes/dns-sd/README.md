# dns-sd

CoreDNS on VIP `10.0.0.213`, serving the unicast DNS-SD records that let
iPhones discover LAN services without mDNS. Today that is two printers: the
`Selphy` print queue in `kubernetes/cups/`, and the Brother MFC-L3740CDW at
`10.100.0.44`, advertised directly with no print server in front of it.

## How iOS finds it

iOS browses DNS-SD in its DHCP search domain, `lan.skaal.lan`, as well as over
mDNS. It asks:

1. `b._dns-sd._udp.lan.skaal.lan PTR` — is there a browse domain? → `lan.skaal.lan`
2. `_universal._sub._ipp._tcp.lan.skaal.lan PTR` — any AirPrint printers? → `Selphy._ipp._tcp`
3. `Selphy._ipp._tcp.lan.skaal.lan SRV` and `TXT` — where, and what it can do
4. `cups.lan.skaal.lan A` — the SRV target, `10.0.0.212`

All but the last are in `lan.skaal.lan.db`.

Guests on Hotspot search `guest.skaal.lan` instead. `guest.skaal.lan.db` answers
step 1 for that domain with `lan.skaal.lan`, so guests then browse the same
records as the LAN. The firewall allows Hotspot to reach both printers on 631.

Apple devices also look for browse domains under their subnet.s reverse name
(RFC 6763 §11), e.g. `b._dns-sd._udp.0.0.20.10.in-addr.arpa`. That does not
depend on search domains, so it may work where they do not — with Tailscale on,
where iOS and macOS ignore Tailscale-supplied search domains for browsing.
`reverse-20.db` (LAN) and `reverse-110.db` (Hotspot) answer it with
`lan.skaal.lan`. Experimental: confirm the queries arrive before relying on it.

## UniFi configuration

UniFi's own DNS cannot hold PTR or SRV records, so it forwards just these
names here. Under **Settings → Policy Table → DNS**:

| Type | Domain | Target |
|---|---|---|
| Forward Domain | `_dns-sd._udp.lan.skaal.lan` | `10.0.0.213` |
| Forward Domain | `_tcp.lan.skaal.lan` | `10.0.0.213` |
| Forward Domain | `_dns-sd._udp.guest.skaal.lan` | `10.0.0.213` |
| Forward Domain | `_dns-sd._udp.0.0.20.10.in-addr.arpa` | `10.0.0.213` |
| Forward Domain | `_dns-sd._udp.0.0.110.10.in-addr.arpa` | `10.0.0.213` |
| A | `cups.lan.skaal.lan` | `10.0.0.212` |
| A | `brother.lan.skaal.lan` | `10.100.0.44` |

`_tcp.lan.skaal.lan` rather than `_ipp._tcp.lan.skaal.lan` so a future
service needs a record here and nothing in UniFi.

The A records live in UniFi, not in the zone file, because UniFi would never
forward `cups.lan.skaal.lan` or `brother.lan.skaal.lan` here. The Brother stays
on IoT; its SRV target is a `lan.skaal.lan` name rather than
`printer.iot.skaal.lan` because tailnet split DNS only covers `lan.skaal.lan`,
so with Tailscale on the IoT name never resolves and iOS drops the printer.

With these records, UniFi's mDNS proxy has printer services switched off, so
the printers' own adverts never reach the phones. A printer advertised here
is copied from the printer's `get-printer-attributes` response (`ipptool -tv
ipp://<ip>/ipp/print get-printer-attributes.test`): iOS silently ignores one
whose `URF=` it cannot use.

## Zone file

Each zone claims all of its domain but only ever receives the forwarded
subdomains, so its SOA and NS records are never seen by clients. Queries
outside the zone get `REFUSED`; there is no forwarding and no egress.

Bump the SOA serial when editing it. The ConfigMap name is hashed, so any
change also rolls the pods.

## Exposure

`10.0.0.213`, announced from the control-plane nodes by the `dns-sd`
`CiliumL2AnnouncementPolicy`. Only the UniFi gateway needs to reach it, on
UDP and TCP 53. Cilium SNATs at the announcing node, so the pods see a node
IP, never the client.

The CoreDNS binary carries a file capability, so the container keeps
`NET_BIND_SERVICE`: with every capability dropped, `exec` fails `EPERM`
before CoreDNS starts. The same capability lets it bind 53 as non-root.

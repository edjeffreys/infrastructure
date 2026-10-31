# dns-sd

CoreDNS on VIP `10.0.0.213`, serving the unicast DNS-SD records that let
iPhones discover LAN services without mDNS. Today that is one service: the
`Selphy` print queue in `kubernetes/cups/`.

## How iOS finds it

iOS browses DNS-SD in its DHCP search domain, `lan.skaal.lan`, as well as over
mDNS. It asks:

1. `b._dns-sd._udp.lan.skaal.lan PTR` — is there a browse domain? → `lan.skaal.lan`
2. `_universal._sub._ipp._tcp.lan.skaal.lan PTR` — any AirPrint printers? → `Selphy._ipp._tcp`
3. `Selphy._ipp._tcp.lan.skaal.lan SRV` and `TXT` — where, and what it can do
4. `cups.lan.skaal.lan A` — the SRV target, `10.0.0.212`

All but the last are in `lan.skaal.lan.db`.

## UniFi configuration

UniFi's own DNS cannot hold PTR or SRV records, so it forwards just these
names here. Under **Settings → Policy Table → DNS**:

| Type | Domain | Target |
|---|---|---|
| Forward Domain | `_dns-sd._udp.lan.skaal.lan` | `10.0.0.213` |
| Forward Domain | `_tcp.lan.skaal.lan` | `10.0.0.213` |
| A | `cups.lan.skaal.lan` | `10.0.0.212` |

`_tcp.lan.skaal.lan` rather than `_ipp._tcp.lan.skaal.lan` so a future
service needs a record here and nothing in UniFi.

The A record lives in UniFi, not in the zone file, because UniFi would never
forward `cups.lan.skaal.lan` here.

## Zone file

The zone claims all of `lan.skaal.lan` but only ever receives the forwarded
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

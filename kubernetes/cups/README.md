# cups

A CUPS print server in front of the Canon SELPHY CP1500 at `10.100.0.232`, so
iPhones print through a queue defined in git instead of to the printer's own
AirPrint endpoint.

## Why not print to the Selphy directly

Printing straight to the Selphy gave visibly worse output, and its own app
lays photos out badly. The queue here fixes the settings that matter:

- **Borderless by default.** `Selphy.ppd` defaults to `Postcard.Borderless`
  (100×148 mm, zero margins). The printer's own default is the *bordered*
  postcard. Every size the printer supports is still offered, bordered and
  borderless, for the card and L-size papers.
- **JPEG and URF pass through untouched.** The `cupsFilter2 ... 0 -` lines hand
  both formats to the printer as received. Nothing on this side re-encodes.

`Selphy.ppd` came from `lpadmin -m everywhere` against the printer, with the
four `*Default*` size lines pointed at `Postcard.Borderless` and the
macOS-only `*AP*` keys dropped. If the printer's firmware changes what it
advertises, regenerate it the same way rather than editing sizes by hand.

## Discovery: an AirPrint profile, not mDNS

iPhones normally find printers by mDNS, which a pod behind a Cilium VIP cannot
send onto the LAN. Instead, `airprint.mobileconfig` tells iOS about the queue
directly: `10.0.0.212:631`, resource path `printers/Selphy`. It works across
VLANs and needs no reflector.

Install it once per phone: AirDrop the file to the iPhone, then Settings →
Profile Downloaded → Install. The printer then appears in every print dialog
as **Selphy**. It is unsigned, so iOS labels it "Not Verified"; that is
expected.

`Browsing No` in `cupsd.conf` is deliberate. The image has no avahi, and a
second advertisement would only add another identically named Selphy to the
print dialog.

## Exposure

`10.0.0.212` (IPv4 only — the profile names an address), announced from the
control-plane nodes by the `cups` `CiliumL2AnnouncementPolicy`. Cilium SNATs
at the announcing node, so cupsd sees a node IP, never the phone: **source
filtering happens at the router** — allow the phones' VLAN to `10.0.0.212` on
TCP 631, nothing else. Egress is the printer on TCP 631 and nothing else.

Admin operations over the network are refused (`lpadmin -h` gets
`Forbidden`). Root inside the pod can still run `lpadmin`, but `/etc/cups` is
an `emptyDir` rebuilt from the ConfigMap on every start, so such a change is
lost on the next restart. Change the queue in git.

## The Selphy sleeps

The printer powers itself off when idle. `ErrorPolicy retry-job` keeps the job
queued and retries when it comes back; the CUPS default, `stop-printer`, would
disable the queue on the first failed attempt and leave it disabled.

## Checking what the phone sent

`PreserveJobFiles Yes` keeps every submitted document so the iPhone path can be
verified end to end:

```bash
kubectl -n cups exec deploy/cups -- sh -c 'ls -l /var/spool/cups/d*'
```

A ~1–2 MB JPEG at 1748×1181 means the print Shortcut's output arrived unchanged.
Once that is confirmed, remove `PreserveJobFiles` (the spool is an `emptyDir`,
so it is only cleared by a pod restart).

## Image

`images/cups/` is Debian `cups` + `cups-filters`, built by
`.github/workflows/build-cups.yaml` on the in-cluster buildkitd and pushed to
`ghcr.io/edjeffreys/cups`. The ConfigMap is copied into an `emptyDir` at
startup, because cupsd rewrites `printers.conf` while running.

As with `claude-agent`, the package must be made public once, by hand, after
the first build, at
<https://github.com/users/edjeffreys/packages/container/cups/settings> —
otherwise the pull fails with no imagePullSecret to fall back on. A new push
needs `kubectl -n cups rollout restart deploy/cups` to roll out.

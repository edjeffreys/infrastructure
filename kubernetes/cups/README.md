# cups

A CUPS print server in front of the Canon SELPHY CP1500 at `10.100.0.232`, so
iPhones print through a queue defined in git instead of to the printer's own
AirPrint endpoint.

## Why not print to the Selphy directly

Printing straight to the Selphy gave visibly worse output, and its own app
lays photos out badly. The queue here fixes the settings that matter:

- **Borderless only.** `Selphy.ppd` offers the three paper sizes — card,
  L-size and postcard — in their borderless forms only, defaulting to
  `Postcard.Borderless` (100×148 mm). The bordered forms are removed, not just
  demoted: with both offered, the first iPhone print came out with a white
  border. iOS lists each size once, and most likely asked for "Postcard"
  without zero margins, so CUPS matched the bordered entry.
- **JPEG and URF pass through untouched.** The `cupsFilter2 ... 0 -` lines hand
  both formats to the printer as received. Nothing on this side re-encodes.

`Selphy.ppd` came from `lpadmin -m everywhere` against the printer, with the
bordered size entries deleted, the four `*Default*` size lines pointed at
`Postcard.Borderless`, and the macOS-only `*AP*` keys dropped. If the printer's firmware changes what it
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

`Browsing No` in `cupsd.conf` is deliberate: there is no avahi daemon in the
pod. A side effect is that `printer-dns-sd-name` is empty — cupsd only sets it
after a successful avahi registration — and iOS appears to take a profile
printer's display name from it, so the printer shows with no name.

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

`images/cups/` builds upstream CUPS from the OpenPrinting release tarball
(`CUPS_VERSION`, tracked by Renovate) over Debian's `cups-filters` and
Ghostscript. Debian only ships CUPS 2.4.10 with backported fixes; its libcups
is overwritten by upstream's, which is ABI-compatible within 2.4. Built by
`.github/workflows/build-cups.yaml` on the in-cluster buildkitd and pushed to
`ghcr.io/edjeffreys/cups`. The ConfigMap is copied into an `emptyDir` at
startup, because cupsd rewrites `printers.conf` while running.

The workflow also rebuilds weekly with `--no-cache`, because the Debian
packages are installed unpinned: without a rebuild their security updates
never arrive, and Renovate cannot see them. A new image still only reaches
the pod when it restarts.

As with `claude-agent`, the package must be made public once, by hand, after
the first build, at
<https://github.com/users/edjeffreys/packages/container/cups/settings> —
otherwise the pull fails with no imagePullSecret to fall back on.

To roll out a new image, delete the pod:

```bash
kubectl -n cups delete pod -l app=cups
```

Not `kubectl rollout restart`: Flux removes the `restartedAt` annotation on
its next reconcile, which changes the pod template back and restarts the pod
a second time, minutes later — wiping the spool and any queued jobs.

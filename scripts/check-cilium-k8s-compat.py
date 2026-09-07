#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.9"
# ///
"""Check the Kubernetes version against Cilium's own support list.

renovate.json carries a hand-set allowedVersions ceiling for
kubernetes/kubernetes, but that only stops Renovate *offering* an unsupported
bump. It did nothing about the hand-made one that left this cluster on
Kubernetes 1.36.1 with Cilium 1.19.2, which stood until the dual-stack work
tripped over the native-nft blocker.

The list is read from Cilium's requirements doc **at the tag of the version the
repo deploys**, so it cannot go stale: bump Cilium and the next run reads that
release's own list. Both versions likewise come from what is deployed --
Kubernetes from talos/talconfig.yaml, Cilium from the agent image in
talos/manifests/cilium.yaml (the rendered bootstrap manifest, not the README,
whose helm line is a template).

Anything unexpected -- no network, a moved doc, a layout change -- fails. A
check that passes when it could not read the list is worse than no check.
"""

import pathlib
import re
import sys
import urllib.error
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parent.parent
TALCONFIG = ROOT / "talos" / "talconfig.yaml"
MANIFEST = ROOT / "talos" / "manifests" / "cilium.yaml"

DOC = (
    "https://raw.githubusercontent.com/cilium/cilium/v{version}"
    "/Documentation/network/kubernetes/requirements.rst"
)
TIMEOUT = 30


def fail(message):
    print(message, file=sys.stderr)
    sys.exit(1)


def show(version):
    return ".".join(str(part) for part in version)


def read_kubernetes():
    match = re.search(
        r"^kubernetesVersion:\s*v?(\d+)\.(\d+)\.(\d+)\s*$",
        TALCONFIG.read_text(),
        re.MULTILINE,
    )
    if not match:
        fail(f"no kubernetesVersion found in {TALCONFIG.relative_to(ROOT)}")
    return tuple(int(part) for part in match.groups())


def read_cilium():
    tags = set(
        re.findall(r"quay\.io/cilium/cilium:v(\d+)\.(\d+)\.(\d+)", MANIFEST.read_text())
    )
    if not tags:
        fail(f"no cilium agent image found in {MANIFEST.relative_to(ROOT)}")
    if len(tags) > 1:
        found = ", ".join(sorted(show([int(p) for p in t]) for t in tags))
        fail(
            f"{MANIFEST.relative_to(ROOT)} pins more than one cilium agent "
            f"version ({found}) -- re-render it"
        )
    return tuple(int(part) for part in tags.pop())


def fetch_supported(cilium):
    url = DOC.format(version=show(cilium))
    try:
        with urllib.request.urlopen(url, timeout=TIMEOUT) as response:
            doc = response.read().decode()
    except (urllib.error.URLError, TimeoutError) as error:
        fail(f"could not fetch {url}: {error}")

    section = re.search(r"Kubernetes Version\n=+\n(.*?)\nAdditionally", doc, re.S)
    if not section:
        fail(
            f"{url} no longer has a parseable 'Kubernetes Version' section -- "
            f"upstream changed the doc, so this script needs updating"
        )
    supported = re.findall(r"^\* (\d+)\.(\d+)$", section.group(1), re.M)
    if not supported:
        fail(f"{url} listed no Kubernetes versions -- upstream changed the format")
    return [tuple(int(part) for part in entry) for entry in supported]


def main():
    kubernetes = read_kubernetes()
    cilium = read_cilium()
    supported = fetch_supported(cilium)
    pair = f"Cilium {show(cilium)} / Kubernetes {show(kubernetes)}"
    listed = ", ".join(show(entry) for entry in supported)

    if kubernetes[:2] not in supported:
        fail(
            f"{pair}: Kubernetes {show(kubernetes[:2])} is not e2e-tested against "
            f"this Cilium. Cilium {show(cilium)} lists {listed}. Upgrade Cilium "
            f"first, never in the same change."
        )

    print(f"{pair}: e2e-tested (Cilium {show(cilium)} lists {listed})")


if __name__ == "__main__":
    main()

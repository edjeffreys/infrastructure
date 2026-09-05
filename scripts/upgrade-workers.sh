#!/usr/bin/env bash
set -e

# Kept in step with talosVersion in talos/talconfig.yaml by Renovate.
# The factory image (not ghcr.io/siderolabs/installer) is required: the plain
# installer carries no system extensions, so upgrading with it would strip
# iscsi-tools, nfs-utils and tailscale off the node.
#
# Adding or removing an extension in talconfig.yaml changes this hash, and
# running an upgrade is the ONLY way existing nodes pick the change up —
# talosctl apply-config does not touch the installed system. Re-read it after
# any extension change with:
#   talhelper genurl installer -c talos/talconfig.yaml
# and update talos_worker_schematic_id in terraform/proxmox/talos.tf to match.
#
# Current set: i915, intel-ucode, iscsi-tools, util-linux-tools, nfs-utils,
# qemu-guest-agent, tailscale.
SCHEMATIC="bb2bf97151447e3e0b8e3b0726805f1050a59e4e780b366211ba71bc19c07a7c"
IMAGE="factory.talos.dev/metal-installer/${SCHEMATIC}:v1.13.10"

# Control planes run a DIFFERENT schematic (qemu-guest-agent + tailscale only)
# and are deliberately not in this script: they must go one at a time with an
# etcd quorum check between each. Take that image from talhelper genurl
# installer -- using this one on a control plane adds extensions it should not
# have, and using the plain installer strips the ones it should.

# Addressed by IP rather than tailnet hostname on purpose: ext-tailscale starts
# late in the boot sequence, so a just-rebooted node answers on its LAN address
# before its MagicDNS name resolves again.
NODES=(
  "10.11.0.10:talos-worker-0"
  "10.11.0.11:talos-worker-1"
  "10.11.0.12:talos-worker-2"
  "10.200.0.10:talos-worker-pia-0"
  "10.200.0.11:talos-worker-pia-1"
)

# Checked after each upgrade. A wrong image does not fail the upgrade -- it
# silently returns a node without i915 or nfs-utils, which surfaces much later
# as an unschedulable plex or a PVC that will not mount.
EXPECTED_EXTENSIONS="i915,intel-ucode,iscsi-tools,nfs-utils,qemu-guest-agent,tailscale,util-linux-tools"

# Wait until the cluster is ready for the next node, not merely until this one
# reports Ready.
#
# Talos cordons and drains on upgrade but ignores PodDisruptionBudgets while
# doing so (siderolabs/talos#9882), so nothing else stops the next node going
# down while a 2-replica Longhorn volume is still rebuilding from the last one.
# Rebuilds here run to ~90s; the fixed `sleep 30` this replaced was too short
# and gave no signal that it was.
wait_healthy() {
  local deadline=$((SECONDS + ${1:-900}))
  local notready degraded cnpg
  while [ "$SECONDS" -lt "$deadline" ]; do
    notready=$(kubectl get nodes --no-headers 2>/dev/null | awk '$2!="Ready"' | wc -l | tr -d ' ')
    degraded=$(kubectl -n longhorn-system get volumes.longhorn.io --no-headers 2>/dev/null \
                 | awk '$3=="attached" && $4!="healthy"' | wc -l | tr -d ' ')
    cnpg=$(kubectl -n arr get cluster arr-postgres -o jsonpath='{.status.readyInstances}' 2>/dev/null)
    printf '    nodes_notready=%s longhorn_degraded=%s cnpg_ready=%s/2\n' \
           "$notready" "$degraded" "${cnpg:-?}"
    if [ "$notready" = "0" ] && [ "$degraded" = "0" ] && [ "${cnpg:-0}" = "2" ]; then
      echo "    cluster healthy"
      return 0
    fi
    sleep 20
  done
  echo "!!! timed out waiting for the cluster to settle - stopping before the next node" >&2
  return 1
}

check_extensions() {
  local ip="$1" name="$2" got
  got=$(talosctl -n "$ip" -e "$ip" get extensions -o json 2>/dev/null | python3 -c '
import sys, json
dec = json.JSONDecoder(); buf = sys.stdin.read(); i = 0; out = []
while i < len(buf):
    while i < len(buf) and buf[i].isspace(): i += 1
    if i >= len(buf): break
    obj, i = dec.raw_decode(buf, i)
    n = obj.get("spec", {}).get("metadata", {}).get("name")
    if n and n not in ("modules.dep", "schematic"): out.append(n)
print(",".join(sorted(out)))
')
  if [ "$got" != "$EXPECTED_EXTENSIONS" ]; then
    echo "!!! ${name} came back with the wrong extensions - wrong schematic?" >&2
    echo "    expected: ${EXPECTED_EXTENSIONS}" >&2
    echo "    got:      ${got}" >&2
    return 1
  fi
  echo "    extensions intact"
}

for entry in "${NODES[@]}"; do
  IP="${entry%%:*}"
  NAME="${entry##*:}"

  echo "==> Upgrading ${NAME} (${IP})"
  talosctl upgrade --nodes "$IP" -e "$IP" --image "$IMAGE" --wait=false

  echo "    Waiting for ${NAME} to go NotReady..."
  kubectl wait node/"$NAME" --for=condition=Ready=false --timeout=120s 2>/dev/null || true

  echo "    Waiting for ${NAME} to come back Ready..."
  kubectl wait node/"$NAME" --for=condition=Ready --timeout=300s

  check_extensions "$IP" "$NAME"

  echo "    Waiting for the cluster to settle before the next node..."
  wait_healthy
done

echo "All nodes upgraded."

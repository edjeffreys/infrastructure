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

NODES=(
  "10.11.0.10:talos-worker-0"
  "10.11.0.11:talos-worker-1"
  "10.11.0.12:talos-worker-2"
  "10.200.0.10:talos-worker-pia-0"
  "10.200.0.11:talos-worker-pia-1"
)

for entry in "${NODES[@]}"; do
  IP="${entry%%:*}"
  NAME="${entry##*:}"

  echo "==> Upgrading ${NAME} (${IP})"
  talosctl upgrade --nodes "$IP" -e "$IP" --image "$IMAGE" --wait=false

  echo "    Waiting for ${NAME} to go NotReady..."
  kubectl wait node/"$NAME" --for=condition=Ready=false --timeout=120s 2>/dev/null || true

  echo "    Waiting for ${NAME} to come back Ready..."
  kubectl wait node/"$NAME" --for=condition=Ready --timeout=300s

  echo "    ${NAME} ready. Sleeping 30s before next node..."
  sleep 30
done

echo "All nodes upgraded."

#!/usr/bin/env bash
# Usage: refresh-nats-mac-certs.sh [install]   — see kubernetes/nats/README.md
set -euo pipefail

DEST="${HOME}/.config/nats/homelab"
LABEL="dev.skaal.nats-certs"
PLIST="${HOME}/Library/LaunchAgents/${LABEL}.plist"
# launchd gives agents a bare PATH.
export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"

if [ "${1:-}" = "install" ]; then
  script="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
  mkdir -p "$(dirname "$PLIST")" "$DEST"
  cat >"$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>${LABEL}</string>
  <key>ProgramArguments</key><array><string>${script}</string></array>
  <key>StartCalendarInterval</key><dict><key>Hour</key><integer>9</integer><key>Minute</key><integer>15</integer></dict>
  <key>RunAtLoad</key><true/>
  <key>StandardOutPath</key><string>${DEST}/refresh.log</string>
  <key>StandardErrorPath</key><string>${DEST}/refresh.log</string>
</dict>
</plist>
EOF
  launchctl bootout "gui/$(id -u)/${LABEL}" 2>/dev/null || true
  launchctl bootstrap "gui/$(id -u)" "$PLIST"
  echo "installed ${PLIST}; log: ${DEST}/refresh.log"
  exit 0
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
umask 077

kubectl -n nats get configmap homelab-ca -o jsonpath='{.data.ca\.crt}' >"$tmp/ca.crt"
for s in ed-nats ed-sys-nats; do
  kubectl -n nats get secret "$s" -o jsonpath='{.data.tls\.crt}' | base64 -d >"$tmp/$s.crt"
  kubectl -n nats get secret "$s" -o jsonpath='{.data.tls\.key}' | base64 -d >"$tmp/$s.key"
  # A half-written Secret mid-rotation must not replace a working pair.
  [ "$(openssl x509 -in "$tmp/$s.crt" -noout -pubkey)" = "$(openssl pkey -in "$tmp/$s.key" -pubout)" ]
  openssl x509 -in "$tmp/$s.crt" -noout -checkend 0 >/dev/null
done

mkdir -p "$DEST"
chmod 700 "$DEST"
changed=0
for f in ca.crt ed-nats.crt ed-nats.key ed-sys-nats.crt ed-sys-nats.key; do
  if ! cmp -s "$tmp/$f" "$DEST/$f"; then
    mv -f "$tmp/$f" "$DEST/$f"
    changed=1
  fi
done
chmod 600 "$DEST"/*.key
chmod 644 "$DEST"/*.crt

echo "$(date '+%F %T') $([ "$changed" = 1 ] && echo updated || echo unchanged), expires $(openssl x509 -in "$DEST/ed-nats.crt" -noout -enddate | cut -d= -f2)"

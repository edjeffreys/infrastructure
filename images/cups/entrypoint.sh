#!/bin/sh
set -eu

# cupsd rewrites printers.conf at runtime, so it cannot run off the read-only
# ConfigMap mount.
mkdir -p /etc/cups/ppd /etc/cups/ssl
cp -L /config/cupsd.conf /config/cups-files.conf /config/printers.conf /etc/cups/
cp -L /config/*.ppd /etc/cups/ppd/

exec cupsd -f

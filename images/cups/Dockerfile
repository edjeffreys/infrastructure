FROM debian:trixie-slim

# No avahi: phones find the queue through an AirPrint profile, not mDNS
# (kubernetes/cups/README.md).
RUN apt-get update \
 && apt-get install -y --no-install-recommends cups cups-filters \
 && rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh /usr/local/bin/entrypoint.sh

EXPOSE 631
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]

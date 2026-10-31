FROM debian:trixie-slim AS build

# renovate: datasource=github-releases depName=OpenPrinting/cups extractVersion=^v(?<version>.+)$
ARG CUPS_VERSION=2.4.20

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      build-essential ca-certificates curl pkg-config \
      libavahi-client-dev libgnutls28-dev zlib1g-dev \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /src
RUN curl -fsSL "https://github.com/OpenPrinting/cups/releases/download/v${CUPS_VERSION}/cups-${CUPS_VERSION}-source.tar.gz" \
      | tar -xz --strip-components=1 \
 && ./configure \
      --prefix=/usr --sysconfdir=/etc --localstatedir=/var \
      --libdir="/usr/lib/$(gcc -dumpmachine)" \
      --with-tls=gnutls --with-dnssd=avahi \
      --disable-pam --disable-gssapi --with-systemd=no --with-ondemand=no --with-rcdir=no \
      --with-cups-user=lp --with-cups-group=lp --with-system-groups=root \
 && make -j"$(nproc)" \
 && make install DESTDIR=/out \
 && rm -rf /out/var/run

FROM debian:trixie-slim

# cups-filters and ghostscript are Debian's and pull in Debian's libcups 2.4;
# the COPY below replaces it with upstream's, which keeps the same ABI.
RUN apt-get update \
 && apt-get upgrade -y \
 && apt-get install -y --no-install-recommends cups-filters ghostscript libavahi-client3 python3-pil \
 && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/ /
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
COPY --chmod=0755 selphy-bleed /usr/lib/cups/filter/selphy-bleed

EXPOSE 631
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]

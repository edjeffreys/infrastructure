# renovate: datasource=docker depName=golang
ARG GO_VERSION=1.25.1
# renovate: datasource=docker depName=debian
ARG DEBIAN_VERSION=trixie-slim

FROM golang:${GO_VERSION}-trixie AS build
WORKDIR /src
# Dependencies first, so a source-only change reuses the module layer.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/conform ./cmd/conform

FROM debian:${DEBIAN_VERSION}
# QuickSync needs the iHD VA-API driver, which Debian ships only in non-free —
# not enabled by default in the slim image, so the component is added here
# rather than silently falling back to software encoding at runtime.
RUN . /etc/os-release \
 && echo "deb http://deb.debian.org/debian ${VERSION_CODENAME} main non-free non-free-firmware" \
      > /etc/apt/sources.list.d/non-free.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends \
      ffmpeg \
      intel-media-va-driver-non-free \
      libvpl2 \
      vainfo \
      ca-certificates \
      tini \
 && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/conform /usr/local/bin/conform

# Matches the arr stack, plex and tdarr so rewritten files keep consistent
# ownership on the shared NFS export.
USER 3000:3000
ENV LIBVA_DRIVER_NAME=iHD

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/conform"]
CMD ["apply", "-config", "/config/conform.yaml", "-interval", "6h"]

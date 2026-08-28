FROM node:26-bookworm-slim

# renovate: datasource=github-releases depName=kubernetes/kubernetes
ARG KUBECTL_VERSION=v1.36.1
# renovate: datasource=npm depName=@anthropic-ai/claude-code
ARG CLAUDE_CODE_VERSION=2.1.237
# renovate: datasource=github-releases depName=cli/cli extractVersion=^v(?<version>.+)$
ARG GH_VERSION=2.63.2
# renovate: datasource=github-releases depName=github/github-mcp-server extractVersion=^v(?<version>.+)$
ARG GITHUB_MCP_VERSION=1.11.0
# renovate: datasource=github-releases depName=containers/kubernetes-mcp-server extractVersion=^v(?<version>.+)$
ARG K8S_MCP_VERSION=0.0.66

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl git jq less ripgrep tini tzdata unzip \
 && rm -rf /var/lib/apt/lists/*

# kubectl only. talosctl is deliberately absent: a talosconfig is an admin
# cert with no revocation short of rotating the Talos CA, and talosctl can
# reboot or wipe nodes. Add it as a separate break-glass step, never here.
RUN arch="$(dpkg --print-architecture)" \
 && curl -fsSLo /usr/local/bin/kubectl \
      "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${arch}/kubectl" \
 && chmod 0755 /usr/local/bin/kubectl \
 && kubectl version --client

RUN arch="$(dpkg --print-architecture)" \
 && curl -fsSL "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_${arch}.tar.gz" \
      | tar -xz -C /tmp \
 && install -m 0755 "/tmp/gh_${GH_VERSION}_linux_${arch}/bin/gh" /usr/local/bin/gh \
 && rm -rf /tmp/gh_* \
 && gh --version

# MCP servers. Both are single static binaries, so they cost a download rather
# than a whole runtime — no npx-at-startup, and the versions stay pinned and
# Renovate-managed like everything else here.
RUN arch="$(dpkg --print-architecture)" \
 && case "${arch}" in amd64) gh_arch=x86_64 ;; arm64) gh_arch=arm64 ;; esac \
 && curl -fsSL "https://github.com/github/github-mcp-server/releases/download/v${GITHUB_MCP_VERSION}/github-mcp-server_Linux_${gh_arch}.tar.gz" \
      | tar -xz -C /tmp github-mcp-server \
 && install -m 0755 /tmp/github-mcp-server /usr/local/bin/github-mcp-server \
 && rm -f /tmp/github-mcp-server \
 && curl -fsSLo /usr/local/bin/kubernetes-mcp-server \
      "https://github.com/containers/kubernetes-mcp-server/releases/download/v${K8S_MCP_VERSION}/kubernetes-mcp-server-linux-${arch}" \
 && chmod 0755 /usr/local/bin/kubernetes-mcp-server \
 && github-mcp-server --version \
 && kubernetes-mcp-server --version

RUN npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
 && npm cache clean --force

COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 0755 /usr/local/bin/entrypoint.sh

# node:22 ships uid/gid 1000 as "node"; the Deployment pins the same numbers.
USER 1000:1000
ENV HOME=/home/agent
# claude-code is installed globally as root but runs as uid 1000, so the
# auto-updater can never write to the npm prefix and retries noisily forever.
# It should be off regardless: the version is pinned by CLAUDE_CODE_VERSION and
# bumped by Renovate, and an in-pod update would drift from that pin and vanish
# on the next restart anyway.
ENV DISABLE_AUTOUPDATER=1
WORKDIR /home/agent

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/entrypoint.sh"]

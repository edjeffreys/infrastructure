# claude-agent

A headless Claude Code session running in-cluster, paired to your Claude account
via Remote Control so you can prompt it from the Claude mobile app.

## Shape

The pod runs `claude --remote-control homelab` against a checkout of this repo.
Remote Control is an **outbound** connection to Anthropic — there is no
listening port, no Service, and no Tailscale ingress. Nothing in the cluster is
exposed by running this.

## What it can and cannot do

**Kubernetes: read-only, no Secrets.** The ServiceAccount is bound to the
built-in `view` ClusterRole — which upstream excludes Secrets from by design —
plus `claude-agent-cluster-read` for cluster-scoped resources. No `create`,
`update`, `delete`, `patch` or `pods/exec` anywhere.

**Git: PR-write.** This is where the agent actually changes things. Everything
here deploys through Flux, so the agent doesn't need mutating kube verbs to
change the cluster — it edits the repo and opens a PR, and you merge from your
phone. That merge is the only path from "agent decided something" to "cluster
changed".

**Talos: none.** `talosctl` is not in the image. See the note in the Dockerfile.

## MCP servers

Two, both pinned static binaries in the image and configured by
`claude-agent-mcp` (mounted into `/etc/claude` alongside the settings via a
projected volume). Started with `--strict-mcp-config`, so that ConfigMap is the
complete set — the agent cannot add servers of its own.

| Server | Binary | Notes |
|--------|--------|-------|
| `kubernetes` | `kubernetes-mcp-server` | `--cluster-provider in-cluster --read-only`. Uses the pod's ServiceAccount, so it inherits the RBAC above — the flag is defence in depth, not the boundary. Pre-allowed in settings, so it never prompts. |
| `github` | `github-mcp-server` | `stdio`, authenticated with `${GH_TOKEN}`. Deliberately *not* `--read-only`: the token's own scope (one repo, contents + PRs) is the boundary, and PR creation is the point. Writes still prompt on your phone. |

## Bootstrap

### 1. Create the 1Password item

Vault `Secrets`, item name `Claude Agent`, one field named exactly:

| Field | Value |
|-------|-------|
| `GH_TOKEN` | fine-grained PAT, `contents:write` + `pull_requests:write`, scoped to `edjeffreys/infrastructure` only |

**Anthropic auth is deliberately not here.** `claude setup-token` /
`CLAUDE_CODE_OAUTH_TOKEN` produces an *inference-only* credential, and Remote
Control refuses it outright:

> Remote Control requires a full-scope login token. Long-lived tokens (from
> `claude setup-token` or `CLAUDE_CODE_OAUTH_TOKEN`) are limited to
> inference-only for security reasons.

So auth is a one-time interactive `/login` (step 5) whose credentials live on
the PVC. The entrypoint unsets `CLAUDE_CODE_OAUTH_TOKEN` defensively, because if
it were ever set it would take precedence over those credentials and silently
put the session back into inference-only mode.

The GitHub token needs `contents:write` because opening a PR requires pushing a
branch first. Branch protection on `master` is what keeps that honest — set it
if you haven't.

### 2. Build and push the image

Built in CI: `.github/workflows/build-claude-agent.yaml` runs on a push to
`master` that touches `images/claude-agent/**`, or on demand
(`workflow_dispatch`) — including when Renovate bumps the `ARG` pins in the
Dockerfile and merges. It runs as a Kubernetes-mode ARC job pod holding only
the `buildctl` client — the build itself happens in the in-cluster `buildkitd`
(`kubernetes/buildkit/`), so this still needs no Docker daemon in the runner
pod and no privileged dind sidecar — see `containerMode` in
`kubernetes/arc-runners/runner-scale-set-values.yaml`. It pushes both
`:latest` and a `:sha-<short-sha>` tag, authenticated with the workflow's own
`GITHUB_TOKEN` (scoped to this repo, unlike the classic PAT the steps below
need).

The package must still be made public once, by hand, at
<https://github.com/users/edjeffreys/packages/container/claude-agent/settings>.
The image holds no credentials — tokens arrive as env vars at runtime — and a
public package avoids an imagePullSecret here. That secret would have to be a
hand-applied `op inject` bootstrap file: `imagePullSecrets` requires type
`kubernetes.io/dockerconfigjson`, and `OnePasswordItem` only ever produces
`Opaque`.

CI does not touch the cluster (the ARC runner ServiceAccount has no
`patch`/`update` on Deployments), so a new push still needs a manual restart to
actually roll out:

```bash
kubectl -n claude-agent rollout restart deploy/claude-agent
```

**Local/manual build (break-glass, e.g. testing a Dockerfile change before
pushing).** The cluster is amd64; if you're on arm64, `--platform` is
mandatory or the pod crashloops with `exec format error`. This needs a
**classic** PAT with `write:packages` — fine-grained PATs cannot publish
user-owned ghcr packages. It is local-only and is never stored in the cluster.

```bash
docker login ghcr.io -u edjeffreys
docker buildx build --platform linux/amd64 --push -t ghcr.io/edjeffreys/claude-agent:latest images/claude-agent
```

### 3. Deploy

Merge to `master`; Flux's `claude-agent` Kustomization
(`flux/apps/claude-agent.yaml`) picks up `kubernetes/claude-agent/`.

### 4. Verify the boundary holds

```bash
kubectl auth can-i --list --as=system:serviceaccount:claude-agent:claude-agent
kubectl auth can-i get secrets -A --as=system:serviceaccount:claude-agent:claude-agent
```

The second must print `no`. Re-run it after installing any new operator: `view`
is an aggregated ClusterRole, so a chart shipping a rule labelled
`rbac.authorization.k8s.io/aggregate-to-view: "true"` can widen it without
touching anything in this directory.

### 5. Log in and pair (one-time, interactive)

This step cannot be automated — see step 1. Attach to the pty:

```bash
kubectl -n claude-agent attach -it deploy/claude-agent
```

Then, in the session:

1. `/login` — completes the OAuth paste-code flow. Needs a real terminal; a
   piped stdin fails with `tty and stderr cannot both be true`.
2. `/remote-control` — registers the session with your account.

Detach with **Ctrl-P Ctrl-Q**. **Not** `Ctrl-C`, which kills the session and
restarts the pod.

Credentials land in `~/.claude/.credentials.json` on the Longhorn PVC, so this
is once per volume, not once per restart. Delete the PVC and you redo it.

Confirm it actually registered — a session file alone is not proof, since every
session writes one for local IPC regardless. Look for a live outbound
connection instead:

```bash
kubectl -n claude-agent exec deploy/claude-agent -- sh -c 'ls -l /proc/7/fd | grep -c socket'
```

## Gotchas

- **The pty is load-bearing.** `stdin: true` + `tty: true` on the container are
  required; Remote Control runs the interactive TUI. Remove them and the pod
  crashloops with no useful error.
- **The PVC holds the login.** `~/.claude/.credentials.json` lives on
  `claude-agent-home`. This is the one piece of state that is *not* reproducible
  from git or 1Password — Remote Control requires a full-scope interactive
  login, which no secret manager can supply. Delete the PVC and you redo step 5
  by hand. Worth knowing before any Longhorn volume surgery.
- **`Recreate` strategy, not `RollingUpdate`.** RWO Longhorn volume, and two
  sessions registering the same Remote Control name at once is not a state
  worth discovering.
- **Prompt injection is the real risk here.** The agent reads pod logs, Flux
  status and GitHub issues — all attacker-influenceable text reaching something
  that holds a GitHub token. Read-only RBAC caps the blast radius; the human
  merge gate on every PR is the actual mitigation. Don't remove it.

# claude-agent

A headless Claude Code Remote Control server running in-cluster, paired to your
Claude account so you can start sessions from the Claude mobile app.

## Shape

The pod runs a Remote Control **server** in `~/Projects`
(`claude remote-control --spawn same-dir --no-create-session-in-dir`). It shows
in the app as the device `homelab` — the pod's pinned `hostname` — with no
sessions until you start one, up to `CAPACITY` (3) at once.

It is not tied to one repo. Every session gets the managed memory in
`memory.yaml` (`/etc/claude-code/CLAUDE.md`), which tells it to clone into
`~/Projects/<repo>` if missing, fetch if present, and do all work in a worktree
of its own under `~/Projects/.worktrees/<repo>/<branch>`, branched from the
fresh default branch. The main clones are shared by every session and never
worked in, so parallel tasks cannot trample each other or start from stale code.

`--spawn worktree` was the obvious alternative and is wrong here: it needs the
server's own directory to be a repo, so it would tie the device to one.

Capacity is bounded by memory, not by Claude: each session is a full Claude
Code process plus its MCP servers, and the standard workers have 8 GB. Raise
`CAPACITY` and the memory limit together.

Remote Control is an **outbound** connection to Anthropic — there is no
listening port, no Service, and no Tailscale ingress. Nothing in the cluster is
exposed by running this.

## What it can and cannot do

**Kubernetes: read-only, no Secrets.** The ServiceAccount is bound to the
built-in `view` ClusterRole — which upstream excludes Secrets from by design —
plus `claude-agent-cluster-read` for cluster-scoped resources. No `create`,
`update`, `delete`, `patch` or `pods/exec` anywhere.

**Git: PR-write, on every one of your repos.** This is where the agent
actually changes things. Everything here deploys through Flux, so the agent
doesn't need mutating kube verbs to change the cluster — it edits the repo and
opens a PR, and you merge from your phone. That merge is the only path from
"agent decided something" to "cluster changed".

The token reaches **all** repos by choice, for convenience. The cost is the
blast radius: one successful prompt injection can push branches anywhere you
own — `infrastructure-archive` included, which carries credentials in its
history. So **every repo needs branch protection requiring your review on its
default branch**; that, not the token's scope, is what stops a branch becoming
a merge. Check it when you create a repo.

**Talos: none.** `talosctl` is not in the image. See the note in the Dockerfile.

## MCP servers

Two, both pinned static binaries in the image and configured by
`claude-agent-mcp`. Settings and MCP config are both **managed policy**: the
ConfigMaps are mounted read-only at `/etc/claude-code/managed-settings.json` and
`managed-mcp.json`. Server mode has no `--settings` / `--mcp-config` /
`--strict-mcp-config` flags, and managed policy is stricter anyway — every
spawned session gets it, `managed-mcp.json` takes exclusive control (the agent
cannot add servers of its own), and `allowManagedPermissionRulesOnly` stops a
session widening its allow list via the writable `~/.claude/settings.json` on
the PVC. The cost: "always allow" from your phone won't stick — add the rule to
`settings.yaml` instead.

| Server | Binary | Notes |
|--------|--------|-------|
| `kubernetes` | `kubernetes-mcp-server` | `--cluster-provider in-cluster --read-only`. Uses the pod's ServiceAccount, so it inherits the RBAC above — the flag is defence in depth, not the boundary. Pre-allowed in settings, so it never prompts. |
| `github` | `github-mcp-server` | `stdio`, authenticated with `${GH_TOKEN}`. Deliberately *not* `--read-only`: the token's own scope (one repo, contents + PRs) is the boundary, and PR creation is the point. Writes still prompt on your phone. |

## Bootstrap

### 1. Create the 1Password item

Vault `Secrets`, item name `Claude Agent`, one field named exactly:

| Field | Value |
|-------|-------|
| `GH_TOKEN` | fine-grained PAT, `contents:write` + `pull_requests:write`, **All repositories** |

**Anthropic auth is deliberately not here.** `claude setup-token` /
`CLAUDE_CODE_OAUTH_TOKEN` produces an *inference-only* credential, and Remote
Control refuses it outright:

> Remote Control requires a full-scope login token. Long-lived tokens (from
> `claude setup-token` or `CLAUDE_CODE_OAUTH_TOKEN`) are limited to
> inference-only for security reasons.

So auth is an interactive `claude auth login` (step 5) whose credentials live
on the PVC, and it has to be **repeated roughly monthly** — see "The login
expires" under Gotchas. The entrypoint unsets `CLAUDE_CODE_OAUTH_TOKEN` defensively, because if
it were ever set it would take precedence over those credentials and silently
put the session back into inference-only mode.

The GitHub token needs `contents:write` because opening a PR requires pushing a
branch first. Branch protection on each repo's default branch is what keeps
that honest — see "Git" above.

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
actually roll out. Delete the pod rather than `rollout restart`, which Flux
turns into two restarts:

```bash
kubectl -n claude-agent delete pod -l app=claude-agent
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

### 5. Log in (interactive, and again whenever it expires)

This step cannot be automated — see step 1. While logged out the entrypoint
waits, printing the command below every minute, and the pod stays NotReady.

```bash
kubectl -n claude-agent exec -it deploy/claude-agent -- claude auth login
```

It prints a URL; open it, approve, and paste the code back. `-it` is required —
the paste-code flow needs a real terminal. The entrypoint notices the login
within a minute and starts the Remote Control server; the `homelab` device then
appears in the app.

Credentials land in `~/.claude/.credentials.json` on the Longhorn PVC, so this
survives restarts. Delete the PVC and you redo it.

Check it:

```bash
kubectl -n claude-agent exec deploy/claude-agent -- claude auth status --text
kubectl -n claude-agent get pod -l app=claude-agent   # READY 1/1
```

**Re-logging in while the server is running:** log in as above, then delete the
pod so the server restarts on the fresh credentials.

## Gotchas

- **The login expires, roughly monthly, and nothing can renew it.** The
  full-scope OAuth login carries a refresh token with a fixed expiry
  (`refreshTokenExpiresAt`); when it passes, Claude wipes the tokens from
  `.credentials.json` and every session dies. The pod does **not** restart — the
  first time, it sat running and silently signed out for seven weeks. The
  readiness probe (`auth-check` in the image, `claude auth status` plus
  `loginExpiresAt`) now turns the pod NotReady when logged out **or within 3 days
  of expiry**, so `KubePodNotReady` fires with time to re-login at your
  convenience. Tune the margin with `LOGIN_WARN_DAYS`.
- **`stdin`/`tty` were load-bearing for the old TUI mode.** `claude
  --remote-control` ran the interactive TUI and crashlooped without a pty.
  Server mode may not need them; they are kept until that is confirmed.
- **The PVC holds the login.** `~/.claude/.credentials.json` lives on
  `claude-agent-home`. This is the one piece of state that is *not* reproducible
  from git or 1Password — Remote Control requires a full-scope interactive
  login, which no secret manager can supply. Delete the PVC and you redo step 5
  by hand. Worth knowing before any Longhorn volume surgery.
- **`Recreate` strategy, not `RollingUpdate`.** RWO Longhorn volume, and two
  servers registering the same Remote Control name at once is not a state
  worth discovering.
- **The worktree discipline is an instruction, not a mechanism.** Nothing stops
  a session working in a main clone except `memory.yaml`. If a session ever
  ends up on a branch in `~/Projects/<repo>`, the next session to fetch there
  inherits it — the stale-checkout problem the layout exists to avoid.
- **Worktrees accumulate on the PVC.** Sessions are told to remove theirs once
  the PR is open, but an interrupted one leaves it behind. List them with
  `ls ~/Projects/.worktrees/*`; `git worktree prune` in a repo clears entries
  whose directory is already gone.
- **Prompt injection is the real risk here.** The agent reads pod logs, Flux
  status and GitHub issues — all attacker-influenceable text reaching something
  that holds a GitHub token. Read-only RBAC caps the blast radius; the human
  merge gate on every PR is the actual mitigation. Don't remove it.

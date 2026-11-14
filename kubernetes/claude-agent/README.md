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

**Builds: Rust, Zig and a C toolchain.** The image carries `build-essential`,
stable Rust (with clippy and rustfmt) and Zig, all pinned and Renovate-managed,
so sessions can build and test repos such as `shroud` rather than only edit
them. `CARGO_HOME` is on the PVC so the crate cache survives restarts; the
toolchains themselves are in the image and owned by uid 1000, so a session can
`rustup toolchain install nightly` for fuzzing, but it is gone on the next
restart. `lsof` is there because shroud's smoke test uses it to find the
server process; without it, the idle-CPU and server-restart checks fail.

Build intermediates do **not** go on the PVC. `CARGO_BUILD_BUILD_DIR` and
`ZIG_GLOBAL_CACHE_DIR` point at `/scratch`, a 20Gi node-local `emptyDir`.
On the PVC they would be written twice (Longhorn replicas on the one physical
disk) and copied to TrueNAS daily, all to keep files a rebuild recreates in
minutes. Cargo's `{workspace-path-hash}` gives every worktree its own build
dir, so parallel sessions don't serialise on one lock. Only final binaries land
in each worktree's `target/`. `/scratch` is wiped on every restart, so the first
build after one starts cold.

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
| `kubernetes` | `kubernetes-mcp-server` | `cluster_provider_strategy = "in-cluster"` and `read_only = true`, in the `kubernetes-mcp-server.toml` key of `claude-agent-mcp`: 0.1.0 dropped the equivalent CLI flags. Uses the pod's ServiceAccount, so it inherits the RBAC above — the flag is defence in depth, not the boundary. Pre-allowed in settings, so it never prompts. |
| `github` | `github-mcp-server` | `stdio`, authenticated with `${GH_TOKEN}`. Deliberately *not* `--read-only`: the token's own scope (one repo, contents + PRs) is the boundary, and PR creation is the point. Writes still prompt on your phone. |

## Alert dispatch

When Alertmanager fires, the pod starts a Remote Control session to
investigate. It shows in the app under a title Claude generates from the prompt, and in `claude agents` as `alert-<MMDD-HHMM>-<alertname>` (set with `--name`, which is what the dispatcher keys on), runs
in auto mode, may open PRs, and sends a push notification when it's done.

```
Alertmanager → alert-relay → NATS ALERTS → alert-dispatch (this pod) → claude --bg --remote-control
```

`alert-dispatch` (`alert-dispatch.sh`) runs alongside the
Remote Control server in the same container. It reads a NACK `Consumer`
(`consumer.yaml`) that only receives `alerts.firing.warning.>` and
`alerts.firing.critical.>` and starts at `new`, so the 30 days of history in
the stream are never replayed. It logs to `~/.alert-dispatch/dispatch.log` on
the PVC.

The script is not in the image. `kustomization.yaml` puts it in the
`claude-agent-alert-dispatch` ConfigMap next to `config.env`. That ConfigMap
is unhashed, so the kubelet updates the pod's mount in place. When the script
sees its own file change, it exits, and `entrypoint.sh` starts the new version
30s later. **A merged dispatcher fix is live within about two minutes, with no
image build and no pod restart.** It used to be baked into the `:latest`
image. Every fix then needed a CI build *and* a manual pod delete, which also
killed every running session, so fixes sat merged but not deployed for days.

### Usage limits

These are the limits that stop a flood of alerts using up your Claude usage.
They are set in `alert-dispatch.env` and re-read every loop, so a merged
change takes effect within about a minute, with no restart.

| Setting | Default | Effect |
|---------|---------|--------|
| `ENABLED` | `true` | Off switch. While `false`, alerts are acked and dropped, so nothing piles up. |
| `BATCH_SECONDS` | `180` | After the first alert, wait this long and put everything that fired into one session. A cluster-wide blip becomes one investigation, not dozens. |
| `DAILY_CAP` | `3` | Maximum sessions per day. Past it, alerts are logged and dropped. Pushover still tells you about them. |
| `DEDUPE_HOURS` | `6` | An alert name that already started a session is skipped for this long. |
| `IDLE_STOP_HOURS` | `12` | An `alert-*` session started longer ago than this and now idle is stopped, to give back its memory. |
| `ALERT_MAX_HOURS` | `3` | An `alert-*` session older than this no longer holds back new batches, even if it still looks busy. A backstop against a stuck session blocking dispatch for good. |
| `IGNORE_ALERTS` | `Watchdog InfoInhibitor` | Never investigated. |
| `PERMISSION_MODE` | `auto` | Passed to `--permission-mode`. `auto` never prompts, but refuses risky actions. `bypassPermissions` removes even that, for sessions that read logs containing outside text (indexer responses, release names) while holding an all-repos GitHub token and internet access. |

A new batch also waits while an `alert-*` session is still busy, or while
`CAPACITY` sessions are busy. Idle sessions don't count: a finished alert
session or a chat left open in the app would otherwise hold the slot for
`IDLE_STOP_HOURS`. The batch is held on the PVC and retried, not dropped.

`--bg` sessions report `.state` in `claude agents --json`, not `.status`, and
only `working`-type states count. On 2026-11-12, `claude`'s background daemon
retired a finished alert session and exited before it wrote `done` to
`~/.claude/jobs/<id>/state.json`. The session was listed as `blocked`
forever, with no process behind it. The old `.status != "idle"` check counted
it as busy, and every alert for two days was deferred.

### Why it runs in the claude container

`claude agents` and `claude stop` only see sessions in their own PID
namespace. A sidecar couldn't check whether an alert session is still running
or stop a stale one.

### Adding NATS access for this

This pod is a NATS client like any other: `certificate.yaml`, a
`CN=claude-agent.claude-agent` user in `kubernetes/nats/nats-values.yaml`
that may pull from and ack its own consumer and read `HEATING`, statestream and
the heating-curve advisor state (`kubernetes/heating-curve/`), `claude-agent`
in the NATS `allow-client-ingress` policy, and `allow-nats-egress` here.

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
- **Filling `/scratch` evicts the pod.** Going past an `emptyDir`'s
  `sizeLimit` doesn't fail the write: kubelet evicts the whole pod, ending every
  session at once. Stale per-worktree build dirs are what fill it, since a removed
  worktree leaves its `/scratch/cargo/<hash>` behind. `du -sh /scratch/*/*` and
  delete freely; nothing there is precious.
- **The PVC is 20Gi, and Longhorn headroom is the limit.** Standard workers had
  only 15–22Gi of Longhorn space free when it was raised from 10Gi, and every
  GiB is stored twice on one physical disk. Check `.status.diskStatus` on the
  Longhorn `Node`s before growing it again.
- **Prompt injection is the real risk here.** The agent reads pod logs, Flux
  status and GitHub issues — all attacker-influenceable text reaching something
  that holds a GitHub token. Read-only RBAC caps the blast radius; the human
  merge gate on every PR is the actual mitigation. Don't remove it.

#!/usr/bin/env bash
set -euo pipefail

# Checked up front because git reports a missing credential as the opaque
# "could not read Username for 'https://github.com'" rather than as the real
# problem — from the clone below while the repo is private, and from the first
# push once it is public.
if [ -z "${GH_TOKEN:-}" ]; then
  echo "!! missing required env: GH_TOKEN" >&2
  echo "!! It comes from the 'GH_TOKEN' field on the 1Password item" >&2
  echo "!! 'vaults/Secrets/items/Claude Agent'. The field name must match exactly." >&2
  exit 1
fi

# Deliberately NOT set from a secret. CLAUDE_CODE_OAUTH_TOKEN (and anything from
# `claude setup-token`) is inference-only by design and Remote Control refuses
# it: "Remote Control requires a full-scope login token". Auth is instead an
# interactive `claude auth login` over `kubectl exec`, which writes
# ~/.claude/.credentials.json onto the PVC. If the variable were set it would
# take precedence over those credentials and silently keep the session
# inference-only, so unset it defensively.
unset CLAUDE_CODE_OAUTH_TOKEN

# Never block on an interactive credential prompt — there is no one to answer it.
export GIT_TERMINAL_PROMPT=0

PROJECTS_DIR="${PROJECTS_DIR:-${HOME}/Projects}"

# Identity. The fallbacks are the repo owner, deliberately: the previous
# default of `claude-agent <claude-agent@users.noreply.github.com>` was not an
# inert placeholder — that address belongs to a real, unrelated GitHub account,
# so unattended commits were credited to a stranger. Claude is recorded with a
# `Co-Authored-By:` trailer on the commit message instead.
GIT_AUTHOR_NAME="${GIT_AUTHOR_NAME:-Ed Jeffreys}"
GIT_AUTHOR_EMAIL="${GIT_AUTHOR_EMAIL:-ed@edjeffreys.com}"

# Committer is a separate field and git reads it from user.name/user.email, not
# from GIT_AUTHOR_*. Export it explicitly so a repo carrying its own local
# config cannot pull the committer back to something else — a freshly cloned
# repo has no local identity and silently inherits the global one.
export GIT_AUTHOR_NAME GIT_AUTHOR_EMAIL
export GIT_COMMITTER_NAME="${GIT_COMMITTER_NAME:-${GIT_AUTHOR_NAME}}"
export GIT_COMMITTER_EMAIL="${GIT_COMMITTER_EMAIL:-${GIT_AUTHOR_EMAIL}}"

git config --global user.name "${GIT_AUTHOR_NAME}"
git config --global user.email "${GIT_AUTHOR_EMAIL}"
git config --global init.defaultBranch master

# The PVC is owned by uid 1000 via fsGroup, but git still refuses a repo whose
# owner it cannot match after a uid change. "*" since bookworm's git has no globs.
git config --global --replace-all safe.directory '*'

# Clone and push over HTTPS using the same token gh uses. Never written to disk.
git config --global credential.helper \
  '!f() { echo username=x-access-token; echo "password=${GH_TOKEN}"; }; f'

mkdir -p "${PROJECTS_DIR}"
cd "${PROJECTS_DIR}"

# Skip first-run onboarding. Without this the process sits forever on the theme
# picker — it never reaches Remote Control registration, and the only symptom is
# a session that never appears in the app. Merge rather than overwrite so a
# restart keeps whatever state ~/.claude.json has accumulated on the PVC.
node -e '
  const fs = require("fs"), p = process.argv[1];
  let d = {};
  try { d = JSON.parse(fs.readFileSync(p, "utf8")); } catch (e) { /* first boot */ }
  d.hasCompletedOnboarding = true;
  d.theme = d.theme || "dark";
  // ...and the per-directory "do you trust this folder?" prompt, which is a
  // second, separate gate that also blocks startup forever.
  d.projects = d.projects || {};
  d.projects[process.argv[2]] = Object.assign(
    { allowedTools: [], mcpServers: {}, projectOnboardingSeenCount: 1 },
    d.projects[process.argv[2]] || {},
    { hasTrustDialogAccepted: true }
  );
  fs.writeFileSync(p, JSON.stringify(d, null, 2));
' "${HOME}/.claude.json" "${PROJECTS_DIR}"

# Wait rather than crashloop: re-login is manual, and the readiness probe alerts.
until claude auth status >/dev/null 2>&1; do
  echo "!! Not logged in — Remote Control cannot start. Log in with:"
  echo "!!   kubectl -n claude-agent exec -it deploy/claude-agent -- claude auth login"
  sleep 60
done

# In this container, not a sidecar: `claude agents` and `claude stop` only see
# sessions in the same PID namespace, and the dispatcher needs both.
if [ -f /etc/alert-dispatch/config.env ]; then
  ( while true; do bash /etc/alert-dispatch/alert-dispatch.sh; sleep 30; done ) &
fi

echo "==> starting Remote Control server on $(hostname) in ${PROJECTS_DIR} (capacity ${CAPACITY:-3})"
exec claude remote-control \
  --spawn same-dir \
  --no-create-session-in-dir \
  --capacity "${CAPACITY:-3}" \
  --permission-mode default

#!/usr/bin/env bash
set -euo pipefail

# The repo is private, so a missing GH_TOKEN otherwise surfaces as git's opaque
# "could not read Username for 'https://github.com'" from the clone below
# rather than as the real problem.
if [ -z "${GH_TOKEN:-}" ]; then
  echo "!! missing required env: GH_TOKEN" >&2
  echo "!! It comes from the 'GH_TOKEN' field on the 1Password item" >&2
  echo "!! 'vaults/Secrets/items/Claude Agent'. The field name must match exactly." >&2
  exit 1
fi

# Deliberately NOT set from a secret. CLAUDE_CODE_OAUTH_TOKEN (and anything from
# `claude setup-token`) is inference-only by design and Remote Control refuses
# it: "Remote Control requires a full-scope login token". Auth is instead an
# interactive `/login` done once over `kubectl attach`, which writes
# ~/.claude/.credentials.json onto the PVC. If the variable were set it would
# take precedence over those credentials and silently keep the session
# inference-only, so unset it defensively.
unset CLAUDE_CODE_OAUTH_TOKEN

# Never block on an interactive credential prompt — there is no one to answer it.
export GIT_TERMINAL_PROMPT=0

REPO_URL="${REPO_URL:-https://github.com/edjeffreys/infrastructure}"
REPO_DIR="${HOME}/workspace/$(basename "${REPO_URL}" .git)"
SESSION_NAME="${REMOTE_CONTROL_NAME:-homelab}"

git config --global user.name "${GIT_AUTHOR_NAME:-claude-agent}"
git config --global user.email "${GIT_AUTHOR_EMAIL:-claude-agent@users.noreply.github.com}"
git config --global init.defaultBranch master

# The PVC is owned by uid 1000 via fsGroup, but git still refuses a repo whose
# owner it cannot match after a uid change. Scope the exemption to our own tree.
git config --global --add safe.directory "${REPO_DIR}"

# Clone and push over HTTPS using the same token gh uses. Never written to disk.
git config --global credential.helper \
  '!f() { echo username=x-access-token; echo "password=${GH_TOKEN}"; }; f'

if [ ! -d "${REPO_DIR}/.git" ]; then
  echo "==> cloning ${REPO_URL} into ${REPO_DIR}"
  mkdir -p "$(dirname "${REPO_DIR}")"
  git clone "${REPO_URL}" "${REPO_DIR}"
else
  echo "==> refreshing existing checkout at ${REPO_DIR}"
  git -C "${REPO_DIR}" fetch --prune origin || echo "!! fetch failed, continuing with local state"
fi

cd "${REPO_DIR}"

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
' "${HOME}/.claude.json" "${REPO_DIR}"

if [ ! -s "${HOME}/.claude/.credentials.json" ]; then
  echo "!! Not logged in — Remote Control will not register."
  echo "!! Attach and run /login, complete the OAuth paste-code flow, then"
  echo "!! run /remote-control. Detach with Ctrl-P Ctrl-Q (Ctrl-C kills it):"
  echo "!!   kubectl -n claude-agent attach -it deploy/claude-agent"
  echo "!! Credentials persist on the PVC, so this is once per volume, not once"
  echo "!! per restart."
fi

echo "==> starting Claude Code with Remote Control as '${SESSION_NAME}'"
exec claude --remote-control "${SESSION_NAME}" \
  --settings /etc/claude/settings.json \
  --mcp-config /etc/claude/mcp.json \
  --strict-mcp-config

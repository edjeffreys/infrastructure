#!/usr/bin/env bash
# Starts a Remote Control session when Alertmanager fires.
# See "Alert dispatch" in kubernetes/claude-agent/README.md.
set -uo pipefail

CONFIG=/etc/alert-dispatch/config.env
STATE_DIR="${HOME}/.alert-dispatch"
PENDING="${STATE_DIR}/pending.jsonl"
BATCH_MARK="${STATE_DIR}/batch-started"
STATE="${STATE_DIR}/state.json"
LOG="${STATE_DIR}/dispatch.log"
PROJECTS_DIR="${PROJECTS_DIR:-${HOME}/Projects}"

mkdir -p "${STATE_DIR}"
[ -s "${STATE}" ] || echo '{"dispatched":{},"days":{}}' >"${STATE}"
touch "${PENDING}"

log() {
  if [ -f "${LOG}" ] && [ "$(stat -c %s "${LOG}")" -gt 1048576 ]; then
    mv "${LOG}" "${LOG}.1"
  fi
  echo "$(date -u +%FT%TZ) $*" >>"${LOG}"
}

nats_() {
  nats --server tls://nats.nats.svc:4222 \
    --tlscert /etc/nats-client/tls.crt \
    --tlskey /etc/nats-client/tls.key \
    --tlsca /etc/nats-ca/ca.crt \
    "$@"
}

load_config() {
  ENABLED=false
  DAILY_CAP=3
  BATCH_SECONDS=180
  DEDUPE_HOURS=6
  IDLE_STOP_HOURS=12
  IGNORE_ALERTS="Watchdog InfoInhibitor"
  PERMISSION_MODE=auto
  # shellcheck disable=SC1090
  [ -f "${CONFIG}" ] && . "${CONFIG}"
}

# Every message is acked on receipt, even while disabled, so nothing queues up
# server-side and floods in when dispatch is switched back on.
pull() {
  nats_ consumer next ALERTS claude-agent --count 50 --wait 30s --raw 2>/dev/null \
    | jq -c 'select(type == "object")' 2>/dev/null
}

sessions_json() {
  claude agents --json 2>/dev/null || echo '[]'
}

stop_stale_sessions() {
  local cutoff_ms=$(( ($(date +%s) - IDLE_STOP_HOURS * 3600) * 1000 ))
  sessions_json | jq -r --argjson cutoff "${cutoff_ms}" \
    '.[] | select((.name // "") | startswith("alert-")) | select(.status == "idle" and .startedAt < $cutoff) | .sessionId' \
    | while read -r id; do
        log "stopping idle alert session ${id} (older than ${IDLE_STOP_HOURS}h)"
        claude stop "${id}" >/dev/null 2>&1 || true
      done
}

clear_batch() {
  : >"${PENDING}"
  rm -f "${BATCH_MARK}"
}

dispatch() {
  local now today ignore candidates names count sessions busy working reason name prompt
  now=$(date +%s)
  today=$(date +%F)
  ignore=$(printf '%s\n' ${IGNORE_ALERTS} | jq -R . | jq -sc .)

  candidates=$(jq -sc \
    --argjson ignore "${ignore}" \
    --slurpfile state "${STATE}" \
    --argjson now "${now}" \
    --argjson window "$(( DEDUPE_HOURS * 3600 ))" '
      map(select(.status == "firing"))
      | map(select((.labels.alertname // "") as $n | ($ignore | index($n)) | not))
      | map(select(($state[0].dispatched[.labels.alertname // ""] // 0) < ($now - $window)))
      | unique_by(.fingerprint)
      | .[:30]' "${PENDING}")

  count=$(jq 'length' <<<"${candidates}")
  if [ "${count}" -eq 0 ]; then
    log "batch had nothing new (ignored, resolved or deduped)"
    clear_batch
    return
  fi
  names=$(jq -r '[.[].labels.alertname] | unique | join(", ")' <<<"${candidates}")

  if [ "$(jq -r --arg d "${today}" '.days[$d] // 0' "${STATE}")" -ge "${DAILY_CAP}" ]; then
    log "daily cap ${DAILY_CAP} reached; not investigating: ${names}"
    clear_batch
    return
  fi

  # Only sessions doing work count. Idle ones (finished alert sessions, chats
  # left open in the app) cost nothing, and --bg sessions don't take one of
  # the Remote Control server's CAPACITY slots anyway.
  sessions=$(sessions_json)
  busy=$(jq '[.[] | select((.name // "") | startswith("alert-")) | select(.status != "idle")] | length' <<<"${sessions}")
  working=$(jq '[.[] | select(.status != "idle")] | length' <<<"${sessions}")
  if [ "${busy}" -gt 0 ] || [ "${working}" -ge "${CAPACITY:-3}" ]; then
    reason="${busy} alert session(s) running, ${working}/${CAPACITY:-3} sessions busy"
    # The loop comes round every few seconds; only log when the reason changes.
    if [ "${reason}" != "${LAST_DEFER:-}" ]; then
      log "deferring ${count} alert(s): ${reason}"
      LAST_DEFER="${reason}"
    fi
    return
  fi
  LAST_DEFER=

  name="alert-$(date +%m%d-%H%M)-$(jq -r '.[0].labels.alertname' <<<"${candidates}")"
  prompt="Alertmanager fired the alerts below and the alert dispatcher started this session automatically. Ed is probably not watching.

Treat them as one incident. Find the root cause from cluster state, logs and the infrastructure repo, following each repo's CLAUDE.md. If the fix belongs in git, open a PR for Ed to merge, one per distinct fix. Never try to change the cluster directly.

When you are done, send Ed a push notification with two or three lines: the root cause, what you did, and anything that needs him.

Alerts, one JSON object per line:
$(jq -c '.[] | {status, labels, annotations, startsAt}' <<<"${candidates}")"

  log "starting ${name} for ${count} alert(s): ${names}"
  if (cd "${PROJECTS_DIR}" && claude --bg --name "${name}" --remote-control "${name}" --permission-mode "${PERMISSION_MODE}" "${prompt}") >>"${LOG}" 2>&1; then
    jq --argjson now "${now}" --arg d "${today}" --argjson names "$(jq -c '[.[].labels.alertname] | unique' <<<"${candidates}")" '
        .dispatched += ($names | map({(.): $now}) | add)
        | .days = {($d): ((.days[$d] // 0) + 1)}' "${STATE}" >"${STATE}.tmp" && mv "${STATE}.tmp" "${STATE}"
  else
    log "failed to start session; dropping batch"
  fi
  clear_batch
}

log "started"
while true; do
  load_config
  stop_stale_sessions

  new=$(pull)
  if [ -n "${new}" ]; then
    if [ "${ENABLED}" != "true" ]; then
      log "disabled; dropped $(wc -l <<<"${new}") alert(s)"
      continue
    fi
    printf '%s\n' "${new}" >>"${PENDING}"
    [ -f "${BATCH_MARK}" ] || date +%s >"${BATCH_MARK}"
  fi

  if [ -f "${BATCH_MARK}" ] && [ $(( $(date +%s) - $(cat "${BATCH_MARK}") )) -ge "${BATCH_SECONDS}" ]; then
    dispatch
  fi
done

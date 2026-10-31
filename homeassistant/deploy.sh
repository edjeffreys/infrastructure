#!/usr/bin/env bash
set -euo pipefail

HOST=${HA_HOST:-homeassistant}
DIRS=(packages dashboards custom_sentences)
RELOAD=(input_number input_boolean template statistics automation intent_script conversation)
restart=false
[[ ${1:-} == --restart ]] && restart=true

cd "$(dirname "$0")"

files=()
while IFS= read -r f; do files+=("$f"); done < <(find "${DIRS[@]}" -type f -name '*.yaml' | sort)
# HA loads every file in packages/, so macOS's ._* AppleDouble files must not reach it.
COPYFILE_DISABLE=1 tar -cf - "${files[@]}" | ssh "$HOST" 'rm -rf /tmp/ha-deploy && mkdir -p /tmp/ha-deploy && tar -C /tmp/ha-deploy -xf -'

local_sums=$(shasum -a 256 "${files[@]}")
remote_sums=$(ssh "$HOST" 'cd /tmp/ha-deploy && find packages dashboards custom_sentences -type f | sort | xargs sha256sum')
if [[ $local_sums != "$remote_sums" ]]; then
  echo "checksum mismatch after upload; nothing deployed" >&2
  exit 1
fi

ssh "$HOST" sudo bash -s -- "$restart" "$(IFS=,; echo "${RELOAD[*]}")" <<'EOF'
set -euo pipefail
restart=$1; reload=$2
export SUPERVISOR_TOKEN=$(cat /run/s6/container_environment/SUPERVISOR_TOKEN)
backup=/config/.deploy-backup/$(date +%Y%m%d-%H%M%S)
mkdir -p "$backup"

for d in packages dashboards custom_sentences; do
  [[ -d /config/$d ]] && cp -a "/config/$d" "$backup/"
  rm -rf "/config/$d"
  cp -r "/tmp/ha-deploy/$d" "/config/$d"
done

if ! ha core check; then
  echo "config check failed; restoring $backup" >&2
  for d in packages dashboards custom_sentences; do
    rm -rf "/config/$d"
    [[ -d $backup/$d ]] && cp -a "$backup/$d" "/config/$d"
  done
  exit 1
fi

ls -1dt /config/.deploy-backup/* | tail -n +6 | xargs -r rm -rf

if [[ $restart == true ]]; then
  ha core restart
else
  for domain in ${reload//,/ }; do
    curl -fsS -o /dev/null -X POST -H "Authorization: Bearer $SUPERVISOR_TOKEN" \
      "http://supervisor/core/api/services/$domain/reload"
    echo "reloaded $domain"
  done
fi
EOF

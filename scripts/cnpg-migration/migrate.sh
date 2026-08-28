#!/usr/bin/env bash
#
# Migrate one arr app's SQLite database(s) into the arr-postgres CNPG cluster.
#
#   ./migrate.sh radarr
#   ./migrate.sh sonarr
#   ./migrate.sh seerr
#
# This lives outside kubernetes/ on purpose: argocd/apps/arr.yaml syncs
# kubernetes/arr recursively with prune and selfHeal, so a one-shot Job stored
# there would be fought over by ArgoCD.
#
# Read README.md before running. In short, the app's Deployment must already be
# at replicas: 0 *in git* — scaling with kubectl alone is reverted by selfHeal
# partway through the migration.
set -euo pipefail

NS=arr
CLUSTER=arr-postgres
SECRET=${CLUSTER}-app
# Not hardcoded as <cluster>-1: after a restart or a failover the primary can be
# any instance ordinal.
PG_POD=$(kubectl -n $NS get pod \
  -l "cnpg.io/cluster=${CLUSTER},cnpg.io/instanceRole=primary" \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
PGLOADER_IMAGE=dimitri/pgloader:v3.6.7

APP=${1:-}

case "$APP" in
  sonarr)
    IMAGE=$(kubectl -n $NS get deploy sonarr -o jsonpath='{.spec.template.spec.containers[0].image}')
    PVC=sonarr-config
    CONFIG_MOUNT=/config
    # "<sqlite file relative to the config mount>:<target database>"
    PAIRS=("sonarr.db:sonarr-main" "logs.db:sonarr-log")
    SCHEMA_ENV=(
      "SONARR__POSTGRES__HOST=${CLUSTER}-rw"
      "SONARR__POSTGRES__PORT=5432"
      "SONARR__POSTGRES__MAINDB=sonarr-main"
      "SONARR__POSTGRES__LOGDB=sonarr-log"
    )
    SCHEMA_USER_ENV=(SONARR__POSTGRES__USER SONARR__POSTGRES__PASSWORD)
    # hotio images start as root and drop to PUID themselves, so the bare
    # emptyDir in the schema-init Job needs no ownership help.
    SCHEMA_FSGROUP=""
    # VersionInfo is dialect-neutral for the arr apps, so it comes across.
    EXCLUDE_TABLES=()
    ;;
  radarr)
    IMAGE=$(kubectl -n $NS get deploy radarr -o jsonpath='{.spec.template.spec.containers[0].image}')
    PVC=radarr-config
    CONFIG_MOUNT=/config
    PAIRS=("radarr.db:radarr-main" "logs.db:radarr-log")
    SCHEMA_ENV=(
      "RADARR__POSTGRES__HOST=${CLUSTER}-rw"
      "RADARR__POSTGRES__PORT=5432"
      "RADARR__POSTGRES__MAINDB=radarr-main"
      "RADARR__POSTGRES__LOGDB=radarr-log"
    )
    SCHEMA_USER_ENV=(RADARR__POSTGRES__USER RADARR__POSTGRES__PASSWORD)
    SCHEMA_FSGROUP=""
    EXCLUDE_TABLES=()
    ;;
  seerr)
    IMAGE=$(kubectl -n $NS get deploy seerr -o jsonpath='{.spec.template.spec.containers[0].image}')
    PVC=seerr-config
    CONFIG_MOUNT=/app/config
    PAIRS=("db/db.sqlite3:seerr")
    # Seerr maintains SEPARATE migration sets for SQLite and Postgres — see
    # CONTRIBUTING.md upstream, every schema change ships two migration files.
    # Copying SQLite's "migrations" table (53 rows, names from 2020) into
    # Postgres leaves TypeORM believing none of its 19 Postgres migrations have
    # run, so on startup it replays InitialMigration1734786061496 over a schema
    # that already exists and crashloops on
    #   relation "PK_04dc42a96bf0914cda31b579702" already exists
    # The rows written by the schema-init step are the correct ones; keep them.
    #
    # The arr apps do NOT need this: their VersionInfo table is dialect-neutral,
    # so carrying it across is right for them.
    EXCLUDE_TABLES=(migrations)
    SCHEMA_ENV=(
      "DB_TYPE=postgres"
      "DB_HOST=${CLUSTER}-rw"
      "DB_PORT=5432"
      "DB_NAME=seerr"
    )
    SCHEMA_USER_ENV=(DB_USER DB_PASS)
    # Seerr runs as uid 1000 and never elevates. The real Deployment fixes the
    # config volume with a chown initContainer; the schema-init Job gets a bare
    # emptyDir, which is root-owned, so without fsGroup Seerr cannot write
    # settings.json and never reaches its migrations.
    SCHEMA_FSGROUP=1000
    ;;
  *)
    echo "usage: $0 {sonarr|radarr|seerr}" >&2
    exit 64
    ;;
esac

log() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }

psql_db() { # psql_db <database> [psql args...]
  local db=$1; shift
  kubectl -n $NS exec -i "$PG_POD" -c postgres -- psql -v ON_ERROR_STOP=1 -d "$db" "$@"
}

# ---------------------------------------------------------------------------

log "Preflight"

replicas=$(kubectl -n $NS get deploy "$APP" -o jsonpath='{.spec.replicas}')
if [[ "$replicas" != "0" ]]; then
  cat >&2 <<EOF
ERROR: deployment/$APP is at replicas=$replicas.

Set replicas: 0 in kubernetes/arr/$APP/deployment.yaml, commit and push, and
wait for ArgoCD to reconcile. Scaling with kubectl alone does not hold: selfHeal
scales it back up mid-migration, and the app then writes to the database while
pgloader is loading into it.
EOF
  exit 1
fi

if kubectl -n $NS get pod -l "app=$APP" -o name | grep -q .; then
  echo "ERROR: $APP pods are still present; the config PVC is still attached." >&2
  echo "Wait for them to terminate and retry." >&2
  exit 1
fi

if [[ -z "$PG_POD" ]]; then
  echo "ERROR: no primary instance found for cluster $CLUSTER in namespace $NS." >&2
  echo "  kubectl -n $NS get cluster $CLUSTER" >&2
  exit 1
fi
echo "postgres primary: $PG_POD"

# The pgloader Job has to land on the node that currently holds the RWO config
# volume, otherwise it sits Pending forever waiting for an attachment that
# cannot happen.
NODE=$(kubectl -n $NS get volumeattachment -o json \
  | jq -r --arg pvc "$(kubectl -n $NS get pvc "$PVC" -o jsonpath='{.spec.volumeName}')" \
      '.items[] | select(.spec.source.persistentVolumeName == $pvc) | .spec.nodeName' | head -1)
if [[ -z "$NODE" ]]; then
  # Not attached anywhere right now; Longhorn will attach wherever we schedule.
  NODE=$(kubectl get nodes -l node-type=pia -o jsonpath='{.items[0].metadata.name}')
fi
echo "config PVC $PVC -> node $NODE"

# ---------------------------------------------------------------------------

log "Creating the target schema by booting $APP against an empty Postgres"

# A throwaway pod rather than the real Deployment: it gets a scratch emptyDir
# for its config dir, so it never touches the real config PVC and ArgoCD never
# sees it. It boots, runs its own migrations against Postgres, and is deleted.
kubectl -n $NS delete job "${APP}-schema-init" --ignore-not-found --wait=true

env_json=$(printf '%s\n' "${SCHEMA_ENV[@]}" \
  | jq -R 'split("=") | {name: .[0], value: (.[1:] | join("="))}' | jq -s .)
env_json=$(jq -n --argjson e "$env_json" --arg u "${SCHEMA_USER_ENV[0]}" \
  --arg p "${SCHEMA_USER_ENV[1]}" --arg s "$SECRET" '
  $e + [
    {name: $u, valueFrom: {secretKeyRef: {name: $s, key: "username"}}},
    {name: $p, valueFrom: {secretKeyRef: {name: $s, key: "password"}}}
  ]')

kubectl -n $NS apply -f - <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: ${APP}-schema-init
spec:
  backoffLimit: 0
  template:
    metadata:
      labels:
        app: cnpg-migration
    spec:
      restartPolicy: Never
$( [[ -n "$SCHEMA_FSGROUP" ]] && printf '      securityContext:\n        fsGroup: %s\n        runAsUser: %s' "$SCHEMA_FSGROUP" "$SCHEMA_FSGROUP" )
      nodeSelector:
        node-type: pia
      tolerations:
        - key: node-type
          operator: Equal
          value: pia
          effect: NoExecute
      containers:
        - name: app
          image: ${IMAGE}
          env: $(echo "$env_json" | jq -c .)
          volumeMounts:
            - name: scratch
              mountPath: ${CONFIG_MOUNT}
      volumes:
        - name: scratch
          emptyDir: {}
EOF

# Poll Postgres rather than grepping the app's logs for a readiness banner: the
# table count is the thing we actually care about, and it does not change
# meaning when upstream reword a log line. The count has to be *stable* across
# two polls — the apps create tables progressively as they run their migrations.
echo "waiting for $APP to create its schema (this takes a minute or two)..."
prev=""
for _ in $(seq 1 90); do
  sleep 5
  counts=""
  for pair in "${PAIRS[@]}"; do
    db=${pair#*:}
    n=$(psql_db "$db" -tAc \
      "SELECT count(*) FROM information_schema.tables WHERE table_schema='public'" 2>/dev/null | tr -d '[:space:]')
    counts="${counts}${db}=${n:-0} "
  done
  echo "  $counts"
  if [[ "$counts" == "$prev" && "$counts" != *"=0 "* ]]; then
    break
  fi
  prev="$counts"
done

for pair in "${PAIRS[@]}"; do
  db=${pair#*:}
  n=$(psql_db "$db" -tAc \
    "SELECT count(*) FROM information_schema.tables WHERE table_schema='public'" | tr -d '[:space:]')
  if [[ "${n:-0}" == "0" ]]; then
    echo "ERROR: $APP created no tables in $db. Check:" >&2
    echo "  kubectl -n $NS logs job/${APP}-schema-init" >&2
    exit 1
  fi
done

kubectl -n $NS delete job "${APP}-schema-init" --wait=true

# ---------------------------------------------------------------------------

log "Emptying the freshly created schema"

# The app seeds default rows on first boot (quality profiles, config, the
# migration-version table). pgloader --with "data only" would then collide with
# the same rows coming out of SQLite: this is the "duplicate key" failure people
# hit when following the upstream guide, which was written for PostgreSQL 14.
# Tables in EXCLUDE_TABLES must end up holding the rows the schema-init step
# wrote, not the ones in SQLite. They cannot simply be left out of the truncate:
# pgloader would then append SQLite's rows onto them and collide on the primary
# key. And pgloader's CLI has no table filter — INCLUDING ONLY / EXCLUDING are
# command-file syntax only, there is no --exclude flag. So snapshot them here,
# let the load overwrite them, and restore from the snapshot afterwards.
for pair in "${PAIRS[@]}"; do
  db=${pair#*:}
  for t in ${EXCLUDE_TABLES[@]+"${EXCLUDE_TABLES[@]}"}; do
    psql_db "$db" -c "DROP TABLE IF EXISTS \"${t}__premigration\";
                      CREATE TABLE \"${t}__premigration\" AS SELECT * FROM \"${t}\";" >/dev/null
    echo "  $db: snapshotted $t"
  done

  psql_db "$db" <<'SQL'
DO $$
DECLARE stmt TEXT;
BEGIN
  SELECT 'TRUNCATE TABLE ' || string_agg(format('%I.%I', schemaname, tablename), ', ') || ' CASCADE'
    INTO stmt
    FROM pg_tables
   WHERE schemaname = 'public'
     AND tablename NOT LIKE '%\_\_premigration';
  IF stmt IS NOT NULL THEN EXECUTE stmt; END IF;
END $$;
SQL
  echo "  $db truncated"
done

# ---------------------------------------------------------------------------

log "Loading the SQLite data with pgloader"

for pair in "${PAIRS[@]}"; do
  file=${pair%%:*}
  db=${pair#*:}
  job="${APP}-pgloader-$(echo "$db" | tr -d '-')"

  kubectl -n $NS delete job "$job" --ignore-not-found --wait=true
  kubectl -n $NS apply -f - <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: ${job}
spec:
  backoffLimit: 0
  template:
    metadata:
      labels:
        app: cnpg-migration
    spec:
      restartPolicy: Never
      nodeName: ${NODE}
      tolerations:
        - key: node-type
          operator: Equal
          value: pia
          effect: NoExecute
      containers:
        - name: pgloader
          image: ${PGLOADER_IMAGE}
          command:
            - sh
            - -c
            # quote identifiers: the arr schemas use "PascalCase" table and
            # column names, which pgloader would otherwise downcase.
            # data only: the schema already exists, created by the app itself.
            # prefetch/batch: keeps pgloader's memory flat on the larger tables.
            - |
              set -e
              pgloader --with "quote identifiers" \
                       --with "data only" \
                       --with "prefetch rows = 100" \
                       --with "batch size = 1MB" \
                       ${CONFIG_MOUNT}/${file} \
                       "postgresql://\${PGUSER}:\${PGPASSWORD}@${CLUSTER}-rw.${NS}.svc.cluster.local/${db}"
          env:
            - name: PGUSER
              valueFrom:
                secretKeyRef:
                  name: ${SECRET}
                  key: username
            - name: PGPASSWORD
              valueFrom:
                secretKeyRef:
                  name: ${SECRET}
                  key: password
          volumeMounts:
            # Mounted read-write: SQLite replays any -wal file on open, which is
            # what we want. The app is stopped, so nothing else is writing.
            - name: config
              mountPath: ${CONFIG_MOUNT}
      volumes:
        - name: config
          persistentVolumeClaim:
            claimName: ${PVC}
EOF

  # `kubectl wait --for=condition=complete` blocks for the full timeout when the
  # Job fails instead, so poll both conditions.
  status=""
  for _ in $(seq 1 240); do
    if kubectl -n $NS get job "$job" -o jsonpath='{.status.conditions[?(@.type=="Complete")].status}' | grep -q True; then
      status=complete; break
    fi
    if kubectl -n $NS get job "$job" -o jsonpath='{.status.conditions[?(@.type=="Failed")].status}' | grep -q True; then
      status=failed; break
    fi
    sleep 5
  done

  joblog=$(kubectl -n $NS logs "job/${job}" 2>&1 || true)
  echo "$joblog" | tail -40

  if [[ "$status" != "complete" ]]; then
    echo "ERROR: pgloader job $job did not complete (status: ${status:-timeout})." >&2
    echo "The Job has been left in place for inspection: kubectl -n $NS describe job/$job" >&2
    exit 1
  fi

  # A completed Job is not the same as a successful load. pgloader exits 0 even
  # when it aborts on a source-level failure — sonarr's logs.db was corrupt
  # (left over from an unrelated April incident) and pgloader logged
  # "Code CORRUPT: database disk image is malformed", loaded nothing, and still
  # exited 0. Without this check the migration reports success on an empty
  # database. Verify the source, not just the exit code.
  if echo "$joblog" | grep -qiE "Code CORRUPT|database disk image is malformed|^.*ERROR (sqlite|pgsql)"; then
    echo "ERROR: pgloader reported a source/target error loading ${file} into ${db}," >&2
    echo "even though the Job exited cleanly. See the log above. Nothing was loaded." >&2
    exit 1
  fi

  loaded=$(kubectl -n $NS exec -i "$PG_POD" -c postgres -- psql -tA -d "$db" \
    -c "SELECT COALESCE(SUM(n_live_tup), 0) FROM pg_stat_user_tables" | tr -d '[:space:]')
  if [[ "${loaded:-0}" == "0" ]]; then
    echo "ERROR: ${db} is empty after pgloader claimed success. Refusing to continue." >&2
    exit 1
  fi
  echo "  ${db}: ~${loaded} rows loaded"

  kubectl -n $NS delete job "${job}" --wait=true
done

# ---------------------------------------------------------------------------

log "Restoring excluded tables"

# Put back the schema-init rows that pgloader just overwrote with SQLite's, then
# drop the snapshot. Runs before the sequence reset so the sequence is derived
# from the restored rows, not the discarded ones.
for pair in "${PAIRS[@]}"; do
  db=${pair#*:}
  for t in ${EXCLUDE_TABLES[@]+"${EXCLUDE_TABLES[@]}"}; do
    psql_db "$db" -c "
      TRUNCATE TABLE \"${t}\" CASCADE;
      INSERT INTO \"${t}\" SELECT * FROM \"${t}__premigration\";
      DROP TABLE \"${t}__premigration\";" >/dev/null
    n=$(psql_db "$db" -tAc "SELECT count(*) FROM \"${t}\"" | tr -d '[:space:]')
    echo "  $db: restored $t ($n rows)"
  done
done

log "Resetting sequences"

# Belt and braces. pgloader 3.6.7 does run its own "Reset Sequences" step and it
# was correct on the radarr migration, contrary to most writeups of this — but a
# sequence left behind would not surface until the app's first INSERT, which is
# why the verification step says to add and remove an item by hand.
for pair in "${PAIRS[@]}"; do
  db=${pair#*:}
  psql_db "$db" <<'SQL'
DO $$
DECLARE r RECORD;
BEGIN
  FOR r IN
    -- oid::regclass::text, not relname: the arr schemas use PascalCase, and a
    -- bare 'Config_Id_seq' passed to setval() is cast to regclass, which
    -- downcases it to config_id_seq and fails. regclass renders the name
    -- already quoted where quoting is needed.
    SELECT s.oid::regclass::text AS seq, t.relname AS tab, a.attname AS col
      FROM pg_class s
      JOIN pg_depend d ON d.objid = s.oid
       AND d.classid = 'pg_class'::regclass
       AND d.refclassid = 'pg_class'::regclass
      JOIN pg_class t ON t.oid = d.refobjid
      JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = d.refobjsubid
     WHERE s.relkind = 'S'
  LOOP
    EXECUTE format(
      'SELECT setval(%L, COALESCE((SELECT MAX(%I) FROM %I), 0) + 1, false)',
      r.seq, r.col, r.tab);
  END LOOP;
END $$;
SQL
  echo "  $db sequences reset"
done

# ---------------------------------------------------------------------------

log "Row counts"

for pair in "${PAIRS[@]}"; do
  db=${pair#*:}
  echo "--- $db ---"
  psql_db "$db" -c "
    SELECT relname AS table, n_live_tup AS rows
      FROM pg_stat_user_tables
     WHERE n_live_tup > 0
     ORDER BY n_live_tup DESC
     LIMIT 15"
done

cat <<EOF

Done. Compare the counts above against the SQLite source if you want a second
opinion:

  kubectl -n $NS run sqlite-check --rm -it --restart=Never \\
    --image=keinos/sqlite3 --overrides='{"spec":{"nodeName":"$NODE","tolerations":[{"key":"node-type","operator":"Equal","value":"pia","effect":"NoExecute"}],"containers":[{"name":"c","image":"keinos/sqlite3","stdin":true,"tty":true,"command":["sh"],"volumeMounts":[{"name":"c","mountPath":"$CONFIG_MOUNT"}]}],"volumes":[{"name":"c","persistentVolumeClaim":{"claimName":"$PVC"}}]}}'

Then set replicas: 1 in kubernetes/arr/$APP/deployment.yaml, commit and push.

The original SQLite files are untouched on $PVC, so rolling back is reverting
the *__POSTGRES__* / DB_* env vars in git.
EOF

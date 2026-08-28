# arr stack: SQLite → CloudNativePG

One-shot migration tooling for moving Sonarr, Radarr and Seerr off their SQLite
files and onto the `arr-postgres` CNPG cluster (`kubernetes/arr/postgres/`).

This deliberately lives **outside `kubernetes/`**. `argocd/apps/arr.yaml`
syncs `kubernetes/arr` recursively with `prune: true` and `selfHeal: true`; a
completed one-shot Job stored there would be pruned, resurrected, or both.

## Why not just point pgloader at an empty database

That is what the upstream Servarr guide says, and it fails on PostgreSQL 15 and
newer with `duplicate key value violates unique constraint`. Two things have to
happen first:

1. **The app must create its own schema.** pgloader's own SQLite→Postgres schema
   conversion does not produce the types and constraints the arr apps expect.
2. **That schema must then be emptied.** On first boot the apps seed default
   rows — quality profiles, config, the migration-version table — and
   `--with "data only"` collides with the same rows arriving from SQLite.

`migrate.sh` does both, then loads, then resets sequences.

On the sequence reset: several writeups claim pgloader leaves every sequence at 1.
That is **not** what 3.6.7 does — its run summary includes a "Reset Sequences" step, and
on the radarr migration it correctly left `Movies_Id_seq` at `last_value=63, is_called=t`
against a max `Id` of 63. The reset in `migrate.sh` is therefore a belt-and-braces check,
not a fix for a known breakage. It is idempotent, so it costs nothing to keep — and it is
cheap insurance against a table pgloader skips.

The schema is created by a **throwaway Job** running the app's own image against
a scratch `emptyDir`, not by the real Deployment. That keeps the real config PVC
detached throughout and means ArgoCD never sees anything it wants to revert.

## Procedure, per app

Run in this order: **radarr, sonarr, seerr** — Seerr talks to the other two, so
it goes last.

### 0. The env vars each app needs

Added to the container's `env:` in `kubernetes/arr/<app>/deployment.yaml`, in the same
commit as step 1. The `-config` PVC stays either way — it still holds `config.xml`, logs,
MediaCover and Backups (Seerr: `settings.json`).

Sonarr (`SONARR__`) and Radarr (`RADARR__`) take the same shape:

```yaml
- name: SONARR__POSTGRES__HOST
  value: arr-postgres-rw
- name: SONARR__POSTGRES__PORT
  value: "5432"
- name: SONARR__POSTGRES__MAINDB
  value: sonarr-main
- name: SONARR__POSTGRES__LOGDB
  value: sonarr-log
- name: SONARR__POSTGRES__USER
  valueFrom:
    secretKeyRef: { name: arr-postgres-app, key: username }
- name: SONARR__POSTGRES__PASSWORD
  valueFrom:
    secretKeyRef: { name: arr-postgres-app, key: password }
```

Seerr:

```yaml
- name: DB_TYPE
  value: postgres
- name: DB_HOST
  value: arr-postgres-rw
- name: DB_PORT
  value: "5432"
- name: DB_NAME
  value: seerr
- name: DB_USER
  valueFrom:
    secretKeyRef: { name: arr-postgres-app, key: username }
- name: DB_PASS
  valueFrom:
    secretKeyRef: { name: arr-postgres-app, key: password }
```

### 1. Stop the app, in git

Set `replicas: 0` in `kubernetes/arr/<app>/deployment.yaml`, commit, push, wait
for ArgoCD to reconcile.

This has to go through git. `kubectl scale` is reverted by `selfHeal` within the
sync interval, and an app that comes back up mid-load will write to the database
underneath pgloader.

The Postgres env vars can be added in this same commit — they do nothing while
the Deployment is scaled to zero.

Confirm no pods remain (the RWO config volume has to detach):

```bash
kubectl -n arr get pod -l app=radarr
```

### 2. Run the migration

```bash
./scripts/cnpg-migration/migrate.sh radarr
```

It refuses to run if the Deployment is not at `replicas: 0`. At the end it prints
the row counts it loaded.

### 3. Start the app, in git

Set `replicas: 1`, commit, push.

### 4. Verify

- Library, history and settings look right in the UI over Tailscale
  (`radarr.tail5f17e.ts.net`).
- **Add and then remove one item** — a tag or an indexer. This is the only thing
  that actually exercises an INSERT, and therefore the only check that the
  sequence reset worked. A migration with unreset sequences looks completely fine
  until the first write.
- No `duplicate key value violates unique constraint` in `kubectl -n arr logs`.

## Rolling back

The original SQLite files are never modified — they are still sitting on the
config PVC. Rolling back is reverting the `*__POSTGRES__*` (or `DB_*`) env vars
in git. The Postgres databases can be left in place; nothing reads them once the
env vars are gone.

## Notes

- **Seerr's WAL.** Seerr keeps a multi-megabyte `db.sqlite3-wal` alongside a
  small `db.sqlite3`. Most of the data is in the WAL. pgloader opens the file
  with SQLite's own library, which replays the WAL on open, so this is handled —
  but it is why the pgloader Job mounts the volume read-write rather than
  read-only.
- **Node pinning.** The config PVCs are ReadWriteOnce on Longhorn, so the
  pgloader Job is pinned with `nodeName` to whichever PIA node currently holds
  the volume. Without that it sits `Pending` on a multi-attach error.
- **pgloader version.** `dimitri/pgloader:v3.6.7` is the last tagged release.
  It is old but it is what every working writeup of this migration uses.
- **Dialect-specific migration tables must not be copied.** Seerr keeps
  *separate* migration sets for SQLite and Postgres — 53 SQLite migrations named
  from 2020, 19 Postgres ones named from 2024, no overlap. Copying SQLite's
  `migrations` table across leaves TypeORM believing no Postgres migration has
  run, so on startup it replays `InitialMigration1734786061496` over a schema
  that already exists and crashloops on `relation "PK_04dc42a96bf0914cda31b579702"
  already exists`. `EXCLUDE_TABLES` in `migrate.sh` handles this: the table is
  snapshotted before the truncate and restored after the load.

  The arr apps are the opposite case — their `VersionInfo` is dialect-neutral, so
  it *should* be carried across, and `EXCLUDE_TABLES` is empty for them.

  Note this cannot be done with a pgloader flag: its CLI has no table filter at
  all. `INCLUDING ONLY TABLE NAMES` / `EXCLUDING TABLE NAMES` are command-file
  syntax only.
- **pgloader exits 0 on a failed load.** Sonarr's `logs.db` was corrupt — an
  artifact of an unrelated April incident, alongside the `sonarr.db.corrupt` and
  `.bak` files still on that volume. pgloader logged `Code CORRUPT: database
  disk image is malformed`, loaded nothing, and **still exited 0**, so a
  Job-completion check alone reported success on an empty database. The script
  now greps the job log for source errors and asserts the target is non-empty.
- **A failed load leaves the target mid-procedure.** The schema is created and
  then truncated *before* pgloader runs, so a failure leaves tables present with
  an empty `VersionInfo`. The arr apps read that table to decide which
  migrations to run: finding tables but no migration history, they try to
  `CREATE TABLE` over the top and fail at startup. If a load fails and you
  intend to give the app an empty database instead, reset it properly:

  ```sql
  DROP SCHEMA public CASCADE;
  CREATE SCHEMA public AUTHORIZATION arr;
  ```

  That is what was done to `sonarr-log`, so Sonarr rebuilds its log schema from
  scratch. Losing that history costs nothing — it is application logs, and in
  this case they had been unreadable since April anyway.

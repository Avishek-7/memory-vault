# Rollback plan

Two different failure modes need two different rollbacks: a bad **image**
(the code is wrong) and a bad **migration** (the schema is wrong). They are
not the same problem and don't share a fix — a schema change that already
ran doesn't un-run just because you go back to the previous image.

## 1. Bad deploy (new image, same schema)

CI (`.github/workflows/ci.yml`) pushes every commit on `master` as both
`ghcr.io/avishek-7/memory-vault:latest` and
`ghcr.io/avishek-7/memory-vault:<full commit sha>`. That per-commit tag is
the rollback mechanism: `:latest` always moves, the sha tag never does.

```bash
# On the host running memory-vault (docker-compose.yml):
git log --oneline -5                 # find the last-known-good commit
docker pull ghcr.io/avishek-7/memory-vault:<good-sha>
docker tag ghcr.io/avishek-7/memory-vault:<good-sha> \
           ghcr.io/avishek-7/memory-vault:latest
docker compose up -d memory-vault
```

Re-tagging locally rather than editing `docker-compose.yml` to pin a sha
keeps the compose file unchanged, so the next real deploy (a push to
`master`) overwrites `:latest` normally instead of silently staying pinned
to the old rollback forever — an easy way to "fix" an incident and then
forget you did.

**Verify before declaring it fixed**, not just that the container started:

```bash
curl -s -H "Host: <your ALLOWED_HOSTS value>" http://localhost:8080/healthz
docker logs memory-vault --tail 20
```

`store.Open` runs the schema migration on every startup (see below) — even
rolling back to an older image still runs *that image's* migration against
whatever the schema currently is. If the bad deploy included a migration,
rolling back the image does not undo it; see part 2.

## 2. Bad migration

`internal/store/store.go`'s `migrate()` wraps every schema change in a
single transaction (`internal/store/store.go:448-461`) and is followed
by `checkEmbedDim`/`supportsIterativeScan`, both of which fail startup
loudly. That covers exactly one failure mode well: **a migration that
doesn't fully apply.** Either it fully commits or it fully rolls back —
there is no state where memory-vault is running against a half-migrated
schema.

**It does not cover the other failure mode: a migration that fully applies
but is wrong** — a column dropped that shouldn't have been, a `NOT NULL`
that breaks existing rows in a way the migration's own logic didn't
account for, a bad default. `migrationSQL` has no down-migration and this
repo has no migration-versioning tool (Goose, migrate, etc.) — every schema
change lives forward-only inside one idempotent SQL block. **The only way
back from a migration that successfully ran and turned out wrong is
restoring from backup.** Don't go looking for a `migrate down` command;
there isn't one.

```bash
# 1. Stop the app so nothing writes against the bad schema while you work.
docker compose stop memory-vault

# 2. Fetch and decrypt the last good backup — same repo/branch backup.sh
#    pushes to, same steps standby-sync.sh already automates. AGE_IDENTITY
#    is the private half of backup.sh's AGE_RECIPIENT.
git clone --quiet "$BACKUP_GIT_REMOTE" /tmp/mv-restore
age -d -i "$AGE_IDENTITY" -o /tmp/mv-restore/dump.sql \
    /tmp/mv-restore/memory-vault-dump.sql.age

# 3. Restore it. --clean --if-exists means this replaces the current
#    schema, it does not merge with it. See README's "Backups cover every
#    tenant" for what SUPERUSER_DATABASE_URL needs to be.
psql "$SUPERUSER_DATABASE_URL" -v ON_ERROR_STOP=1 -f /tmp/mv-restore/dump.sql

# 4. Roll the image back too if the bad migration shipped inside a bad
#    commit (part 1) — otherwise the same image just re-runs the same
#    migration against the just-restored schema on its next start.

# 5. Bring the app back up and verify (see part 1's checks) before
#    declaring the incident over.
docker compose up -d memory-vault
rm -rf /tmp/mv-restore
```

Any write made between the bad migration landing and the restore is lost —
`--clean --if-exists` is a point-in-time restore, not a merge. That gap is
bounded by how often backups actually run.

**This exact procedure — the `pg_dump`+`age` path above — has not itself
been run end-to-end**, because as section 3 below explains, `backup.sh`
isn't deployed yet, so there is no encrypted dump to fetch. What *has* been
verified is a different backup lineage (the raw-tar cron job) restoring
cleanly; that's meaningful evidence the underlying data isn't corrupt, but
it is not a test of the commands directly above. Treat this section's
commands as reviewed-correct against `backup.sh`/`standby-sync.sh`'s own
logic, not as drilled.

## 3. Raw-tar backup check (verified 2026-08-09, not the pg_dump path above)

This was tested end-to-end on the live host, non-destructively — extracted
a production backup into a throwaway container and diffed it against the
running database, not just assumed to work:

```bash
# Fresh volume + throwaway container, never the live one:
docker volume create restore-test
docker run --rm -v restore-test:/var/lib/postgresql/data \
  -v <backup-source>:/backup:ro alpine \
  sh -c "tar xzf /backup/<file>.tar.gz -C /var/lib/postgresql/data && chown -R 999:999 /var/lib/postgresql/data"
docker run -d --name restore-test -v restore-test:/var/lib/postgresql/data \
  -e POSTGRES_USER=<user> -e POSTGRES_PASSWORD=<password> pgvector/pgvector:pg16
```

Result: Postgres detected the unclean shutdown (expected — the tar was
taken from a *live*, running server) and completed WAL crash recovery
automatically on startup. Row counts, a full content hash across every
memory, RLS (`relrowsecurity`/`relforcerowsecurity`), and the pgvector
extension version all matched the live database exactly.

**What this does and doesn't prove.** It proves the currently-running daily
backup (`~/scripts/container-backups.sh`, a cron job — see caveat below)
produced a restorable snapshot *today*. It does not prove every future tar
will: a plain `tar` of a live `$PGDATA` is not Postgres's documented-safe
backup method (that's `pg_dump`/`pg_basebackup`, or stopping the server
first) — it works here because the tar happened to capture enough of
`pg_wal` for crash recovery to reconcile it, which isn't guaranteed under
heavier write load or an unlucky checkpoint boundary. Re-run this check
periodically, not once and assume it holds forever.

**Known gap, not yet closed:** `deploy/backup.sh` (proper `pg_dump`,
encrypted, pushed off-host) and `deploy/standby-sync.sh` exist with systemd
unit files in this directory, but as of this writing neither is installed
(`systemctl status memory-vault-backup.timer` → not found) — the only
backup actually running is the raw-tar cron job above, and it's local to
this one host's disk. There is currently no off-host copy of memory-vault's
data. Losing this host's disk loses the backups along with the primary.

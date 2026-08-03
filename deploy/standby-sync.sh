#!/bin/sh
# memory-vault standby sync
#
# Pulls the latest encrypted backup pushed by deploy/backup.sh (same
# BACKUP_GIT_REMOTE/BACKUP_GIT_BRANCH), decrypts it with age, and restores the
# pg_dump into this host's own local Postgres, so the standby is never far
# behind the primary. Meant to run on the standby host only, on a short
# interval (default every 15 minutes via memory-vault-standby-sync.timer).
#
# The dump is --clean --if-exists, so a restore replaces whatever the standby
# already had: this is a mirror of the primary, not a merge. It carries every
# tenant, their API keys, and the RLS policy, none of which the older
# JSON-import path covered.
#
# The memory_vault_app role must already exist on the standby (deploy/init-db.sql),
# because the dump reassigns table ownership to it by name. Restoring without
# it leaves the tables owned by the restoring superuser, and the app would
# then fail its own migration on "must be owner of table memories".
#
# Every run appends one line to STANDBY_SYNC_LOG recording what happened
# (synced / skipped-no-change / failed, with counts), so a person debugging
# a failover event (deploy/failover-watch.sh reads this same log) can see
# how current the standby's data actually was when it took over.
#
# Required env (e.g. via /etc/memory-vault-standby-sync.env, loaded by the
# systemd unit):
#   RESTORE_DATABASE_URL - the standby's own local Postgres, not the primary's,
#                        as a SUPERUSER: the restore recreates the schema,
#                        reassigns ownership, and reinstalls the RLS policy,
#                        none of which the unprivileged app role may do
#   BACKUP_GIT_REMOTE  - same private backup repo deploy/backup.sh pushes to
#   AGE_IDENTITY        - path to the age private key file that can decrypt
#                        backups (the secret half of backup.sh's AGE_RECIPIENT)
# Optional env:
#   BACKUP_GIT_DIR       - local mirror clone (default: /var/lib/memory-vault-backup/standby-repo;
#                          deliberately separate from backup.sh's BACKUP_GIT_DIR
#                          in case both scripts ever run on the same host)
#   BACKUP_GIT_BRANCH    - branch to pull from (default: main)
#   BACKUP_FILE_NAME     - encrypted file name inside the repo (default: memory-vault-dump.sql.age)
#   STANDBY_SYNC_LOG     - log file (default: /var/log/memory-vault-standby-sync.log)
#   STANDBY_STATE_DIR    - where the last-synced-commit marker lives
#                          (default: /var/lib/memory-vault-backup/standby-state)

set -eu

: "${RESTORE_DATABASE_URL:?RESTORE_DATABASE_URL is required (the standby Postgres, as a superuser)}"
: "${BACKUP_GIT_REMOTE:?BACKUP_GIT_REMOTE is required}"
: "${AGE_IDENTITY:?AGE_IDENTITY is required (path to the age private key file)}"
BACKUP_GIT_DIR="${BACKUP_GIT_DIR:-/var/lib/memory-vault-backup/standby-repo}"
BACKUP_GIT_BRANCH="${BACKUP_GIT_BRANCH:-main}"
BACKUP_FILE_NAME="${BACKUP_FILE_NAME:-memory-vault-dump.sql.age}"
STANDBY_SYNC_LOG="${STANDBY_SYNC_LOG:-/var/log/memory-vault-standby-sync.log}"
STANDBY_STATE_DIR="${STANDBY_STATE_DIR:-/var/lib/memory-vault-backup/standby-state}"

log() {
	# Never echo AGE_IDENTITY/DATABASE_URL contents — only ever the fixed
	# fields below.
	printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$1" >>"$STANDBY_SYNC_LOG"
}

if ! command -v age >/dev/null 2>&1; then
	log "FAIL age not found on PATH"
	echo "standby-sync.sh: 'age' not found on PATH" >&2
	exit 1
fi
if ! command -v psql >/dev/null 2>&1; then
	log "FAIL psql not found on PATH"
	echo "standby-sync.sh: 'psql' not found on PATH (install the postgresql client tools)" >&2
	exit 1
fi
if [ ! -r "$AGE_IDENTITY" ]; then
	log "FAIL age identity file unreadable"
	echo "standby-sync.sh: AGE_IDENTITY '$AGE_IDENTITY' not readable" >&2
	exit 1
fi

mkdir -p "$STANDBY_STATE_DIR"
last_commit_file="$STANDBY_STATE_DIR/last-synced-commit"

if [ ! -d "$BACKUP_GIT_DIR/.git" ]; then
	mkdir -p "$(dirname "$BACKUP_GIT_DIR")"
	git clone --quiet "$BACKUP_GIT_REMOTE" "$BACKUP_GIT_DIR"
else
	git -C "$BACKUP_GIT_DIR" fetch origin --quiet
fi

if ! git -C "$BACKUP_GIT_DIR" show-ref --verify --quiet "refs/remotes/origin/$BACKUP_GIT_BRANCH"; then
	log "FAIL no backups pushed yet (branch $BACKUP_GIT_BRANCH not found on remote)"
	echo "standby-sync.sh: branch '$BACKUP_GIT_BRANCH' not found on $BACKUP_GIT_REMOTE — has backup.sh run yet?" >&2
	exit 1
fi
git -C "$BACKUP_GIT_DIR" checkout --quiet -B "$BACKUP_GIT_BRANCH" "origin/$BACKUP_GIT_BRANCH"

current_commit="$(git -C "$BACKUP_GIT_DIR" rev-parse --short "origin/$BACKUP_GIT_BRANCH")"
last_commit="$( [ -f "$last_commit_file" ] && cat "$last_commit_file" || true )"

if [ "$current_commit" = "$last_commit" ]; then
	log "SKIP commit=$current_commit (no new backup)"
	exit 0
fi

enc_file="$BACKUP_GIT_DIR/$BACKUP_FILE_NAME"
if [ ! -f "$enc_file" ]; then
	log "FAIL commit=$current_commit missing $BACKUP_FILE_NAME"
	echo "standby-sync.sh: $enc_file not found at commit $current_commit" >&2
	exit 1
fi

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
plain_file="$work_dir/dump.sql"

if ! age -d -i "$AGE_IDENTITY" -o "$plain_file" "$enc_file" 2>"$work_dir/age.err"; then
	log "FAIL commit=$current_commit decrypt error"
	echo "standby-sync.sh: age decrypt failed: $(cat "$work_dir/age.err")" >&2
	exit 1
fi

# ON_ERROR_STOP so a partially-applied dump is a loud failure rather than a
# standby that looks synced while holding half the primary's data.
if ! psql "$RESTORE_DATABASE_URL" -v ON_ERROR_STOP=1 --quiet -f "$plain_file" >"$work_dir/restore.out" 2>"$work_dir/restore.err"; then
	log "FAIL commit=$current_commit restore error"
	echo "standby-sync.sh: restore failed: $(tail -n 5 "$work_dir/restore.err")" >&2
	exit 1
fi

counts="$(psql "$RESTORE_DATABASE_URL" -tAc \
	"SELECT 'memories='||(SELECT count(*) FROM memories)||\
	        ' tenants='||(SELECT count(*) FROM tenants)||\
	        ' api_keys='||(SELECT count(*) FROM api_keys)" 2>/dev/null || echo "counts unavailable")"
echo "$current_commit" >"$last_commit_file"
log "OK commit=$current_commit $counts"
echo "standby-sync.sh: synced commit $current_commit ($counts)"

#!/bin/sh
# memory-vault encrypted backup
#
# Dumps the whole database with pg_dump, encrypts the result with age, and
# commits+pushes it to a private git repo used only for backups.
#
# pg_dump rather than `memory-vault export`, because export goes through the
# RLS-scoped storage layer and therefore covers exactly ONE tenant. On a
# multi-tenant vault that would silently back up a fraction of the data. The
# dump also preserves what a JSON export deliberately drops: embeddings, API
# keys, tenant rows, table ownership, and the RLS policy itself.
#
# NOTE: BACKUP_DATABASE_URL must be a SUPERUSER or BYPASSRLS role — NOT the
# app's own DATABASE_URL. `memories` has FORCE ROW LEVEL SECURITY, which
# applies to the table owner too, so pg_dump connecting as the app role fails
# outright ("query would be affected by row-level security policy").
#
# IMPORTANT: BACKUP_GIT_REMOTE must point at a *separate, private* repo —
# never this (memory-vault) repo. Committing an encrypted export here would
# mix backup history into public source history, and a misconfigured
# recipient key would make that history permanently unrecoverable-looking
# noise at best, an accidental leak at worst.
#
# Requires `age` (https://github.com/FiloSottile/age) for encryption. If
# you'd rather use gpg instead: replace the `age -r ... -o "$enc_file"`
# line below with
#   gpg --batch --yes --recipient "$GPG_RECIPIENT" --encrypt --output "$enc_file" "$plain_file"
# and swap standby-sync.sh's `age -d` for `gpg --decrypt`.
#
# Required env (e.g. via /etc/memory-vault-backup.env, loaded by the
# systemd unit):
#   BACKUP_DATABASE_URL - Postgres URL for a superuser or BYPASSRLS role (see
#                        the note above; the app's unprivileged DATABASE_URL
#                        cannot dump its own tables)
#   AGE_RECIPIENT      - one or more age public keys (space-separated) to
#                        encrypt to; only the matching private key can
#                        decrypt (kept on the standby host, not here)
#   BACKUP_GIT_REMOTE  - git remote URL of the private backup repo
# Optional env:
#   BACKUP_GIT_DIR     - local clone of BACKUP_GIT_REMOTE (default: /var/lib/memory-vault-backup/repo)
#   BACKUP_GIT_BRANCH  - branch to commit/push to (default: main)
#   BACKUP_FILE_NAME   - encrypted file name inside the repo (default: memory-vault-dump.sql.age)

set -eu

: "${BACKUP_DATABASE_URL:?BACKUP_DATABASE_URL is required (a superuser or BYPASSRLS role, not the unprivileged app DATABASE_URL)}"
: "${AGE_RECIPIENT:?AGE_RECIPIENT is required (one or more age public keys, space-separated)}"
: "${BACKUP_GIT_REMOTE:?BACKUP_GIT_REMOTE is required (a private git repo, not this one)}"

BACKUP_GIT_DIR="${BACKUP_GIT_DIR:-/var/lib/memory-vault-backup/repo}"
BACKUP_GIT_BRANCH="${BACKUP_GIT_BRANCH:-main}"
BACKUP_FILE_NAME="${BACKUP_FILE_NAME:-memory-vault-dump.sql.age}"

if ! command -v age >/dev/null 2>&1; then
	echo "backup.sh: 'age' not found on PATH (see this script's header for a gpg alternative)" >&2
	exit 1
fi
for tool in pg_dump psql; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		echo "backup.sh: '$tool' not found on PATH (install the postgresql client tools)" >&2
		exit 1
	fi
done

# Fail with an explanation rather than pg_dump's opaque RLS error. An
# unprivileged role can authenticate and read its own rows perfectly well, so
# this misconfiguration would otherwise only surface as a failed backup.
can_read_all="$(psql "$BACKUP_DATABASE_URL" -tAc \
	"SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user")"
if [ "$can_read_all" != "t" ]; then
	echo "backup.sh: BACKUP_DATABASE_URL connects as a role that is neither a superuser nor" >&2
	echo "BYPASSRLS. memories has FORCE ROW LEVEL SECURITY, so pg_dump would dump it as empty" >&2
	echo "or fail. Use the database superuser, or create a dedicated reader:" >&2
	echo "    CREATE ROLE memory_vault_backup LOGIN BYPASSRLS PASSWORD '...';" >&2
	echo "    GRANT USAGE ON SCHEMA public TO memory_vault_backup;" >&2
	echo "    GRANT SELECT ON ALL TABLES IN SCHEMA public TO memory_vault_backup;" >&2
	exit 1
fi

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
plain_file="$work_dir/dump.sql"
enc_file="$work_dir/$BACKUP_FILE_NAME"

# --clean --if-exists so the dump restores over an existing standby database
# instead of colliding with it. Ownership is deliberately NOT stripped: the
# restore has to hand the tables back to the app role, which is what lets the
# app run its own migrations (only a table's owner may ALTER it).
pg_dump --clean --if-exists "$BACKUP_DATABASE_URL" >"$plain_file"

# age's ciphertext is never byte-stable run-to-run (it uses a fresh
# ephemeral key each time), even for identical plaintext, so diffing the
# encrypted blob can't tell "nothing changed" from "everything changed".
# Hash the plaintext instead and compare against the last hash committed
# alongside the encrypted file, before spending an encrypt+commit+push on
# a no-op run.
# Recent pg_dump wraps its output in \restrict/\unrestrict lines carrying a
# freshly generated random token, so two dumps of identical data are never
# byte-identical. Excluding just those lines from the change-detection hash
# keeps the "nothing changed, skip the commit" path working; the stored dump
# itself is untouched and keeps the markers.
plain_hash="$(grep -vE '^\\(un)?restrict ' "$plain_file" | sha256sum | cut -d' ' -f1)"
hash_file_name="$BACKUP_FILE_NAME.sha256"

if [ ! -d "$BACKUP_GIT_DIR/.git" ]; then
	mkdir -p "$(dirname "$BACKUP_GIT_DIR")"
	git clone --quiet "$BACKUP_GIT_REMOTE" "$BACKUP_GIT_DIR"
	if git -C "$BACKUP_GIT_DIR" show-ref --verify --quiet "refs/remotes/origin/$BACKUP_GIT_BRANCH"; then
		git -C "$BACKUP_GIT_DIR" checkout --quiet -B "$BACKUP_GIT_BRANCH" "origin/$BACKUP_GIT_BRANCH"
	else
		# Brand-new/empty backup repo: BACKUP_GIT_BRANCH doesn't exist yet
		# upstream, so start it as an orphan branch instead of failing.
		git -C "$BACKUP_GIT_DIR" checkout --quiet --orphan "$BACKUP_GIT_BRANCH"
		git -C "$BACKUP_GIT_DIR" rm --quiet -rf . >/dev/null 2>&1 || true
	fi
else
	git -C "$BACKUP_GIT_DIR" fetch origin --quiet
	git -C "$BACKUP_GIT_DIR" checkout "$BACKUP_GIT_BRANCH" --quiet
	if git -C "$BACKUP_GIT_DIR" show-ref --verify --quiet "refs/remotes/origin/$BACKUP_GIT_BRANCH"; then
		git -C "$BACKUP_GIT_DIR" reset --hard "origin/$BACKUP_GIT_BRANCH" --quiet
	fi
fi

if [ -f "$BACKUP_GIT_DIR/$hash_file_name" ] && [ "$(cat "$BACKUP_GIT_DIR/$hash_file_name")" = "$plain_hash" ]; then
	echo "backup.sh: export unchanged since last backup, nothing to do"
	exit 0
fi

age_recipient_args=""
for recipient in $AGE_RECIPIENT; do
	age_recipient_args="$age_recipient_args -r $recipient"
done
# shellcheck disable=SC2086 # word-splitting into -r flags is intentional here
age $age_recipient_args -o "$enc_file" "$plain_file"
rm -f "$plain_file"

cp "$enc_file" "$BACKUP_GIT_DIR/$BACKUP_FILE_NAME"
rm -f "$enc_file"
echo "$plain_hash" >"$BACKUP_GIT_DIR/$hash_file_name"

cd "$BACKUP_GIT_DIR"
git add "$BACKUP_FILE_NAME" "$hash_file_name"

git -c user.email="memory-vault-backup@localhost" -c user.name="memory-vault-backup" \
	commit --quiet -m "backup $(date -u +%Y-%m-%dT%H:%M:%SZ)"
git push --quiet origin "$BACKUP_GIT_BRANCH"
echo "backup.sh: pushed new backup ($(date -u +%Y-%m-%dT%H:%M:%SZ))"

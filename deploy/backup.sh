#!/bin/sh
# memory-vault encrypted backup
#
# Exports every space via `memory-vault export` (no --space), encrypts the
# result with age, and commits+pushes it to a private git repo used only
# for backups.
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
#   DATABASE_URL       - same as the memory-vault server's
#   AGE_RECIPIENT      - one or more age public keys (space-separated) to
#                        encrypt to; only the matching private key can
#                        decrypt (kept on the standby host, not here)
#   BACKUP_GIT_REMOTE  - git remote URL of the private backup repo
# Optional env:
#   MEMORY_VAULT_BIN   - path to the memory-vault binary (default: memory-vault, must be on PATH)
#   BACKUP_GIT_DIR     - local clone of BACKUP_GIT_REMOTE (default: /var/lib/memory-vault-backup/repo)
#   BACKUP_GIT_BRANCH  - branch to commit/push to (default: main)
#   BACKUP_FILE_NAME   - encrypted file name inside the repo (default: memory-vault-export.age)

set -eu

: "${DATABASE_URL:?DATABASE_URL is required}"
: "${AGE_RECIPIENT:?AGE_RECIPIENT is required (one or more age public keys, space-separated)}"
: "${BACKUP_GIT_REMOTE:?BACKUP_GIT_REMOTE is required (a private git repo, not this one)}"

MEMORY_VAULT_BIN="${MEMORY_VAULT_BIN:-memory-vault}"
BACKUP_GIT_DIR="${BACKUP_GIT_DIR:-/var/lib/memory-vault-backup/repo}"
BACKUP_GIT_BRANCH="${BACKUP_GIT_BRANCH:-main}"
BACKUP_FILE_NAME="${BACKUP_FILE_NAME:-memory-vault-export.age}"

if ! command -v age >/dev/null 2>&1; then
	echo "backup.sh: 'age' not found on PATH (see this script's header for a gpg alternative)" >&2
	exit 1
fi
if ! command -v "$MEMORY_VAULT_BIN" >/dev/null 2>&1 && [ ! -x "$MEMORY_VAULT_BIN" ]; then
	echo "backup.sh: memory-vault binary not found at '$MEMORY_VAULT_BIN' (set MEMORY_VAULT_BIN)" >&2
	exit 1
fi

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
plain_file="$work_dir/export.json"
enc_file="$work_dir/$BACKUP_FILE_NAME"

# No --space: exports every space, per export_memories'/the CLI's existing
# "omit to export everything" behavior.
"$MEMORY_VAULT_BIN" export >"$plain_file"

# age's ciphertext is never byte-stable run-to-run (it uses a fresh
# ephemeral key each time), even for identical plaintext, so diffing the
# encrypted blob can't tell "nothing changed" from "everything changed".
# Hash the plaintext instead and compare against the last hash committed
# alongside the encrypted file, before spending an encrypt+commit+push on
# a no-op run.
plain_hash="$(sha256sum "$plain_file" | cut -d' ' -f1)"
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

# Changelog

## Unreleased

**Multi-tenant backups, and a vector-search recall fix.** Both close gaps
opened by the move to row-level security.

- `deploy/backup.sh` now uses `pg_dump`, and `deploy/standby-sync.sh`
  restores it with `psql`. The old chain ran `memory-vault export`, which
  goes through the RLS-scoped storage layer and so covered exactly one
  tenant — a multi-tenant vault was silently backing up a fraction of its
  data. The dump also carries what a JSON export drops: embeddings, API
  keys, tenant rows, table ownership, and the RLS policy.
- **Breaking (failover tooling only):** `backup.sh` now needs
  `BACKUP_DATABASE_URL` and `standby-sync.sh` needs `RESTORE_DATABASE_URL`,
  both privileged. `FORCE ROW LEVEL SECURITY` binds the table owner, so
  `pg_dump` as the app role fails outright; `backup.sh` checks for
  superuser/`BYPASSRLS` up front rather than producing an empty backup.
  `MEMORY_VAULT_BIN` and `OLLAMA_URL` are no longer used by either script,
  and the backup file is now `memory-vault-dump.sql.age`.
- Backup change-detection no longer commits on every run. Recent `pg_dump`
  wraps output in `\restrict`/`\unrestrict` lines carrying a random token,
  so no two dumps were byte-identical and the "nothing changed" skip never
  fired. Those lines are excluded from the hash only.
- The JSON `export_memories`/`import_memories` path is now documented as a
  portability tool — it survives a change of embedding model, which a dump
  does not — rather than as a backup.
- Vector searches now enable pgvector's iterative index scans per
  transaction. Note the subtlety this exposed: pgvector's settings do not
  exist until its library is loaded into the backend, so setting
  `hnsw.iterative_scan` on a fresh pooled connection silently succeeds as a
  placeholder GUC and does nothing. The bind statement now forces the
  library to load first, and a test asserts the setting is real
  (`vartype='enum'`) on a cold connection.

**Step 2 of multi-tenancy — API keys.** A request's bearer token now decides
which tenant's memories it reaches. Step 1 built the boundary; this is what
puts requests on the correct side of it.

- New `api_keys` table (`id`, `tenant_id`, `key_hash`, `label`,
  `created_at`, `revoked_at`). Keys are 32 bytes of CSPRNG output, prefixed
  `mv_`, and stored only as a SHA-256 digest — a lost key is reissued, never
  recovered, and a database dump yields no usable credentials. Like
  `tenants`, this table carries no RLS policy: authentication reads it
  *before* it knows which tenant the request belongs to.
- `/mcp` resolves the token to a tenant and runs every tool against a
  `ForTenant`-scoped store, threaded through `handle`/`callTool` as a
  parameter instead of the package-global store.
- New CLI: `memory-vault tenant create|list` and
  `memory-vault key create|list|revoke`. Deliberately CLI-only — there is no
  admin HTTP surface, so there is none to secure. `tenant create` mints the
  tenant's first key, since a tenant without one cannot reach the server.
- `AUTH_TOKEN` keeps its old meaning: a shared static credential
  authenticating as the bootstrap tenant. Existing self-hosted clients keep
  working with no configuration change.
- Anonymous access to `/mcp` now closes itself. It remains available while
  `AUTH_TOKEN` is unset *and* no API key exists, so a fresh local vault
  needs no configuration; minting the first key turns it off immediately, so
  a deploy that grows real tenants can't serve them to an anonymous caller.
Fixed in review of the above, before release:

- Revoking the last outstanding API key no longer re-opens anonymous
  access. The gate counted only *live* keys, so an operator revoking a
  leaked key would have flipped a closed vault into one serving the
  bootstrap tenant to any unauthenticated caller. Issuing a key is now a
  one-way door out of single-tenant mode.
- `memory-vault export`/`import` take `-tenant`, and refuse to run
  unscoped once a non-bootstrap tenant exists. Both go through the
  RLS-scoped store, so an unscoped `export` covered only the bootstrap
  tenant — meaning `deploy/backup.sh` would have silently backed up a
  fraction of the vault, and `deploy/standby-sync.sh` restored that
  fraction, with no error and no visible change in the backup. A working
  multi-tenant backup still needs an all-tenants export format, which is
  not designed yet; until then it fails loudly instead of quietly.
- On a vault open to anonymous callers, a request carrying an
  unrecognized token is accepted rather than rejected. Rejecting it would
  have been stricter than rejecting nothing at all, and broke clients
  still sending a stale bearer header after `AUTH_TOKEN` was cleared.
- The destructive `internal/store` tests now refuse to run against a
  database whose name doesn't look disposable (override with
  `MEMORY_VAULT_ALLOW_DESTRUCTIVE_TESTS=1`). They drop tables and delete
  every API key, and keys are unrecoverable by design, so a `go test` in a
  shell that had sourced a deployment's `.env` would have forced every
  client to be re-issued. CI's database is now `memory_vault_test`.
- `deploy/init-db.sql` is now idempotent. Roles are cluster-wide, so its
  `CREATE ROLE` failed on a second database in the same cluster and on any
  re-run — aborting the script before the `GRANT` and leaving the role
  unusable. This broke the hand-application path the README documents for
  upgrading an existing deployment.
- Server version reported over MCP is now `0.10.0`.

**Step 1 of multi-tenancy — tenants and row-level security.** Isolation is
now enforced by Postgres rather than by application code filtering
correctly. Search, summarization, and provenance logic is untouched; it
simply operates inside a tenant-scoped connection.

- New `tenants` table (`id`, `email`, `plan`, `created_at`,
  `stripe_customer_id`) and a `tenant_id` on `memories`, defaulted from the
  session variable so no `INSERT` had to change.
- RLS policy `memories_tenant_isolation` filters every statement on
  `current_setting('app.tenant_id')`, with `FORCE ROW LEVEL SECURITY` so
  the table owner is bound by it too. Primary key widened to
  `(tenant_id, space, name, chunk_index)`.
- The storage layer reaches Postgres only through an unexported
  tenant-bound wrapper that opens a transaction and binds `app.tenant_id`
  before any query. `Store` no longer exposes a `*sql.DB`, so no caller can
  reach the pool and skip the binding. `Store.Ping` replaces `st.DB.Exec`
  for `/healthz`.
- **Breaking:** the server now refuses to start when `DATABASE_URL` names a
  superuser or a `BYPASSRLS` role, because RLS cannot be enforced for such
  a role and tenant isolation would be silently absent. `POSTGRES_USER` in
  the standard Postgres image is a superuser, so existing deployments must
  switch to the unprivileged role created by the new `deploy/init-db.sql`.
  `docker-compose.yml` now applies that automatically and connects as
  `memory_vault_app`.
- Rows written before multi-tenancy are adopted by a bootstrap tenant
  during migration, so an existing single-tenant vault keeps working
  unchanged. The migration runs in a single transaction.
- CI now runs a Postgres service, so the tenant-isolation tests actually
  execute on every PR instead of skipping.

## 0.9.0

Health-check-based DNS failover to a standby instance, plus a fix to a
bug found while shipping it. No default behavior changed for normal
operation — `/healthz` is new and additive; everything else is external
scripting/ops tooling in `deploy/`.

**`save_memory` multi-chunk fix.** `ChunkContent`'s word-count estimate
for `all-minilm`'s 256-token budget assumed a words/token ratio that
doesn't hold for markdown/technical prose (paths, hyphens, numbers
tokenize denser than plain English) — a chunk could still exceed the
real token budget and get rejected by Ollama, surfacing as a bare
internal error. `saveMemory` now detects that rejection
(`embed.ErrContextLength`) and adaptively splits the offending chunk,
instead of trusting the word-count estimate to always hold. Lowered the
chunk target from 150/15 words to 100/10 as a saner default.

**Phase 1 — `GET /healthz`.** New unauthenticated (but still
`ALLOWED_HOSTS`-gated) endpoint: pings Postgres and returns 200/`{"status":"ok","db":"ok"}`
or 503/`{"status":"error","db":"unreachable"}`. Checks only Postgres, not
Ollama — "can this instance serve MCP requests" is a different, more
urgent question than whether embedding/compaction currently works.

**Phase 2 — `deploy/backup.sh`.** Exports every space via the existing
export CLI, encrypts with age, and commits+pushes to a configurable
private git remote (never this repo). Idempotent — hashes the plaintext
export and skips the commit/push if unchanged, since age's ciphertext
isn't byte-stable run-to-run. `memory-vault-backup.service`/`.timer`
(systemd, default hourly) run it unattended.

**Phase 3 — `deploy/standby-sync.sh`.** Pulls the latest backup, decrypts
it, and imports it with `overwrite: true` into a standby instance's own
local memory-vault. Tracks the last-synced commit so an unchanged backup
is a no-op instead of redoing the decrypt/import/re-embed pipeline every
run. Logs each sync's outcome (synced/skipped/failed, with import
counts). `memory-vault-standby-sync.service`/`.timer` (systemd, default
every 15 minutes).

**Phase 4 — `deploy/failover-watch.sh`.** Runs on the standby, polls the
primary's `/healthz`, and flips a Cloudflare DNS record to the standby's
IP after `FAILOVER_THRESHOLD` consecutive failures (default 3),
reverting after `RECOVERY_THRESHOLD` consecutive successes (default 3).
A cooldown after any flip prevents flapping; an optional
`NOTIFY_WEBHOOK_URL` fires a JSON payload on every transition. All
Cloudflare API calls and state transitions fail loudly to the log.
Ships as a long-running systemd service (`Restart=always`), not a timer
— sub-minute timer granularity isn't reliable in systemd — with a
`--once` mode available for anyone who'd rather drive it from cron
instead.

**Phase 5 — Documentation.** New README "Fallback / failover" section
covering the design, the explicit DNS-TTL/sync-lag caveats, prerequisites
(standby host, private backup repo, age key pair, DNS-edit-scoped
Cloudflare token, low DNS TTL), setup steps, and a full env var table for
the new tooling — plus an explicit statement that reconciliation after a
failover window is manual, not automatic bidirectional sync.

`serverInfo.version` bumped to `0.9.0`.

## 0.8.0

Four scoped additive features for using the vault from multiple agents and
across sessions. No default behavior changed — every new argument is
optional and every default reproduces prior behavior.

**Phase 1 — `source` field.** Memories now carry a `source` (default
`"unspecified"`), a free-form string naming whichever agent wrote them.
`save_memory`/`list_memories`/`search_memories`/`export_memories` all
accept an optional `source`; `save_memory`'s new `expect_source` argument
rejects a write instead of silently overwriting a different source's
same-named memory (naming the existing source in the error). `space`
remains the actual isolation boundary — `source` is provenance and
collision-avoidance only. `export_memories`/`import_memories` carry
`source` through the JSON payload and CLI flags. `memory-vault-tui` shows
`source` as a tag in the memory list and prompts for it on `n` (new).

**Phase 2 — `kind` field.** Memories now carry a `kind`, one of `fact`,
`decision`, `preference`, `task`, `note` (default `note`), validated at
the tool boundary — an unrecognized value is rejected with the list of
valid ones rather than a DB error. `save_memory` accepts it;
`list_memories`/`search_memories` accept an optional `kind` filter;
`search_memories` can boost a kind's score via `SEARCH_KIND_BOOST_<KIND>`
env vars (all default `0`). `compact_memories` now excludes `kind =
"decision"` memories from grouping entirely — a decision can never be
silently merged or pruned for staleness. `export_memories`/
`import_memories` carry `kind` through the payload (`"note"` for
pre-Phase-2 exports). `memory-vault-tui` shows `kind` alongside `source`
and prompts for it (validated, re-prompts on a bad value) on `n`.

**Phase 3 — `flag_memory`.** New tool sets a single current flag
(`useful`, `stale`, or `wrong`, plus an optional free-text `note`) on a
memory, overwriting any previous one (no flag history). `compact_memories`
folds flags into its existing similarity/staleness candidate selection:
`stale`/`wrong` makes a memory a candidate regardless of age; `useful`
protects it from age-based selection alone (not from a genuine
similarity-based merge). The `dry_run` plan explains when a flag
influenced selection ("flagged stale"/"flagged wrong" vs. plain "stale").

**Phase 4 — `get_session_summary`.** New read-only tool distinct from
`search_memories`: pulls a space's most-recently-updated memories
(favoring `task`/`decision` kind), and asks the local Ollama chat model
for a short resume — current state, what was decided, what's still open,
likely next step — via the same call path `compact_memories` already
uses. Told to say "unclear from stored memories" rather than invent a
next step. Optional `limit` (default 15, max 50). `memory-vault-tui` gets
an `s` keybinding to show the same summary for the current space.

`serverInfo.version` bumped to `0.8.0`.

## 0.7.0

Pre-launch polish pass — docs, packaging, and two scoped additive
features. No default behavior changed; everything here is opt-in.

**Phase 1 — Positioning.** Rewrote the top of the README: what's
actually distinctive (single Go binary, fully local via Ollama, active
compaction, a TUI, MCP resources), a fair comparison table against mem0
and Zep, and an explicit tradeoffs/limitations section.

**Phase 2 — Quickstart.** Added `docker-compose.yml` (Postgres/pgvector,
Ollama, memory-vault, plus a one-shot job that pulls the configured
Ollama models on first run) and `.env.example`, so `docker compose up`
is a complete local setup. Added a README Quickstart section.

**Phase 3 — Repo hygiene.** Added `LICENSE` (MIT), `CONTRIBUTING.md`,
GitHub issue/PR templates, and CI/license/Go-version badges. Updated the
repo's GitHub topics.

**Phase 4a — Pluggable embedding backend.** New `internal/embed` package
with an `Embedder` interface; the existing Ollama HTTP call moved behind
`OllamaEmbedder` (default, unchanged), plus a new `OpenAIEmbedder` for
any OpenAI-compatible embeddings endpoint. New opt-in env vars:
`EMBED_PROVIDER`, `EMBED_DIM`, `OPENAI_EMBED_BASE_URL`,
`OPENAI_EMBED_API_KEY`, `OPENAI_EMBED_MODEL`. The embedding dimension is
now configurable and validated on every embed call instead of hardcoded
to 384.

**Phase 4b — Export / import.** New `export_memories`/`import_memories`
MCP tools (JSON, no embeddings — those regenerate on import through the
normal chunk/embed/save path) and matching `memory-vault export`/`import`
CLI subcommands for scripting a backup without an MCP client.

`serverInfo.version` bumped to `0.7.0`.

## 0.6.0

- **`compact_memories` tool**: finds near-duplicate (embedding cosine
  distance under `COMPACT_SIMILARITY_THRESHOLD`) or stale
  (`COMPACT_STALE_DAYS`) memories within a space (or all spaces) and
  merges/summarizes them via a local Ollama chat model
  (`OLLAMA_CHAT_MODEL`, separate from the embedding model). Defaults to
  `dry_run: true`. Manual/on-demand — wire it into a cron or workflow for
  automatic runs.
- **`memory-vault-tui`**: a new standalone terminal binary
  (`cmd/memory-vault-tui`) for browsing, searching, editing (`$EDITOR`),
  creating, and deleting memories directly against Postgres — no MCP
  client needed. Built with bubbletea/bubbles/lipgloss. Not included in
  the Docker image; run it on the host.
- Extracted the DB/chunking/embedding/search logic shared by the MCP
  server and the new TUI into `internal/store`. Pure refactor — the MCP
  server's tool schemas and JSON-RPC responses are unchanged.

## 0.5.0

- Chunking: `save_memory` splits long content into overlapping chunks so
  it fits the embedding model's context window; reassembled transparently
  on read.
- HNSW index on embeddings; tunable Postgres connection pool.
- Namespaces (`space` argument) so the same memory name can exist
  independently in different spaces.
- Hybrid search: semantic + keyword (`ts_rank`) + recency, with
  configurable weights.
- MCP `resources/list` / `resources/read`, addressing each memory as
  `memory://<space>/<name>`.

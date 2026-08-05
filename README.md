# memory-vault

[![CI](https://github.com/Avishek-7/memory-vault/actions/workflows/ci.yml/badge.svg)](https://github.com/Avishek-7/memory-vault/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/github/license/Avishek-7/memory-vault)](LICENSE)
[![Go 1.26](https://img.shields.io/badge/go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)

An MCP server that gives LLMs persistent, searchable memory, backed by
Postgres/pgvector and a local Ollama model for embeddings. It ships as a
single static Go binary — no Python runtime, no Node, no separate vector
DB service beyond Postgres. Everything that touches a model (embeddings
for search/save, summarization for compaction) calls a local Ollama
server; nothing here calls an external LLM API unless you opt into one
(see [Using a different embedding backend](#using-a-different-embedding-backend)).
Memories don't just accumulate — `compact_memories` finds near-duplicate
or stale entries and merges them, on demand. There's also a terminal UI
(`memory-vault-tui`) for browsing, searching, editing, and deleting
memories directly, without going through an MCP client. Talks the MCP
Streamable HTTP transport (`POST /mcp`) and exposes both `tools/*` and
`resources/*`.

## Why this instead of X

There are more mature, more featureful memory layers out there — this is
a smaller, simpler tool that trades breadth for being self-contained and
easy to run yourself. Fair comparison, not a sales pitch:

| | memory-vault | [mem0](https://github.com/mem0ai/mem0) | [Zep](https://www.getzep.com/) |
|---|---|---|---|
| Runtime | Single static Go binary | Python/TS library or self-hosted server | Managed platform (Python service); self-hosted Community Edition retired in 2025 |
| Fully local, no external API key required | Yes — Ollama only | Yes, if self-hosted against a local Ollama + local vector store | No — cloud/managed by default; full self-host no longer offered |
| Vector backend | Postgres/pgvector only | Many: Qdrant, Chroma, Weaviate, Milvus, pgvector, Redis, and more | Proprietary temporal graph engine (Graphiti, open source on its own) |
| Memory model | Flat text memories, namespaced by space | LLM-extracted facts/entities with automatic dedup on write | Temporal knowledge graph (tracks how facts change over time) |
| Prunes/compacts memories | Yes — explicit `compact_memories` tool, on demand | Partial — fact-level dedup happens automatically as memories are written | Not really — the graph grows and versions facts over time instead of merging them |
| Terminal UI | Yes | No | No |
| License | MIT | Apache-2.0 | Apache-2.0 (Graphiti only; Zep itself is commercial) |

If you need multiple vector backends, framework integrations (LangChain,
CrewAI, etc.), or entity/relationship-level temporal reasoning, mem0 or
Zep/Graphiti are more capable choices. memory-vault is for when you want
something small enough to read in one sitting, running entirely on
hardware you control.

## Tradeoffs / not a fit if...

- **Single-node Postgres.** There's no built-in replication, sharding,
  or HA — if you need that, you're running your own Postgres cluster in
  front of this.
- **No ACLs or RBAC within a tenant.** API keys identify a *tenant*, and
  Postgres row-level security keeps tenants from seeing each other — but
  there's no per-user identity or permissions inside one. Spaces namespace
  memories; any valid key for a tenant can read/write any of that tenant's
  spaces.
- **No entity or relationship modeling.** Memories are chunked text with
  embeddings, not a knowledge graph — there's no notion of "this fact
  superseded that one" beyond what `compact_memories` merges.
- **Compaction is manual.** `compact_memories` doesn't run on a
  schedule; you call it (or wire it into your own cron/workflow).
- **Small, single-maintainer project.** It hasn't been run at the scale
  or under the scrutiny mem0/Zep have — read the code before trusting it
  with anything sensitive.

## Quickstart

```
docker compose up
```

That builds `memory-vault` and brings up Postgres/pgvector and Ollama
alongside it, pulling the embedding and chat models on first run (a few
minutes — Ollama has no models pre-pulled). Once it's up, point an MCP
client at `http://localhost:8080/mcp` (see
[Connecting an MCP client](#connecting-an-mcp-client) below). Copy
`.env.example` to `.env` first if you want to set `AUTH_TOKEN`,
`ALLOWED_HOSTS`, or different Ollama models — otherwise the defaults
(no auth, `all-minilm` / `llama3.1:8b`) apply.

## Tools

| Tool | Description |
|---|---|
| `save_memory` | Create or overwrite a memory by name. Chunks and embeds the content for semantic search. Optional `source` records who wrote it; optional `expect_source` rejects the write instead of overwriting if it collides with a different source. Optional `kind` (default `note`) is one of `fact`, `decision`, `preference`, `task`, `note`. |
| `get_memory` | Fetch a memory's content by exact name. |
| `list_memories` | List stored memory names. Optional `source`/`kind` filters. |
| `search_memories` | Hybrid (semantic + keyword + recency + optional per-kind boost) search, top `limit` matches (default 5, max 20). Optional `source`/`kind` filters. |
| `delete_memory` | Delete a memory by name. |
| `flag_memory` | Set a usage/quality flag (`useful`, `stale`, or `wrong`) on a memory, with an optional `note`. Overwrites any previous flag — one current flag per memory, not a history. Influences `compact_memories` candidate selection. |
| `compact_memories` | Merge/summarize near-duplicate or stale memories via the local Ollama chat model. Never selects `decision`-kind memories. |
| `get_session_summary` | Read-only resume of a space's recent state (what's current, decided, open, likely next) via the local Ollama chat model. Use this to pick up broad context after a session/context reset; use `search_memories` to find one specific fact instead. Optional `limit` (default 15, max 50). |
| `export_memories` | Export memories as JSON (no embeddings). Optional `space`/`source`; omit both to export everything. |
| `import_memories` | Import memories from JSON in the shape `export_memories` produces. Re-chunks/re-embeds through the normal save path. Optional `space`/`source` overrides. |

All tools accept an optional `space` argument (default `"default"`) to
namespace memories — the same `name` can exist independently in different
spaces. `list_memories` without a `space` lists everything grouped by
space; with one, it lists just that space's memory names.

## Multiple agents writing to the same vault

Every memory also carries a `source` — a free-form string naming whichever
agent wrote it (e.g. `"claude-code"`, `"copilot"`, `"n8n"`), defaulting to
`"unspecified"` if you don't pass one. It's **provenance and
collision-avoidance, not an access boundary**: anyone holding a valid
credential for a tenant can still read or write any source's memories in
any of that tenant's spaces. What it does buy you is protection against silent
same-name clobbering — pass `expect_source` on `save_memory` and the write
is rejected (naming the existing source) instead of silently overwriting
a memory a different agent wrote under that name. Omit `expect_source` and
`save_memory` overwrites unconditionally, exactly as before this field
existed. If you want real isolation between agents, use separate `space`s
instead — spaces are the actual boundary here.

## Kinds

Every memory also carries a `kind`, one of:

| Kind | For |
|---|---|
| `fact` | Something true and largely static (a config value, an account detail) |
| `decision` | Something explicitly decided — an architecture choice, a "we're doing X not Y" |
| `preference` | How the user/agent wants things done, independent of any one task |
| `task` | Something in progress or still to do |
| `note` | Everything else — the default |

It defaults to `note` if omitted, and is validated at the tool boundary —
an unrecognized value is rejected with the list of valid ones, rather than
failing deep in a database error. `search_memories` can optionally boost
matches of a given kind above others at equal similarity via
`SEARCH_KIND_BOOST_<KIND>` env vars (all default to `0`, so ranking is
unchanged unless you opt in). See [Compaction](#compaction) for how `kind`
affects `compact_memories`.

## Compaction

`compact_memories` keeps the vault from growing unbounded with stale or
overlapping notes. Within a space (or all spaces, if `space` is omitted),
it groups memories whose embeddings are near-duplicates (cosine distance
under `COMPACT_SIMILARITY_THRESHOLD`) or that haven't been touched in
`COMPACT_STALE_DAYS`, sends each group's reassembled content to a local
Ollama **chat** model (`OLLAMA_CHAT_MODEL`, separate from the embedding
model used for search/save), and asks it to produce one consolidated
memory that preserves every distinct fact and drops redundancy. The
result is re-chunked, re-embedded, and saved (under the original name for
a solo re-summarization, or `<first-name>-merged` when multiple sources
are combined); the memories it replaced are deleted.

Call it with `dry_run: true` (the default) to see the proposed plan
without writing anything, or `dry_run: false` to actually merge. It's
manual/on-demand — there's no background cron. If you want it to run
automatically, wire a periodic call to the tool into a cron job or an
n8n/similar workflow.

**Memories of kind `decision` are never compaction candidates** — they're
excluded before grouping, so a decision can neither be merged into another
memory nor picked up by staleness pruning, no matter how old or similar to
something else it is. If you want a decision reconsidered or superseded,
do that explicitly (`save_memory` a new one, `delete_memory` the old one)
rather than relying on compaction to do it for you.

**`flag_memory` shifts candidate selection beyond age/similarity alone.**
A memory flagged `stale` or `wrong` becomes a compaction candidate
regardless of how recently it was updated (`compact_memories`'s dry-run
plan says why: "flagged stale"/"flagged wrong" instead of just "stale"). A
memory flagged `useful` is protected from age-based selection — being old
alone won't pull it in — but it can still be grouped into a merge if it's a
genuine near-duplicate by embedding similarity; `useful` guards against
"nobody's touched this in 90 days" pruning, not against real dedup. A
flag survives a later `save_memory` overwrite of that same memory (editing
content doesn't retroactively undo a quality judgment) — only another
`flag_memory` call changes it.

## Resuming a session

`get_session_summary` is a different tool from `search_memories`, for a
different job: **resuming broad context, not finding one fact.** Pull the
most-recently-updated memories in a space (favoring `task`/`decision` kind,
since those represent state rather than static facts), send them to the
local Ollama chat model, and ask for a short resume — current state, what
was decided, what's still open, likely next step. It's read-only; it never
writes anything. Use `search_memories` when you know roughly what you're
looking for ("what did we decide about auth timeouts"); use
`get_session_summary` when you're picking a space back up after a context
reset and want the gist before diving in. If the stored memories don't
clearly imply a next step, the prompt instructs the model to say so rather
than invent one.

## Export / import

`export_memories` returns a JSON array of `{name, space, source, kind,
content, updated_at}` — no embeddings, since they're cheap to regenerate on import
and including them would tie the export to whatever embedding
model/dimension was active when it was taken. `import_memories` takes
that same JSON (as its `data` argument) and re-chunks/re-embeds every
memory through the normal save path, so it's applied consistently with
whatever `Embedder` is currently configured. By default it skips
`(space, name)` pairs that already exist and reports which ones it
skipped; pass `overwrite: true` to replace them instead. An optional
`space` argument sends every imported memory there regardless of what's
recorded in the data — useful for merging a backup into a differently
named space; `source` works the same way (an export taken before the
`source` field existed imports as `"unspecified"` unless overridden).

Both are also available as CLI subcommands on the `memory-vault` binary
itself, for scripting a backup without going through an MCP client:

```
memory-vault export --space default > backup.json
memory-vault import --file backup.json
# or: cat backup.json | memory-vault import
```

**This is a portability tool, not a backup tool.** Both operate on one tenant
at a time, because they go through the same RLS-scoped storage layer as
everything else — they default to the bootstrap tenant, and once `tenant
create` has been used, `-tenant <id>` is required rather than assumed. Their
value is that content is re-chunked and re-embedded on import, so an export
survives a change of embedding model or dimension. For disaster recovery use
`deploy/backup.sh`, which dumps every tenant; see
[Backups cover every tenant](#backups-cover-every-tenant).

`export`/`import` need the same `DATABASE_URL` (and `OLLAMA_URL` or
`EMBED_PROVIDER=openai` config, for `import`'s re-embedding) as the
server itself. `import --space other-space --source other-source
--overwrite` mirrors the tool's `space`/`source`/`overwrite` arguments;
`export --source` filters the same way.

## Browsing memories

`memory-vault-tui` is a standalone terminal browser that talks to Postgres
directly (via `internal/store`, the same code the MCP server uses) — no
MCP client or LLM chat loop needed to look through, edit, or clean up
memories.

```
go build -o memory-vault-tui ./cmd/memory-vault-tui
DATABASE_URL="postgres://memory_vault_app:pass@localhost:5432/memory_vault?sslmode=disable" \
OLLAMA_URL="http://localhost:11434" \
./memory-vault-tui
```

(or `go run ./cmd/memory-vault-tui` without building a binary first.)

`DATABASE_URL` must name `memory_vault_app`, not the superuser: the TUI
opens the store through the same `internal/store` code the server does, so
it refuses to start against a superuser or `BYPASSRLS` role, for the reason
in "Database roles and tenant isolation" below. It has no API key and no
tenant flag — it always connects as the bootstrap tenant, and RLS confines
it to that tenant's memories. It is a local admin tool for the vault the
server was started on, not a way to browse other tenants.

| Key | Action |
|---|---|
| `↑`/`k`, `↓`/`j` | Move selection |
| `/` | Semantic search within the current space |
| `e` | Edit the selected memory in `$EDITOR` (falls back to `nvim`); saves and re-embeds on exit |
| `n` | Create a new memory: prompts for name, space, source (default `unspecified`), and kind (default `note`, re-prompts on an invalid value), then opens `$EDITOR` for content |
| `d` | Delete the selected memory (confirm with `y`) |
| `s` | Show a `get_session_summary` resume for the current space in the content pane |
| `esc` | Back out of search results / a prompt |
| `q` | Quit |

It's a separate binary meant to run on the host or server with an
interactive terminal — it is not built into the Docker image the MCP
server ships in.

## Resources

Every stored memory is also browsable as an MCP resource
(`resources/list`, `resources/read`), addressed by URI
`memory://<space>/<name>`, alongside the `tools/call` interface above.

## Requirements

- Postgres with the `vector` extension available (e.g. `pgvector/pgvector:pg16`)
- Ollama running with `all-minilm` pulled (`ollama pull all-minilm`), reachable from wherever this server runs
- Go 1.26+ to build
- **An unprivileged Postgres role for the server to connect as** — see [Database roles and tenant isolation](#database-roles-and-tenant-isolation)

## Database roles and tenant isolation

Memories are isolated per tenant by Postgres row-level security, not by
application code remembering to filter correctly. Every table storing
per-tenant data carries a `tenant_id`, and a policy on `memories` filters
every statement on `current_setting('app.tenant_id')`, which the storage
layer binds to a transaction before any query runs.

**This only works if the server connects as a role that cannot bypass RLS.**
Superusers and `BYPASSRLS` roles ignore policies entirely — including
`FORCE ROW LEVEL SECURITY` — which would leave isolation fully configured
and enforcing nothing. Because `POSTGRES_USER` in the standard Postgres
image creates a *superuser*, the obvious setup lands in exactly that state,
so the server refuses to start rather than serve a boundary that isn't there.

`deploy/init-db.sql` provisions the extension and the unprivileged role;
`docker-compose.yml` applies it automatically when the data directory is
first created. Applying it to an existing database by hand:

```bash
psql "$SUPERUSER_DATABASE_URL" -f deploy/init-db.sql
```

then point `DATABASE_URL` at `memory_vault_app` rather than the superuser.
The script is idempotent, so re-running it is safe. On an existing vault it
also transfers ownership of the already-created tables to the app role —
only an owner may `ALTER` a table, so without that the server would connect
fine and then fail its migration on "must be owner of table memories".
Change the role's password from the placeholder if Postgres is reachable
beyond the container network.

Rows written before multi-tenancy are adopted by a bootstrap tenant
(`00000000-0000-0000-0000-000000000001`) during migration, so an existing
single-tenant vault keeps working with no configuration change.

## Tenants and API keys

A request's bearer token decides which tenant's memories it can reach. There
are two kinds of credential, and they're checked in this order:

1. **`AUTH_TOKEN`** — the shared static token, which authenticates as the
   bootstrap tenant. This is the self-hosted single-tenant path and behaves
   exactly as it did before multi-tenancy.
2. **An API key** (`mv_…`) — authenticates as the tenant it was minted for.

Tenants and keys are managed from the CLI. There is no admin HTTP endpoint,
so there is no admin endpoint to secure:

```bash
# Create a tenant and mint its first key (printed once — only the hash is stored)
memory-vault tenant create -email someone@example.com -plan builder

memory-vault tenant list                            # find a tenant id
memory-vault key create -tenant <id> -label laptop  # mint another key
memory-vault key list   -tenant <id>                # id, label, created, active/revoked
memory-vault key revoke -id <key-id>                # withdraw one, by key id
```

Keys are stored as a SHA-256 digest, never in plaintext — a lost key is
reissued, not recovered, and a database dump contains no usable credentials.
Revocation takes effect on the next request; no restart.

Under Docker, prefix with `docker compose exec memory-vault`.

**Anonymous access closes automatically, and stays closed.** With no
`AUTH_TOKEN` set and no API key ever minted, `/mcp` serves the bootstrap
tenant unauthenticated, so a fresh local vault needs no configuration. In that
open state a request carrying an unrecognized token is also accepted — on a
vault that serves anyone, rejecting a caller *because* they presented a stale
credential would be stricter than rejecting nothing at all.

Setting `AUTH_TOKEN`, or minting the first API key, turns that off
immediately. Issuing a key is a **one-way door**: revoking it later does not
re-open the vault, so responding to a leaked key by revoking it can't
accidentally leave `/mcp` open to everyone.

## Plans, rate limits, and quotas

Each tenant's `plan` (`free`, `builder`, or `team`) decides what it may
consume. Limits are enforced in two places: request rate at the HTTP handler,
storage quota inside the write transaction.

| Plan | Requests/min | Burst | Memories | Storage |
|---|---|---|---|---|
| `free` | 60 | 30 | 200 | 10 MB |
| `builder` | 600 | 120 | 5,000 | 500 MB |
| `team` | 3,000 | 600 | 50,000 | 5 GB |

**The bootstrap tenant is exempt.** A self-hosted vault — everything reached
via `AUTH_TOKEN`, or anonymously on a vault that has never minted a key — runs
with no limits at all, so this feature changes nothing for an existing
single-tenant deployment.

Override any value with `PLAN_<PLAN>_RPM`, `_BURST`, `_MAX_MEMORIES`, or
`_MAX_MB` (e.g. `PLAN_FREE_MAX_MEMORIES=500`). Setting one to `0` removes that
limit for the plan. An unrecognised plan name falls back to `free`'s limits
rather than to no limits, so a typo cannot hand out unlimited storage.

Exceeding the request rate returns HTTP `429` with a `Retry-After` header.
Exceeding a storage quota fails the individual `save_memory`/`import_memories`
call with a message naming the limit and what to do about it — reads are never
blocked, so a tenant at its cap can still search and delete its way back under.

Quotas count the whole tenant, across every space: spaces are a namespace, not
a billing boundary.

> Rate limit state is held in memory, so it is per instance and resets on
> restart. That is exact for the intended deployment, which runs one active
> instance (failover points DNS at a single host rather than load-balancing).
> Running several instances behind a load balancer would let each one grant
> the full rate independently, and would need shared counters instead.

## Configuration

Environment variables:

| Var | Default | Description |
|---|---|---|
| `DATABASE_URL` | *(required)* | Postgres connection string, e.g. `postgres://user:pass@host:5432/dbname?sslmode=disable`. Must name a role that is **not** a superuser and lacks `BYPASSRLS` — see [Database roles and tenant isolation](#database-roles-and-tenant-isolation) |
| `OLLAMA_URL` | `http://localhost:11434` | Base URL of the Ollama server |
| `OLLAMA_EMBED_MODEL` | `all-minilm` | Ollama embedding model name |
| `OLLAMA_CHAT_MODEL` | `llama3.1:8b` | Ollama chat model used only by `compact_memories` |
| `EMBED_PROVIDER` | `ollama` | Embedding backend: `ollama` (default) or `openai` — see [Using a different embedding backend](#using-a-different-embedding-backend) |
| `EMBED_DIM` | `384` | Dimension of the configured embedder's vectors; baked into the `embedding` column at table-creation time |
| `OPENAI_EMBED_BASE_URL` | `https://api.openai.com/v1` | Base URL for an OpenAI-compatible embeddings endpoint; only used when `EMBED_PROVIDER=openai` |
| `OPENAI_EMBED_API_KEY` | *(none)* | API key for the OpenAI-compatible endpoint; only used when `EMBED_PROVIDER=openai` |
| `OPENAI_EMBED_MODEL` | `text-embedding-3-small` | Model name for the OpenAI-compatible endpoint; only used when `EMBED_PROVIDER=openai` |
| `PORT` | `8080` | HTTP listen port |
| `MAX_REQUEST_BODY_MB` | `25` | Max `/mcp` request body size, in MB — guards against unbounded-memory requests (e.g. an oversized `import_memories` payload) |
| `AUTH_TOKEN` | *(none)* | Shared bearer token(s) authenticating as the bootstrap tenant (comma-separated for multiple clients). If unset, `/mcp` is open only while no API key exists — see [Tenants and API keys](#tenants-and-api-keys) |
| `ALLOWED_HOSTS` | *(none)* | Comma-separated `Host` header allowlist, guards against DNS-rebinding. If unset, the check is skipped — set this in production. |
| `PLAN_<PLAN>_RPM` | per plan | Sustained requests/minute for `FREE`/`BUILDER`/`TEAM`; `0` disables the limit — see [Plans, rate limits, and quotas](#plans-rate-limits-and-quotas) |
| `PLAN_<PLAN>_BURST` | per plan | How many requests an idle tenant may make at once (capped at that plan's `RPM`) |
| `PLAN_<PLAN>_MAX_MEMORIES` | per plan | Maximum memories a tenant on that plan may store; `0` disables the limit |
| `PLAN_<PLAN>_MAX_MB` | per plan | Maximum total stored content in MB for that plan; `0` disables the limit |
| `DB_MAX_OPEN_CONNS` | `10` | Max open Postgres connections |
| `DB_MAX_IDLE_CONNS` | `5` | Max idle Postgres connections |
| `DB_CONN_MAX_LIFETIME_MIN` | `30` | Max connection lifetime, in minutes |
| `SEARCH_WEIGHT_SEMANTIC` | `1.0` | Weight of pgvector cosine similarity in `search_memories` ranking |
| `SEARCH_WEIGHT_KEYWORD` | `0.0` | Weight of full-text (`ts_rank`) similarity |
| `SEARCH_WEIGHT_RECENCY` | `0.0` | Weight of recency (exponential decay by `updated_at`) |
| `SEARCH_RECENCY_HALFLIFE_DAYS` | `30` | Half-life, in days, for the recency decay factor |
| `SEARCH_KIND_BOOST_FACT`, `_DECISION`, `_PREFERENCE`, `_TASK`, `_NOTE` | `0` each | Flat score boost added to matches of that kind in `search_memories` |
| `COMPACT_SIMILARITY_THRESHOLD` | `0.15` | Cosine-distance threshold under which two memories' embeddings are treated as near-duplicates by `compact_memories` |
| `COMPACT_STALE_DAYS` | `90` | Age (in days since `updated_at`) past which a lone memory is still a `compact_memories` candidate for solo re-summarization |

## Using a different embedding backend

The local-first default (Ollama, `all-minilm`, 384-dim) is intentional —
it's the whole point of this project — not a placeholder waiting for you
to switch to a hosted API. But if you'd rather point at an
OpenAI-compatible embeddings endpoint (OpenAI itself, or a self-hosted
OpenAI-compatible server like LiteLLM or text-embeddings-inference), set:

```
EMBED_PROVIDER=openai
OPENAI_EMBED_BASE_URL=https://api.openai.com/v1
OPENAI_EMBED_API_KEY=sk-...
OPENAI_EMBED_MODEL=text-embedding-3-small
EMBED_DIM=1536
```

`EMBED_DIM` must match whatever the configured model actually returns —
it's checked on every embed call, and gets baked into the Postgres
`vector` column when the table is first created. Switching `EMBED_DIM`
or embedding models on an existing database won't retroactively
re-embed anything or resize the column; start from a fresh database (or
`export_memories`/`import_memories` — see below — into a new one) if you
change backends after memories already exist.

## Run locally

```
go build -o memory-vault .
DATABASE_URL="postgres://user:pass@localhost:5432/memory_vault?sslmode=disable" \
OLLAMA_URL="http://localhost:11434" \
./memory-vault
```

## Run with Docker

```
docker build -t memory-vault .
docker run -p 8080:8080 \
  -e DATABASE_URL="postgres://user:pass@host:5432/memory_vault?sslmode=disable" \
  -e OLLAMA_URL="http://host.docker.internal:11434" \
  memory-vault
```

If Ollama runs on the Docker host rather than in a container, point
`OLLAMA_URL` at `host.docker.internal` and add `--add-host
host.docker.internal:host-gateway` on Linux (Docker Desktop on
mac/Windows resolves it automatically).

## Connecting an MCP client

Point any MCP client that supports the Streamable HTTP transport at
`http://<host>:8080/mcp`. For clients that only support local stdio
servers (e.g. Claude Desktop), bridge through
[`mcp-remote`](https://www.npmjs.com/package/mcp-remote):

```json
{
  "mcpServers": {
    "memory-vault": {
      "command": "npx",
      "args": ["-y", "mcp-remote", "http://<host>:8080/mcp", "--allow-http"]
    }
  }
}
```

(`--allow-http` is only needed for a plain-HTTP, non-localhost URL.)

If `AUTH_TOKEN` is set on the server, add a `--header "Authorization: Bearer
<token>"` arg to `mcp-remote` (or the equivalent header config for clients
that talk Streamable HTTP directly).

## CI/CD

On every push to `master`, GitHub Actions (`.github/workflows/ci.yml`) runs
`go vet` + `go test` + a build, and if that passes, builds and pushes a
Docker image to `ghcr.io/avishek-7/memory-vault` tagged `latest` and with
the commit SHA. It does not deploy — pull and restart on the server
yourself when ready:

```
docker pull ghcr.io/avishek-7/memory-vault:latest
cd /home/avishek/Docker && docker compose up -d memory-vault
```

(`develop` is where in-progress work lands; merge to `master` once stable
to trigger the pipeline.)

## Chunking

`all-minilm` has a 256-token context window. `save_memory` automatically
splits content longer than ~100 words into overlapping chunks (100-word
target, 10-word overlap), embeds each chunk separately, and stores them
under the same memory name. `get_memory` and `search_memories` transparently
reassemble the full content from its chunks, so long memories no longer
fail to save. The word-count target is only an estimate — real token
density varies with content (markdown/code/paths tokenize denser than
plain prose) — so if the embedder still rejects a chunk as too long,
`save_memory` adaptively splits that chunk further and retries, rather
than trusting the estimate to always hold.

## Fallback / failover

If the host running memory-vault goes down, clients (Claude Desktop,
Claude Code, Copilot, n8n, or anything else pointed at the same MCP URL)
can be made to fail over to a standby instance automatically, without
touching every client's config. The pieces, in order:

1. **Backup** (`deploy/backup.sh`, on the primary): periodically dumps the
   whole database with `pg_dump`, encrypts it with
   [age](https://github.com/FiloSottile/age), and pushes it to a private
   git repo used only for backups.
2. **Standby sync** (`deploy/standby-sync.sh`, on the standby): a second,
   always-on memory-vault instance periodically pulls the latest backup and
   restores it with `psql`, so it's never far behind the primary. The
   restore is a *mirror*, not a merge: the dump is taken with `--clean
   --if-exists` and replaces whatever the standby held.
3. **Health-check-based DNS failover** (`deploy/failover-watch.sh`, on the
   standby): polls the primary's `GET /healthz` (a cheap Postgres ping —
   see below). After several consecutive failures it flips the Cloudflare
   DNS record clients use to point at the standby's IP; once the primary
   is healthy again for several consecutive checks, it flips back.

**Be clear-eyed about what this is and isn't.** This is DNS-based
failover, not instant failover: it's bounded by the DNS record's TTL and
whatever clients/resolvers cache on top of that, so expect the switch to
take effect on the order of the TTL, not immediately. The standby's data
can also lag the primary by `standby-sync.sh`'s sync interval (15 minutes
by default) — a write made against the primary in the minutes before it
went down may not have reached the standby yet. And **there is no
automatic bidirectional sync**: while DNS points at the standby, writes
land only there; when the primary comes back, its data does not
automatically merge with whatever the standby accumulated during the
outage. Reconciling the two sides is a manual step — decide which
instance has the data you want to keep (usually the standby, since it was
the one taking live writes), run `backup.sh` against it, and import that
into the other side — not something this tooling does for you. This is a
fallback for read/write continuity during an outage, not a true
multi-primary setup.

### Backups cover every tenant

`backup.sh` uses `pg_dump` rather than `memory-vault export`, because export
goes through the RLS-scoped storage layer and therefore covers exactly **one**
tenant. On a multi-tenant vault that would silently back up a fraction of the
data. The dump also preserves what a JSON export deliberately drops:
embeddings, API keys, tenant rows, table ownership, and the RLS policy itself.

`BACKUP_DATABASE_URL` must name a **superuser or `BYPASSRLS`** role, and
*cannot* be the app's own `DATABASE_URL`. `memories` has `FORCE ROW LEVEL
SECURITY`, which binds the table owner too, so `pg_dump` connecting as the app
role fails outright with *"query would be affected by row-level security
policy"*. Either use the database superuser, or create a least-privilege
reader:

```sql
CREATE ROLE memory_vault_backup LOGIN BYPASSRLS PASSWORD '...';
GRANT USAGE ON SCHEMA public TO memory_vault_backup;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO memory_vault_backup;
```

`backup.sh` checks this up front and refuses to run otherwise, rather than
producing a backup that is quietly empty.

The standby must already have the `memory_vault_app` role (`deploy/init-db.sql`)
before its first restore: the dump reassigns table ownership to that role by
name, and without it the tables stay owned by the restoring superuser — after
which the app fails its own migration on *"must be owner of table memories"*.

### `GET /healthz`

A plain HTTP endpoint (not an MCP tool), used by `failover-watch.sh` to
decide whether to fail over. No `AUTH_TOKEN` required — health checkers
won't carry a bearer token — but it is still subject to `ALLOWED_HOSTS`
like every other endpoint. It pings Postgres only (`SELECT 1`), not
Ollama: whether this instance can serve MCP requests at all is a
different, more urgent question than whether embedding/`compact_memories`
calls currently work. Returns `{"status":"ok","db":"ok"}` with HTTP 200 on
success, or a 503 with `{"status":"error","db":"unreachable"}` on
failure.

### Prerequisites

- A standby host running memory-vault + Postgres/pgvector — `docker-compose.yml`
  is one way to stand this up; it just needs to be a separate host from
  the primary, reachable from wherever `failover-watch.sh` runs.
- A **private** git repo (not this one) to hold encrypted backups.
- An [age](https://github.com/FiloSottile/age) key pair: the public key
  (`AGE_RECIPIENT`) goes on the primary for `backup.sh` to encrypt to; the
  private key (`AGE_IDENTITY`) goes on the standby for `standby-sync.sh`
  to decrypt with, and nowhere else.
- A Cloudflare API token scoped to **DNS edit only** on the relevant
  zone/record — deliberately separate from any existing token used for
  DNS-01 Let's Encrypt certificate issuance, so a compromised failover
  token can't touch certificates and vice versa.
- A **low TTL** on the DNS record `failover-watch.sh` flips, so a flip
  actually takes effect promptly once triggered — a high TTL makes the
  whole mechanism much slower than the health-check thresholds suggest.

### Setup

1. On the primary: install `age`, set up `/etc/memory-vault-backup.env`
   (see `deploy/memory-vault-backup.service`'s header comment for the
   required variables), then `systemctl enable --now
   memory-vault-backup.timer`.
2. On the standby: install `age`, place the age private key somewhere
   only this service can read, set up
   `/etc/memory-vault-standby-sync.env`, then `systemctl enable --now
   memory-vault-standby-sync.timer`.
3. Also on the standby: set up `/etc/memory-vault-failover.env` with the
   Cloudflare zone/record/token and the primary/standby IPs, then
   `systemctl enable --now memory-vault-failover.service` (this one is a
   long-running service, not a timer — see its unit file's comment for
   why).

### Configuration (failover tooling)

These are read by the `deploy/*.sh` scripts, not the `memory-vault`
server binary itself:

| Var | Default | Description |
|---|---|---|
| `AGE_RECIPIENT` | *(required)* | One or more age public keys (space-separated) `backup.sh` encrypts backups to |
| `AGE_IDENTITY` | *(required)* | Path to the age private key file `standby-sync.sh` decrypts backups with |
| `BACKUP_GIT_REMOTE` | *(required)* | Private git repo backups are pushed to / pulled from (never this repo) |
| `BACKUP_GIT_DIR` | `/var/lib/memory-vault-backup/repo` (backup.sh) / `/var/lib/memory-vault-backup/standby-repo` (standby-sync.sh) | Local clone of `BACKUP_GIT_REMOTE` |
| `BACKUP_GIT_BRANCH` | `main` | Branch backups are committed to / pulled from |
| `BACKUP_FILE_NAME` | `memory-vault-dump.sql.age` | Encrypted dump's file name inside the backup repo |
| `BACKUP_DATABASE_URL` | *(required by `backup.sh`)* | Postgres URL for `pg_dump`. Must be a **superuser or `BYPASSRLS`** role — see [Backups cover every tenant](#backups-cover-every-tenant) |
| `RESTORE_DATABASE_URL` | *(required by `standby-sync.sh`)* | The standby's own Postgres, as a **superuser**: the restore recreates the schema, reassigns ownership, and reinstalls the RLS policy |
| `STANDBY_SYNC_LOG` | `/var/log/memory-vault-standby-sync.log` | `standby-sync.sh`'s outcome log (synced/skipped/failed, with counts) |
| `STANDBY_STATE_DIR` | `/var/lib/memory-vault-backup/standby-state` | Where `standby-sync.sh` tracks the last-synced backup commit |
| `HEALTH_URL` | *(required)* | The primary's `/healthz` URL, polled by `failover-watch.sh` |
| `STANDBY_IP` | *(required)* | This host's IP, DNS is pointed here on failover |
| `PRIMARY_IP` | *(required)* | The primary's IP, DNS is pointed back here on recovery |
| `CF_ZONE_ID` | *(required)* | Cloudflare zone ID containing the DNS record to flip |
| `CF_RECORD_ID` | *(required)* | Cloudflare DNS record ID to flip |
| `CF_API_TOKEN` | *(required)* | Cloudflare API token, scoped to DNS edit only — not the DNS-01 cert token |
| `FAILOVER_THRESHOLD` | `3` | Consecutive health check failures before failing over |
| `RECOVERY_THRESHOLD` | `3` | Consecutive health check successes before failing back |
| `CHECK_INTERVAL_SECONDS` | `30` | Seconds between health checks in `failover-watch.sh`'s loop mode |
| `FAILOVER_COOLDOWN_MIN` | `10` | Minutes to wait after any DNS flip before flipping again |
| `NOTIFY_WEBHOOK_URL` | *(none)* | If set, POSTs a small JSON payload here on every failover/recovery transition |
| `CF_API_BASE_URL` | `https://api.cloudflare.com/client/v4` | Cloudflare API base URL (override for testing against a mock) |
| `FAILOVER_STATE_DIR` | `/var/lib/memory-vault-failover` | Where `failover-watch.sh` persists its consecutive-check counters and cooldown timer |
| `FAILOVER_LOG` | `/var/log/memory-vault-failover.log` | `failover-watch.sh`'s transition log |

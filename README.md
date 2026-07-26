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
- **No multi-tenant auth.** `AUTH_TOKEN` is a shared bearer token (or a
  comma-separated list of them) checked with a constant-time compare —
  there's no per-user identity, ACLs, or RBAC. Spaces namespace
  memories, but anyone with a valid token can read/write any space.
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
| `compact_memories` | Merge/summarize near-duplicate or stale memories via the local Ollama chat model. Never selects `decision`-kind memories. |
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
collision-avoidance, not an access boundary**: anyone with a valid
`AUTH_TOKEN` can still read or write any source's memories in any space
they can reach. What it does buy you is protection against silent
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
DATABASE_URL="postgres://user:pass@localhost:5432/memory_vault?sslmode=disable" \
OLLAMA_URL="http://localhost:11434" \
./memory-vault-tui
```

(or `go run ./cmd/memory-vault-tui` without building a binary first.)

| Key | Action |
|---|---|
| `↑`/`k`, `↓`/`j` | Move selection |
| `/` | Semantic search within the current space |
| `e` | Edit the selected memory in `$EDITOR` (falls back to `nvim`); saves and re-embeds on exit |
| `n` | Create a new memory: prompts for name, space, source (default `unspecified`), and kind (default `note`, re-prompts on an invalid value), then opens `$EDITOR` for content |
| `d` | Delete the selected memory (confirm with `y`) |
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

## Configuration

Environment variables:

| Var | Default | Description |
|---|---|---|
| `DATABASE_URL` | *(required)* | Postgres connection string, e.g. `postgres://user:pass@host:5432/dbname?sslmode=disable` |
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
| `AUTH_TOKEN` | *(none)* | Bearer token(s) required on `/mcp` (comma-separated for multiple clients). If unset, auth is disabled — set this in production. |
| `ALLOWED_HOSTS` | *(none)* | Comma-separated `Host` header allowlist, guards against DNS-rebinding. If unset, the check is skipped — set this in production. |
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
splits content longer than ~150 words into overlapping chunks (150-word
target, 15-word overlap), embeds each chunk separately, and stores them
under the same memory name. `get_memory` and `search_memories` transparently
reassemble the full content from its chunks, so long memories no longer
fail to save.

# Changelog

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

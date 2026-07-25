# Changelog

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

# Changelog

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

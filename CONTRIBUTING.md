# Contributing

## Running tests locally

```
go vet ./...
go test ./...
go build ./...
```

Most tests (`TestChunkContent`, `TestCheckAuth`, `TestCheckHost`,
`TestParseResourceURI`, and the `memory-vault-tui` row/cursor tests) are
pure unit tests and run with no external services.

The `TestIntegration*` tests in `main_test.go` exercise real save/get/
delete/search/resources round trips against Postgres+pgvector and
Ollama. They skip automatically unless both `DATABASE_URL` and
`OLLAMA_URL` are set:

```
DATABASE_URL="postgres://user:pass@localhost:5432/memory_vault?sslmode=disable" \
OLLAMA_URL="http://localhost:11434" \
go test ./... -v
```

(`docker compose up postgres ollama` gives you both without running the
server itself.)

## Code style

This project deliberately stays small and dependency-light. When adding
to it, match what's already here rather than introducing new patterns:

- Plain `net/http` for the JSON-RPC transport — no MCP SDK dependency.
  The `bubbletea`/`bubbles`/`lipgloss` trio used by `cmd/memory-vault-tui`
  is the one case a dependency was worth it (a terminal UI); don't take
  that as precedent for pulling in a framework elsewhere.
- `envOr` / `envOrInt` / `envOrFloat` for all configuration — no config
  files, no flag parsing beyond what's already there.
- `internalErr`: log the real error server-side via `log.Printf`, return
  a generic `"internal error"` message to the client. Never let DB/
  network internals (or secrets, like API keys) leak into a tool result
  or JSON-RPC error message.
- Shared DB/chunking/embedding/search logic lives in `internal/store`,
  used by both `main.go` (the MCP server) and `cmd/memory-vault-tui`.
  New capabilities that both need should go there, not be duplicated.
- New MCP tools follow the existing `tools[]`/`schema()`/`callTool()`
  pattern in `main.go`.
- Every new capability should be additive and off-by-default where it
  changes behavior — existing defaults (Ollama, `all-minilm`, no auth)
  should keep working unmodified.

## Opening a PR

- Keep commits scoped to one logical change each; a clear commit message
  matters more than a small diff.
- Run `go vet`, `go test`, and `go build` before pushing — CI
  (`.github/workflows/ci.yml`) runs the same checks and blocks merge on
  push/PR to `master`.
- Update `README.md` if the change adds or changes user-facing behavior
  (a tool, an env var, a CLI flag).
- PRs target `master`... but for anything more than a one-line fix,
  branch from and target `develop` first (see the CI/CD section of the
  README) — that's where in-progress work lands before it's merged up.

# memory-vault

A minimal MCP server exposing persistent memory tools to LLMs, backed by
Postgres/pgvector for storage and a local Ollama model (`all-minilm`,
384-dim) for embeddings. Talks the MCP Streamable HTTP transport
(`POST /mcp`).

## Tools

| Tool | Description |
|---|---|
| `save_memory` | Create or overwrite a memory by name. Chunks and embeds the content for semantic search. |
| `get_memory` | Fetch a memory's content by exact name. |
| `list_memories` | List stored memory names. |
| `search_memories` | Hybrid (semantic + keyword + recency) search, top `limit` matches (default 5, max 20). |
| `delete_memory` | Delete a memory by name. |

All tools accept an optional `space` argument (default `"default"`) to
namespace memories — the same `name` can exist independently in different
spaces. `list_memories` without a `space` lists everything grouped by
space; with one, it lists just that space's memory names.

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
| `PORT` | `8080` | HTTP listen port |
| `AUTH_TOKEN` | *(none)* | Bearer token(s) required on `/mcp` (comma-separated for multiple clients). If unset, auth is disabled — set this in production. |
| `ALLOWED_HOSTS` | *(none)* | Comma-separated `Host` header allowlist, guards against DNS-rebinding. If unset, the check is skipped — set this in production. |
| `DB_MAX_OPEN_CONNS` | `10` | Max open Postgres connections |
| `DB_MAX_IDLE_CONNS` | `5` | Max idle Postgres connections |
| `DB_CONN_MAX_LIFETIME_MIN` | `30` | Max connection lifetime, in minutes |
| `SEARCH_WEIGHT_SEMANTIC` | `1.0` | Weight of pgvector cosine similarity in `search_memories` ranking |
| `SEARCH_WEIGHT_KEYWORD` | `0.0` | Weight of full-text (`ts_rank`) similarity |
| `SEARCH_WEIGHT_RECENCY` | `0.0` | Weight of recency (exponential decay by `updated_at`) |
| `SEARCH_RECENCY_HALFLIFE_DAYS` | `30` | Half-life, in days, for the recency decay factor |

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

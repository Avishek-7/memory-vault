# memory-vault

A minimal MCP server exposing persistent memory tools to LLMs, backed by
Postgres/pgvector for storage and a local Ollama model (`all-minilm`,
384-dim) for embeddings. Talks the MCP Streamable HTTP transport
(`POST /mcp`).

## Tools

| Tool | Description |
|---|---|
| `save_memory` | Create or overwrite a memory by name. Embeds the content for semantic search. |
| `get_memory` | Fetch a memory's content by exact name. |
| `list_memories` | List all stored memory names. |
| `search_memories` | Semantic search via pgvector cosine distance, top 5 matches. |
| `delete_memory` | Delete a memory by name. |

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

## Known limitation

`all-minilm` has a 256-token context window. Content longer than that
will fail to embed (`save_memory`/`search_memories` return an error from
Ollama) — keep saved memories short, or chunk longer content before
saving.

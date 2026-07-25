// memory-vault: a minimal MCP server exposing persistent memory tools to
// LLMs over the MCP Streamable HTTP transport, backed by Postgres/pgvector
// with embeddings from a local Ollama server (all-minilm / all-MiniLM-L6-v2,
// 384-dim).
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

const embedDim = 384

// chunkTargetWords/chunkOverlapWords approximate all-minilm's 256-token
// budget via word count (~0.75 words/token), leaving headroom.
const chunkTargetWords = 150
const chunkOverlapWords = 15

var db *sql.DB

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envOrInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envOrFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}

func ollamaURL() string   { return envOr("OLLAMA_URL", "http://localhost:11434") }
func ollamaModel() string { return envOr("OLLAMA_EMBED_MODEL", "all-minilm") }

func initDB() error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL is not set")
	}
	var err error
	db, err = sql.Open("postgres", dsn)
	if err != nil {
		return err
	}
	if err := db.Ping(); err != nil {
		return err
	}
	db.SetMaxOpenConns(envOrInt("DB_MAX_OPEN_CONNS", 10))
	db.SetMaxIdleConns(envOrInt("DB_MAX_IDLE_CONNS", 5))
	db.SetConnMaxLifetime(time.Duration(envOrInt("DB_CONN_MAX_LIFETIME_MIN", 30)) * time.Minute)
	_, err = db.Exec(`
		CREATE EXTENSION IF NOT EXISTS vector;
		CREATE TABLE IF NOT EXISTS memories (
			space       TEXT NOT NULL DEFAULT 'default',
			name        TEXT NOT NULL,
			chunk_index INT NOT NULL DEFAULT 0,
			content     TEXT NOT NULL,
			embedding   vector(384) NOT NULL,
			updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (space, name, chunk_index)
		);
		ALTER TABLE memories ADD COLUMN IF NOT EXISTS chunk_index INT NOT NULL DEFAULT 0;
		ALTER TABLE memories ADD COLUMN IF NOT EXISTS space TEXT NOT NULL DEFAULT 'default';
		DO $$
		BEGIN
			IF (SELECT array_length(conkey, 1) FROM pg_constraint
				WHERE conrelid = 'memories'::regclass AND contype = 'p') < 3 THEN
				ALTER TABLE memories DROP CONSTRAINT memories_pkey;
				ALTER TABLE memories ADD PRIMARY KEY (space, name, chunk_index);
			END IF;
		END $$;
		CREATE INDEX IF NOT EXISTS memories_embedding_idx
			ON memories USING hnsw (embedding vector_cosine_ops);
		ALTER TABLE memories ADD COLUMN IF NOT EXISTS content_tsv tsvector
			GENERATED ALWAYS AS (to_tsvector('english', content)) STORED;
		CREATE INDEX IF NOT EXISTS memories_content_tsv_idx ON memories USING GIN (content_tsv);
	`)
	return err
}

// search scoring weights: default to semantic-only ranking (identical to
// pre-hybrid-search behavior) unless overridden.
func searchWeights() (semantic, keyword, recency, halfLifeDays float64) {
	return envOrFloat("SEARCH_WEIGHT_SEMANTIC", 1.0),
		envOrFloat("SEARCH_WEIGHT_KEYWORD", 0.0),
		envOrFloat("SEARCH_WEIGHT_RECENCY", 0.0),
		envOrFloat("SEARCH_RECENCY_HALFLIFE_DAYS", 30.0)
}

const defaultSpace = "default"

func argSpace(args map[string]interface{}) string {
	if s, ok := args["space"].(string); ok && s != "" {
		return s
	}
	return defaultSpace
}

// chunkContent splits content into overlapping word-based chunks that
// safely fit all-minilm's 256-token context window. Reassembly (get_memory)
// simply concatenates chunks back together; the small overlap is kept for
// embedding quality and is not deduplicated on read.
func chunkContent(content string) []string {
	words := strings.Fields(content)
	if len(words) <= chunkTargetWords {
		return []string{content}
	}
	var chunks []string
	start := 0
	for start < len(words) {
		end := start + chunkTargetWords
		if end > len(words) {
			end = len(words)
		}
		chunks = append(chunks, strings.Join(words[start:end], " "))
		if end == len(words) {
			break
		}
		start = end - chunkOverlapWords
	}
	return chunks
}

// embed calls a local Ollama server to turn text into a 384-dim vector.
func embed(text string) ([]float32, error) {
	reqBody, _ := json.Marshal(map[string]string{
		"model":  ollamaModel(),
		"prompt": text,
	})
	resp, err := http.Post(ollamaURL()+"/api/embeddings", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("ollama request failed (is Ollama running with %q pulled?): %w", ollamaModel(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama returned status %d", resp.StatusCode)
	}
	var out struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Embedding) != embedDim {
		return nil, fmt.Errorf("expected %d-dim embedding, got %d", embedDim, len(out.Embedding))
	}
	return out.Embedding, nil
}

// vectorLiteral formats a float32 slice as pgvector's text input format, e.g. "[0.1,0.2,0.3]".
func vectorLiteral(v []float32) string {
	parts := make([]string, len(v))
	for i, f := range v {
		parts[i] = fmt.Sprintf("%g", f)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// --- JSON-RPC plumbing ---

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func resultMsg(id json.RawMessage, result interface{}) *response {
	return &response{JSONRPC: "2.0", ID: id, Result: result}
}

func errorMsg(id json.RawMessage, code int, msg string) *response {
	return &response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

// --- tools ---

type tool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

var tools = []tool{
	{
		Name:        "save_memory",
		Description: "Create or overwrite a memory by name with the given content, optionally in a named space (default: \"default\"). Content is chunked and embedded via Ollama for later semantic search.",
		InputSchema: schema([]string{"name", "content"}, map[string]string{"name": "string", "content": "string", "space": "string"}),
	},
	{
		Name:        "get_memory",
		Description: "Fetch the content of a memory by exact name, optionally scoped to a space (default: \"default\").",
		InputSchema: schema([]string{"name"}, map[string]string{"name": "string", "space": "string"}),
	},
	{
		Name:        "list_memories",
		Description: "List the names of stored memories. Pass space to filter to one space; omit it to list all memories grouped by space.",
		InputSchema: schema(nil, map[string]string{"space": "string"}),
	},
	{
		Name:        "search_memories",
		Description: "Hybrid search (semantic + keyword + recency) over stored memories in a space (default: \"default\"). Returns up to `limit` (default 5, max 20) closest matches with their full content.",
		InputSchema: schema([]string{"query"}, map[string]string{"query": "string", "space": "string", "limit": "number"}),
	},
	{
		Name:        "delete_memory",
		Description: "Delete a memory by name, optionally scoped to a space (default: \"default\").",
		InputSchema: schema([]string{"name"}, map[string]string{"name": "string", "space": "string"}),
	},
}

func schema(required []string, props map[string]string) map[string]interface{} {
	p := map[string]interface{}{}
	for k, t := range props {
		p[k] = map[string]string{"type": t}
	}
	s := map[string]interface{}{"type": "object", "properties": p}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func textResult(s string) map[string]interface{} {
	return map[string]interface{}{"content": []map[string]string{{"type": "text", "text": s}}}
}

func errResult(s string) map[string]interface{} {
	return map[string]interface{}{"content": []map[string]string{{"type": "text", "text": s}}, "isError": true}
}

// internalErr logs the real error server-side and returns a generic message
// to the client, so DB/network internals never leak over the wire.
func internalErr(context string, err error) map[string]interface{} {
	log.Printf("%s: %v", context, err)
	return errResult("internal error")
}

// reassemble fetches all chunks for (space, name) ordered by chunk_index
// and joins them back into the original content.
func reassemble(space, name string) (string, error) {
	rows, err := db.Query(`SELECT content FROM memories WHERE space = $1 AND name = $2 ORDER BY chunk_index`, space, name)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return "", err
		}
		parts = append(parts, c)
	}
	return strings.Join(parts, " "), nil
}

type nameDistance struct {
	name string
	dist float64
}

func callTool(name string, args map[string]interface{}) map[string]interface{} {
	switch name {
	case "save_memory":
		n, _ := args["name"].(string)
		content, _ := args["content"].(string)
		space := argSpace(args)
		if n == "" || content == "" {
			return errResult("name and content are required")
		}
		chunks := chunkContent(content)
		tx, err := db.Begin()
		if err != nil {
			return internalErr("save_memory begin", err)
		}
		if _, err := tx.Exec(`DELETE FROM memories WHERE space = $1 AND name = $2`, space, n); err != nil {
			tx.Rollback()
			return internalErr("save_memory delete", err)
		}
		for i, chunk := range chunks {
			vec, err := embed(chunk)
			if err != nil {
				tx.Rollback()
				return internalErr("save_memory embed", err)
			}
			if _, err := tx.Exec(`
				INSERT INTO memories (space, name, chunk_index, content, embedding, updated_at)
				VALUES ($1, $2, $3, $4, $5::vector, now())
			`, space, n, i, chunk, vectorLiteral(vec)); err != nil {
				tx.Rollback()
				return internalErr("save_memory insert", err)
			}
		}
		if err := tx.Commit(); err != nil {
			return internalErr("save_memory commit", err)
		}
		return textResult(fmt.Sprintf("saved memory %q in space %q (%d bytes, %d chunk(s))", n, space, len(content), len(chunks)))

	case "get_memory":
		n, _ := args["name"].(string)
		space := argSpace(args)
		content, err := reassemble(space, n)
		if err != nil {
			return internalErr("get_memory query", err)
		}
		if content == "" {
			return errResult(fmt.Sprintf("memory %q not found in space %q", n, space))
		}
		return textResult(content)

	case "list_memories":
		if s, ok := args["space"].(string); ok && s != "" {
			rows, err := db.Query(`SELECT DISTINCT name FROM memories WHERE space = $1 ORDER BY name`, s)
			if err != nil {
				return internalErr("list_memories query", err)
			}
			defer rows.Close()
			var names []string
			for rows.Next() {
				var n string
				if err := rows.Scan(&n); err != nil {
					return internalErr("list_memories scan", err)
				}
				names = append(names, n)
			}
			if len(names) == 0 {
				return textResult(fmt.Sprintf("(no memories yet in space %q)", s))
			}
			return textResult(strings.Join(names, "\n"))
		}

		rows, err := db.Query(`SELECT DISTINCT space, name FROM memories ORDER BY space, name`)
		if err != nil {
			return internalErr("list_memories query", err)
		}
		defer rows.Close()
		var lines []string
		curSpace := ""
		for rows.Next() {
			var s, n string
			if err := rows.Scan(&s, &n); err != nil {
				return internalErr("list_memories scan", err)
			}
			if s != curSpace {
				lines = append(lines, fmt.Sprintf("[%s]", s))
				curSpace = s
			}
			lines = append(lines, "  "+n)
		}
		if len(lines) == 0 {
			return textResult("(no memories yet)")
		}
		return textResult(strings.Join(lines, "\n"))

	case "search_memories":
		q, _ := args["query"].(string)
		space := argSpace(args)
		if q == "" {
			return errResult("query is required")
		}
		limit := 5
		if l, ok := args["limit"].(float64); ok && l > 0 {
			limit = int(l)
		}
		if limit > 20 {
			limit = 20
		}
		vec, err := embed(q)
		if err != nil {
			return internalErr("search_memories embed", err)
		}
		semanticW, keywordW, recencyW, halfLife := searchWeights()
		rows, err := db.Query(`
			SELECT name, embedding <=> $1::vector AS distance,
			       ts_rank(content_tsv, plainto_tsquery('english', $2)) AS rank,
			       updated_at
			FROM memories
			WHERE space = $3
			ORDER BY distance ASC
			LIMIT $4
		`, vectorLiteral(vec), q, space, limit*5)
		if err != nil {
			return internalErr("search_memories query", err)
		}
		best := map[string]nameDistance{}
		for rows.Next() {
			var n string
			var dist, rank float64
			var updatedAt time.Time
			if err := rows.Scan(&n, &dist, &rank, &updatedAt); err != nil {
				rows.Close()
				return internalErr("search_memories scan", err)
			}
			days := time.Since(updatedAt).Hours() / 24
			recency := math.Exp(-math.Ln2 * days / halfLife)
			score := semanticW*(1-dist) + keywordW*rank + recencyW*recency
			if cur, ok := best[n]; !ok || score > cur.dist {
				best[n] = nameDistance{name: n, dist: score}
			}
		}
		rows.Close()
		matches := make([]nameDistance, 0, len(best))
		for _, m := range best {
			matches = append(matches, m)
		}
		sort.Slice(matches, func(i, j int) bool { return matches[i].dist > matches[j].dist })
		if len(matches) > limit {
			matches = matches[:limit]
		}
		if len(matches) == 0 {
			return textResult("(no matches)")
		}
		var out []string
		for _, m := range matches {
			content, err := reassemble(space, m.name)
			if err != nil {
				return internalErr("search_memories reassemble", err)
			}
			out = append(out, fmt.Sprintf("%s (score %.4f): %s", m.name, m.dist, content))
		}
		return textResult(strings.Join(out, "\n\n"))

	case "delete_memory":
		n, _ := args["name"].(string)
		space := argSpace(args)
		res, err := db.Exec(`DELETE FROM memories WHERE space = $1 AND name = $2`, space, n)
		if err != nil {
			return internalErr("delete_memory exec", err)
		}
		if affected, _ := res.RowsAffected(); affected == 0 {
			return errResult(fmt.Sprintf("memory %q not found in space %q", n, space))
		}
		return textResult(fmt.Sprintf("deleted memory %q from space %q", n, space))

	default:
		return errResult(fmt.Sprintf("unknown tool %q", name))
	}
}

type resource struct {
	URI      string `json:"uri"`
	Name     string `json:"name"`
	MimeType string `json:"mimeType"`
}

func resourceURI(space, name string) string {
	return fmt.Sprintf("memory://%s/%s", space, name)
}

// errNotFound marks a not-found condition distinct from internal errors,
// so the caller can pick the right JSON-RPC error code.
var errNotFound = fmt.Errorf("not found")

func listResources() (interface{}, error) {
	rows, err := db.Query(`SELECT DISTINCT space, name FROM memories ORDER BY space, name`)
	if err != nil {
		log.Printf("resources/list query: %v", err)
		return nil, fmt.Errorf("internal error")
	}
	defer rows.Close()
	res := []resource{}
	for rows.Next() {
		var s, n string
		if err := rows.Scan(&s, &n); err != nil {
			log.Printf("resources/list scan: %v", err)
			return nil, fmt.Errorf("internal error")
		}
		res = append(res, resource{URI: resourceURI(s, n), Name: fmt.Sprintf("%s/%s", s, n), MimeType: "text/plain"})
	}
	return map[string]interface{}{"resources": res}, nil
}

// parseResourceURI splits a "memory://<space>/<name>" URI into its parts.
func parseResourceURI(uri string) (space, name string, ok bool) {
	rest := strings.TrimPrefix(uri, "memory://")
	space, name, cut := strings.Cut(rest, "/")
	if !cut || space == "" || name == "" {
		return "", "", false
	}
	return space, name, true
}

func readResource(uri string) (interface{}, error) {
	space, name, ok := parseResourceURI(uri)
	if !ok {
		return nil, fmt.Errorf("invalid resource uri %q", uri)
	}
	content, err := reassemble(space, name)
	if err != nil {
		log.Printf("resources/read query: %v", err)
		return nil, fmt.Errorf("internal error")
	}
	if content == "" {
		return nil, errNotFound
	}
	return map[string]interface{}{
		"contents": []map[string]string{{"uri": uri, "mimeType": "text/plain", "text": content}},
	}, nil
}

// handle processes one JSON-RPC message and returns the reply to send,
// or nil for notifications (which get no body, just a 202).
func handle(req request) *response {
	switch req.Method {
	case "initialize":
		return resultMsg(req.ID, map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]interface{}{
				"tools":     map[string]interface{}{},
				"resources": map[string]interface{}{},
			},
			"serverInfo": map[string]string{"name": "memory-vault", "version": "0.5.0"},
		})

	case "notifications/initialized":
		return nil

	case "tools/list":
		return resultMsg(req.ID, map[string]interface{}{"tools": tools})

	case "tools/call":
		var params struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return errorMsg(req.ID, -32602, "invalid params")
		}
		return resultMsg(req.ID, callTool(params.Name, params.Arguments))

	case "resources/list":
		result, err := listResources()
		if err != nil {
			return errorMsg(req.ID, -32000, err.Error())
		}
		return resultMsg(req.ID, result)

	case "resources/read":
		var params struct {
			URI string `json:"uri"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return errorMsg(req.ID, -32602, "invalid params")
		}
		result, err := readResource(params.URI)
		if err != nil {
			if err == errNotFound {
				return errorMsg(req.ID, -32001, fmt.Sprintf("resource %q not found", params.URI))
			}
			return errorMsg(req.ID, -32000, err.Error())
		}
		return resultMsg(req.ID, result)

	case "ping":
		return resultMsg(req.ID, map[string]interface{}{})

	default:
		if len(req.ID) > 0 {
			return errorMsg(req.ID, -32601, "method not found: "+req.Method)
		}
		return nil
	}
}

func newSessionID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// authTokens returns the set of accepted bearer tokens from AUTH_TOKEN
// (comma-separated for multiple clients). Empty means auth is disabled.
func authTokens() []string {
	raw := os.Getenv("AUTH_TOKEN")
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ",")
}

func checkAuth(r *http.Request) bool {
	tokens := authTokens()
	if len(tokens) == 0 {
		return true // no AUTH_TOKEN configured: auth disabled
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if got == "" {
		return false
	}
	for _, t := range tokens {
		if subtle.ConstantTimeCompare([]byte(got), []byte(t)) == 1 {
			return true
		}
	}
	return false
}

// allowedHosts returns the Host header allowlist from ALLOWED_HOSTS
// (comma-separated). Empty means the check is skipped (not recommended).
func allowedHosts() []string {
	raw := os.Getenv("ALLOWED_HOSTS")
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ",")
}

// checkHost guards against DNS-rebinding attacks by rejecting requests
// whose Host header isn't in the configured allowlist.
func checkHost(r *http.Request) bool {
	hosts := allowedHosts()
	if len(hosts) == 0 {
		return true
	}
	for _, h := range hosts {
		if r.Host == h {
			return true
		}
	}
	return false
}

func mcpHandler(w http.ResponseWriter, r *http.Request) {
	if !checkHost(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !checkAuth(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="memory-vault"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON-RPC message", http.StatusBadRequest)
		return
	}

	resp := handle(req)
	if resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if req.Method == "initialize" {
		w.Header().Set("Mcp-Session-Id", newSessionID())
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func main() {
	if err := initDB(); err != nil {
		log.Fatalf("db init failed: %v", err)
	}
	defer db.Close()

	addr := ":" + envOr("PORT", "8080")
	http.HandleFunc("/mcp", mcpHandler)
	log.Printf("memory-vault listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

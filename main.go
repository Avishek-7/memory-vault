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

func ollamaURL() string       { return envOr("OLLAMA_URL", "http://localhost:11434") }
func ollamaModel() string     { return envOr("OLLAMA_EMBED_MODEL", "all-minilm") }
func ollamaChatModel() string { return envOr("OLLAMA_CHAT_MODEL", "llama3.1:8b") }

// compactionSettings returns the cosine-distance threshold under which two
// memories' centroid embeddings are considered near-duplicates, and the
// staleness window (in days) past which a lone memory is still a compaction
// candidate (for re-summarization) even without a similar sibling.
func compactionSettings() (similarityThreshold float64, staleDays int) {
	return envOrFloat("COMPACT_SIMILARITY_THRESHOLD", 0.15), envOrInt("COMPACT_STALE_DAYS", 90)
}

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

// parseVectorLiteral parses pgvector's text output format ("[0.1,0.2,0.3]")
// back into a float64 slice.
func parseVectorLiteral(s string) ([]float64, error) {
	s = strings.Trim(s, "[]")
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]float64, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return nil, err
		}
		out[i] = f
	}
	return out, nil
}

// cosineDistance mirrors pgvector's <=> operator (1 - cosine similarity).
func cosineDistance(a, b []float64) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 1
	}
	return 1 - dot/(math.Sqrt(na)*math.Sqrt(nb))
}

// ollamaChat sends a single-turn prompt to the local Ollama chat model and
// returns its response text. Used only by compact_memories, never search/save.
func ollamaChat(prompt string) (string, error) {
	reqBody, _ := json.Marshal(map[string]interface{}{
		"model":    ollamaChatModel(),
		"messages": []map[string]string{{"role": "user", "content": prompt}},
		"stream":   false,
	})
	resp, err := http.Post(ollamaURL()+"/api/chat", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("ollama chat request failed (is Ollama running with %q pulled?): %w", ollamaChatModel(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama chat returned status %d", resp.StatusCode)
	}
	var out struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Message.Content, nil
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
	{
		Name:        "compact_memories",
		Description: "Find near-duplicate or stale memories (within a space, or all spaces if omitted) and merge/summarize them via the local Ollama chat model. dry_run (default true) returns the proposed plan without writing anything.",
		InputSchema: schema(nil, map[string]string{"space": "string", "dry_run": "boolean"}),
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

// doSaveMemory chunks, embeds, and (re)writes content under (space, name),
// replacing any existing chunks for that name. Shared by the save_memory
// tool and compact_memories.
func doSaveMemory(space, name, content string) (int, error) {
	chunks := chunkContent(content)
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM memories WHERE space = $1 AND name = $2`, space, name); err != nil {
		tx.Rollback()
		return 0, err
	}
	for i, chunk := range chunks {
		vec, err := embed(chunk)
		if err != nil {
			tx.Rollback()
			return 0, err
		}
		if _, err := tx.Exec(`
			INSERT INTO memories (space, name, chunk_index, content, embedding, updated_at)
			VALUES ($1, $2, $3, $4, $5::vector, now())
		`, space, name, i, chunk, vectorLiteral(vec)); err != nil {
			tx.Rollback()
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(chunks), nil
}

// memInfo is one memory's centroid embedding (average of its chunk
// embeddings) and freshness, used to find compaction candidates.
type memInfo struct {
	space       string
	name        string
	centroid    []float64
	lastUpdated time.Time
}

// compactGroupsForSpace groups a space's memories into compaction
// candidates: connected components under the similarity threshold (any
// chain of near-duplicate centroids), plus lone memories past the
// staleness window (candidates for solo re-summarization).
func compactGroupsForSpace(space string) ([][]memInfo, error) {
	rows, err := db.Query(`SELECT name, avg(embedding)::text, max(updated_at) FROM memories WHERE space = $1 GROUP BY name`, space)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var infos []memInfo
	for rows.Next() {
		var name, centroidStr string
		var lastUpdated time.Time
		if err := rows.Scan(&name, &centroidStr, &lastUpdated); err != nil {
			return nil, err
		}
		centroid, err := parseVectorLiteral(centroidStr)
		if err != nil {
			return nil, err
		}
		infos = append(infos, memInfo{space: space, name: name, centroid: centroid, lastUpdated: lastUpdated})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	threshold, staleDays := compactionSettings()
	staleCutoff := time.Now().AddDate(0, 0, -staleDays)

	parent := make([]int, len(infos))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}
	for i := 0; i < len(infos); i++ {
		for j := i + 1; j < len(infos); j++ {
			if cosineDistance(infos[i].centroid, infos[j].centroid) < threshold {
				union(i, j)
			}
		}
	}

	byRoot := map[int][]int{}
	for i := range infos {
		r := find(i)
		byRoot[r] = append(byRoot[r], i)
	}
	var groups [][]memInfo
	for _, idxs := range byRoot {
		if len(idxs) == 1 && infos[idxs[0]].lastUpdated.After(staleCutoff) {
			continue // neither similar to a sibling nor stale: leave alone
		}
		g := make([]memInfo, len(idxs))
		for k, idx := range idxs {
			g[k] = infos[idx]
		}
		groups = append(groups, g)
	}
	return groups, nil
}

// compactTargetName picks the name a compacted group is saved under: the
// original name for a solo re-summarization, or a "-merged" name when
// multiple sources are combined.
func compactTargetName(names []string) string {
	if len(names) == 1 {
		return names[0]
	}
	return names[0] + "-merged"
}

// compactGroup either describes (dry_run) or performs the merge of one
// compaction group: reassemble each source, ask the Ollama chat model to
// consolidate them, save the result, and delete the sources it replaced.
func compactGroup(g []memInfo, dryRun bool) (string, error) {
	names := make([]string, len(g))
	for i, m := range g {
		names[i] = m.name
	}
	newName := compactTargetName(names)

	if dryRun {
		reason := "similar"
		if len(g) == 1 {
			reason = "stale"
		}
		return fmt.Sprintf("space %q: [%s] -> would merge into %q (%s)", g[0].space, strings.Join(names, ", "), newName, reason), nil
	}

	var parts []string
	for _, m := range g {
		content, err := reassemble(m.space, m.name)
		if err != nil {
			return "", err
		}
		parts = append(parts, fmt.Sprintf("--- %s ---\n%s", m.name, content))
	}
	prompt := fmt.Sprintf("The following are related notes. Merge them into a single consolidated note that preserves every distinct fact and drops redundancy. Respond with only the merged note text, no preamble.\n\n%s", strings.Join(parts, "\n\n"))
	merged, err := ollamaChat(prompt)
	if err != nil {
		return "", err
	}
	merged = strings.TrimSpace(merged)
	if merged == "" {
		return "", fmt.Errorf("ollama returned an empty summary")
	}
	if _, err := doSaveMemory(g[0].space, newName, merged); err != nil {
		return "", err
	}
	for _, m := range g {
		if m.name == newName {
			continue
		}
		if _, err := db.Exec(`DELETE FROM memories WHERE space = $1 AND name = $2`, m.space, m.name); err != nil {
			return "", err
		}
	}
	log.Printf("compact_memories: space=%q sources=%v -> %q via model=%q", g[0].space, names, newName, ollamaChatModel())
	return fmt.Sprintf("space %q: merged [%s] into %q", g[0].space, strings.Join(names, ", "), newName), nil
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
		numChunks, err := doSaveMemory(space, n, content)
		if err != nil {
			return internalErr("save_memory", err)
		}
		return textResult(fmt.Sprintf("saved memory %q in space %q (%d bytes, %d chunk(s))", n, space, len(content), numChunks))

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

	case "compact_memories":
		dryRun := true
		if d, ok := args["dry_run"].(bool); ok {
			dryRun = d
		}
		var spaces []string
		if s, ok := args["space"].(string); ok && s != "" {
			spaces = []string{s}
		} else {
			rows, err := db.Query(`SELECT DISTINCT space FROM memories ORDER BY space`)
			if err != nil {
				return internalErr("compact_memories spaces", err)
			}
			for rows.Next() {
				var s string
				if err := rows.Scan(&s); err != nil {
					rows.Close()
					return internalErr("compact_memories spaces scan", err)
				}
				spaces = append(spaces, s)
			}
			rows.Close()
		}

		var lines []string
		for _, sp := range spaces {
			groups, err := compactGroupsForSpace(sp)
			if err != nil {
				return internalErr("compact_memories groups", err)
			}
			for _, g := range groups {
				line, err := compactGroup(g, dryRun)
				if err != nil {
					return internalErr("compact_memories merge", err)
				}
				lines = append(lines, line)
			}
		}
		if len(lines) == 0 {
			if dryRun {
				return textResult("(no compaction candidates found)")
			}
			return textResult("(nothing to compact)")
		}
		verb := "would compact"
		if !dryRun {
			verb = "compacted"
		}
		return textResult(fmt.Sprintf("%s %d group(s):\n%s", verb, len(lines), strings.Join(lines, "\n")))

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

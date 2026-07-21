// memory-vault: a minimal MCP server exposing persistent memory tools to
// LLMs over the MCP Streamable HTTP transport, backed by Postgres/pgvector
// with embeddings from a local Ollama server (all-minilm / all-MiniLM-L6-v2,
// 384-dim).
package main

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	_ "github.com/lib/pq"
)

const embedDim = 384

var db *sql.DB

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
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
	_, err = db.Exec(`
		CREATE EXTENSION IF NOT EXISTS vector;
		CREATE TABLE IF NOT EXISTS memories (
			name       TEXT PRIMARY KEY,
			content    TEXT NOT NULL,
			embedding  vector(384) NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);
	`)
	return err
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
		Description: "Create or overwrite a memory by name with the given content. Embeds the content via Ollama for later semantic search.",
		InputSchema: schema([]string{"name", "content"}, map[string]string{"name": "string", "content": "string"}),
	},
	{
		Name:        "get_memory",
		Description: "Fetch the content of a memory by exact name.",
		InputSchema: schema([]string{"name"}, map[string]string{"name": "string"}),
	},
	{
		Name:        "list_memories",
		Description: "List the names of all stored memories.",
		InputSchema: schema(nil, nil),
	},
	{
		Name:        "search_memories",
		Description: "Semantic search over stored memories using pgvector cosine distance. Returns the closest matches with their content.",
		InputSchema: schema([]string{"query"}, map[string]string{"query": "string"}),
	},
	{
		Name:        "delete_memory",
		Description: "Delete a memory by name.",
		InputSchema: schema([]string{"name"}, map[string]string{"name": "string"}),
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

func callTool(name string, args map[string]interface{}) map[string]interface{} {
	switch name {
	case "save_memory":
		n, _ := args["name"].(string)
		content, _ := args["content"].(string)
		if n == "" || content == "" {
			return errResult("name and content are required")
		}
		vec, err := embed(content)
		if err != nil {
			return errResult(err.Error())
		}
		_, err = db.Exec(`
			INSERT INTO memories (name, content, embedding, updated_at)
			VALUES ($1, $2, $3::vector, now())
			ON CONFLICT (name) DO UPDATE SET content = $2, embedding = $3::vector, updated_at = now()
		`, n, content, vectorLiteral(vec))
		if err != nil {
			return errResult(err.Error())
		}
		return textResult(fmt.Sprintf("saved memory %q (%d bytes)", n, len(content)))

	case "get_memory":
		n, _ := args["name"].(string)
		var content string
		err := db.QueryRow(`SELECT content FROM memories WHERE name = $1`, n).Scan(&content)
		if err == sql.ErrNoRows {
			return errResult(fmt.Sprintf("memory %q not found", n))
		} else if err != nil {
			return errResult(err.Error())
		}
		return textResult(content)

	case "list_memories":
		rows, err := db.Query(`SELECT name FROM memories ORDER BY name`)
		if err != nil {
			return errResult(err.Error())
		}
		defer rows.Close()
		var names []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return errResult(err.Error())
			}
			names = append(names, n)
		}
		if len(names) == 0 {
			return textResult("(no memories yet)")
		}
		return textResult(strings.Join(names, "\n"))

	case "search_memories":
		q, _ := args["query"].(string)
		if q == "" {
			return errResult("query is required")
		}
		vec, err := embed(q)
		if err != nil {
			return errResult(err.Error())
		}
		rows, err := db.Query(`
			SELECT name, content, embedding <=> $1::vector AS distance
			FROM memories
			ORDER BY distance ASC
			LIMIT 5
		`, vectorLiteral(vec))
		if err != nil {
			return errResult(err.Error())
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var n, content string
			var dist float64
			if err := rows.Scan(&n, &content, &dist); err != nil {
				return errResult(err.Error())
			}
			out = append(out, fmt.Sprintf("%s (distance %.4f): %s", n, dist, content))
		}
		if len(out) == 0 {
			return textResult("(no matches)")
		}
		return textResult(strings.Join(out, "\n\n"))

	case "delete_memory":
		n, _ := args["name"].(string)
		res, err := db.Exec(`DELETE FROM memories WHERE name = $1`, n)
		if err != nil {
			return errResult(err.Error())
		}
		if affected, _ := res.RowsAffected(); affected == 0 {
			return errResult(fmt.Sprintf("memory %q not found", n))
		}
		return textResult(fmt.Sprintf("deleted memory %q", n))

	default:
		return errResult(fmt.Sprintf("unknown tool %q", name))
	}
}

// handle processes one JSON-RPC message and returns the reply to send,
// or nil for notifications (which get no body, just a 202).
func handle(req request) *response {
	switch req.Method {
	case "initialize":
		return resultMsg(req.ID, map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":      map[string]string{"name": "memory-vault", "version": "0.3.0"},
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

func mcpHandler(w http.ResponseWriter, r *http.Request) {
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

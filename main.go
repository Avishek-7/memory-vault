// memory-vault: a minimal MCP server exposing persistent memory tools to
// LLMs over the MCP Streamable HTTP transport, backed by Postgres/pgvector
// with embeddings from a local Ollama server (all-minilm / all-MiniLM-L6-v2,
// 384-dim).
package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"memory-vault/internal/chat"
	"memory-vault/internal/embed"
	"memory-vault/internal/ratelimit"
	"memory-vault/internal/store"
)

var st *store.Store

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

// maxRequestBodyBytes caps /mcp request bodies so a huge payload (an
// oversized import_memories call, or just abuse) can't exhaust memory.
// 25MB comfortably fits a large import_memories backup as JSON text.
func maxRequestBodyBytes() int64 {
	return int64(envOrInt("MAX_REQUEST_BODY_MB", 25)) * 1024 * 1024
}

// compactionSettings returns the cosine-distance threshold under which two
// memories' centroid embeddings are considered near-duplicates, and the
// staleness window (in days) past which a lone memory is still a compaction
// candidate (for re-summarization) even without a similar sibling.
func compactionSettings() (similarityThreshold float64, staleDays int) {
	return envOrFloat("COMPACT_SIMILARITY_THRESHOLD", 0.15), envOrInt("COMPACT_STALE_DAYS", 90)
}

// embedderFromEnv picks the Embedder per EMBED_PROVIDER (default "ollama",
// today's only behavior). "openai" is an escape hatch for an
// OpenAI-compatible embeddings endpoint (OpenAI itself, or a self-hosted
// server like LiteLLM/text-embeddings-inference) — not a recommendation
// to move off the local-first default. Any other value is a configuration
// error (most likely a typo) rather than a silent fallback to Ollama.
func embedderFromEnv() (embed.Embedder, error) {
	switch provider := envOr("EMBED_PROVIDER", "ollama"); provider {
	case "ollama":
		return &embed.OllamaEmbedder{URL: ollamaURL(), Model: ollamaModel()}, nil
	case "openai":
		return &embed.OpenAIEmbedder{
			BaseURL: envOr("OPENAI_EMBED_BASE_URL", "https://api.openai.com/v1"),
			APIKey:  os.Getenv("OPENAI_EMBED_API_KEY"),
			Model:   envOr("OPENAI_EMBED_MODEL", "text-embedding-3-small"),
		}, nil
	default:
		return nil, fmt.Errorf("unrecognized EMBED_PROVIDER %q (want \"ollama\" or \"openai\")", provider)
	}
}

func storeConfig() (store.Config, error) {
	embedder, err := embedderFromEnv()
	if err != nil {
		return store.Config{}, err
	}
	return store.Config{
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		Embedder:        embedder,
		EmbedDim:        envOrInt("EMBED_DIM", store.DefaultEmbedDim),
		MaxOpenConns:    envOrInt("DB_MAX_OPEN_CONNS", 10),
		MaxIdleConns:    envOrInt("DB_MAX_IDLE_CONNS", 5),
		ConnMaxLifetime: time.Duration(envOrInt("DB_CONN_MAX_LIFETIME_MIN", 30)) * time.Minute,
	}, nil
}

// search scoring weights: default to semantic-only ranking (identical to
// pre-hybrid-search behavior) unless overridden. Per-kind boosts all default
// to 0 (no ranking change) unless a SEARCH_KIND_BOOST_<KIND> env var opts in.
func searchWeights() store.SearchWeights {
	boosts := map[string]float64{}
	for _, k := range store.ValidKinds {
		boosts[k] = envOrFloat("SEARCH_KIND_BOOST_"+strings.ToUpper(k), 0.0)
	}
	return store.SearchWeights{
		Semantic:     envOrFloat("SEARCH_WEIGHT_SEMANTIC", 1.0),
		Keyword:      envOrFloat("SEARCH_WEIGHT_KEYWORD", 0.0),
		Recency:      envOrFloat("SEARCH_WEIGHT_RECENCY", 0.0),
		HalfLifeDays: envOrFloat("SEARCH_RECENCY_HALFLIFE_DAYS", 30.0),
		KindBoost:    boosts,
	}
}

// validKindsList renders store.ValidKinds for error messages, e.g.
// `"fact", "decision", "preference", "task", "note"`.
func validKindsList() string {
	return quotedList(store.ValidKinds)
}

// validFlagsList renders store.ValidFlags for error messages.
func validFlagsList() string {
	return quotedList(store.ValidFlags)
}

func quotedList(vals []string) string {
	quoted := make([]string, len(vals))
	for i, v := range vals {
		quoted[i] = fmt.Sprintf("%q", v)
	}
	return strings.Join(quoted, ", ")
}

func argSpace(args map[string]interface{}) string {
	if s, ok := args["space"].(string); ok && s != "" {
		return s
	}
	return store.DefaultSpace
}

// ollamaChat sends a single-turn prompt to the local Ollama chat model and
// returns its response text. Used by compact_memories and
// get_session_summary, never search/save.
func ollamaChat(prompt string) (string, error) {
	return chat.Chat(ollamaURL(), ollamaChatModel(), prompt)
}

// sessionSummaryPrompt builds a resume prompt from a space's recent
// memories (via the shared internal/chat template, so the TUI's "s"
// keybinding can't drift out of sync) and sends it through ollamaChat.
func sessionSummaryPrompt(space string, recents []store.RecentMemory) (string, error) {
	summary, err := ollamaChat(chat.SessionSummaryPrompt(space, recents))
	if err != nil {
		return "", err
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return "", fmt.Errorf("ollama returned an empty summary")
	}
	return summary, nil
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
		Description: "Create or overwrite a memory by name with the given content, optionally in a named space (default: \"default\"). Content is chunked and embedded via Ollama for later semantic search. Optional source records which agent wrote it (default: \"unspecified\"); optional expect_source rejects the write instead of overwriting if a memory already exists there under a different source. Optional kind (default: \"note\") is one of fact, decision, preference, task, note.",
		InputSchema: schema([]string{"name", "content"}, map[string]string{"name": "string", "content": "string", "space": "string", "source": "string", "expect_source": "string", "kind": "string"}),
	},
	{
		Name:        "get_memory",
		Description: "Fetch the content of a memory by exact name, optionally scoped to a space (default: \"default\").",
		InputSchema: schema([]string{"name"}, map[string]string{"name": "string", "space": "string"}),
	},
	{
		Name:        "list_memories",
		Description: "List the names of stored memories. Pass space to filter to one space; omit it to list all memories grouped by space. Optional source/kind filters narrow the results.",
		InputSchema: schema(nil, map[string]string{"space": "string", "source": "string", "kind": "string"}),
	},
	{
		Name:        "search_memories",
		Description: "Hybrid search (semantic + keyword + recency + optional per-kind boost) over stored memories in a space (default: \"default\"). Returns up to `limit` (default 5, max 20) closest matches with their full content. Optional source/kind filters narrow the candidates.",
		InputSchema: schema([]string{"query"}, map[string]string{"query": "string", "space": "string", "limit": "number", "source": "string", "kind": "string"}),
	},
	{
		Name:        "delete_memory",
		Description: "Delete a memory by name, optionally scoped to a space (default: \"default\").",
		InputSchema: schema([]string{"name"}, map[string]string{"name": "string", "space": "string"}),
	},
	{
		Name:        "flag_memory",
		Description: "Set a usage/quality flag on a memory: useful, stale, or wrong. A new flag overwrites any previous one — a memory has one current flag, not a history. Influences compact_memories candidate selection: stale/wrong make a memory a stronger candidate regardless of age; useful protects it from age-based (but not similarity-based) selection. Optional note explains why.",
		InputSchema: schema([]string{"name", "flag"}, map[string]string{"name": "string", "space": "string", "flag": "string", "note": "string"}),
	},
	{
		Name:        "compact_memories",
		Description: "Find near-duplicate or stale memories (within a space, or all spaces if omitted) and merge/summarize them via the local Ollama chat model. Memories of kind \"decision\" are never selected for compaction. dry_run (default true) returns the proposed plan without writing anything.",
		InputSchema: schema(nil, map[string]string{"space": "string", "dry_run": "boolean"}),
	},
	{
		Name:        "export_memories",
		Description: "Export memories as JSON (name, space, source, kind, content, updated_at — no embeddings, which are cheap to regenerate on import). Optional space exports just that space; optional source exports just that source; omit both to export everything.",
		InputSchema: schema(nil, map[string]string{"space": "string", "source": "string"}),
	},
	{
		Name:        "get_session_summary",
		Description: "Read-only resume of a space's recent state (default space: \"default\") for picking up work after a new session/context reset: what's the current state, what was decided, what's still open, likely next step. Summarizes the most recently updated memories (favoring task/decision kind) via the local Ollama chat model. Use this to resume broad context; use search_memories to find one specific fact instead. Optional limit caps how many recent memories it considers (default 15, max 50).",
		InputSchema: schema(nil, map[string]string{"space": "string", "limit": "number"}),
	},
	{
		Name:        "import_memories",
		Description: "Import memories from a JSON payload in the shape export_memories produces. Re-chunks and re-embeds every memory through the normal save path. Optional space sends every imported memory there regardless of what's in the data (useful for merging a backup into a differently-named space); optional source does the same for source (otherwise each memory keeps its recorded source, or \"unspecified\" if the export predates the source field); optional overwrite (default false) controls whether memories that already exist are replaced or skipped.",
		InputSchema: schema([]string{"data"}, map[string]string{"data": "string", "space": "string", "source": "string", "overwrite": "boolean"}),
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

// quotaResult renders a quota rejection as a message the user can act on,
// returning nil for any other error (including none). A quota is a fact about
// the caller's plan, not a server fault, so it must not go through
// internalErr and come back as a generic "internal error" the user cannot
// interpret or fix.
func quotaResult(err error) map[string]interface{} {
	var quota *store.QuotaError
	if !errors.As(err, &quota) {
		return nil
	}
	switch quota.Resource {
	case "memories":
		return errResult(fmt.Sprintf(
			"memory limit reached: this plan allows %d memories and %d are already stored. "+
				"Delete some (delete_memory) or merge them (compact_memories) to free space.",
			quota.Limit, quota.Current))
	default:
		return errResult(fmt.Sprintf(
			"storage limit reached: this plan allows %.1f MB and %.1f MB are already stored; "+
				"this write would add %.1f KB. Delete or compact some memories to free space.",
			float64(quota.Limit)/(1<<20), float64(quota.Current)/(1<<20), float64(quota.Adding)/1024))
	}
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

// compactGroupsForSpace groups a space's memories into compaction
// candidates: connected components under the similarity threshold (any
// chain of near-duplicate centroids), plus lone memories past the
// staleness window (candidates for solo re-summarization).
func compactGroupsForSpace(vault *store.Store, space string) ([][]store.MemoryCentroid, error) {
	all, err := vault.MemoryCentroids(space)
	if err != nil {
		return nil, err
	}
	// Decisions are never compaction candidates — they record something
	// that was explicitly decided, and merging or age-based pruning could
	// silently lose that. Excluded here, before grouping, so they can't be
	// pulled into a sibling's merge either.
	var infos []store.MemoryCentroid
	for _, m := range all {
		if m.Kind != "decision" {
			infos = append(infos, m)
		}
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
			if store.CosineDistance(infos[i].Centroid, infos[j].Centroid) < threshold {
				union(i, j)
			}
		}
	}

	byRoot := map[int][]int{}
	for i := range infos {
		r := find(i)
		byRoot[r] = append(byRoot[r], i)
	}
	var groups [][]store.MemoryCentroid
	for _, idxs := range byRoot {
		if len(idxs) == 1 {
			m := infos[idxs[0]]
			flaggedBad := m.Flag == "stale" || m.Flag == "wrong"
			stale := !m.LastUpdated.After(staleCutoff)
			// "useful" protects against age-based pruning alone (it's not
			// grouped here anyway, since len==1 means no similar sibling
			// pulled it in); "stale"/"wrong" make a memory a candidate
			// regardless of age.
			if m.Flag == "useful" || (!stale && !flaggedBad) {
				continue // neither similar to a sibling, stale, nor flagged stale/wrong
			}
		}
		g := make([]store.MemoryCentroid, len(idxs))
		for k, idx := range idxs {
			g[k] = infos[idx]
		}
		groups = append(groups, g)
	}
	return groups, nil
}

// compactGroup either describes (dry_run) or performs the merge of one
// compaction group: reassemble each source, ask the Ollama chat model to
// consolidate them, save the result, and delete the sources it replaced.
func compactGroup(vault *store.Store, space string, g []store.MemoryCentroid, dryRun bool) (string, error) {
	names := make([]string, len(g))
	for i, m := range g {
		names[i] = m.Name
	}
	newName := compactTargetName(names)

	if dryRun {
		reason := "similar"
		if len(g) == 1 {
			switch g[0].Flag {
			case "wrong":
				reason = "flagged wrong"
			case "stale":
				reason = "flagged stale"
			default:
				reason = "stale"
			}
		}
		return fmt.Sprintf("space %q: [%s] -> would merge into %q (%s)", space, strings.Join(names, ", "), newName, reason), nil
	}

	var parts []string
	for _, m := range g {
		content, err := vault.Reassemble(space, m.Name)
		if err != nil {
			return "", err
		}
		parts = append(parts, fmt.Sprintf("--- %s ---\n%s", m.Name, content))
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
	// A solo re-summarization keeps its source/kind; a multi-source merge
	// has no single rightful owner, so it's recorded as DefaultSource and,
	// since it's now a synthesized note rather than any one original
	// memory's structured type, DefaultKind (decisions are already
	// excluded from grouping above, so this never demotes one).
	mergedSource, mergedKind := store.DefaultSource, store.DefaultKind
	if len(g) == 1 {
		mergedSource, mergedKind = g[0].Source, g[0].Kind
	}
	// Exempt from quota on purpose. compactGroup writes the merged memory
	// BEFORE deleting the sources it replaces — the safe order, since losing
	// the sources to a failed save would destroy data — which means usage
	// transiently holds both. A tenant at their cap would therefore be unable
	// to compact, and compaction is precisely what quotaResult tells them to
	// do to get back under it. The overshoot is bounded by one summary and
	// disappears on the next few lines when the sources are deleted.
	if _, err := vault.WithLimits(store.Unlimited).SaveMemory(space, newName, merged, mergedSource, mergedKind); err != nil {
		return "", err
	}
	for _, m := range g {
		if m.Name == newName {
			continue
		}
		if _, err := vault.DeleteMemory(space, m.Name); err != nil {
			return "", err
		}
	}
	log.Printf("compact_memories: space=%q sources=%v -> %q via model=%q", space, names, newName, ollamaChatModel())
	return fmt.Sprintf("space %q: merged [%s] into %q", space, strings.Join(names, ", "), newName), nil
}

func callTool(vault *store.Store, name string, args map[string]interface{}) map[string]interface{} {
	switch name {
	case "save_memory":
		n, _ := args["name"].(string)
		content, _ := args["content"].(string)
		space := argSpace(args)
		source, _ := args["source"].(string)
		if source == "" {
			source = store.DefaultSource
		}
		kind, _ := args["kind"].(string)
		if kind == "" {
			kind = store.DefaultKind
		}
		if n == "" || content == "" {
			return errResult("name and content are required")
		}
		if !store.IsValidKind(kind) {
			return errResult(fmt.Sprintf("invalid kind %q, want one of: %s", kind, validKindsList()))
		}
		var numChunks int
		var err error
		if expectSource, _ := args["expect_source"].(string); expectSource != "" {
			numChunks, err = vault.SaveMemoryExpectSource(space, n, content, source, kind, expectSource)
			var conflict *store.SourceConflictError
			if errors.As(err, &conflict) {
				return errResult(fmt.Sprintf("memory %q in space %q already exists with source %q, not %q — refusing to overwrite", n, space, conflict.Existing, expectSource))
			}
		} else {
			numChunks, err = vault.SaveMemory(space, n, content, source, kind)
		}
		if quotaErr := quotaResult(err); quotaErr != nil {
			return quotaErr
		}
		if err != nil {
			return internalErr("save_memory", err)
		}
		return textResult(fmt.Sprintf("saved memory %q in space %q (source %q, kind %q, %d bytes, %d chunk(s))", n, space, source, kind, len(content), numChunks))

	case "get_memory":
		n, _ := args["name"].(string)
		space := argSpace(args)
		content, err := vault.Reassemble(space, n)
		if err != nil {
			return internalErr("get_memory query", err)
		}
		if content == "" {
			return errResult(fmt.Sprintf("memory %q not found in space %q", n, space))
		}
		return textResult(content)

	case "list_memories":
		sourceFilter, _ := args["source"].(string)
		kindFilter, _ := args["kind"].(string)
		if s, ok := args["space"].(string); ok && s != "" {
			names, err := vault.ListMemoryNames(s, sourceFilter, kindFilter)
			if err != nil {
				return internalErr("list_memories query", err)
			}
			if len(names) == 0 {
				return textResult(fmt.Sprintf("(no memories yet in space %q)", s))
			}
			return textResult(strings.Join(names, "\n"))
		}

		all, err := vault.ListAll(sourceFilter, kindFilter)
		if err != nil {
			return internalErr("list_memories query", err)
		}
		var lines []string
		curSpace := ""
		for _, sn := range all {
			if sn.Space != curSpace {
				lines = append(lines, fmt.Sprintf("[%s]", sn.Space))
				curSpace = sn.Space
			}
			lines = append(lines, "  "+sn.Name)
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
		sourceFilter, _ := args["source"].(string)
		kindFilter, _ := args["kind"].(string)
		matches, err := vault.SearchMemories(space, q, limit, searchWeights(), sourceFilter, kindFilter)
		if err != nil {
			return internalErr("search_memories", err)
		}
		if len(matches) == 0 {
			return textResult("(no matches)")
		}
		var out []string
		for _, m := range matches {
			out = append(out, fmt.Sprintf("%s (score %.4f): %s", m.Name, m.Score, m.Content))
		}
		return textResult(strings.Join(out, "\n\n"))

	case "delete_memory":
		n, _ := args["name"].(string)
		space := argSpace(args)
		found, err := vault.DeleteMemory(space, n)
		if err != nil {
			return internalErr("delete_memory exec", err)
		}
		if !found {
			return errResult(fmt.Sprintf("memory %q not found in space %q", n, space))
		}
		return textResult(fmt.Sprintf("deleted memory %q from space %q", n, space))

	case "flag_memory":
		n, _ := args["name"].(string)
		space := argSpace(args)
		flag, _ := args["flag"].(string)
		note, _ := args["note"].(string)
		if n == "" || flag == "" {
			return errResult("name and flag are required")
		}
		if !store.IsValidFlag(flag) {
			return errResult(fmt.Sprintf("invalid flag %q, want one of: %s", flag, validFlagsList()))
		}
		found, err := vault.FlagMemory(space, n, flag, note)
		if err != nil {
			return internalErr("flag_memory", err)
		}
		if !found {
			return errResult(fmt.Sprintf("memory %q not found in space %q", n, space))
		}
		return textResult(fmt.Sprintf("flagged memory %q in space %q as %q", n, space, flag))

	case "compact_memories":
		dryRun := true
		if d, ok := args["dry_run"].(bool); ok {
			dryRun = d
		}
		var spaces []string
		if s, ok := args["space"].(string); ok && s != "" {
			spaces = []string{s}
		} else {
			var err error
			spaces, err = vault.Spaces()
			if err != nil {
				return internalErr("compact_memories spaces", err)
			}
		}

		var lines []string
		for _, sp := range spaces {
			groups, err := compactGroupsForSpace(vault, sp)
			if err != nil {
				return internalErr("compact_memories groups", err)
			}
			for _, g := range groups {
				line, err := compactGroup(vault, sp, g, dryRun)
				if quotaErr := quotaResult(err); quotaErr != nil {
					return quotaErr
				}
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

	case "get_session_summary":
		space := argSpace(args)
		limit := 15
		if l, ok := args["limit"].(float64); ok && l > 0 {
			limit = int(l)
		}
		if limit > 50 {
			limit = 50
		}
		recents, err := vault.RecentMemories(space, limit)
		if err != nil {
			return internalErr("get_session_summary query", err)
		}
		if len(recents) == 0 {
			return textResult(fmt.Sprintf("(no memories yet in space %q)", space))
		}
		summary, err := sessionSummaryPrompt(space, recents)
		if err != nil {
			return internalErr("get_session_summary chat", err)
		}
		return textResult(summary)

	case "export_memories":
		space, _ := args["space"].(string)
		source, _ := args["source"].(string)
		items, err := vault.Export(space, source)
		if err != nil {
			return internalErr("export_memories", err)
		}
		payload, err := json.MarshalIndent(items, "", "  ")
		if err != nil {
			return internalErr("export_memories marshal", err)
		}
		return textResult(string(payload))

	case "import_memories":
		raw, _ := args["data"].(string)
		if raw == "" {
			return errResult("data is required")
		}
		var items []store.MemoryExport
		if err := json.Unmarshal([]byte(raw), &items); err != nil {
			return errResult("data is not valid export JSON: " + err.Error())
		}
		spaceOverride, _ := args["space"].(string)
		sourceOverride, _ := args["source"].(string)
		overwrite, _ := args["overwrite"].(bool)
		res, err := vault.Import(items, spaceOverride, sourceOverride, overwrite)
		if quotaErr := quotaResult(err); quotaErr != nil {
			return quotaErr
		}
		if err != nil {
			return internalErr("import_memories", err)
		}
		summary := fmt.Sprintf("imported %d, skipped %d", len(res.Imported), len(res.Skipped))
		if len(res.Skipped) > 0 {
			summary += fmt.Sprintf("\nskipped (already exist, overwrite=false): %s", strings.Join(res.Skipped, ", "))
		}
		return textResult(summary)

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

func listResources(vault *store.Store) (interface{}, error) {
	all, err := vault.ListAll("", "")
	if err != nil {
		log.Printf("resources/list query: %v", err)
		return nil, fmt.Errorf("internal error")
	}
	res := []resource{}
	for _, sn := range all {
		res = append(res, resource{URI: resourceURI(sn.Space, sn.Name), Name: fmt.Sprintf("%s/%s", sn.Space, sn.Name), MimeType: "text/plain"})
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

func readResource(vault *store.Store, uri string) (interface{}, error) {
	space, name, ok := parseResourceURI(uri)
	if !ok {
		return nil, fmt.Errorf("invalid resource uri %q", uri)
	}
	content, err := vault.Reassemble(space, name)
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
func handle(vault *store.Store, req request) *response {
	switch req.Method {
	case "initialize":
		return resultMsg(req.ID, map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]interface{}{
				"tools":     map[string]interface{}{},
				"resources": map[string]interface{}{},
			},
			"serverInfo": map[string]string{"name": "memory-vault", "version": "0.10.0"},
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
		return resultMsg(req.ID, callTool(vault, params.Name, params.Arguments))

	case "resources/list":
		result, err := listResources(vault)
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
		result, err := readResource(vault, params.URI)
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

// tenantLimiter holds the per-tenant request buckets. Buckets untouched for
// an hour are swept, so a server that has seen many tenants over months does
// not accumulate one entry per tenant forever.
var tenantLimiter = ratelimit.New(time.Hour)

// planLimits returns a plan's caps, with each value overridable by
// environment variable (PLAN_FREE_RPM, PLAN_BUILDER_MAX_MEMORIES, ...) so an
// operator can change the economics without a code change. Setting any of
// them to 0 removes that limit for the plan.
func planLimits(plan string) store.PlanLimits {
	// Both the limits and the override prefix have to come from the SAME
	// name. Deriving the prefix from the raw value meant an unrecognised plan
	// loaded free's defaults but then looked for PLAN_<TYPO>_* overrides,
	// silently ignoring the PLAN_FREE_* limits the operator had configured.
	plan = store.CanonicalPlan(plan)
	l := store.LimitsForPlan(plan)
	prefix := "PLAN_" + strings.ToUpper(plan) + "_"
	l.RequestsPerMinute = envOrInt(prefix+"RPM", l.RequestsPerMinute)
	l.Burst = envOrInt(prefix+"BURST", l.Burst)
	l.MaxMemories = envOrInt(prefix+"MAX_MEMORIES", l.MaxMemories)
	l.MaxContentBytes = int64(envOrInt(prefix+"MAX_MB", int(l.MaxContentBytes>>20))) << 20
	return l
}

// retryAfterSeconds converts a refill delay into the whole seconds a
// Retry-After header carries. It rounds UP: truncating 1.3s to 1 tells a
// compliant client to retry before a token exists, which just earns it
// another 429. Never returns less than 1, since Retry-After: 0 invites an
// immediate retry.
func retryAfterSeconds(d time.Duration) int {
	seconds := int(math.Ceil(d.Seconds()))
	if seconds < 1 {
		return 1
	}
	return seconds
}

// matchesAuthToken reports whether token is one of the AUTH_TOKEN values.
// AUTH_TOKEN keeps exactly its pre-multi-tenancy meaning — a shared static
// credential for the bootstrap tenant — so existing self-hosted clients keep
// working untouched after the upgrade. Per-tenant credentials are API keys.
func matchesAuthToken(token string) bool {
	if token == "" {
		return false
	}
	for _, t := range authTokens() {
		if subtle.ConstantTimeCompare([]byte(token), []byte(t)) == 1 {
			return true
		}
	}
	return false
}

// authenticate resolves a request's credentials to the tenant-scoped store
// its tools should run against. The second return is false when the request
// must be rejected; every path that returns false returns a nil store, so a
// caller that ignores the bool cannot accidentally run unscoped.
//
// Precedence: AUTH_TOKEN first (cheapest, and the self-hosted path), then an
// API key lookup, then the anonymous case.
func authenticate(r *http.Request) (*store.Store, bool) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

	if matchesAuthToken(token) {
		return st, true
	}

	if token != "" {
		tenantID, plan, err := st.TenantForKey(token)
		if err != nil {
			log.Printf("auth: resolving api key: %v", err)
			return nil, false
		}
		if tenantID != "" {
			return st.ForTenant(tenantID).WithLimits(planLimits(plan)), true
		}
		// Unknown or revoked key: falls through to the open-vault check
		// below rather than rejecting outright. On a vault that serves
		// anonymous callers anyway, refusing a request *because* it carried
		// an unrecognized token would be stricter than refusing nothing at
		// all — and it would break a client still sending a stale bearer
		// header from before AUTH_TOKEN was cleared.
	}

	// Either no credential, or one that matched nothing. Anonymous access to
	// the bootstrap tenant is the pre-multi-tenancy behavior and stays
	// available for a fresh local vault — but only while the vault actually
	// is single-tenant. AUTH_TOKEN being set, or any API key having been
	// minted, turns it off, so a deploy that grows real tenants without
	// setting AUTH_TOKEN fails closed instead of handing the bootstrap tenant
	// to anyone who can reach /mcp.
	if len(authTokens()) > 0 {
		return nil, false
	}
	minted, err := st.HasMintedAPIKey()
	if err != nil {
		log.Printf("auth: checking for api keys: %v", err)
		return nil, false
	}
	if minted {
		return nil, false
	}
	return st, true
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

// healthzHandler answers "can this instance serve MCP requests" — a cheap
// Postgres ping, nothing more. It deliberately doesn't check Ollama:
// embedding/chat calls failing because Ollama is down is a lesser,
// separate concern from the whole instance being unreachable, and this
// endpoint exists for failover-watch.sh (deploy/failover-watch.sh) to
// decide whether to flip DNS to a standby. No AUTH_TOKEN check — health
// checkers won't carry a bearer token — but still subject to checkHost:
// a legitimate health checker hits the same allowlisted hostname clients
// do, so this doesn't weaken the DNS-rebinding guard, just doesn't gate
// health on possession of a secret.
func healthzHandler(w http.ResponseWriter, r *http.Request) {
	if !checkHost(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := st.Ping(); err != nil {
		log.Printf("healthz: db ping failed: %v", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "db": "unreachable"})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "db": "ok"})
}

func mcpHandler(w http.ResponseWriter, r *http.Request) {
	if !checkHost(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	vault, ok := authenticate(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="memory-vault"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Rate limiting comes after authentication on purpose: the bucket is per
	// tenant, so there is nothing to charge until we know whose request this
	// is. An unlimited plan (the bootstrap tenant) short-circuits inside
	// Allow without allocating anything.
	if limits := vault.Limits(); limits.RequestsPerMinute > 0 {
		tenant := vault.TenantID()
		if !tenantLimiter.Allow(tenant, limits.RequestsPerMinute, limits.Burst) {
			retry := tenantLimiter.RetryAfter(tenant, limits.RequestsPerMinute)
			w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(retry)))
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
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

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes())

	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid or too-large JSON-RPC message", http.StatusBadRequest)
		return
	}

	resp := handle(vault, req)
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

// cliStore picks which tenant the export/import CLI operates on.
//
// These commands go through the same RLS-scoped store as everything else, so
// they see exactly one tenant's memories. Defaulting silently to the
// bootstrap tenant on a vault that has others would mean deploy/backup.sh
// quietly backing up a fraction of the data — with no error, and no visible
// difference in the backup — and deploy/standby-sync.sh restoring that
// fraction. So the bootstrap default holds only while it is the only tenant;
// past that, -tenant is required rather than assumed.
func cliStore(tenantID string) *store.Store {
	if tenantID != "" {
		return st.ForTenant(tenantID)
	}
	tenants, err := st.Tenants()
	if err != nil {
		log.Fatalf("listing tenants: %v", err)
	}
	for _, t := range tenants {
		if t.ID != store.BootstrapTenantID {
			log.Fatalf("this vault has %d tenants, so an unscoped export/import would cover "+
				"only the bootstrap tenant's memories and silently omit the rest. "+
				"Pass -tenant <id> (see `memory-vault tenant list`).", len(tenants))
		}
	}
	return st
}

// runExportCLI backs `memory-vault export`, a non-MCP way to back up the
// vault (or one space of it) without going through an MCP client.
func runExportCLI(args []string) {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	space := fs.String("space", "", "space to export (all spaces if omitted)")
	source := fs.String("source", "", "source to export (all sources if omitted)")
	tenant := fs.String("tenant", "", "tenant to export (defaults to the bootstrap tenant; required once other tenants exist)")
	fs.Parse(args)

	items, err := cliStore(*tenant).Export(*space, *source)
	if err != nil {
		log.Fatalf("export: %v", err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(items); err != nil {
		log.Fatalf("export: %v", err)
	}
}

// runImportCLI backs `memory-vault import`, the counterpart to
// runExportCLI. Reads stdin unless -file is given.
func runImportCLI(args []string) {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	file := fs.String("file", "", "JSON file to import (reads stdin if omitted)")
	space := fs.String("space", "", "send every imported memory to this space, overriding what's in the data")
	source := fs.String("source", "", "send every imported memory to this source, overriding what's in the data")
	overwrite := fs.Bool("overwrite", false, "overwrite existing (space, name) pairs instead of skipping them")
	tenant := fs.String("tenant", "", "tenant to import into (defaults to the bootstrap tenant; required once other tenants exist)")
	fs.Parse(args)

	var r io.Reader = os.Stdin
	if *file != "" {
		f, err := os.Open(*file)
		if err != nil {
			log.Fatalf("import: %v", err)
		}
		defer f.Close()
		r = f
	}
	var items []store.MemoryExport
	if err := json.NewDecoder(r).Decode(&items); err != nil {
		log.Fatalf("import: invalid JSON: %v", err)
	}
	res, err := cliStore(*tenant).Import(items, *space, *source, *overwrite)
	if err != nil {
		log.Fatalf("import: %v", err)
	}
	fmt.Printf("imported %d, skipped %d\n", len(res.Imported), len(res.Skipped))
	if len(res.Skipped) > 0 {
		fmt.Printf("skipped (already exist, overwrite=false): %s\n", strings.Join(res.Skipped, ", "))
	}
}

// runTenantCLI backs `memory-vault tenant create|list`. Tenant and key
// administration is deliberately CLI-only for now: there is no admin HTTP
// surface to authenticate, so there is none to get wrong.
func runTenantCLI(args []string) {
	if len(args) == 0 {
		log.Fatal("usage: memory-vault tenant create|list")
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("tenant create", flag.ExitOnError)
		email := fs.String("email", "", "tenant's email address (required, unique)")
		plan := fs.String("plan", "free", "plan: free, builder, or team")
		label := fs.String("label", "initial", "label for the key minted alongside the tenant")
		fs.Parse(args[1:])
		if *email == "" {
			log.Fatal("tenant create: -email is required")
		}
		id, err := st.CreateTenant(*email, *plan)
		if err != nil {
			log.Fatalf("tenant create: %v", err)
		}
		// Mint the first key here: a tenant with no key cannot reach the
		// server at all, so creating one without it is never what's wanted.
		key, err := st.CreateAPIKey(id, *label)
		if err != nil {
			log.Fatalf("tenant create: tenant %s created but key minting failed: %v", id, err)
		}
		fmt.Printf("tenant   %s\nemail    %s\nplan     %s\napi key  %s\n\nStore the key now — only its hash is kept, so it cannot be shown again.\n", id, *email, *plan, key)

	case "list":
		tenants, err := st.Tenants()
		if err != nil {
			log.Fatalf("tenant list: %v", err)
		}
		for _, t := range tenants {
			fmt.Printf("%s\t%s\t%s\t%s\n", t.ID, t.Email, t.Plan, t.CreatedAt.Format(time.RFC3339))
		}

	default:
		log.Fatalf("unknown tenant subcommand %q (want create or list)", args[0])
	}
}

// runKeyCLI backs `memory-vault key create|list|revoke`.
func runKeyCLI(args []string) {
	if len(args) == 0 {
		log.Fatal("usage: memory-vault key create|list|revoke")
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("key create", flag.ExitOnError)
		tenant := fs.String("tenant", "", "tenant id to mint the key for (see `tenant list`)")
		label := fs.String("label", "", "what this key is for, e.g. \"laptop\" or \"ci\"")
		fs.Parse(args[1:])
		if *tenant == "" {
			log.Fatal("key create: -tenant is required")
		}
		key, err := st.CreateAPIKey(*tenant, *label)
		if err != nil {
			log.Fatalf("key create: %v", err)
		}
		fmt.Printf("%s\n\nStore it now — only its hash is kept, so it cannot be shown again.\n", key)

	case "list":
		fs := flag.NewFlagSet("key list", flag.ExitOnError)
		tenant := fs.String("tenant", "", "tenant id whose keys to list")
		fs.Parse(args[1:])
		if *tenant == "" {
			log.Fatal("key list: -tenant is required")
		}
		keys, err := st.APIKeys(*tenant)
		if err != nil {
			log.Fatalf("key list: %v", err)
		}
		for _, k := range keys {
			status := "active"
			if k.RevokedAt.Valid {
				status = "revoked " + k.RevokedAt.Time.Format(time.RFC3339)
			}
			fmt.Printf("%s\t%s\t%s\t%s\n", k.ID, k.Label, k.CreatedAt.Format(time.RFC3339), status)
		}

	case "revoke":
		fs := flag.NewFlagSet("key revoke", flag.ExitOnError)
		id := fs.String("id", "", "key id to revoke (see `key list`; not the key itself)")
		fs.Parse(args[1:])
		if *id == "" {
			log.Fatal("key revoke: -id is required")
		}
		revoked, err := st.RevokeAPIKey(*id)
		if err != nil {
			log.Fatalf("key revoke: %v", err)
		}
		if !revoked {
			log.Fatalf("key revoke: no active key with id %s", *id)
		}
		fmt.Printf("revoked key %s\n", *id)

	default:
		log.Fatalf("unknown key subcommand %q (want create, list, or revoke)", args[0])
	}
}

func main() {
	cfg, err := storeConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}
	st, err = store.Open(cfg)
	if err != nil {
		log.Fatalf("db init failed: %v", err)
	}
	defer st.Close()

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "export":
			runExportCLI(os.Args[2:])
			return
		case "import":
			runImportCLI(os.Args[2:])
			return
		case "tenant":
			runTenantCLI(os.Args[2:])
			return
		case "key":
			runKeyCLI(os.Args[2:])
			return
		}
	}

	addr := ":" + envOr("PORT", "8080")
	http.HandleFunc("/mcp", mcpHandler)
	http.HandleFunc("/healthz", healthzHandler)
	log.Printf("memory-vault listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

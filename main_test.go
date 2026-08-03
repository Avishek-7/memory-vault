package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"memory-vault/internal/store"
)

func TestChunkContent(t *testing.T) {
	short := "just a few words"
	if got := store.ChunkContent(short); len(got) != 1 || got[0] != short {
		t.Errorf("ChunkContent(short) = %v, want single unchanged chunk", got)
	}

	words := make([]string, 500)
	for i := range words {
		words[i] = "w"
	}
	long := strings.Join(words, " ")
	chunks := store.ChunkContent(long)
	if len(chunks) < 2 {
		t.Fatalf("ChunkContent(long) = %d chunks, want >1", len(chunks))
	}
	const chunkTargetWords = 150 // mirrors store's internal target
	for _, c := range chunks {
		n := len(strings.Fields(c))
		if n > chunkTargetWords {
			t.Errorf("chunk has %d words, want <= %d", n, chunkTargetWords)
		}
	}
}

func TestMatchesAuthToken(t *testing.T) {
	cases := []struct {
		name      string
		authToken string
		token     string
		want      bool
	}{
		{"no match when AUTH_TOKEN unset", "", "", false},
		{"no match for empty token", "secret", "", false},
		{"rejects wrong token", "secret", "wrong", false},
		{"accepts correct token", "secret", "secret", true},
		{"accepts one of multiple tokens", "a,b,c", "b", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("AUTH_TOKEN", c.authToken)
			if got := matchesAuthToken(c.token); got != c.want {
				t.Errorf("matchesAuthToken(%q) = %v, want %v", c.token, got, c.want)
			}
		})
	}
}

// TestAuthenticateWithoutDatabase pins the two authenticate() paths that must
// resolve without touching Postgres at all. It deliberately runs with the
// package-level store nil: if either path ever starts reaching for the
// database — an API-key lookup on a token AUTH_TOKEN already matched, or a
// HasAPIKeys call when no credential can possibly be valid — this panics
// instead of quietly costing a query per request. The database-backed paths
// are covered in internal/store's api key tests.
func TestAuthenticateWithoutDatabase(t *testing.T) {
	t.Setenv("AUTH_TOKEN", "secret")

	r, _ := http.NewRequest(http.MethodPost, "/mcp", nil)
	r.Header.Set("Authorization", "Bearer secret")
	vault, ok := authenticate(r)
	if !ok {
		t.Error("a valid AUTH_TOKEN was rejected")
	}
	if vault != st {
		t.Error("AUTH_TOKEN authenticated to something other than the bootstrap store")
	}

	// No credential, but AUTH_TOKEN is configured: rejected outright, with no
	// anonymous fallback to check for.
	bare, _ := http.NewRequest(http.MethodPost, "/mcp", nil)
	if vault, ok := authenticate(bare); ok || vault != nil {
		t.Errorf("credential-less request with AUTH_TOKEN set = (%v, %v), want (nil, false)", vault, ok)
	}
}

func TestCheckHost(t *testing.T) {
	cases := []struct {
		name         string
		allowedHosts string
		host         string
		want         bool
	}{
		{"skipped when ALLOWED_HOSTS unset", "", "evil.example.com", true},
		{"rejects host not in allowlist", "good.example.com", "evil.example.com", false},
		{"accepts host in allowlist", "good.example.com", "good.example.com", true},
		{"accepts one of multiple hosts", "a.com,b.com", "b.com", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("ALLOWED_HOSTS", c.allowedHosts)
			r, _ := http.NewRequest(http.MethodPost, "/mcp", nil)
			r.Host = c.host
			if got := checkHost(r); got != c.want {
				t.Errorf("checkHost() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestParseResourceURI(t *testing.T) {
	cases := []struct {
		uri       string
		wantSpace string
		wantName  string
		wantOK    bool
	}{
		{"memory://default/foo", "default", "foo", true},
		{"memory://work/my-note", "work", "my-note", true},
		{"memory://default/", "", "", false},
		{"memory:///foo", "", "", false},
		{"not-a-memory-uri", "", "", false},
	}
	for _, c := range cases {
		space, name, ok := parseResourceURI(c.uri)
		if space != c.wantSpace || name != c.wantName || ok != c.wantOK {
			t.Errorf("parseResourceURI(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.uri, space, name, ok, c.wantSpace, c.wantName, c.wantOK)
		}
	}
}

// setupIntegrationDB skips the test unless DATABASE_URL and (depending on
// EMBED_PROVIDER) the configured embedding backend are set, since these
// tests exercise real chunking/embedding/search round trips against a
// live Postgres+pgvector and either Ollama or an OpenAI-compatible
// endpoint.
func setupIntegrationDB(t *testing.T) {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	switch envOr("EMBED_PROVIDER", "ollama") {
	case "openai":
		if os.Getenv("OPENAI_EMBED_API_KEY") == "" {
			t.Skip("EMBED_PROVIDER=openai but OPENAI_EMBED_API_KEY not set, skipping integration test")
		}
	default:
		if os.Getenv("OLLAMA_URL") == "" {
			t.Skip("OLLAMA_URL not set, skipping integration test")
		}
	}
	cfg, err := storeConfig()
	if err != nil {
		t.Fatalf("storeConfig: %v", err)
	}
	st, err = store.Open(cfg)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
}

func TestIntegrationHealthz(t *testing.T) {
	setupIntegrationDB(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	healthzHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Errorf("body = %q, want it to contain status:ok", rec.Body.String())
	}
}

func TestIntegrationChunkingRoundTrip(t *testing.T) {
	setupIntegrationDB(t)
	words := make([]string, 400)
	for i := range words {
		words[i] = "word"
	}
	content := strings.Join(words, " ")
	res := callTool(st, "save_memory", map[string]interface{}{"name": "chunk-roundtrip", "content": content})
	if res["isError"] == true {
		t.Fatalf("save_memory failed: %v", res)
	}
	defer callTool(st, "delete_memory", map[string]interface{}{"name": "chunk-roundtrip"})

	got := callTool(st, "get_memory", map[string]interface{}{"name": "chunk-roundtrip"})
	text := got["content"].([]map[string]string)[0]["text"]
	if len(strings.Fields(text)) < len(words) {
		t.Errorf("reassembled content lost words: got %d, want >= %d", len(strings.Fields(text)), len(words))
	}
}

func TestIntegrationNamespaceIsolation(t *testing.T) {
	setupIntegrationDB(t)
	callTool(st, "save_memory", map[string]interface{}{"name": "dup", "content": "space A content", "space": "space-a"})
	callTool(st, "save_memory", map[string]interface{}{"name": "dup", "content": "space B content", "space": "space-b"})
	defer callTool(st, "delete_memory", map[string]interface{}{"name": "dup", "space": "space-a"})
	defer callTool(st, "delete_memory", map[string]interface{}{"name": "dup", "space": "space-b"})

	a := callTool(st, "get_memory", map[string]interface{}{"name": "dup", "space": "space-a"})
	b := callTool(st, "get_memory", map[string]interface{}{"name": "dup", "space": "space-b"})
	aText := a["content"].([]map[string]string)[0]["text"]
	bText := b["content"].([]map[string]string)[0]["text"]
	if aText == bText {
		t.Errorf("expected different content per space, got identical: %q", aText)
	}
}

func TestIntegrationResources(t *testing.T) {
	setupIntegrationDB(t)
	callTool(st, "save_memory", map[string]interface{}{"name": "res-test", "content": "resource content", "space": "res-space"})
	defer callTool(st, "delete_memory", map[string]interface{}{"name": "res-test", "space": "res-space"})

	listed, err := listResources(st)
	if err != nil {
		t.Fatalf("listResources: %v", err)
	}
	found := false
	for _, r := range listed.(map[string]interface{})["resources"].([]resource) {
		if r.URI == "memory://res-space/res-test" {
			found = true
		}
	}
	if !found {
		t.Errorf("resources/list missing memory://res-space/res-test")
	}

	read, err := readResource(st, "memory://res-space/res-test")
	if err != nil {
		t.Fatalf("readResource: %v", err)
	}
	text := read.(map[string]interface{})["contents"].([]map[string]string)[0]["text"]
	if text != "resource content" {
		t.Errorf("readResource text = %q, want %q", text, "resource content")
	}
}

// mustText fails the test immediately with the full tool result if res is
// an error, then returns its text content — so a failed save/get/export/
// import surfaces as a clear message instead of a type-assertion panic.
func mustText(t *testing.T, tool string, res map[string]interface{}) string {
	t.Helper()
	if res["isError"] == true {
		t.Fatalf("%s failed: %v", tool, res)
	}
	return res["content"].([]map[string]string)[0]["text"]
}

func TestIntegrationExportImportRoundTrip(t *testing.T) {
	setupIntegrationDB(t)
	mustText(t, "save_memory", callTool(st, "save_memory", map[string]interface{}{"name": "note-a", "content": "alpha content", "space": "export-src"}))
	mustText(t, "save_memory", callTool(st, "save_memory", map[string]interface{}{"name": "note-b", "content": "beta content", "space": "export-src"}))
	defer callTool(st, "delete_memory", map[string]interface{}{"name": "note-a", "space": "export-src"})
	defer callTool(st, "delete_memory", map[string]interface{}{"name": "note-b", "space": "export-src"})
	defer callTool(st, "delete_memory", map[string]interface{}{"name": "note-a", "space": "export-dst"})
	defer callTool(st, "delete_memory", map[string]interface{}{"name": "note-b", "space": "export-dst"})

	payload := mustText(t, "export_memories", callTool(st, "export_memories", map[string]interface{}{"space": "export-src"}))
	mustText(t, "import_memories", callTool(st, "import_memories", map[string]interface{}{"data": payload, "space": "export-dst"}))

	if got := mustText(t, "get_memory", callTool(st, "get_memory", map[string]interface{}{"name": "note-a", "space": "export-dst"})); got != "alpha content" {
		t.Errorf("get_memory(note-a) after import = %q, want %q", got, "alpha content")
	}
	if got := mustText(t, "get_memory", callTool(st, "get_memory", map[string]interface{}{"name": "note-b", "space": "export-dst"})); got != "beta content" {
		t.Errorf("get_memory(note-b) after import = %q, want %q", got, "beta content")
	}

	// re-importing without overwrite should skip both, since they now exist.
	summary := mustText(t, "import_memories", callTool(st, "import_memories", map[string]interface{}{"data": payload, "space": "export-dst"}))
	if !strings.Contains(summary, "skipped 2") {
		t.Errorf("second import summary = %q, want it to report 2 skipped", summary)
	}
}

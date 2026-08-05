package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

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

// TestQuotaResult covers the rendering of a quota rejection without needing a
// database or an embedding backend. The important property is that a quota is
// NOT treated as a server fault: internalErr would log it and hand the user a
// generic "internal error" they can neither interpret nor act on.
func TestQuotaResult(t *testing.T) {
	if got := quotaResult(nil); got != nil {
		t.Errorf("quotaResult(nil) = %v, want nil", got)
	}
	if got := quotaResult(errors.New("connection refused")); got != nil {
		t.Errorf("quotaResult(non-quota error) = %v, want nil so it falls through to internalErr", got)
	}

	mem := quotaResult(&store.QuotaError{Resource: "memories", Limit: 200, Current: 200, Adding: 1})
	if mem == nil {
		t.Fatal("quotaResult did not recognise a *store.QuotaError")
	}
	if mem["isError"] != true {
		t.Error("a quota rejection should be marked isError")
	}
	text := mem["content"].([]map[string]string)[0]["text"]
	for _, want := range []string{"200", "delete_memory", "compact_memories"} {
		if !strings.Contains(text, want) {
			t.Errorf("memory-quota message %q does not mention %q, so the user is not told how to recover", text, want)
		}
	}

	bytes := quotaResult(&store.QuotaError{Resource: "storage", Limit: 10 << 20, Current: 10 << 20, Adding: 2048})
	btext := bytes["content"].([]map[string]string)[0]["text"]
	if !strings.Contains(btext, "10.0 MB") {
		t.Errorf("storage-quota message %q does not report the limit in human units", btext)
	}

	// A wrapped quota error must still be recognised: saveMemory's callers
	// may wrap on the way out.
	wrapped := fmt.Errorf("saving: %w", &store.QuotaError{Resource: "memories", Limit: 5, Current: 5, Adding: 1})
	if quotaResult(wrapped) == nil {
		t.Error("quotaResult missed a wrapped *store.QuotaError; errors.As should see through the wrapping")
	}
}

// TestPlanLimitsOverridesUseTheCanonicalPlan covers a mismatch that made
// operator configuration silently ineffective: an unrecognised plan loaded
// free's limits but derived its override prefix from the raw name, so it
// looked for PLAN_<TYPO>_* and ignored the PLAN_FREE_* values actually set.
func TestPlanLimitsOverridesUseTheCanonicalPlan(t *testing.T) {
	t.Setenv("PLAN_FREE_RPM", "7")
	t.Setenv("PLAN_FREE_MAX_MEMORIES", "11")

	known := planLimits("free")
	if known.RequestsPerMinute != 7 || known.MaxMemories != 11 {
		t.Fatalf("planLimits(\"free\") = %+v, want the configured overrides applied", known)
	}

	// An unrecognised plan falls back to free's limits, so it must also pick
	// up free's overrides rather than looking for PLAN_TYPO_*.
	unknown := planLimits("not-a-real-plan")
	if unknown != known {
		t.Errorf("planLimits(unknown) = %+v, want the same as free %+v — the override prefix "+
			"is being derived from the unrecognised name", unknown, known)
	}
}

func TestRetryAfterSecondsRoundsUp(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want int
	}{
		{0, 1},                         // never advertise an immediate retry
		{-time.Second, 1},              // nor a negative one
		{100 * time.Millisecond, 1},    // sub-second still waits a second
		{time.Second, 1},               // exact
		{1330 * time.Millisecond, 2},   // the bug: truncation said 1, client retries early
		{11500 * time.Millisecond, 12}, // rounds up, not to nearest
	}
	for _, c := range cases {
		if got := retryAfterSeconds(c.in); got != c.want {
			t.Errorf("retryAfterSeconds(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

// fixedEmbedder is a stand-in for Ollama so compaction can be exercised
// without a model server; the vectors it returns are never compared.
type fixedEmbedder struct{}

func (fixedEmbedder) Embed(string) ([]float32, error) {
	return make([]float32, store.DefaultEmbedDim), nil
}

// TestCompactionRefusesToGrowStorage covers the hole the quota exemption
// would otherwise open. compactGroup writes the merged memory with limits
// disabled — necessary, because a tenant at their cap must still be able to
// compact — and then deletes the sources. A chat model that returns MORE than
// it was given would therefore leave the tenant permanently over quota, and
// nothing bounds a model's output length.
func TestCompactionRefusesToGrowStorage(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	vault, err := store.Open(store.Config{DatabaseURL: url, Embedder: fixedEmbedder{}})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer vault.Close()

	const space = "compact-growth"
	sources := []string{"alpha content here", "beta content here"}
	sourceBytes := 0
	for i, c := range sources {
		name := fmt.Sprintf("src-%d", i)
		if _, err := vault.SaveMemory(space, name, c, store.DefaultSource, store.DefaultKind); err != nil {
			t.Fatalf("SaveMemory: %v", err)
		}
		sourceBytes += len(c)
		defer vault.DeleteMemory(space, name)
	}

	centroids, err := vault.MemoryCentroids(space)
	if err != nil {
		t.Fatalf("MemoryCentroids: %v", err)
	}
	if len(centroids) != len(sources) {
		t.Fatalf("got %d centroids, want %d", len(centroids), len(sources))
	}

	original := ollamaChat
	defer func() { ollamaChat = original }()
	// A model that rambles: far more output than it was given.
	ollamaChat = func(string) (string, error) {
		return strings.Repeat("bloat ", sourceBytes), nil
	}

	line, err := compactGroup(vault, space, centroids, false)
	if err != nil {
		t.Fatalf("compactGroup: %v", err)
	}
	if !strings.Contains(line, "skipped") {
		t.Errorf("compactGroup reported %q, want the group skipped for growing storage", line)
	}
	defer vault.DeleteMemory(space, compactTargetName([]string{centroids[0].Name, centroids[1].Name}))

	// The sources must survive: nothing was written, so nothing may be lost.
	for i := range sources {
		name := fmt.Sprintf("src-%d", i)
		if got, _ := vault.Reassemble(space, name); got == "" {
			t.Errorf("source %s was deleted even though the merge was skipped", name)
		}
	}
	// And the oversized merge must not be stored.
	merged := compactTargetName([]string{centroids[0].Name, centroids[1].Name})
	if got, _ := vault.Reassemble(space, merged); got != "" {
		t.Errorf("the oversized merge was written anyway (%d bytes)", len(got))
	}
}

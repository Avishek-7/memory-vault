package main

import (
	"net/http"
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

func TestCheckAuth(t *testing.T) {
	cases := []struct {
		name      string
		authToken string
		header    string
		want      bool
	}{
		{"disabled when AUTH_TOKEN unset", "", "", true},
		{"rejects missing header", "secret", "", false},
		{"rejects wrong token", "secret", "Bearer wrong", false},
		{"accepts correct token", "secret", "Bearer secret", true},
		{"accepts one of multiple tokens", "a,b,c", "Bearer b", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("AUTH_TOKEN", c.authToken)
			r, _ := http.NewRequest(http.MethodPost, "/mcp", nil)
			if c.header != "" {
				r.Header.Set("Authorization", c.header)
			}
			if got := checkAuth(r); got != c.want {
				t.Errorf("checkAuth() = %v, want %v", got, c.want)
			}
		})
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

// setupIntegrationDB skips the test unless DATABASE_URL and OLLAMA_URL are
// set, since these tests exercise real chunking/embedding/search round
// trips against a live Postgres+pgvector and Ollama instance.
func setupIntegrationDB(t *testing.T) {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" || os.Getenv("OLLAMA_URL") == "" {
		t.Skip("DATABASE_URL/OLLAMA_URL not set, skipping integration test")
	}
	var err error
	st, err = store.Open(storeConfig())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
}

func TestIntegrationChunkingRoundTrip(t *testing.T) {
	setupIntegrationDB(t)
	words := make([]string, 400)
	for i := range words {
		words[i] = "word"
	}
	content := strings.Join(words, " ")
	res := callTool("save_memory", map[string]interface{}{"name": "chunk-roundtrip", "content": content})
	if res["isError"] == true {
		t.Fatalf("save_memory failed: %v", res)
	}
	defer callTool("delete_memory", map[string]interface{}{"name": "chunk-roundtrip"})

	got := callTool("get_memory", map[string]interface{}{"name": "chunk-roundtrip"})
	text := got["content"].([]map[string]string)[0]["text"]
	if len(strings.Fields(text)) < len(words) {
		t.Errorf("reassembled content lost words: got %d, want >= %d", len(strings.Fields(text)), len(words))
	}
}

func TestIntegrationNamespaceIsolation(t *testing.T) {
	setupIntegrationDB(t)
	callTool("save_memory", map[string]interface{}{"name": "dup", "content": "space A content", "space": "space-a"})
	callTool("save_memory", map[string]interface{}{"name": "dup", "content": "space B content", "space": "space-b"})
	defer callTool("delete_memory", map[string]interface{}{"name": "dup", "space": "space-a"})
	defer callTool("delete_memory", map[string]interface{}{"name": "dup", "space": "space-b"})

	a := callTool("get_memory", map[string]interface{}{"name": "dup", "space": "space-a"})
	b := callTool("get_memory", map[string]interface{}{"name": "dup", "space": "space-b"})
	aText := a["content"].([]map[string]string)[0]["text"]
	bText := b["content"].([]map[string]string)[0]["text"]
	if aText == bText {
		t.Errorf("expected different content per space, got identical: %q", aText)
	}
}

func TestIntegrationResources(t *testing.T) {
	setupIntegrationDB(t)
	callTool("save_memory", map[string]interface{}{"name": "res-test", "content": "resource content", "space": "res-space"})
	defer callTool("delete_memory", map[string]interface{}{"name": "res-test", "space": "res-space"})

	listed, err := listResources()
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

	read, err := readResource("memory://res-space/res-test")
	if err != nil {
		t.Fatalf("readResource: %v", err)
	}
	text := read.(map[string]interface{})["contents"].([]map[string]string)[0]["text"]
	if text != "resource content" {
		t.Errorf("readResource text = %q, want %q", text, "resource content")
	}
}

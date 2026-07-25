package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestChunkContent(t *testing.T) {
	short := "just a few words"
	if got := chunkContent(short); len(got) != 1 || got[0] != short {
		t.Errorf("chunkContent(short) = %v, want single unchanged chunk", got)
	}

	words := make([]string, 500)
	for i := range words {
		words[i] = "w"
	}
	long := strings.Join(words, " ")
	chunks := chunkContent(long)
	if len(chunks) < 2 {
		t.Fatalf("chunkContent(long) = %d chunks, want >1", len(chunks))
	}
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

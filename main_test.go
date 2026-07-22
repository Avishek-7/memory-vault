package main

import (
	"net/http"
	"testing"
)

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

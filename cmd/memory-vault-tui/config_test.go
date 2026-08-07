package main

import "testing"

func TestValidateDatabaseURLShape(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"empty", "", true},
		{"malformed scheme", "mysql://user:pass@host:5432/db", true},
		{"no scheme at all", "hello", true},
		{"missing host", "postgres:///db", true},
		{"valid postgres scheme", "postgres://user:pass@host:5432/db", false},
		{"valid postgresql scheme", "postgresql://user:pass@host:5432/db", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateDatabaseURLShape(c.url)
			if (err != nil) != c.wantErr {
				t.Errorf("validateDatabaseURLShape(%q) = %v, wantErr %v", c.url, err, c.wantErr)
			}
		})
	}
}

func TestRedactDatabaseURL(t *testing.T) {
	got := redactDatabaseURL("postgres://user:secret@host:5432/db")
	want := "postgres://user:***@host:5432/db"
	if got != want {
		t.Errorf("redactDatabaseURL: got %q, want %q", got, want)
	}
	// No password: nothing to redact, string passes through unchanged.
	got = redactDatabaseURL("postgres://user@host:5432/db")
	want = "postgres://user@host:5432/db"
	if got != want {
		t.Errorf("redactDatabaseURL (no password): got %q, want %q", got, want)
	}
}

func TestConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.toml"

	cfg := &tuiConfig{
		Active: "home",
		Profiles: map[string]tuiProfile{
			"home":         {DatabaseURL: "postgres://user:pass@192.168.1.44:5432/memory_vault", OllamaURL: "http://192.168.1.44:11434"},
			"laptop-local": {DatabaseURL: `postgres://user:p"ss\word@localhost:5432/memory_vault`, OllamaURL: ""},
		},
	}
	if err := writeConfig(path, cfg); err != nil {
		t.Fatalf("writeConfig: %v", err)
	}

	got, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got.Active != cfg.Active {
		t.Errorf("Active = %q, want %q", got.Active, cfg.Active)
	}
	for name, want := range cfg.Profiles {
		gotProfile, ok := got.Profiles[name]
		if !ok {
			t.Errorf("profile %q missing after round trip", name)
			continue
		}
		if gotProfile.DatabaseURL != want.DatabaseURL {
			t.Errorf("profile %q DatabaseURL = %q, want %q", name, gotProfile.DatabaseURL, want.DatabaseURL)
		}
		if gotProfile.OllamaURL != want.OllamaURL {
			t.Errorf("profile %q OllamaURL = %q, want %q", name, gotProfile.OllamaURL, want.OllamaURL)
		}
	}
}

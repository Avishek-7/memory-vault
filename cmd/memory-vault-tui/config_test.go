package main

import (
	"bufio"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

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
		{"missing hostname with port", "postgres://:5432/db", true},
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

func TestRedactURLPassword(t *testing.T) {
	got := redactURLPassword("postgres://user:secret@host:5432/db")
	want := "postgres://user:***@host:5432/db"
	if got != want {
		t.Errorf("redactURLPassword: got %q, want %q", got, want)
	}
	// No password: nothing to redact, string passes through unchanged.
	got = redactURLPassword("postgres://user@host:5432/db")
	want = "postgres://user@host:5432/db"
	if got != want {
		t.Errorf("redactURLPassword (no password): got %q, want %q", got, want)
	}
	// Ollama also accepts Basic Auth in its URL — same redaction applies.
	got = redactURLPassword("http://user:secret@192.168.1.44:11434")
	want = "http://user:***@192.168.1.44:11434"
	if got != want {
		t.Errorf("redactURLPassword (ollama URL): got %q, want %q", got, want)
	}
	got = redactURLPassword("postgres://user:se@cret@host:5432/db")
	want = "postgres://user:***@host:5432/db"
	if got != want {
		t.Errorf("redactURLPassword (password with @): got %q, want %q", got, want)
	}
	got = redactURLPassword("postgres://user@host:5432/db?password=secret&sslmode=disable")
	want = "postgres://user@host:5432/db?password=***&sslmode=disable"
	if got != want {
		t.Errorf("redactURLPassword (query password): got %q, want %q", got, want)
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

// TestWriteConfigFixesLoosePermissions guards against os.WriteFile's mode
// argument being a no-op on an existing file: it pre-creates the config at
// 0644, writes through writeConfig, and requires the mode end at 0600.
func TestWriteConfigFixesLoosePermissions(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.toml"

	if err := os.WriteFile(path, []byte("stale"), 0644); err != nil {
		t.Fatalf("pre-creating file: %v", err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatalf("chmod pre-created file: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat pre-created file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0644 {
		t.Fatalf("mode before writeConfig = %o, want 0644", got)
	}

	cfg := &tuiConfig{Active: "home", Profiles: map[string]tuiProfile{"home": {DatabaseURL: "postgres://user:pass@host:5432/db"}}}
	if err := writeConfig(path, cfg); err != nil {
		t.Fatalf("writeConfig: %v", err)
	}

	info, err = os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Errorf("mode after writeConfig over a pre-existing 0644 file = %o, want 0600", got)
	}
}

func TestPromptYesNoEOF(t *testing.T) {
	_, err := promptYesNo(bufio.NewReader(strings.NewReader("")), "confirm? ", true)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("promptYesNo error = %v, want EOF", err)
	}
}

// TestPromptYesNoAcceptsFinalLineWithoutNewline guards against a real
// regression: ReadString returns a non-empty final line bundled with
// io.EOF when the input has no trailing newline (e.g. `printf 'yes'`
// piped in, as opposed to `echo`). That's a valid answer, not a failure.
func TestPromptYesNoAcceptsFinalLineWithoutNewline(t *testing.T) {
	got, err := promptYesNo(bufio.NewReader(strings.NewReader("yes")), "confirm? ", false)
	if err != nil {
		t.Fatalf("promptYesNo error = %v, want nil for a valid final answer", err)
	}
	if !got {
		t.Errorf("promptYesNo(%q) = false, want true", "yes")
	}
}

func TestPromptWizardEOF(t *testing.T) {
	_, _, err := promptWizard(bufio.NewReader(strings.NewReader("")), "home")
	if !errors.Is(err, io.EOF) {
		t.Fatalf("promptWizard error = %v, want EOF", err)
	}
}

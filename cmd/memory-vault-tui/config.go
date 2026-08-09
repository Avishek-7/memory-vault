// Connection profile config: where the TUI keeps saved DATABASE_URLs so it
// only has to be told about a Postgres instance once per machine.
//
// The on-disk format is TOML-shaped but the reader/writer here are not a
// general TOML implementation — they only understand the flat
// active/[profiles.NAME]/database_url shape this file ever writes. That's
// enough since nothing but this program is expected to edit the file, and
// it avoids a dependency for three fields.
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"memory-vault/internal/store"
)

type tuiConfig struct {
	Active   string
	Profiles map[string]tuiProfile
}

type tuiProfile struct {
	DatabaseURL string
	// OllamaURL is where this profile's memory-vault instance's Ollama
	// lives — often not localhost, since the TUI is a local client but the
	// vault (and the Ollama serving its embeddings/chat) may run on a
	// server elsewhere. Empty means the default, http://localhost:11434.
	OllamaURL string
}

const defaultOllamaURL = "http://localhost:11434"

var profileNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func configFilePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolving config directory: %w", err)
	}
	return filepath.Join(dir, "memory-vault", "config.toml"), nil
}

// loadConfig returns the raw os.ReadFile error on a missing file so callers
// can check os.IsNotExist themselves, the same idiom as reading any file.
func loadConfig(path string) (*tuiConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &tuiConfig{Profiles: map[string]tuiProfile{}}
	current := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[profiles.") && strings.HasSuffix(line, "]") {
			current = strings.TrimSuffix(strings.TrimPrefix(line, "[profiles."), "]")
			cfg.Profiles[current] = tuiProfile{}
			continue
		}
		key, val, ok := splitTOMLLine(line)
		if !ok {
			continue
		}
		switch {
		case current == "" && key == "active":
			cfg.Active = val
		case current != "" && key == "database_url":
			p := cfg.Profiles[current]
			p.DatabaseURL = val
			cfg.Profiles[current] = p
		case current != "" && key == "ollama_url":
			p := cfg.Profiles[current]
			p.OllamaURL = val
			cfg.Profiles[current] = p
		}
	}
	return cfg, nil
}

func splitTOMLLine(line string) (key, val string, ok bool) {
	idx := strings.Index(line, "=")
	if idx < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:idx])
	val, ok = unquoteTOMLString(strings.TrimSpace(line[idx+1:]))
	return key, val, ok
}

func unquoteTOMLString(s string) (string, bool) {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return "", false
	}
	inner := s[1 : len(s)-1]
	var b strings.Builder
	for i := 0; i < len(inner); i++ {
		if inner[i] == '\\' && i+1 < len(inner) {
			i++
			switch inner[i] {
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			default:
				b.WriteByte(inner[i])
			}
			continue
		}
		b.WriteByte(inner[i])
	}
	return b.String(), true
}

func quoteTOMLString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// writeConfig writes the whole config through a private temp file in the
// target directory, then renames it into place so the config is never
// replaced by a partially-written file. The file holds Postgres
// credentials, so every write keeps it at 0600.
func writeConfig(path string, cfg *tuiConfig) error {
	var b strings.Builder
	b.WriteString("active = ")
	b.WriteString(quoteTOMLString(cfg.Active))
	b.WriteByte('\n')
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		b.WriteString("\n[profiles.")
		b.WriteString(name)
		b.WriteString("]\n")
		b.WriteString("database_url = ")
		b.WriteString(quoteTOMLString(cfg.Profiles[name].DatabaseURL))
		b.WriteByte('\n')
		b.WriteString("ollama_url = ")
		b.WriteString(quoteTOMLString(cfg.Profiles[name].OllamaURL))
		b.WriteByte('\n')
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config.toml-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(b.String()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// validateDatabaseURLShape rejects obviously-wrong input before it ever
// reaches the driver, so a typo gets a specific message instead of whatever
// lib/pq happens to say.
func validateDatabaseURLShape(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("connection URL cannot be empty")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return fmt.Errorf("that doesn't look like a Postgres connection URL — expected something starting with postgres://")
	}
	if u.Hostname() == "" {
		return fmt.Errorf("that doesn't look like a Postgres connection URL — missing a host")
	}
	return nil
}

// testConnection opens and immediately closes a real Store against the
// candidate URL. store.Open already pings, checks the connecting role can't
// bypass row-level security, and runs the idempotent schema migration — the
// same checks a real launch would hit, so a wizard pass that succeeds here
// is a wizard pass that will actually work.
func testConnection(databaseURL string) error {
	// The embedder is never invoked by store.Open (only by save/search
	// later), so no real Ollama URL is needed just to test the DB.
	cfg, err := storeConfig(databaseURL, defaultOllamaURL)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg)
	if err != nil {
		return fmt.Errorf("couldn't connect: %w. Check the host is reachable and the credentials are correct", err)
	}
	return st.Close()
}

// redactURLPassword masks a password for display without touching anything
// else in the URL — used for both the Postgres and Ollama URLs a profile
// holds, since Ollama also accepts Basic Auth in its URL. It works on the
// raw string rather than round-tripping through net/url, because
// URL.String() percent-encodes characters like "*" in userinfo — which
// would print a mangled mask instead of "***".
func redactURLPassword(raw string) string {
	redacted := raw
	schemeEnd := strings.Index(raw, "://")
	if schemeEnd < 0 {
		return redactSensitiveQueryParams(redacted)
	}
	authorityStart := schemeEnd + 3
	authorityEnd := len(raw)
	for _, sep := range []string{"/", "?", "#"} {
		if idx := strings.Index(raw[authorityStart:], sep); idx >= 0 && authorityStart+idx < authorityEnd {
			authorityEnd = authorityStart + idx
		}
	}
	authority := raw[authorityStart:authorityEnd]
	at := strings.LastIndex(authority, "@")
	if at >= 0 {
		userinfo := authority[:at]
		if colon := strings.Index(userinfo, ":"); colon >= 0 {
			redacted = raw[:authorityStart] + userinfo[:colon] + ":***" + authority[at:] + raw[authorityEnd:]
		}
	}
	return redactSensitiveQueryParams(redacted)
}

func redactSensitiveQueryParams(raw string) string {
	queryStart := strings.Index(raw, "?")
	if queryStart < 0 {
		return raw
	}
	queryEnd := len(raw)
	if frag := strings.Index(raw[queryStart+1:], "#"); frag >= 0 {
		queryEnd = queryStart + 1 + frag
	}
	parts := strings.Split(raw[queryStart+1:queryEnd], "&")
	for i, part := range parts {
		key := part
		eq := strings.Index(part, "=")
		if eq >= 0 {
			key = part[:eq]
		}
		decodedKey, err := url.QueryUnescape(key)
		if err == nil && strings.EqualFold(decodedKey, "password") && eq >= 0 {
			parts[i] = part[:eq+1] + "***"
		}
	}
	return raw[:queryStart+1] + strings.Join(parts, "&") + raw[queryEnd:]
}

// promptLine reads one line. ReadString returns io.EOF alongside whatever
// was read when the input ends without a trailing newline — a perfectly
// valid final answer from a pipe like `printf 'yes'`, not a failure — so
// EOF is only treated as fatal when nothing came back with it. A prior
// answer here (bare "line, _ := ...") silently discarded EOF entirely and
// could loop forever re-prompting into closed/empty stdin; this keeps that
// case an error while no longer rejecting a real trailing answer.
func promptLine(reader *bufio.Reader, prompt string) (string, error) {
	if prompt != "" {
		fmt.Print(prompt)
	}
	line, err := reader.ReadString('\n')
	trimmed := strings.TrimSpace(line)
	if err == io.EOF && trimmed != "" {
		err = nil
	}
	return trimmed, err
}

func promptYesNo(reader *bufio.Reader, prompt string, defaultYes bool) (bool, error) {
	answer, err := promptLine(reader, prompt)
	if err != nil {
		return false, err
	}
	answer = strings.ToLower(answer)
	if answer == "" {
		return defaultYes, nil
	}
	return answer == "y" || answer == "yes", nil
}

// promptWizard collects and validates one profile: a name, a Postgres URL
// that must be shaped correctly and actually connect before it returns, and
// an Ollama URL for the vault's embeddings/chat. Shared by first-run setup
// and `config add` — the only difference between those callers is what they
// do with the result.
func promptWizard(reader *bufio.Reader, defaultName string) (name string, profile tuiProfile, err error) {
	for {
		name, err = promptLine(reader, fmt.Sprintf("Profile name [%s]: ", defaultName))
		if err != nil {
			return "", tuiProfile{}, err
		}
		if name == "" {
			name = defaultName
		}
		if profileNameRe.MatchString(name) {
			break
		}
		fmt.Println("Profile name can only contain letters, numbers, - and _.")
	}

	for {
		profile.DatabaseURL, err = promptLine(reader, "Postgres connection URL (postgres://user:pass@host:5432/dbname): ")
		if err != nil {
			return "", tuiProfile{}, err
		}
		if shapeErr := validateDatabaseURLShape(profile.DatabaseURL); shapeErr != nil {
			fmt.Println(shapeErr)
			continue
		}
		fmt.Println("Testing connection...")
		if connErr := testConnection(profile.DatabaseURL); connErr != nil {
			fmt.Println(connErr)
			retry, err := promptYesNo(reader, "Try again? [Y/n]: ", true)
			if err != nil {
				return "", tuiProfile{}, err
			}
			if !retry {
				return "", tuiProfile{}, fmt.Errorf("setup cancelled")
			}
			continue
		}
		fmt.Println("Connected.")
		break
	}

	profile.OllamaURL, err = promptLine(reader, fmt.Sprintf("Ollama URL [%s]: ", defaultOllamaURL))
	if err != nil {
		return "", tuiProfile{}, err
	}
	if profile.OllamaURL == "" {
		profile.OllamaURL = defaultOllamaURL
	}
	if err := pingOllama(profile.OllamaURL); err != nil {
		// Ollama being unreachable right now doesn't block saving the
		// profile — same reasoning as /healthz not checking it: embeddings
		// failing is a lesser, separate concern from the vault itself being
		// unreachable, and Ollama may simply not be running yet. The
		// underlying error is not printed: an http.Client connection error
		// embeds the request URL verbatim, which would leak a Basic Auth
		// credential the same way printing the raw URL would.
		fmt.Printf("warning: could not reach Ollama at %s — saving anyway.\n", redactURLPassword(profile.OllamaURL))
	}

	return name, profile, nil
}

// pingOllama does a quick reachability check, nothing more.
func pingOllama(ollamaURL string) error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, strings.TrimSuffix(ollamaURL, "/")+"/api/tags", nil)
	if err != nil {
		return err
	}
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	return nil
}

// resolveActiveProfile decides which Postgres/Ollama URLs a normal launch
// (not a `config ...` subcommand) should connect to.
//
// DATABASE_URL always wins when set, for both fields: it's the pre-config
// escape hatch that already worked before profiles existed (CI, one-off
// scripts), and OLLAMA_URL is checked alongside it rather than mixed with a
// saved profile — a script overriding one dependency's address is very
// likely overriding the other on purpose too. Otherwise the on-disk config
// supplies both, running the first-run wizard if no config exists yet.
func resolveActiveProfile(profileOverride string) (tuiProfile, error) {
	if envURL := os.Getenv("DATABASE_URL"); envURL != "" {
		return tuiProfile{DatabaseURL: envURL, OllamaURL: envOr("OLLAMA_URL", defaultOllamaURL)}, nil
	}

	path, err := configFilePath()
	if err != nil {
		return tuiProfile{}, err
	}

	cfg, err := loadConfig(path)
	if os.IsNotExist(err) {
		fmt.Println("memory-vault-tui isn't configured yet. Let's set up a connection.")
		reader := bufio.NewReader(os.Stdin)
		name, profile, wizErr := promptWizard(reader, "home")
		if wizErr != nil {
			return tuiProfile{}, wizErr
		}
		cfg := &tuiConfig{Active: name, Profiles: map[string]tuiProfile{name: profile}}
		if err := writeConfig(path, cfg); err != nil {
			return tuiProfile{}, fmt.Errorf("saving config: %w", err)
		}
		fmt.Printf("Saved profile %q to %s\n\n", name, path)
		return profile, nil
	}
	if err != nil {
		return tuiProfile{}, fmt.Errorf("reading config: %w", err)
	}

	name := cfg.Active
	if profileOverride != "" {
		name = profileOverride
	}
	if name == "" {
		return tuiProfile{}, fmt.Errorf("no active profile set; run \"memory-vault-tui config add\" or \"memory-vault-tui config use <name>\"")
	}
	profile, ok := cfg.Profiles[name]
	if !ok {
		return tuiProfile{}, fmt.Errorf("profile %q not found; run \"memory-vault-tui config list\" to see available profiles", name)
	}
	if profile.OllamaURL == "" {
		profile.OllamaURL = defaultOllamaURL
	}
	if envOllama := os.Getenv("OLLAMA_URL"); envOllama != "" {
		profile.OllamaURL = envOllama
	}
	return profile, nil
}

func runConfigCommand(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: memory-vault-tui config <add|list|use|remove> [args]")
	}
	path, err := configFilePath()
	if err != nil {
		return err
	}
	switch args[0] {
	case "add":
		return configAdd(path)
	case "list":
		return configList(path)
	case "use":
		if len(args) < 2 {
			return fmt.Errorf("usage: memory-vault-tui config use <name>")
		}
		return configUse(path, args[1])
	case "remove":
		if len(args) < 2 {
			return fmt.Errorf("usage: memory-vault-tui config remove <name> [--force]")
		}
		force := len(args) > 2 && args[2] == "--force"
		return configRemove(path, args[1], force)
	default:
		return fmt.Errorf("unknown config subcommand %q (want add, list, use, or remove)", args[0])
	}
}

func configAdd(path string) error {
	cfg, err := loadConfig(path)
	if os.IsNotExist(err) {
		cfg = &tuiConfig{Profiles: map[string]tuiProfile{}}
	} else if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}

	reader := bufio.NewReader(os.Stdin)
	name, profile, err := promptWizard(reader, "home")
	if err != nil {
		return err
	}

	if _, exists := cfg.Profiles[name]; exists {
		overwrite, err := promptYesNo(reader, fmt.Sprintf("Profile %q already exists. Overwrite? [y/N]: ", name), false)
		if err != nil {
			return err
		}
		if !overwrite {
			return fmt.Errorf("cancelled")
		}
	}

	wasEmpty := len(cfg.Profiles) == 0
	cfg.Profiles[name] = profile
	if wasEmpty {
		cfg.Active = name
	}
	if err := writeConfig(path, cfg); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	fmt.Printf("Saved profile %q to %s\n", name, path)
	return nil
}

func configList(path string) error {
	cfg, err := loadConfig(path)
	if os.IsNotExist(err) {
		fmt.Println("No config found yet. Run memory-vault-tui once, or `memory-vault-tui config add`, to set up a profile.")
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}
	if len(cfg.Profiles) == 0 {
		fmt.Println("No profiles configured. Run `memory-vault-tui config add`.")
		return nil
	}
	names := make([]string, 0, len(cfg.Profiles))
	width := 0
	for name := range cfg.Profiles {
		names = append(names, name)
		if len(name) > width {
			width = len(name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		marker := " "
		if name == cfg.Active {
			marker = "*"
		}
		ollamaURL := cfg.Profiles[name].OllamaURL
		if ollamaURL == "" {
			ollamaURL = defaultOllamaURL
		}
		fmt.Printf("%s %-*s  %s  (ollama: %s)\n", marker, width, name, redactURLPassword(cfg.Profiles[name].DatabaseURL), redactURLPassword(ollamaURL))
	}
	return nil
}

func configUse(path, name string) error {
	cfg, err := loadConfig(path)
	if os.IsNotExist(err) {
		return fmt.Errorf("no config found; run \"memory-vault-tui config add\" first")
	}
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}
	if _, ok := cfg.Profiles[name]; !ok {
		return fmt.Errorf("profile %q not found; run \"memory-vault-tui config list\" to see available profiles", name)
	}
	cfg.Active = name
	if err := writeConfig(path, cfg); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	fmt.Printf("Active profile is now %q\n", name)
	return nil
}

func configRemove(path, name string, force bool) error {
	cfg, err := loadConfig(path)
	if os.IsNotExist(err) {
		return fmt.Errorf("no config found; nothing to remove")
	}
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}
	if _, ok := cfg.Profiles[name]; !ok {
		return fmt.Errorf("profile %q not found; run \"memory-vault-tui config list\" to see available profiles", name)
	}
	if name == cfg.Active && !force {
		reader := bufio.NewReader(os.Stdin)
		confirm, err := promptYesNo(reader, fmt.Sprintf("%q is the active profile. Remove anyway? [y/N]: ", name), false)
		if err != nil {
			return err
		}
		if !confirm {
			return fmt.Errorf("cancelled (use --force to skip this prompt)")
		}
	}
	delete(cfg.Profiles, name)
	if cfg.Active == name {
		cfg.Active = ""
	}
	if err := writeConfig(path, cfg); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	fmt.Printf("Removed profile %q\n", name)
	if cfg.Active == "" && len(cfg.Profiles) > 0 {
		fmt.Println("No active profile set. Run \"memory-vault-tui config use <name>\" to pick one.")
	}
	return nil
}

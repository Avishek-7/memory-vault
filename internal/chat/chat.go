// Package chat holds the Ollama /api/chat call and prompt templates shared
// by the MCP server (main.go, for compact_memories/get_session_summary) and
// memory-vault-tui's "s" (session summary) keybinding, so the two binaries
// can't drift out of sync on prompt wording.
package chat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"memory-vault/internal/store"
)

// Chat sends a single-turn prompt to an Ollama-compatible /api/chat endpoint
// at url using model, and returns the response text.
func Chat(url, model, prompt string) (string, error) {
	reqBody, _ := json.Marshal(map[string]interface{}{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
		"stream":   false,
	})
	resp, err := http.Post(url+"/api/chat", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("ollama chat request failed (is Ollama running with %q pulled?): %w", model, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama chat returned status %d", resp.StatusCode)
	}
	var out struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Message.Content, nil
}

// SessionSummaryPrompt builds get_session_summary's resume prompt from a
// space's recently-updated memories.
func SessionSummaryPrompt(space string, recents []store.RecentMemory) string {
	var parts []string
	for _, m := range recents {
		parts = append(parts, fmt.Sprintf("--- %s (kind: %s, updated %s) ---\n%s", m.Name, m.Kind, m.UpdatedAt.Format(time.RFC3339), m.Content))
	}
	return fmt.Sprintf(`The following are the most recently updated memories from space %q, most state-carrying (task/decision) ones first. Based only on what's here, write a short resume covering: what's the current state, what was decided, what's still open, and what's the likely next step. If the memories don't clearly imply a next step, say "unclear from stored memories" rather than inventing one.

%s`, space, strings.Join(parts, "\n\n"))
}

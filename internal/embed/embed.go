// Package embed defines the Embedder interface used to turn text into
// vectors for storage/search, plus the two implementations memory-vault
// ships with: a local Ollama server (the default) and an OpenAI-compatible
// HTTP embeddings endpoint (an opt-in escape hatch).
package embed

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Embedder turns text into a vector. Implementations must always return
// vectors of the same length for a given configuration; Store validates
// that length against EMBED_DIM.
type Embedder interface {
	Embed(text string) ([]float32, error)
}

// OllamaEmbedder calls a local Ollama server's /api/embeddings endpoint.
type OllamaEmbedder struct {
	URL   string
	Model string
}

func (o *OllamaEmbedder) Embed(text string) ([]float32, error) {
	reqBody, _ := json.Marshal(map[string]string{
		"model":  o.Model,
		"prompt": text,
	})
	resp, err := http.Post(o.URL+"/api/embeddings", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("ollama request failed (is Ollama running with %q pulled?): %w", o.Model, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama returned status %d", resp.StatusCode)
	}
	var out struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Embedding, nil
}

// OpenAIEmbedder calls an OpenAI-compatible embeddings endpoint — OpenAI
// itself, or a self-hosted OpenAI-compatible server (LiteLLM,
// text-embeddings-inference, etc.) via BaseURL.
type OpenAIEmbedder struct {
	BaseURL string
	APIKey  string
	Model   string
}

func (o *OpenAIEmbedder) Embed(text string) ([]float32, error) {
	reqBody, _ := json.Marshal(map[string]string{
		"model": o.Model,
		"input": text,
	})
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(o.BaseURL, "/")+"/embeddings", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.APIKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai-compatible embeddings request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai-compatible embeddings endpoint returned status %d", resp.StatusCode)
	}
	var out struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, fmt.Errorf("openai-compatible embeddings endpoint returned no data")
	}
	return out.Data[0].Embedding, nil
}

package embed

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOllamaEmbedder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Model != "all-minilm" || body.Prompt != "hello" {
			t.Errorf("unexpected request body: %+v", body)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"embedding": []float32{0.1, 0.2, 0.3}})
	}))
	defer srv.Close()

	e := &OllamaEmbedder{URL: srv.URL, Model: "all-minilm"}
	vec, err := e.Embed("hello")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != 3 {
		t.Errorf("Embed() = %v, want length 3", vec)
	}
}

func TestOllamaEmbedderErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	e := &OllamaEmbedder{URL: srv.URL, Model: "all-minilm"}
	if _, err := e.Embed("hello"); err == nil {
		t.Error("Embed() with 500 response = nil error, want error")
	}
}

func TestOpenAIEmbedder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization header = %q, want %q", got, "Bearer sk-test")
		}
		var body struct {
			Model string `json:"model"`
			Input string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Model != "text-embedding-3-small" || body.Input != "hello" {
			t.Errorf("unexpected request body: %+v", body)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []map[string]interface{}{{"embedding": []float32{0.4, 0.5}}},
		})
	}))
	defer srv.Close()

	e := &OpenAIEmbedder{BaseURL: srv.URL, APIKey: "sk-test", Model: "text-embedding-3-small"}
	vec, err := e.Embed("hello")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != 2 {
		t.Errorf("Embed() = %v, want length 2", vec)
	}
}

func TestOpenAIEmbedderEmptyData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"data": []map[string]interface{}{}})
	}))
	defer srv.Close()

	e := &OpenAIEmbedder{BaseURL: srv.URL, Model: "text-embedding-3-small"}
	if _, err := e.Embed("hello"); err == nil {
		t.Error("Embed() with empty data = nil error, want error")
	}
}

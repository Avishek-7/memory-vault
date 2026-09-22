package typesafe

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestJudgeMerge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization header = %q, want Bearer test-key", got)
		}
		var body request
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		if body.State["memory_a"] != "a" || body.State["memory_b"] != "b" {
			t.Errorf("state = %+v, want memory_a=a memory_b=b", body.State)
		}
		if _, ok := body.Questions["merge"]; !ok {
			t.Error("request missing merge question")
		}
		if _, ok := body.Questions["conflict"]; !ok {
			t.Error("request missing conflict question")
		}
		json.NewEncoder(w).Encode(response{Answers: map[string]noulAnswer{
			"merge":    {Noul: 0.9},
			"conflict": {Noul: 0.1},
		}})
	}))
	defer srv.Close()

	orig := apiURL
	apiURL = srv.URL
	defer func() { apiURL = orig }()

	merge, conflict, err := JudgeMerge("test-key", "a", "b")
	if err != nil {
		t.Fatalf("JudgeMerge: %v", err)
	}
	if merge != 0.9 || conflict != 0.1 {
		t.Errorf("JudgeMerge = (%v, %v), want (0.9, 0.1)", merge, conflict)
	}
}

func TestJudgeMergeNoAPIKey(t *testing.T) {
	if _, _, err := JudgeMerge("", "a", "b"); err == nil {
		t.Error("JudgeMerge with no API key: want error, got nil")
	}
}

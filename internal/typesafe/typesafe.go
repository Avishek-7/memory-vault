// Package typesafe calls TypeSafe's Jev model (System One) to judge whether
// two memory contents say the same thing or conflict — a semantic call that
// a cosine-distance threshold on embeddings can't make, since near-duplicate
// wording and outright contradiction ("JWT tokens" vs "reverted to session
// cookies") can embed just as close as true restatements.
package typesafe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

// apiURL is a var, not a const, purely so tests can point it at an
// httptest server instead of the real API.
var apiURL = "https://api.typesafe.ai/v1/systemone"

type noulQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

type request struct {
	State     map[string]string       `json:"state"`
	Model     string                  `json:"model"`
	Questions map[string]noulQuestion `json:"questions"`
}

type noulAnswer struct {
	Noul float64 `json:"noul"`
}

type response struct {
	Answers map[string]noulAnswer `json:"answers"`
}

// JudgeMerge asks whether memory contents a and b say the same underlying
// thing (mergeProb) or whether one contradicts/supersedes the other
// (conflictProb), each a 0-1 probability. apiKey must be non-empty.
func JudgeMerge(apiKey, a, b string) (mergeProb, conflictProb float64, err error) {
	if apiKey == "" {
		return 0, 0, fmt.Errorf("typesafe: no API key configured")
	}
	body, err := json.Marshal(request{
		State: map[string]string{"memory_a": a, "memory_b": b},
		Model: "jev-latest",
		Questions: map[string]noulQuestion{
			"merge": {
				Type: "noul",
				Instructions: "`memory_a` and `memory_b` are two stored notes about the same project. Do they express the same underlying " +
					"fact/decision (allowing paraphrase, or one being a more detailed restatement of the other), such that keeping both is " +
					"redundant and they should be consolidated into one memory?",
				Criteria: map[string]string{
					"true": "Same underlying information, just reworded or at different detail levels; no information would be lost by " +
						"keeping only the more detailed one.",
					"false": "They describe different facts, even if topically related, OR memory_b updates/reverses/contradicts memory_a " +
						"rather than restating it.",
				},
			},
			"conflict": {
				Type: "noul",
				Instructions: "`memory_a` and `memory_b` are two stored notes about the same project. Does one contradict, reverse, or " +
					"supersede the other (a value, decision, or state changed), rather than merely restating the same information?",
				Criteria: map[string]string{
					"true":  "They disagree, or memory_b represents a later state that invalidates memory_a.",
					"false": "No contradiction; consistent with each other (whether duplicate or unrelated).",
				},
			},
		},
	})
	if err != nil {
		return 0, 0, err
	}
	req, err := http.NewRequest(http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, 0, fmt.Errorf("typesafe request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("typesafe returned status %d", resp.StatusCode)
	}
	var out response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, 0, err
	}
	merge, ok := out.Answers["merge"]
	if !ok {
		return 0, 0, fmt.Errorf("typesafe response missing %q answer", "merge")
	}
	conflict, ok := out.Answers["conflict"]
	if !ok {
		return 0, 0, fmt.Errorf("typesafe response missing %q answer", "conflict")
	}
	return merge.Noul, conflict.Noul, nil
}

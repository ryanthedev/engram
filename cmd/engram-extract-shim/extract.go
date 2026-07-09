package main

import (
	"encoding/json"
	"strings"
)

// fact mirrors the wire shape internal/ingest.ParseExtraction validates
// (see internal/ingest/extraction.go's wireFact). Re-marshaling model output
// through this struct both confirms structural validity and drops any
// unexpected extra fields the model may have emitted.
type fact struct {
	Subject   string `json:"subject"`
	Predicate string `json:"predicate"`
	Object    string `json:"object"`
	Statement string `json:"statement,omitempty"`
	ValidAt   string `json:"valid_at,omitempty"`
}

// emptyFactArray is the canonical degrade-to-nothing response: legal per the
// wire contract (internal/ingest/extraction.go treats "[]" as ErrNoFacts, a
// clean no-op, never a crash).
const emptyFactArray = "[]"

// parseFacts turns raw backend stdout into a canonical JSON fact array,
// never failing: fenced or prose-wrapped JSON is unwrapped and returned as
// the real array; anything structurally broken (not an array, invalid JSON,
// empty output) degrades to "[]" rather than propagating an error that would
// turn into a 500 and dead-letter the event upstream. count reports how many
// facts survived, for the synthetic usage estimate.
func parseFacts(raw string) (canonical []byte, count int) {
	stripped := stripCodeFences(raw)
	arr := extractArraySubstring(stripped)
	if arr == "" {
		return []byte(emptyFactArray), 0
	}
	var facts []fact
	if err := json.Unmarshal([]byte(arr), &facts); err != nil {
		return []byte(emptyFactArray), 0
	}
	out, err := json.Marshal(facts)
	if err != nil {
		return []byte(emptyFactArray), 0
	}
	return out, len(facts)
}

// stripCodeFences removes a surrounding markdown code fence (```json ... ```
// or ``` ... ```) — mirrors internal/ingest/extraction.go's stripCodeFences
// since chat models wrap JSON in fences despite instructions not to.
func stripCodeFences(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return s
	}
	t = strings.TrimPrefix(t, "```")
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		t = t[i+1:] // drop the language tag line, if any
	}
	t = strings.TrimSuffix(strings.TrimSpace(t), "```")
	return t
}

// extractArraySubstring returns the substring spanning the first '[' through
// the last ']' in s, tolerating leading/trailing prose around a JSON array
// ("Here is the result:\n[...]\nThanks!"). Returns "" when no bracket pair is
// found (empty output, or a bare JSON object with no array at all) so the
// caller degrades to "[]" instead of guessing.
func extractArraySubstring(s string) string {
	start := strings.IndexByte(s, '[')
	end := strings.LastIndexByte(s, ']')
	if start < 0 || end < 0 || end < start {
		return ""
	}
	return s[start : end+1]
}

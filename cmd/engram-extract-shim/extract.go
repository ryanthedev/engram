package main

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
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

// factsSchema is the JSON Schema every backend enforces through its CLI's
// structured-output flag (claude --json-schema, agy --json-schema, codex
// --output-schema). The root is an object because OpenAI structured outputs
// reject a bare array, and every field is required (valid_at as nullable)
// because strict mode rejects optional properties. parseFacts unwraps it
// back to the array engramd expects.
const factsSchema = `{"type":"object","additionalProperties":false,"required":["facts"],"properties":{"facts":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["subject","predicate","object","statement","valid_at"],"properties":{"subject":{"type":"string"},"predicate":{"type":"string"},"object":{"type":"string"},"statement":{"type":"string"},"valid_at":{"type":["string","null"]}}}}}}`

// emptyFactArray is the legal "nothing durable here" response
// (internal/ingest/extraction.go treats "[]" as ErrNoFacts and completes the
// event with nothing indexed).
const emptyFactArray = "[]"

// ErrUnparseable reports backend output that is neither a {"facts": [...]}
// object nor a fact array. It is a backend failure, not an empty result:
// answering "[]" would complete the event and lose its facts for good, while
// an error becomes a retryable 502.
var ErrUnparseable = errors.New("engram-extract-shim: unparseable backend output")

// parseFacts turns backend output into facts. It accepts the schema's
// {"facts": [...]} object, and also a bare array (fenced or prose-wrapped)
// from a model that ignored the schema. An empty list is a real answer;
// anything else structurally broken is ErrUnparseable. valid_at values are
// normalized so one malformed date can't make engramd reject the whole
// extraction.
func parseFacts(raw string) ([]fact, error) {
	stripped := strings.TrimSpace(stripCodeFences(raw))
	var wrapped struct {
		Facts *[]fact `json:"facts"`
	}
	var facts []fact
	if err := json.Unmarshal([]byte(stripped), &wrapped); err == nil && wrapped.Facts != nil {
		facts = *wrapped.Facts
	} else {
		arr := extractArraySubstring(stripped)
		if arr == "" {
			return nil, ErrUnparseable
		}
		if err := json.Unmarshal([]byte(arr), &facts); err != nil {
			return nil, ErrUnparseable
		}
	}
	for i := range facts {
		facts[i].ValidAt = normalizeValidAt(facts[i].ValidAt)
	}
	return facts, nil
}

// normalizeValidAt returns s as an RFC 3339 timestamp, promotes a bare date
// to midnight UTC, and drops anything else. engramd decodes valid_at as a
// time.Time, so a single "2026-10-23" or "last week" would fail the whole
// event; an omitted valid_at defaults to the event's own time instead.
func normalizeValidAt(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.Format(time.RFC3339)
	}
	return ""
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
// found (empty output, or a bare JSON object with no array at all).
func extractArraySubstring(s string) string {
	start := strings.IndexByte(s, '[')
	end := strings.LastIndexByte(s, ']')
	if start < 0 || end < 0 || end < start {
		return ""
	}
	return s[start : end+1]
}

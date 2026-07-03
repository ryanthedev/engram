package ingest_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/ingest"
	"github.com/ryanthedev/engram/internal/memory"
)

func TestParseExtraction(t *testing.T) {
	long := strings.Repeat("x", ingest.MaxFieldLen+1)
	tooMany, _ := json.Marshal(make([]map[string]string, ingest.MaxFactsPerExtraction+1))
	tests := []struct {
		name    string
		raw     string
		wantErr error
		want    int // fact count when wantErr == nil
	}{
		{"valid triple", `[{"subject":"svc","predicate":"owner","object":"ana"}]`, nil, 1},
		{"valid retraction (empty object)", `[{"subject":"svc","predicate":"owner","object":""}]`, nil, 1},
		{"fenced JSON from a chatty model", "```json\n[{\"subject\":\"svc\",\"predicate\":\"owner\",\"object\":\"ana\"}]\n```", nil, 1},
		{"garbage", `{{{ not json`, ingest.ErrMalformed, 0},
		{"wrong shape", `{"subject":"svc"}`, ingest.ErrMalformed, 0},
		{"missing subject", `[{"subject":"","predicate":"owner","object":"ana"}]`, ingest.ErrMalformed, 0},
		{"missing predicate", `[{"subject":"svc","predicate":" ","object":"ana"}]`, ingest.ErrMalformed, 0},
		{"oversized field", `[{"subject":"` + long + `","predicate":"owner","object":"ana"}]`, ingest.ErrMalformed, 0},
		{"too many facts", string(tooMany), ingest.ErrMalformed, 0},
		{"empty array", `[]`, ingest.ErrNoFacts, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			facts, err := ingest.ParseExtraction([]byte(tt.raw))
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(facts) != tt.want {
				t.Fatalf("got %d facts, want %d", len(facts), tt.want)
			}
			if facts[0].Statement == "" {
				t.Error("statement must be synthesized when absent (BM25 target)")
			}
		})
	}
}

func TestRuleExtractorDirectives(t *testing.T) {
	ex := &ingest.RuleExtractor{}
	at := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	ev := memory.Episodic{Text: fmt.Sprintf("prose is ignored\nfact: svc | owner | ana @ %s\nretract: svc | deprecated", at.Format(time.RFC3339))}

	facts, err := ex.Extract(context.Background(), []memory.Episodic{ev})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("got %d facts, want 2: %+v", len(facts), facts)
	}
	if facts[0].Subject != "svc" || facts[0].Predicate != "owner" || facts[0].Object != "ana" || !facts[0].ValidAt.Equal(at) {
		t.Errorf("fact 0 = %+v, want svc/owner/ana @ %v", facts[0], at)
	}
	if facts[1].Object != "" {
		t.Errorf("retract directive must produce an empty object, got %q", facts[1].Object)
	}
	if ex.Calls() != 1 {
		t.Errorf("Calls() = %d, want 1", ex.Calls())
	}
}

// TestRuleExtractorMalformedAndEmpty pins the fixture paths DW-2.7 needs:
// the !malformed directive fails through the REAL ParseExtraction barricade,
// and a directive-free event reports ErrNoFacts.
func TestRuleExtractorMalformedAndEmpty(t *testing.T) {
	ex := &ingest.RuleExtractor{}
	if _, err := ex.Extract(context.Background(), []memory.Episodic{{Text: "!malformed"}}); !errors.Is(err, ingest.ErrMalformed) {
		t.Errorf("malformed fixture error = %v, want ErrMalformed", err)
	}
	if _, err := ex.Extract(context.Background(), []memory.Episodic{{Text: "just prose"}}); !errors.Is(err, ingest.ErrNoFacts) {
		t.Errorf("fact-free fixture error = %v, want ErrNoFacts", err)
	}
}

// TestHTTPExtractorParsesChatCompletion covers the real cheap-model path:
// the OpenAI-compatible request shape goes out, the choices content comes
// back through ParseExtraction, and REAL usage tokens land in the meter.
func TestHTTPExtractorParsesChatCompletion(t *testing.T) {
	var gotPath string
	var gotReq map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant",
				"content": `[{"subject":"svc","predicate":"owner","object":"ana"}]`}}},
			"usage": map[string]any{"prompt_tokens": 321, "completion_tokens": 45},
		})
	}))
	defer srv.Close()

	meter := &ingest.CostMeter{}
	ex := ingest.NewHTTPExtractor(srv.Client(), srv.URL, "gpt-4o-mini")
	ex.Meter = meter
	facts, err := ex.Extract(context.Background(), []memory.Episodic{{EventID: "ev-1", Text: "deploy log"}})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(facts) != 1 || facts[0].Object != "ana" {
		t.Fatalf("facts = %+v, want one svc/owner/ana", facts)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", gotPath)
	}
	if gotReq["model"] != "gpt-4o-mini" {
		t.Errorf("model = %v, want gpt-4o-mini", gotReq["model"])
	}
	u := meter.Snapshot()
	if u.Events != 1 || u.PromptTokens != 321 || u.CompletionTokens != 45 {
		t.Errorf("usage = %+v, want real token counts from the response", u)
	}
}

// TestHTTPExtractorRejectsBadResponses: transport-level and contract-level
// failures reject without indexing (the worker's retry/dead-letter path owns
// what happens next).
func TestHTTPExtractorRejectsBadResponses(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		wantErr error // nil means any error is acceptable
	}{
		{"http 500", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }, nil},
		{"no choices", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{}})
		}, ingest.ErrMalformed},
		{"non-JSON content", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "I could not find any facts, sorry!"}}}})
		}, ingest.ErrMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			ex := ingest.NewHTTPExtractor(srv.Client(), srv.URL, "gpt-4o-mini")
			_, err := ex.Extract(context.Background(), []memory.Episodic{{EventID: "ev-1", Text: "x"}})
			if err == nil {
				t.Fatal("want an error, got none")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

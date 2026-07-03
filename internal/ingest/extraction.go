package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ryanthedev/engram/internal/memory"
)

// ErrNoFacts reports an extraction that parsed cleanly but yielded zero
// facts. Per the Extractor contract an empty extraction is an error (never
// indexed), but callers distinguish it from ErrMalformed: a fact-free event
// completes with nothing written, while malformed output is retried and
// eventually dead-lettered.
var ErrNoFacts = errors.New("ingest: extraction produced no facts")

// ErrMalformed reports LLM extraction output that failed parsing or
// validation. It is rejected at this barricade and never indexed (the LLM is
// external input — D4's gate exists exactly here).
var ErrMalformed = errors.New("ingest: malformed extraction")

// Defensive bounds on one extraction (barricade limits: LLM output is
// untrusted input and must not be able to flood the write path).
const (
	// MaxFactsPerExtraction caps how many facts one extraction may assert.
	MaxFactsPerExtraction = 100
	// MaxFieldLen caps each fact field's byte length.
	MaxFieldLen = 4096
)

// wireFact is the extraction wire shape shared by every Extractor
// implementation: a JSON array of these is what the LLM (or the deterministic
// fake) must produce, so fixtures exercise the exact validation the real
// path runs.
type wireFact struct {
	Subject   string     `json:"subject"`
	Predicate string     `json:"predicate"`
	Object    string     `json:"object"`
	Statement string     `json:"statement,omitempty"`
	ValidAt   *time.Time `json:"valid_at,omitempty"`
}

// ParseExtraction validates raw extractor output and converts it into
// content-only candidate facts (no system timestamps, no tenancy — the
// writer stamps those). It is the single trust barricade for LLM output:
// syntax errors, missing subject/predicate, and oversized payloads all
// reject with ErrMalformed; a clean-but-empty array rejects with ErrNoFacts.
// An empty Object is legal and means retraction intent (the reconciler maps
// it to INVALIDATE/NOOP; it is never indexed as an assertion).
func ParseExtraction(raw []byte) ([]memory.SemanticFact, error) {
	trimmed := strings.TrimSpace(stripCodeFences(string(raw)))
	var wire []wireFact
	if err := json.Unmarshal([]byte(trimmed), &wire); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if len(wire) == 0 {
		return nil, ErrNoFacts
	}
	if len(wire) > MaxFactsPerExtraction {
		return nil, fmt.Errorf("%w: %d facts exceeds the %d cap", ErrMalformed, len(wire), MaxFactsPerExtraction)
	}
	facts := make([]memory.SemanticFact, 0, len(wire))
	for i, wf := range wire {
		if strings.TrimSpace(wf.Subject) == "" || strings.TrimSpace(wf.Predicate) == "" {
			return nil, fmt.Errorf("%w: fact %d is missing subject or predicate", ErrMalformed, i)
		}
		for _, field := range []string{wf.Subject, wf.Predicate, wf.Object, wf.Statement} {
			if len(field) > MaxFieldLen {
				return nil, fmt.Errorf("%w: fact %d has a field over %d bytes", ErrMalformed, i, MaxFieldLen)
			}
		}
		f := memory.SemanticFact{
			Subject:   strings.TrimSpace(wf.Subject),
			Predicate: strings.TrimSpace(wf.Predicate),
			Object:    strings.TrimSpace(wf.Object),
			Statement: strings.TrimSpace(wf.Statement),
		}
		if f.Statement == "" {
			f.Statement = synthesizeStatement(f)
		}
		if wf.ValidAt != nil {
			f.ValidAt = wf.ValidAt.UTC()
		}
		facts = append(facts, f)
	}
	return facts, nil
}

// synthesizeStatement renders a triple as its natural-language default (the
// BM25 retrieval target must never be empty for an indexed fact).
func synthesizeStatement(f memory.SemanticFact) string {
	if f.Object == "" {
		return fmt.Sprintf("%s %s: retracted", f.Subject, f.Predicate)
	}
	return fmt.Sprintf("%s %s %s", f.Subject, f.Predicate, f.Object)
}

// stripCodeFences removes a surrounding markdown code fence — chat models
// often wrap JSON in ```json ... ``` despite instructions.
func stripCodeFences(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return s
	}
	t = strings.TrimPrefix(t, "```")
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		t = t[i+1:] // drop the language tag line
	}
	t = strings.TrimSuffix(strings.TrimSpace(t), "```")
	return t
}

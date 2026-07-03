package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ryanthedev/engram/internal/memory"
)

// RuleExtractor is the deterministic, fixture-driven Extractor: it
// synthesizes the extraction wire payload from directive lines in each
// event's Text and funnels it through the SAME ParseExtraction barricade the
// HTTP path uses, so malformed/empty/contradictory fixtures exercise the
// real validation. Directive syntax, one per line:
//
//	fact: subject | predicate | object [@ RFC3339 valid_at]
//	retract: subject | predicate [@ RFC3339 valid_at]
//	!malformed        → the payload becomes non-JSON garbage
//
// Non-directive lines are ignored. It counts calls (DW-2.4 verifies the LLM
// is NOT re-called on a cached resume by this count) and records simulated
// token usage into Meter when set.
type RuleExtractor struct {
	// Meter, when non-nil, accumulates simulated usage for the cost metric.
	Meter *CostMeter

	calls atomic.Int64
}

var _ Extractor = (*RuleExtractor)(nil)

// Calls reports how many times Extract ran — the DW-2.4 idempotency probe.
func (e *RuleExtractor) Calls() int64 { return e.calls.Load() }

// Extract implements Extractor deterministically from the events' directive
// lines. See RuleExtractor for the fixture syntax and error contract
// (ErrMalformed / ErrNoFacts via ParseExtraction).
func (e *RuleExtractor) Extract(_ context.Context, events []memory.Episodic) ([]memory.SemanticFact, error) {
	e.calls.Add(1)
	raw, textLen, factCount := synthesizeWire(events)
	if e.Meter != nil {
		// Simulated token counts, calibrated to a cheap chat model: a fixed
		// prompt overhead plus ~4 chars/token of event text in, ~24 tokens
		// per extracted fact out.
		e.Meter.Record(len(events), 200+textLen/4, 24*factCount+8)
	}
	return ParseExtraction(raw)
}

// synthesizeWire builds the wire JSON (or deliberate garbage) from the
// events' directives.
func synthesizeWire(events []memory.Episodic) (raw []byte, textLen, factCount int) {
	var wire []wireFact
	for _, ev := range events {
		textLen += len(ev.Text)
		for _, line := range strings.Split(ev.Text, "\n") {
			line = strings.TrimSpace(line)
			switch {
			case line == "!malformed":
				return []byte("{{{ this is not JSON"), textLen, 0
			case strings.HasPrefix(line, "fact:"):
				if wf, ok := parseDirective(strings.TrimPrefix(line, "fact:"), 3); ok {
					wire = append(wire, wf)
				}
			case strings.HasPrefix(line, "retract:"):
				if wf, ok := parseDirective(strings.TrimPrefix(line, "retract:"), 2); ok {
					wf.Object = ""
					wire = append(wire, wf)
				}
			}
		}
	}
	raw, err := json.Marshal(wire)
	if err != nil { // unreachable for these field types; keep the barricade honest
		raw = []byte(fmt.Sprintf("marshal failure: %v", err))
	}
	if wire == nil {
		raw = []byte("[]")
	}
	return raw, textLen, len(wire)
}

// parseDirective splits "s | p | o [@ time]" into a wireFact with n required
// parts (3 for fact, 2 for retract). ok=false drops silently: the fake
// mirrors an LLM, and an LLM never errors on prose it merely ignores.
func parseDirective(body string, n int) (wireFact, bool) {
	body = strings.TrimSpace(body)
	var validAt *time.Time
	if at := strings.LastIndex(body, "@"); at >= 0 {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(body[at+1:])); err == nil {
			ts := t.UTC()
			validAt = &ts
			body = body[:at]
		}
	}
	parts := strings.Split(body, "|")
	if len(parts) < n {
		return wireFact{}, false
	}
	wf := wireFact{
		Subject:   strings.TrimSpace(parts[0]),
		Predicate: strings.TrimSpace(parts[1]),
		ValidAt:   validAt,
	}
	if n >= 3 {
		wf.Object = strings.TrimSpace(parts[2])
	}
	return wf, true
}

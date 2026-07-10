package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// fixedHitsBackend is a Backend that returns a preset slice of Hits
// regardless of query, sliced to k. It lets budget/facet tests control exact
// hit content (real per-tier fields_json) that fakeBackend's substring
// filter can't produce.
type fixedHitsBackend struct{ hits []Hit }

func (b *fixedHitsBackend) Ingest(context.Context, string, string, string) (string, error) {
	return "", nil
}

func (b *fixedHitsBackend) Search(_ context.Context, _ string, k int) ([]Hit, error) {
	if k <= 0 || k >= len(b.hits) {
		return b.hits, nil
	}
	return b.hits[:k], nil
}

func (b *fixedHitsBackend) Status(context.Context) (Status, error) { return Status{}, nil }

// semanticHit builds a Hit whose Fields is a realistic semantic-tier
// fields_json blob (per Phase 1's allowlist), padded to at least size bytes
// so tests can force budget overflow deterministically.
func semanticHit(id, subject, predicate, object string, pad int) Hit {
	fields := map[string]any{
		"statement": strings.Repeat("x", pad),
		"subject":   subject,
		"predicate": predicate,
		"object":    object,
		"valid_at":  "2026-01-01T00:00:00Z",
	}
	b, _ := json.Marshal(fields)
	return Hit{ID: id, Score: 1.0, Source: "semantic", Fields: string(b)}
}

// searchViaWire drives memory_search end to end through the JSON-RPC wire
// (refClient) and returns the raw structured payload text (exactly what
// toolResult marshaled as the tool's content block) plus the decoded result.
func searchViaWire(t *testing.T, backend Backend, args map[string]any) (text string, decoded map[string]any) {
	t.Helper()
	c := startServer(t, backend)
	c.call("initialize", nil)
	resp := c.call("tools/call", map[string]any{"name": ToolSearch, "arguments": args})
	res, _ := resp["result"].(map[string]any)
	if res == nil || res["isError"] == true {
		t.Fatalf("memory_search call failed: %v", resp)
	}
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("memory_search returned no content: %v", res)
	}
	block, _ := content[0].(map[string]any)
	text, _ = block["text"].(string)
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatalf("decoding memory_search result text: %v (%s)", err, text)
	}
	return text, decoded
}

// TestDW_2_1_DefaultSearchFitsBudget: a default memory_search (no explicit k)
// returns a response whose FULL serialized size (hits + envelope) is <= the
// configured byte budget.
func TestDW_2_1_DefaultSearchFitsBudget(t *testing.T) {
	hits := make([]Hit, 0, defaultRequestK+10)
	for i := 0; i < defaultRequestK+10; i++ {
		hits = append(hits, semanticHit(fmt.Sprintf("f%d", i), "s", "p", "o", 20))
	}
	text, _ := searchViaWire(t, &fixedHitsBackend{hits: hits}, map[string]any{"query": "anything"})
	if got, want := len(text), searchByteBudget(); got > want {
		t.Errorf("serialized size = %d bytes, want <= %d", got, want)
	}
}

// TestDW_2_2_OmissionFieldsPresentOnlyWhenOmitted: over-budget carries
// omitted>0 + non-empty omitted_facets + a hint; all-fit carries none of
// those (absent, due to omitempty, or zero).
func TestDW_2_2_OmissionFieldsPresentOnlyWhenOmitted(t *testing.T) {
	hits := []Hit{
		semanticHit("h1", "alice", "knows", "bob", 50),
		semanticHit("h2", "alice", "knows", "carol", 50),
		semanticHit("h3", "dave", "manages", "erin", 50),
	}

	t.Run("all fit", func(t *testing.T) {
		_, decoded := searchViaWire(t, &fixedHitsBackend{hits: hits}, map[string]any{"query": "q"})
		if _, ok := decoded["omitted"]; ok {
			t.Errorf("omitted present when all hits fit: %v", decoded)
		}
		if _, ok := decoded["omitted_facets"]; ok {
			t.Errorf("omitted_facets present when all hits fit: %v", decoded)
		}
		if _, ok := decoded["hint"]; ok {
			t.Errorf("hint present when all hits fit: %v", decoded)
		}
		gotHits, _ := decoded["hits"].([]any)
		if len(gotHits) != len(hits) {
			t.Errorf("hits = %d, want %d", len(gotHits), len(hits))
		}
	})

	t.Run("over budget", func(t *testing.T) {
		t.Setenv(searchBudgetBytesEnv, "200")
		_, decoded := searchViaWire(t, &fixedHitsBackend{hits: hits}, map[string]any{"query": "q"})
		omitted, _ := decoded["omitted"].(float64)
		if omitted <= 0 {
			t.Fatalf("omitted = %v, want > 0", decoded["omitted"])
		}
		facets, _ := decoded["omitted_facets"].(map[string]any)
		if len(facets) == 0 {
			t.Errorf("omitted_facets empty, want non-empty: %v", decoded)
		}
		if hint, _ := decoded["hint"].(string); hint == "" {
			t.Errorf("hint empty, want non-empty: %v", decoded)
		}
	})
}

// TestDW_2_3_SearchByteBudgetFromEnv: the byte budget is configurable via
// ENGRAM_MCP_SEARCH_BUDGET_BYTES, defaulting to 16384; unset or invalid
// values fall back to the default (dirty cases).
func TestDW_2_3_SearchByteBudgetFromEnv(t *testing.T) {
	tests := []struct {
		name string
		val  string // "" means unset
		want int
	}{
		{"unset", "", searchBudgetBytesDefault},
		{"valid override", "8000", 8000},
		{"minimal valid", "1", 1},
		{"zero (dirty)", "0", searchBudgetBytesDefault},
		{"negative (dirty)", "-5", searchBudgetBytesDefault},
		{"non-numeric (dirty)", "abc", searchBudgetBytesDefault},
		{"whitespace (dirty)", "  ", searchBudgetBytesDefault},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(searchBudgetBytesEnv, tc.val)
			if got := searchByteBudget(); got != tc.want {
				t.Errorf("searchByteBudget() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestDW_2_4_SingleOverBudgetHitStillEmitted: a single hit already at/over
// budget is still emitted — never an empty page when hits exist.
func TestDW_2_4_SingleOverBudgetHitStillEmitted(t *testing.T) {
	huge := semanticHit("h1", "s", "p", "o", 5000)

	t.Run("only one hit, alone over budget", func(t *testing.T) {
		result := packSearchResult([]Hit{huge}, 1)
		if len(result.Hits) != 1 {
			t.Fatalf("Hits = %d, want 1 (never empty when hits exist)", len(result.Hits))
		}
		if result.Omitted != 0 {
			t.Errorf("Omitted = %d, want 0 (nothing left to omit)", result.Omitted)
		}
	})

	t.Run("huge first hit forces the rest into omitted", func(t *testing.T) {
		small := semanticHit("h2", "s2", "p2", "o2", 5)
		result := packSearchResult([]Hit{huge, small}, 10)
		if len(result.Hits) != 1 || result.Hits[0].ID != "h1" {
			t.Fatalf("Hits = %+v, want exactly [h1]", result.Hits)
		}
		if result.Omitted != 1 {
			t.Errorf("Omitted = %d, want 1", result.Omitted)
		}
	})
}

// TestDW_2_5_TopFacetsStableOnTies: facet counts are computed over the
// omitted set, and ties are broken deterministically by first-encountered
// order — not by Go's randomized map iteration.
func TestDW_2_5_TopFacetsStableOnTies(t *testing.T) {
	aFirst := []Hit{
		semanticHit("1", "alice", "p", "o", 1),
		semanticHit("2", "bob", "p", "o", 1),
	}
	bFirst := []Hit{
		semanticHit("1", "bob", "p", "o", 1),
		semanticHit("2", "alice", "p", "o", 1),
	}

	for i := 0; i < 5; i++ { // repeat: catch accidental dependence on map iteration order
		if got := topFacets(aFirst)["subject"]; got != "alice" {
			t.Fatalf("run %d: topFacets(aFirst)[subject] = %q, want %q (first-encountered tie-break)", i, got, "alice")
		}
		if got := topFacets(bFirst)["subject"]; got != "bob" {
			t.Fatalf("run %d: topFacets(bFirst)[subject] = %q, want %q (first-encountered tie-break)", i, got, "bob")
		}
	}
}

// TestTopFacetsSkipsMalformedOrMissingFields: a hit with unparseable or
// wrong-typed Fields contributes no facets and never panics (external-input
// defense, matching Phase 1's "tolerate a missing field" precedent).
func TestTopFacetsSkipsMalformedOrMissingFields(t *testing.T) {
	hits := []Hit{
		{ID: "bad-json", Fields: "not json"},
		{ID: "wrong-type", Fields: `{"subject": 42}`},
		{ID: "no-fields", Fields: ""},
		semanticHit("good", "alice", "knows", "bob", 1),
	}
	facets := topFacets(hits) // must not panic
	if facets["subject"] != "alice" {
		t.Errorf("subject facet = %q, want %q (only the well-formed hit counts)", facets["subject"], "alice")
	}
}

// TestPackSearchResultZeroHits: a zero-hit result produces an empty,
// non-nil page — not a panic, not a null hits array (DW-2.1's zero-hit edge
// case; the packing loop must test at the beginning).
func TestPackSearchResultZeroHits(t *testing.T) {
	result := packSearchResult(nil, searchBudgetBytesDefault)
	if result.Hits == nil {
		t.Error("Hits is nil, want a non-nil empty slice (marshals to [] not null)")
	}
	if len(result.Hits) != 0 {
		t.Errorf("Hits = %d, want 0", len(result.Hits))
	}
	if result.Omitted != 0 {
		t.Errorf("Omitted = %d, want 0", result.Omitted)
	}
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if hits, ok := decoded["hits"].([]any); !ok || len(hits) != 0 {
		t.Errorf("decoded hits = %v, want an empty array", decoded["hits"])
	}
}

// TestRefineHintDeterministicFieldOrder: the hint text lists facets in a
// fixed field order regardless of map iteration, so repeated calls with the
// same facets produce byte-identical hints.
func TestRefineHintDeterministicFieldOrder(t *testing.T) {
	facets := map[string]string{"kind": "note", "subject": "alice", "predicate": "knows"}
	first := refineHint(3, facets)
	for i := 0; i < 5; i++ {
		if got := refineHint(3, facets); got != first {
			t.Fatalf("refineHint not deterministic: %q vs %q", got, first)
		}
	}
	if !strings.Contains(first, "subject=") || !strings.Contains(first, "predicate=") || !strings.Contains(first, "kind=") {
		t.Errorf("hint missing a facet field: %q", first)
	}
}

// TestCallSearchDefaultKUsesDefaultRequestK: memory_search with no k arg
// requests defaultRequestK hits from the Backend, not whatever the Backend's
// own default happens to be.
func TestCallSearchDefaultKUsesDefaultRequestK(t *testing.T) {
	var gotK int
	backend := &recordingKBackend{onSearch: func(k int) { gotK = k }}
	_, _ = searchViaWireIgnoringHits(t, backend, map[string]any{"query": "q"})
	if gotK != defaultRequestK {
		t.Errorf("requested k = %d, want defaultRequestK = %d", gotK, defaultRequestK)
	}
}

// recordingKBackend records the k passed to Search and returns no hits.
type recordingKBackend struct{ onSearch func(k int) }

func (b *recordingKBackend) Ingest(context.Context, string, string, string) (string, error) {
	return "", nil
}
func (b *recordingKBackend) Search(_ context.Context, _ string, k int) ([]Hit, error) {
	b.onSearch(k)
	return nil, nil
}
func (b *recordingKBackend) Status(context.Context) (Status, error) { return Status{}, nil }

func searchViaWireIgnoringHits(t *testing.T, backend Backend, args map[string]any) (string, map[string]any) {
	t.Helper()
	return searchViaWire(t, backend, args)
}

package mcp

// Phase-6 tests for the `expanded` block at the MCP surface: the block is
// rendered as a distinct, delimited section and budgeted SEPARATELY from the
// matched hits — matched pack first against the whole budget, expansions live on
// whatever is left, and expansions are the first thing dropped under pressure.
// Every expanded hit is a token cost on every LLM call, so none of this is
// cosmetic.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// padGraphHit builds a Hit shaped like the expander’s output: source "graph", a
// statement, and a hop — padded so tests can force budget pressure.
func padGraphHit(id, subject, predicate, object string, pad int) Hit {
	fields := map[string]any{
		"statement": strings.Repeat("g", pad),
		"subject":   subject,
		"predicate": predicate,
		"object":    object,
		"hop":       1,
	}
	b, _ := json.Marshal(fields)
	return Hit{ID: id, Score: 0.5, Source: "graph", Fields: string(b)}
}

// TestDW_6_2_SearchResultRendersExpandedSection: expansions reach the caller in
// their own labeled section — never interleaved with the matched hits, and
// visibly marked as not counted against k.
func TestDW_6_2_SearchResultRendersExpandedSection(t *testing.T) {
	backend := &fixedHitsBackend{
		hits:     []Hit{semanticHit("s1", "svc", "owned_by", "team-a", 10)},
		expanded: []Hit{padGraphHit("g1", "team-a", "located_in", "berlin", 10)},
	}
	text, decoded := searchViaWire(t, backend, map[string]any{"query": "q"})

	// Structured: two blocks, never merged.
	hits, _ := decoded["hits"].([]any)
	expanded, _ := decoded["expanded"].([]any)
	if len(hits) != 1 || len(expanded) != 1 {
		t.Fatalf("hits/expanded = %d/%d, want 1/1 — the blocks must stay separate", len(hits), len(expanded))
	}
	if h, _ := hits[0].(map[string]any); h["source"] == "graph" {
		t.Fatalf("a graph hit was rendered into the matched hits array: %v", h)
	}
	if e, _ := expanded[0].(map[string]any); e["source"] != "graph" || e["id"] != "g1" {
		t.Fatalf("expanded[0] = %v, want the graph hit g1", e)
	}

	// Text: the compact-line block an agent actually reads. The expansion must
	// sit BELOW a header that says what it is, not silently among the matches.
	lines := strings.Split(text, "\n")
	if len(lines) != 3 {
		t.Fatalf("compact lines = %d, want 3 (1 matched, 1 header, 1 expanded):\n%s", len(lines), text)
	}
	if !strings.HasPrefix(lines[0], "s1\tsemantic\t") {
		t.Errorf("line 0 = %q, want the matched hit first", lines[0])
	}
	if !strings.Contains(lines[1], "expanded") || !strings.Contains(lines[1], "not counted against k") {
		t.Errorf("line 1 = %q, want a header naming the block and stating it is not counted against k", lines[1])
	}
	if !strings.HasPrefix(lines[2], "g1\tgraph\t") {
		t.Errorf("line 2 = %q, want the graph expansion below the header", lines[2])
	}
}

// TestDW_6_3_ZeroExpansionsOmitsExpandedKey: with no expansions the key is
// ABSENT from the response — not present-and-empty. An empty array still costs
// tokens and still reads as "expansion ran"; absence is the honest encoding.
func TestDW_6_3_ZeroExpansionsOmitsExpandedKey(t *testing.T) {
	backend := &fixedHitsBackend{hits: []Hit{semanticHit("s1", "svc", "owned_by", "team-a", 10)}}
	text, decoded := searchViaWire(t, backend, map[string]any{"query": "q"})

	if _, present := decoded["expanded"]; present {
		t.Errorf("structuredContent carries an `expanded` key with zero expansions: %v", decoded["expanded"])
	}
	if _, present := decoded["expanded_omitted"]; present {
		t.Errorf("structuredContent carries `expanded_omitted` with zero expansions: %v", decoded["expanded_omitted"])
	}
	if strings.Contains(text, "expanded") {
		t.Errorf("compact lines mention an expanded block that does not exist:\n%s", text)
	}

	// And the packed envelope itself marshals without the key at all.
	b, err := json.Marshal(packSearchResult([]Hit{semanticHit("s1", "a", "b", "c", 10)}, 16384, memoryFacetFields))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), `"expanded"`) {
		t.Errorf("searchResult marshaled an empty `expanded` key: %s", b)
	}
}

// TestDW_6_4_MatchedHitsPackFirstExpansionsDroppedFirst: under a budget that
// only fits the matched hits, the expansions are dropped WHOLESALE — they never
// evict a match — and the drop is reported rather than silent.
func TestDW_6_4_MatchedHitsPackFirstExpansionsDroppedFirst(t *testing.T) {
	matched := []Hit{
		semanticHit("s1", "svc", "owned_by", "team-a", 300),
		semanticHit("s2", "svc", "runs_on", "k8s", 300),
	}
	expanded := []Hit{
		padGraphHit("g1", "team-a", "located_in", "berlin", 300),
		padGraphHit("g2", "berlin", "part_of", "de", 300),
	}

	// Budget sized to hold both matched hits and nothing more.
	full := packSearchResult(matched, 100000, memoryFacetFields)
	fullBytes, _ := json.Marshal(full)
	budget := len(fullBytes)

	result := packExpanded(full, expanded, budget)

	if len(result.Hits) != 2 {
		t.Fatalf("len(hits) = %d, want 2: an expansion evicted a matched hit", len(result.Hits))
	}
	if result.Omitted != 0 {
		t.Fatalf("omitted = %d, want 0: matched hits pack first and all of them fit", result.Omitted)
	}
	if len(result.Expanded) != 0 {
		t.Fatalf("len(expanded) = %d, want 0: expansions must be the first thing dropped", len(result.Expanded))
	}
	if result.ExpandedOmitted != 2 {
		t.Errorf("expanded_omitted = %d, want 2: a dropped expansion must be reported, not silently vanish", result.ExpandedOmitted)
	}
}

// TestDW_6_4_ExpandedFillsLeftoverBudget: the block is budgeted separately —
// given room for some but not all expansions, the fitting prefix is kept, the
// rest counted, and the serialized whole still respects the budget.
func TestDW_6_4_ExpandedFillsLeftoverBudget(t *testing.T) {
	matched := []Hit{semanticHit("s1", "svc", "owned_by", "team-a", 100)}
	expanded := []Hit{
		padGraphHit("g1", "team-a", "located_in", "berlin", 100),
		padGraphHit("g2", "berlin", "part_of", "de", 100),
		padGraphHit("g3", "de", "part_of", "eu", 100),
	}

	// A budget that holds the matched hit plus exactly one expansion.
	base := packSearchResult(matched, 100000, memoryFacetFields)
	oneExpansion := base
	oneExpansion.Expanded = expanded[:1]
	oneExpansion.ExpandedOmitted = 2
	sized, _ := json.Marshal(oneExpansion)
	budget := len(sized)

	result := packExpanded(base, expanded, budget)

	if len(result.Hits) != 1 {
		t.Fatalf("len(hits) = %d, want 1", len(result.Hits))
	}
	if len(result.Expanded) != 1 {
		t.Fatalf("len(expanded) = %d, want 1 (the prefix that fits)", len(result.Expanded))
	}
	if result.Expanded[0].ID != "g1" {
		t.Errorf("kept expansion = %q, want g1: the kept block must be a PREFIX (nearest hop first)", result.Expanded[0].ID)
	}
	if result.ExpandedOmitted != 2 {
		t.Errorf("expanded_omitted = %d, want 2", result.ExpandedOmitted)
	}
	got, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(got) > budget {
		t.Errorf("packed result is %d bytes, over the %d-byte budget: the expanded block is not being budgeted", len(got), budget)
	}
}

// TestDW_6_4_MatchedOverflowStillSpills: the expanded block does not disturb the
// existing spill path. With more matched hits than the budget holds, the matched
// overflow still spills to disk and still reports overflow_path — and expansions
// get only what is left after all of that.
func TestDW_6_4_MatchedOverflowStillSpills(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(spillDirEnv, dir)
	t.Setenv(searchBudgetBytesEnv, "700")

	matched := make([]Hit, 0, 6)
	for _, id := range []string{"s1", "s2", "s3", "s4", "s5", "s6"} {
		matched = append(matched, semanticHit(id, "svc", "owned_by", "team-a", 120))
	}
	backend := &fixedHitsBackend{
		hits:     matched,
		expanded: []Hit{padGraphHit("g1", "team-a", "located_in", "berlin", 120)},
	}
	_, decoded := searchViaWire(t, backend, map[string]any{"query": "q"})

	omitted, _ := decoded["omitted"].(float64)
	if omitted == 0 {
		t.Fatalf("nothing was omitted — the budget is not under pressure, so this test proves nothing: %v", decoded)
	}
	path, _ := decoded["overflow_path"].(string)
	if path == "" {
		t.Fatalf("overflow_path missing: the existing spill path broke when `expanded` was added: %v", decoded)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("spill file %q not written: %v", path, err)
	}

	// Matched hits packed FIRST: at least one survived, and under this pressure
	// the expansion is the thing that gave way.
	hits, _ := decoded["hits"].([]any)
	if len(hits) == 0 {
		t.Fatalf("no matched hits survived: matched must pack before expansions: %v", decoded)
	}
	expanded, _ := decoded["expanded"].([]any)
	if len(expanded) > 0 {
		t.Errorf("an expansion was kept while matched hits were being omitted: %v", expanded)
	}
	if n, _ := decoded["expanded_omitted"].(float64); n != 1 {
		t.Errorf("expanded_omitted = %v, want 1: the dropped expansion must be reported", decoded["expanded_omitted"])
	}
}

// TestExpandedHeaderReportsDroppedExpansions: when the budget drops some but not
// all expansions, the compact-line header says so — the caller can tell the
// block is partial rather than assuming it saw every neighbor.
func TestExpandedHeaderReportsDroppedExpansions(t *testing.T) {
	result := searchResult[Hit]{
		Hits:            []Hit{semanticHit("s1", "svc", "owned_by", "team-a", 5)},
		Expanded:        []Hit{padGraphHit("g1", "team-a", "located_in", "berlin", 5)},
		ExpandedOmitted: 3,
	}
	lines := strings.Split(compactLines(renderSearchResult(result)), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(lines))
	}
	if !strings.Contains(lines[1], "3 more dropped") {
		t.Errorf("header = %q, want it to report the 3 dropped expansions", lines[1])
	}
}

// TestPackExpandedNeverPanicsOnEmptyInputs: no expansions, no matched hits —
// ordinary states (expansion off, empty query), not edge cases that blow up.
func TestPackExpandedNeverPanicsOnEmptyInputs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result searchResult[Hit]
		expand []Hit
	}{
		{"no expansions", searchResult[Hit]{Hits: []Hit{semanticHit("s1", "a", "b", "c", 5)}}, nil},
		{"no matched hits", searchResult[Hit]{Hits: []Hit{}}, []Hit{padGraphHit("g1", "a", "b", "c", 5)}},
		{"neither", searchResult[Hit]{Hits: []Hit{}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := packExpanded(tc.result, tc.expand, 16384)
			if len(got.Expanded) != len(tc.expand) {
				t.Errorf("len(expanded) = %d, want %d", len(got.Expanded), len(tc.expand))
			}
			if got.ExpandedOmitted != 0 {
				t.Errorf("expanded_omitted = %d, want 0 (everything fit)", got.ExpandedOmitted)
			}
		})
	}
}

package mcp

// Phase-6 tests for the `expanded` block at the MCP surface: the block is
// rendered as a distinct, delimited section and budgeted SEPARATELY from the
// matched hits — matched pack first against the whole budget, expansions live on
// whatever is left, and expansions are the first thing dropped under pressure.
// Every expanded hit is a token cost on every LLM call, so none of this is
// cosmetic.

import (
	"context"
	"encoding/json"
	"fmt"
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
	text, decoded := searchViaWire(t, backend, map[string]any{"query": "q", "expand": true})

	// Structured: two blocks, never merged.
	hits, _ := decoded["hits"].([]any)
	expanded, _ := decoded["expanded"].([]any)
	if len(hits) != 1 || len(expanded) != 1 {
		t.Fatalf("hits/expanded = %d/%d, want 1/1 — the blocks must stay separate", len(hits), len(expanded))
	}
	// Matched rows are self-addressing "<source>:<id>\tdate\ttext"; an
	// expansion row is its bare statement — no id (graph has no drill-down)
	// and no date (graph records carry none).
	if h := fmt.Sprint(hits[0]); strings.HasPrefix(h, "graph:") {
		t.Fatalf("a graph hit was rendered into the matched hits array: %v", h)
	}
	statement := strings.Repeat("g", 10)
	if e := fmt.Sprint(expanded[0]); e != statement {
		t.Fatalf("expanded[0] = %q, want the bare statement %q", e, statement)
	}

	// Text: the compact-line block an agent actually reads. The expansion must
	// sit BELOW a header that says what it is, not silently among the matches.
	lines := strings.Split(text, "\n")
	if len(lines) != 4 {
		t.Fatalf("compact lines = %d, want 4 (block header, 1 matched, graph header, 1 expanded):\n%s", len(lines), text)
	}
	if !strings.HasPrefix(lines[0], "memory (") {
		t.Errorf("line 0 = %q, want the memory block header", lines[0])
	}
	if !strings.HasPrefix(lines[1], "semantic:s1\t") {
		t.Errorf("line 1 = %q, want the matched hit under the block header", lines[1])
	}
	if !strings.HasPrefix(lines[2], "graph (") || !strings.Contains(lines[2], "not counted against k") {
		t.Errorf("line 2 = %q, want a header naming the block and stating it is not counted against k", lines[2])
	}
	if lines[3] != statement {
		t.Errorf("line 3 = %q, want the graph expansion's bare statement below the header", lines[3])
	}
	if strings.Contains(lines[3], "\t") || strings.Contains(lines[3], "graph:") {
		t.Errorf("expansion row %q carries an id/date column", lines[3])
	}
}

// TestDW_6_3_ZeroExpansionsOmitsExpandedBlock: with no expansions the block is
// ABSENT from the response — not present-and-empty. An empty header still
// costs tokens and still reads as "expansion ran"; absence is the honest
// encoding.
func TestDW_6_3_ZeroExpansionsOmitsExpandedBlock(t *testing.T) {
	backend := &fixedHitsBackend{hits: []Hit{semanticHit("s1", "svc", "owned_by", "team-a", 10)}}
	text, decoded := searchViaWire(t, backend, map[string]any{"query": "q", "expand": true})

	if _, present := decoded["expanded"]; present {
		t.Errorf("response carries an expanded block with zero expansions: %v", decoded["expanded"])
	}
	if _, present := decoded["expanded_omitted"]; present {
		t.Errorf("response reports dropped expansions with zero expansions: %v", decoded["expanded_omitted"])
	}
	if strings.Contains(text, "graph (") {
		t.Errorf("compact lines carry a graph header for a block that does not exist:\n%s", text)
	}

	// And an empty (non-nil) expansion slice from the backend renders the same
	// as nil: absent.
	packed := packRendered(renderSearchResult(SearchResult{Hits: []Hit{semanticHit("s1", "a", "b", "c", 10)}, Expanded: []Hit{}}), 16384)
	if packed.Expanded != nil || packed.ExpandedOmitted != 0 {
		t.Errorf("empty expansions packed to %v / %d, want nil / 0", packed.Expanded, packed.ExpandedOmitted)
	}
}

// renderedLen is the byte length of the text a SearchResult renders to with
// nothing cut — the measure every budget below is sized against, because it
// is the measure packRendered itself uses.
func renderedLen(res SearchResult) int {
	return len(compactLines(renderSearchResult(res)))
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
	budget := renderedLen(SearchResult{Hits: matched})

	result := packRendered(renderSearchResult(SearchResult{Hits: matched, Expanded: expanded}), budget)

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
// rest counted, and the rendered whole still respects the budget.
func TestDW_6_4_ExpandedFillsLeftoverBudget(t *testing.T) {
	matched := []Hit{semanticHit("s1", "svc", "owned_by", "team-a", 100)}
	expanded := []Hit{
		padGraphHit("g1", "team-a", "located_in", "berlin", 100),
		padGraphHit("g2", "berlin", "part_of", "de", 100),
		padGraphHit("g3", "de", "part_of", "eu", 100),
	}

	// A budget that holds the matched hit plus exactly one expansion, with the
	// header reporting the other two dropped.
	oneExpansion := renderSearchResult(SearchResult{Hits: matched, Expanded: expanded[:1]})
	oneExpansion.ExpandedOmitted = 2
	budget := len(compactLines(oneExpansion))

	result := packRendered(renderSearchResult(SearchResult{Hits: matched, Expanded: expanded}), budget)

	if len(result.Hits) != 1 {
		t.Fatalf("len(hits) = %d, want 1", len(result.Hits))
	}
	if len(result.Expanded) != 1 {
		t.Fatalf("len(expanded) = %d, want 1 (the prefix that fits)", len(result.Expanded))
	}
	if result.Expanded[0].ID != "graph:g1" { // the row prints no id, but the packed value keeps it
		t.Errorf("kept expansion = %q, want graph:g1: the kept block must be a PREFIX (nearest hop first)", result.Expanded[0].ID)
	}
	if result.ExpandedOmitted != 2 {
		t.Errorf("expanded_omitted = %d, want 2", result.ExpandedOmitted)
	}
	if got := len(compactLines(result)); got > budget {
		t.Errorf("rendered result is %d bytes, over the %d-byte budget: the expanded block is not being budgeted", got, budget)
	}
}

// TestDW_6_4_MatchedOverflowStillReportsOmission: the expanded block does not
// disturb the matched omission signal. With more matched hits than the budget
// holds, the matched overflow is still counted on the omission line — and
// expansions get only what is left after all of that.
func TestDW_6_4_MatchedOverflowStillReportsOmission(t *testing.T) {
	t.Setenv(searchBudgetBytesEnv, "700")

	matched := make([]Hit, 0, 6)
	for _, id := range []string{"s1", "s2", "s3", "s4", "s5", "s6"} {
		matched = append(matched, semanticHit(id, "svc", "owned_by", "team-a", 120))
	}
	backend := &fixedHitsBackend{
		hits:     matched,
		expanded: []Hit{padGraphHit("g1", "team-a", "located_in", "berlin", 120)},
	}
	_, decoded := searchViaWire(t, backend, map[string]any{"query": "q", "expand": true})

	omitted, _ := decoded["omitted"].(float64)
	if omitted == 0 {
		t.Fatalf("nothing was omitted — the budget is not under pressure, so this test proves nothing: %v", decoded)
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
	result := renderSearchResult(SearchResult{
		Hits:     []Hit{semanticHit("s1", "svc", "owned_by", "team-a", 5)},
		Expanded: []Hit{padGraphHit("g1", "team-a", "located_in", "berlin", 5)},
	})
	result.ExpandedOmitted = 3
	// block header, hit, graph header, expansion
	lines := strings.Split(compactLines(result), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines = %d, want 4: %q", len(lines), lines)
	}
	if !strings.Contains(lines[2], "3 more dropped") {
		t.Errorf("header = %q, want it to report the 3 dropped expansions", lines[2])
	}
}

// TestPackRenderedNeverPanicsOnEmptyInputs: no expansions, no matched hits —
// ordinary states (expansion off, empty query), not edge cases that blow up.
func TestPackRenderedNeverPanicsOnEmptyInputs(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  SearchResult
	}{
		{"no expansions", SearchResult{Hits: []Hit{semanticHit("s1", "a", "b", "c", 5)}}},
		{"no matched hits", SearchResult{Hits: []Hit{}, Expanded: []Hit{padGraphHit("g1", "a", "b", "c", 5)}}},
		{"neither", SearchResult{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := packRendered(renderSearchResult(tc.res), 16384)
			if len(got.Hits) != len(tc.res.Hits) || got.Omitted != 0 {
				t.Errorf("hits/omitted = %d/%d, want %d/0", len(got.Hits), got.Omitted, len(tc.res.Hits))
			}
			if len(got.Expanded) != len(tc.res.Expanded) {
				t.Errorf("len(expanded) = %d, want %d", len(got.Expanded), len(tc.res.Expanded))
			}
			if got.ExpandedOmitted != 0 {
				t.Errorf("expanded_omitted = %d, want 0 (everything fit)", got.ExpandedOmitted)
			}
		})
	}
}

// expandRecordingBackend is a fixedHitsBackend that records the filter each
// Search received, so a test can see what reached the seam.
type expandRecordingBackend struct {
	fixedHitsBackend
	lastFilter SearchFilter
}

func (b *expandRecordingBackend) Search(ctx context.Context, q string, k int, f SearchFilter) (SearchResult, error) {
	b.lastFilter = f
	return b.fixedHitsBackend.Search(ctx, q, k, f)
}

// TestExpandFalseDiscardsServerExpansions: expansion is opt-in. Without
// expand=true the request asks the server to skip graph traversal, AND any
// expansions that come back anyway are dropped — the fake here always returns
// them, exactly as a server predating SearchRequest.expand would, since it
// ignores the unknown field. With expand=true they ride through.
func TestExpandFalseDiscardsServerExpansions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      map[string]any
		wantAsked bool
	}{
		{"omitted", map[string]any{"query": "q"}, false},
		{"explicit false", map[string]any{"query": "q", "expand": false}, false},
		{"true", map[string]any{"query": "q", "expand": true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &expandRecordingBackend{fixedHitsBackend: fixedHitsBackend{
				hits:     []Hit{semanticHit("s1", "svc", "owned_by", "team-a", 10)},
				expanded: []Hit{padGraphHit("g1", "team-a", "located_in", "berlin", 10)},
			}}
			text, decoded := searchViaWire(t, backend, tc.args)
			if backend.lastFilter.Expand != tc.wantAsked {
				t.Errorf("SearchFilter.Expand = %v, want %v", backend.lastFilter.Expand, tc.wantAsked)
			}
			_, gotGraph := decoded["expanded"]
			if gotGraph != tc.wantAsked || strings.Contains(text, "graph (") != tc.wantAsked {
				t.Errorf("graph block present = %v, want %v:\n%s", gotGraph, tc.wantAsked, text)
			}
		})
	}
}

// TestExpandRejectsNonBoolean: expand is a boolean, and the barricade says so
// in caller terms rather than letting a string through as truthy.
func TestExpandRejectsNonBoolean(t *testing.T) {
	res := callSearchTool(t, newFakeBackend(), map[string]any{"query": "q", "expand": "yes"})
	if res["isError"] != true {
		t.Fatalf("expand:\"yes\" accepted: %v", res)
	}
	content, _ := res["content"].([]any)
	block, _ := content[0].(map[string]any)
	if msg, _ := block["text"].(string); !strings.Contains(msg, "expand must be a boolean") {
		t.Errorf("error = %q, want it to name expand's shape", msg)
	}
}

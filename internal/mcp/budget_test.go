package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// fixedHitsBackend is a Backend that returns a preset slice of Hits
// regardless of query, sliced to k. It lets budget/facet tests control exact
// hit content (real per-tier fields_json) that fakeBackend's substring
// filter can't produce.
type fixedHitsBackend struct {
	knowledgeStubs
	hits []Hit
	// expanded is the graph-expansion block returned beside hits (Phase 6). It
	// is NOT sliced to k: expansions are not counted against k.
	expanded []Hit
}

func (b *fixedHitsBackend) IngestScoped(context.Context, string, string, string, string, string) (string, error) {
	return "", nil
}

func (b *fixedHitsBackend) Search(_ context.Context, _ string, k int, _ SearchFilter) (SearchResult, error) {
	hits := b.hits
	if k > 0 && k < len(hits) {
		hits = hits[:k]
	}
	// expanded is the graph-expansion block (Phase 6); nil in every pre-existing
	// budget test, so those exercise exactly today's matched-only packing.
	return SearchResult{Hits: hits, Expanded: b.expanded}, nil
}

func (b *fixedHitsBackend) Status(context.Context) (Status, error) { return Status{}, nil }

func (b *fixedHitsBackend) Read(context.Context, string, string) (ReadResult, error) {
	return ReadResult{}, errNotFound
}

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
// and returns the compact-line text — the tool response in full, since search
// is text-only — plus that text parsed back into the field names these tests
// assert on. It fails outright if a structuredContent copy reappears: two
// surfaces let the client pick the JSON, which is exactly how the line format
// came to be rendered and then discarded.
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
	if _, present := res["structuredContent"]; present {
		t.Fatalf("memory_search must be text-only: a structuredContent copy means clients render JSON and the line format is never read: %v", res)
	}
	return text, parseCompactLines(t, text)
}

// omissionCounts reads the omission line, which is the only carrier of the
// budget signal now that search emits no structuredContent.
var (
	omissionCounts = regexp.MustCompile(`^\.\.\. (\d+) more omitted for size`)
	blockHeaderRE  = regexp.MustCompile(`^\S+ \((\d+)(?: of (\d+))?\)$`)
)

// parseCompactLines projects the compact-line block back into the field names
// these tests already assert on (hits/omitted/hint/expanded). It parses the TEXT deliberately: the text block is now the
// entire response contract, so a test that read a structured copy would be
// testing something the caller never sees.
func parseCompactLines(t *testing.T, text string) map[string]any {
	t.Helper()
	out := map[string]any{}
	hits, expanded := []any{}, []any{}
	inExpanded := false
	for i, line := range strings.Split(text, "\n") {
		switch {
		case line == "":
		case i == 0 && blockHeaderRE.MatchString(line):
			// "label (N of T)" — T is the exact total the backend reported.
			if m := blockHeaderRE.FindStringSubmatch(line); m[2] != "" {
				n, _ := strconv.Atoi(m[2])
				out["total"] = float64(n)
			}
		case strings.HasPrefix(line, "graph ("):
			// Everything below the graph header belongs to the expansion
			// block, never the matched hits (DW-6.2).
			inExpanded = true
			if m := regexp.MustCompile(`(\d+) more dropped`).FindStringSubmatch(line); m != nil {
				n, _ := strconv.Atoi(m[1])
				out["expanded_omitted"] = float64(n)
			}
		case strings.HasPrefix(line, "... "):
			out["hint"] = strings.TrimPrefix(line, "... ")
			if m := omissionCounts.FindStringSubmatch(line); m != nil {
				n, _ := strconv.Atoi(m[1])
				out["omitted"] = float64(n)
			}
		case inExpanded:
			expanded = append(expanded, line)
		default:
			hits = append(hits, line)
		}
	}
	out["hits"] = hits
	if len(expanded) > 0 {
		out["expanded"] = expanded
	}
	return out
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

// TestDW_2_2_OmissionFieldsPresentOnlyWhenOmitted: over-budget carries an
// omission line (count + instruction); all-fit carries none.
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
		if hint, _ := decoded["hint"].(string); !strings.HasSuffix(hint, "; narrow the query") {
			t.Errorf("hint = %q, want the narrow-the-query instruction", hint)
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

// TestPackSearchResultZeroHits: a zero-hit result produces an empty,
// non-nil page — not a panic, not a null hits array (DW-2.1's zero-hit edge
// case; the packing loop must test at the beginning).
func TestPackSearchResultZeroHits(t *testing.T) {
	result := packSearchResult[Hit](nil, searchBudgetBytesDefault)
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

// TestOmissionLineIsACountAndOneTrueInstruction pins the omission line's
// exact wording. It carries no overflow_path and no facet summary — neither
// was ever acted on — and its instruction must be one that actually surfaces
// the cut hits: memory_search can only be narrowed (raising k would just grow
// the cut), while a paged source names the offset that resumes this page.
func TestOmissionLineIsACountAndOneTrueInstruction(t *testing.T) {
	shown := []renderedHit{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	tests := []struct {
		name string
		r    renderedResult
		want string
	}{
		{"memory", renderedResult{Hits: shown, Omitted: 7}, "... 7 more omitted for size; narrow the query"},
		{"paged first page", renderedResult{Hits: shown, Omitted: 7, Paged: true}, "... 7 more omitted for size; page with offset=3"},
		{"paged later page", renderedResult{Hits: shown, Omitted: 2, Paged: true, Offset: 10}, "... 2 more omitted for size; page with offset=13"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := omissionLine(tc.r); got != tc.want {
				t.Errorf("omissionLine = %q, want %q", got, tc.want)
			}
			if got := compactLines(tc.r); !strings.HasSuffix(got, "\n"+tc.want) {
				t.Errorf("compactLines does not end with the omission line:\n%s", got)
			}
		})
	}
}

// manyHits builds n semantic hits, each padded so the full set overflows a
// small byte budget deterministically.
func manyHits(n int) []Hit {
	hits := make([]Hit, 0, n)
	for i := 0; i < n; i++ {
		hits = append(hits, semanticHit("h"+string(rune('a'+i)), "s", "p", "o", 50))
	}
	return hits
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
type recordingKBackend struct {
	knowledgeStubs
	onSearch func(k int)
}

func (b *recordingKBackend) IngestScoped(context.Context, string, string, string, string, string) (string, error) {
	return "", nil
}
func (b *recordingKBackend) Search(_ context.Context, _ string, k int, _ SearchFilter) (SearchResult, error) {
	b.onSearch(k)
	return SearchResult{}, nil
}
func (b *recordingKBackend) Status(context.Context) (Status, error) { return Status{}, nil }

func (b *recordingKBackend) Read(context.Context, string, string) (ReadResult, error) {
	return ReadResult{}, errNotFound
}

func searchViaWireIgnoringHits(t *testing.T, backend Backend, args map[string]any) (string, map[string]any) {
	t.Helper()
	return searchViaWire(t, backend, args)
}

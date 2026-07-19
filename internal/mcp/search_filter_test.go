package mcp

// Phase-5 MCP-barricade tests: the flat memory_search filter params are decoded
// and validated at the tool entry — a bad request never reaches the backend, and
// every rejection names the vocabulary the caller may use instead.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// callSearchTool drives one tools/call over the wire and returns the result
// object WITHOUT failing on a tool error — these tests assert on the errors.
func callSearchTool(t *testing.T, backend Backend, args map[string]any) map[string]any {
	t.Helper()
	c := startServer(t, backend)
	c.call("initialize", nil)
	resp := c.call("tools/call", map[string]any{"name": ToolSearch, "arguments": args})
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in response: %v", resp)
	}
	return res
}

// toolErrorText returns the error text of a failed tool call, or fails if the
// call SUCCEEDED — a request that should have been rejected but was not is the
// interesting failure, so it gets the loud message.
func toolErrorText(t *testing.T, res map[string]any) string {
	t.Helper()
	if res["isError"] != true {
		t.Fatalf("call succeeded; want a tool error. result: %v", res)
	}
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("tool error carries no content: %v", res)
	}
	block, _ := content[0].(map[string]any)
	text, _ := block["text"].(string)
	return text
}

// --- DW-5.1 ------------------------------------------------------------

// TestDW_5_1_SearchAcceptsEveryFlatFilterParam: every advertised param is
// accepted and lands on the SearchFilter that crosses the Backend seam, with
// since/until already parsed (nothing downstream re-parses caller text).
func TestDW_5_1_SearchAcceptsEveryFlatFilterParam(t *testing.T) {
	b := newFakeBackend()
	res := callSearchTool(t, b, map[string]any{
		"query":              "orders-svc leak",
		"k":                  7,
		"kind":               "conversation",
		"subject":            "orders-svc",
		"predicate":          "owned_by",
		"object":             "team-a",
		"extractor_version":  "v3",
		"since":              "2026-01-01T00:00:00Z",
		"until":              "2026-06-01T00:00:00Z",
		"include_superseded": true,
		"sources":            []any{"episodic", "semantic"},
	})
	if res["isError"] == true {
		t.Fatalf("call failed: %v", res)
	}
	want := SearchFilter{
		Kind: "conversation", Subject: "orders-svc", Predicate: "owned_by", Object: "team-a",
		ExtractorVersion:  "v3",
		Since:             time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Until:             time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		IncludeSuperseded: true,
		Sources:           []string{"episodic", "semantic"},
	}
	if !filtersEqual(b.lastFilter, want) {
		t.Errorf("filter =\n %+v\nwant\n %+v", b.lastFilter, want)
	}
	if b.lastK != 7 {
		t.Errorf("k = %d, want 7", b.lastK)
	}
}

// TestSearchSchemaAdvertisesEveryFilterParam: the tool schema and the accepted
// vocabulary are the same list. A param the schema advertises but the parser
// rejects (or vice versa) is a contract an LLM cannot follow.
func TestSearchSchemaAdvertisesEveryFilterParam(t *testing.T) {
	var schema map[string]any
	for _, tool := range toolSchemas() {
		if tool.Name == ToolSearch {
			schema = tool.InputSchema
		}
	}
	props, _ := schema["properties"].(map[string]any)
	if len(props) != len(searchParams) {
		t.Errorf("schema advertises %d params, parser accepts %d", len(props), len(searchParams))
	}
	for _, p := range searchParams {
		if _, ok := props[p]; !ok {
			t.Errorf("accepted param %q is not advertised in the schema", p)
		}
	}
}

// --- DW-5.3 ------------------------------------------------------------

// TestDW_5_3_SourcesReachTheBackend: sources:["semantic"] crosses the seam intact
// (the exclusion of episodic and graph hits is enforced in retrieval, asserted
// there).
func TestDW_5_3_SourcesReachTheBackend(t *testing.T) {
	b := newFakeBackend()
	callSearchTool(t, b, map[string]any{"query": "q", "sources": []any{"semantic"}})
	if got := b.lastFilter.Sources; len(got) != 1 || got[0] != "semantic" {
		t.Errorf("Sources = %v, want [semantic]", got)
	}
}

// --- DW-5.4 ------------------------------------------------------------

// TestDW_5_4_InvalidFiltersRejectedAtEntry is the barricade contract: each of
// these is rejected AT THE TOOL, the error names what the caller may say
// instead, and the backend is never called — zero round-trips for a request that
// was never going to work.
func TestDW_5_4_InvalidFiltersRejectedAtEntry(t *testing.T) {
	cases := []struct {
		name     string
		args     map[string]any
		wantText []string
	}{
		{
			name:     "unknown filter field",
			args:     map[string]any{"query": "q", "kinds": "conversation"},
			wantText: []string{`unknown parameter "kinds"`, "kind", "subject", "since", "sources"},
		},
		{
			name:     "unknown filter field that looks like an internal one",
			args:     map[string]any{"query": "q", "valid_only": true},
			wantText: []string{`unknown parameter "valid_only"`, "include_superseded"},
		},
		{
			name:     "malformed time since",
			args:     map[string]any{"query": "q", "since": "last tuesday"},
			wantText: []string{"since", "RFC 3339"},
		},
		{
			name:     "malformed time until",
			args:     map[string]any{"query": "q", "until": "last tuesday"},
			wantText: []string{"until", "RFC 3339"},
		},
		{
			name:     "since after until",
			args:     map[string]any{"query": "q", "since": "2026-06-01T00:00:00Z", "until": "2026-01-01T00:00:00Z"},
			wantText: []string{"since", "until", "empty"},
		},
		{
			name:     "unknown source",
			args:     map[string]any{"query": "q", "sources": []any{"graphs"}},
			wantText: []string{`unknown source "graphs"`, "episodic", "semantic", "experience", "graph"},
		},
		{
			name:     "empty sources",
			args:     map[string]any{"query": "q", "sources": []any{}},
			wantText: []string{"sources is empty", "omit it"},
		},
		{
			name:     "empty query",
			args:     map[string]any{"query": ""},
			wantText: []string{"non-empty query"},
		},
		{
			name:     "wrong value type",
			args:     map[string]any{"query": "q", "kind": 7},
			wantText: []string{"kind", "must be a string"},
		},
		{
			name:     "wrong value type for array field",
			args:     map[string]any{"query": "q", "sources": "episodic"},
			wantText: []string{"sources", "must be an array of strings"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newFakeBackend()
			text := toolErrorText(t, callSearchTool(t, b, tc.args))
			for _, want := range tc.wantText {
				if !strings.Contains(text, want) {
					t.Errorf("error %q does not mention %q", text, want)
				}
			}
			if b.searchCalls != 0 {
				t.Errorf("backend called %d times on a rejected request, want 0 — the barricade leaked", b.searchCalls)
			}
		})
	}
}

// --- DW-5.6 ------------------------------------------------------------

// TestDW_5_6_NoFiltersSendsZeroFilter: the pre-filter call shape still produces
// the pre-filter request — a zero SearchFilter (nil Sources, not an empty slice,
// which is a validation error downstream) and the server-chosen k.
func TestDW_5_6_NoFiltersSendsZeroFilter(t *testing.T) {
	b := newFakeBackend()
	res := callSearchTool(t, b, map[string]any{"query": "anything"})
	if res["isError"] == true {
		t.Fatalf("call failed: %v", res)
	}
	if b.searchCalls != 1 {
		t.Fatalf("backend called %d times, want 1", b.searchCalls)
	}
	if !filtersEqual(b.lastFilter, SearchFilter{}) {
		t.Errorf("filter = %+v, want the zero SearchFilter", b.lastFilter)
	}
	if b.lastFilter.Sources != nil {
		t.Errorf("Sources = %v, want nil", b.lastFilter.Sources)
	}
	if b.lastK != defaultRequestK {
		t.Errorf("k = %d, want the server-chosen default %d", b.lastK, defaultRequestK)
	}
}

// TestSearchKBoundsStillEnforced: k <= 0 means "server-chosen", not an error, and
// an explicit k passes through to the backend (the retriever clamps the upper
// bound — the one place that knows MaxK).
func TestSearchKBoundsStillEnforced(t *testing.T) {
	for _, tc := range []struct{ k, want int }{{0, defaultRequestK}, {-5, defaultRequestK}, {3, 3}, {10000, 10000}} {
		b := newFakeBackend()
		args := map[string]any{"query": "q"}
		if tc.k != 0 {
			args["k"] = tc.k
		}
		callSearchTool(t, b, args)
		if b.lastK != tc.want {
			t.Errorf("k=%d -> backend k=%d, want %d", tc.k, b.lastK, tc.want)
		}
	}
}

// TestIncludeSupersededAloneIsAValidRequest: the history flag needs no companion
// filter — "show me everything, including what was superseded" is a complete,
// legal question.
func TestIncludeSupersededAloneIsAValidRequest(t *testing.T) {
	b := newFakeBackend()
	res := callSearchTool(t, b, map[string]any{"query": "q", "include_superseded": true})
	if res["isError"] == true {
		t.Fatalf("call failed: %v", res)
	}
	if !b.lastFilter.IncludeSuperseded || b.searchCalls != 1 {
		t.Errorf("filter = %+v, calls = %d", b.lastFilter, b.searchCalls)
	}
}

// TestSearchArgsRejectNonObject: arguments that are not a JSON object are caller
// garbage, and the rejection still names the vocabulary.
func TestSearchArgsRejectNonObject(t *testing.T) {
	_, _, _, err := parseSearchArgs(json.RawMessage(`["query"]`))
	if err == nil {
		t.Fatal("a JSON array was accepted as arguments")
	}
	if !strings.Contains(err.Error(), "query") {
		t.Errorf("error does not name the valid parameters: %v", err)
	}
}

// TestSearchArgTypeErrorNamesVocabularyNotGoType: a wrong-typed param must name
// the offending param and its expected shape in caller-facing terms — never
// encoding/json's raw UnmarshalTypeError text, which names the Go struct and
// type (e.g. "Go struct field searchArgs.kind of type string") and would be
// the one rejection in this file that fails to name the valid vocabulary.
func TestSearchArgTypeErrorNamesVocabularyNotGoType(t *testing.T) {
	_, _, _, err := parseSearchArgs(json.RawMessage(`{"query":"q","kind":7}`))
	if err == nil {
		t.Fatal("a wrong-typed kind was accepted")
	}
	msg := err.Error()
	if !strings.Contains(msg, "kind") || !strings.Contains(msg, "must be a string") {
		t.Errorf("error does not name the offending param and its expected type: %v", err)
	}
	for _, leak := range []string{"Go struct field", "searchArgs", "mcp."} {
		if strings.Contains(msg, leak) {
			t.Errorf("error leaks a Go type/struct name: %v", err)
		}
	}
}

// filtersEqual compares two SearchFilters (time.Time needs Equal, not ==).
func filtersEqual(a, b SearchFilter) bool {
	if len(a.Sources) != len(b.Sources) {
		return false
	}
	for i := range a.Sources {
		if a.Sources[i] != b.Sources[i] {
			return false
		}
	}
	return a.Kind == b.Kind && a.Subject == b.Subject && a.Predicate == b.Predicate &&
		a.Object == b.Object && a.ExtractorVersion == b.ExtractorVersion &&
		a.Since.Equal(b.Since) && a.Until.Equal(b.Until) &&
		a.IncludeSuperseded == b.IncludeSuperseded
}

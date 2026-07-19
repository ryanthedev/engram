package retrieval_test

// Phase-5 retrieval-side tests: the contracts the caller-facing memory_search
// surface compiles down to — the tier-neutral time alias, the validity filter
// that include_superseded relaxes (and the ACL it must NOT relax), source
// narrowing, injection safety, and the unfilterable-source rule.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/ryanthedev/engram/internal/acl"
	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/retrieval"
)

// aclFakeEdges is an acl.EdgeSource for wiring a real acl.Filter into the
// retriever, so the ACL clause under test is the production one.
type aclFakeEdges struct{ reach map[string]acl.Reach }

func (f aclFakeEdges) Reachability(_ context.Context, id auth.Identity) (acl.Reach, error) {
	return f.reach[id.UserID], nil
}

// timeRange is the predicate server.compileSearchFilter emits for since/until.
func timeRange(gte, lte string) retrieval.Predicate {
	bounds := map[string]any{}
	if gte != "" {
		bounds["gte"] = gte
	}
	if lte != "" {
		bounds["lte"] = lte
	}
	return retrieval.Predicate{Field: retrieval.TimeField, Op: "range", Value: bounds}
}

// --- DW-5.1: every flat param reaches the right tier ------------------

// TestDW_5_1_TimeAliasRoutesToPerTierField: ONE since/until pair compiles to ONE
// predicate on the tier-neutral time field, and each tier filters on its OWN
// event-time field — episodic occurred_at, semantic valid_at. Neither tier is
// left unconstrained (which is what would happen if since/until named a single
// physical field), and the alias itself never appears in a query body.
func TestDW_5_1_TimeAliasRoutesToPerTierField(t *testing.T) {
	r, captured, _, _ := newFilterRetriever(t, emptyResults)
	_, err := r.Search(context.Background(), retrieval.Query{Text: "x", K: 5}, retrieval.Filter{
		Predicates: []retrieval.Predicate{timeRange("2026-01-01T00:00:00Z", "2026-06-01T00:00:00Z")},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	reqs := captured()

	episodic := bodyFor(t, reqs, "ep-idx")
	if !strings.Contains(episodic, `"range":{"occurred_at":{"gte":"2026-01-01T00:00:00Z","lte":"2026-06-01T00:00:00Z"}}`) {
		t.Errorf("episodic query does not bound occurred_at: %s", episodic)
	}
	semantic := bodyFor(t, reqs, "sem-idx")
	if !strings.Contains(semantic, `"range":{"valid_at":{"gte":"2026-01-01T00:00:00Z","lte":"2026-06-01T00:00:00Z"}}`) {
		t.Errorf("semantic query does not bound valid_at: %s", semantic)
	}
	for _, body := range []string{episodic, semantic} {
		if strings.Contains(body, `"`+retrieval.TimeField+`"`) {
			t.Errorf("the caller-facing alias leaked into the query body as a field name: %s", body)
		}
	}
}

// TestDW_5_1_OpenTimeBoundsCompile: since alone and until alone are legal open
// intervals, not errors.
func TestDW_5_1_OpenTimeBoundsCompile(t *testing.T) {
	for _, tc := range []struct {
		name, gte, lte, want string
	}{
		{"since only", "2026-01-01T00:00:00Z", "", `"range":{"occurred_at":{"gte":"2026-01-01T00:00:00Z"}}`},
		{"until only", "", "2026-06-01T00:00:00Z", `"range":{"occurred_at":{"lte":"2026-06-01T00:00:00Z"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, captured, _, _ := newFilterRetriever(t, emptyResults)
			_, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, retrieval.Filter{
				Predicates: []retrieval.Predicate{timeRange(tc.gte, tc.lte)},
			})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if body := bodyFor(t, captured(), "ep-idx"); !strings.Contains(body, tc.want) {
				t.Errorf("episodic body missing %s: %s", tc.want, body)
			}
		})
	}
}

// TestDW_5_1_SemanticParamsRouteToSemanticOnly: subject/predicate/object/
// extractor_version are semantic-only, so they must constrain the semantic query
// and never appear in the episodic one (whose index does not map them at all).
func TestDW_5_1_SemanticParamsRouteToSemanticOnly(t *testing.T) {
	r, captured, _, _ := newFilterRetriever(t, emptyResults)
	_, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, retrieval.Filter{
		Predicates: []retrieval.Predicate{
			{Field: "subject", Op: "term", Value: "orders-svc"},
			{Field: "predicate", Op: "term", Value: "owned_by"},
			{Field: "object", Op: "term", Value: "team-a"},
			{Field: "extractor_version", Op: "term", Value: "v3"},
		},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	reqs := captured()
	semantic := bodyFor(t, reqs, "sem-idx")
	for _, want := range []string{
		`{"term":{"subject":"orders-svc"}}`, `{"term":{"predicate":"owned_by"}}`,
		`{"term":{"object":"team-a"}}`, `{"term":{"extractor_version":"v3"}}`,
	} {
		if !strings.Contains(semantic, want) {
			t.Errorf("semantic body missing %s: %s", want, semantic)
		}
	}
	episodic := bodyFor(t, reqs, "ep-idx")
	for _, absent := range []string{"subject", "predicate", "object", "extractor_version"} {
		if strings.Contains(episodic, `"`+absent+`"`) {
			t.Errorf("episodic body carries semantic-only field %q (its index does not map it): %s", absent, episodic)
		}
	}
}

// --- DW-5.2: include_superseded relaxes validity, never the ACL -------

// TestDW_5_2_ValidOnlyGatesTheHistoryClause: ValidOnly=true (the default the
// server derives from include_superseded=false) emits the current-state clause;
// ValidOnly=false (include_superseded=true) omits it, which is what surfaces
// superseded and retracted historical versions.
func TestDW_5_2_ValidOnlyGatesTheHistoryClause(t *testing.T) {
	for _, tc := range []struct {
		name          string
		validOnly     bool
		wantValidatee bool
	}{
		{"include_superseded absent => ValidOnly", true, true},
		{"include_superseded true => history visible", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, captured, _, _ := newFilterRetriever(t, emptyResults)
			_, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, retrieval.Filter{ValidOnly: tc.validOnly})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			body := bodyFor(t, captured(), "sem-idx")
			got := strings.Contains(body, `"must_not":[{"exists":{"field":"expired_at"}}]`) &&
				strings.Contains(body, `"invalid_at"`)
			if got != tc.wantValidatee {
				t.Errorf("validity clause present = %v, want %v: %s", got, tc.wantValidatee, body)
			}
		})
	}
}

// TestDW_5_2_IncludeSupersededCannotBypassACL is the security assertion behind
// the new knob: include_superseded flips ONE filter (validity). The ACL clause
// and the tenant term are compiled independently and must still be inside the
// query — a caller cannot use "show me history" to see a scope they may not
// read.
func TestDW_5_2_IncludeSupersededCannotBypassACL(t *testing.T) {
	srv, captured := newFakeSearchServer(t, emptyResults)
	edges := aclFakeEdges{reach: map[string]acl.Reach{"u1": {Agents: []string{"a1"}}}}
	r := retrieval.NewOpenSearchRetriever(srv.Client(), srv.URL, embed.NewFakeEmbedder(4, nil),
		retrieval.WithIndices("ep-idx", "sem-idx"),
		retrieval.WithACL(acl.NewFilter(edges, slog.Default())))

	caller := auth.Identity{TenantID: "t1", UserID: "u1", AgentID: "a1"}
	_, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, retrieval.Filter{
		TenantID:  "t1",
		ValidOnly: false, // include_superseded: true
		Identity:  caller,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, idx := range []string{"ep-idx", "sem-idx"} {
		body := bodyFor(t, captured(), idx)
		if !strings.Contains(body, `"term":{"tenant_id":"t1"}`) {
			t.Errorf("%s: tenant term missing under include_superseded — the tenancy boundary must not depend on the validity filter: %s", idx, body)
		}
		if !strings.Contains(body, "scope") || !strings.Contains(body, "owner_agent_id") {
			t.Errorf("%s: ACL scope clause missing under include_superseded — history must not widen what a caller may read: %s", idx, body)
		}
	}
}

// --- DW-5.3: sources narrow the search end to end ---------------------

// TestDW_5_3_SemanticOnlySearchHasNoEpisodicOrGraphHits: sources:["semantic"]
// queries the semantic index and NOTHING else — the episodic tier is never
// searched, the registered experience tier never runs, and the graph post-hook
// never runs, so no hit from any of them can be in the result.
func TestDW_5_3_SemanticOnlySearchHasNoEpisodicOrGraphHits(t *testing.T) {
	r, captured, tier, hook := newFilterRetriever(t, emptyResults)
	tier.hits = []retrieval.Hit{{ID: "exp-1", Score: 9, Source: "experience"}}

	hits, err := r.Search(context.Background(), retrieval.Query{Text: "x", K: 10}, retrieval.Filter{
		Sources: []string{"semantic"},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if idx := searchedIndices(captured()); len(idx) != 1 || idx[0] != "sem-idx" {
		t.Errorf("searched indices = %v, want only [sem-idx]", idx)
	}
	if tier.calls != 0 || hook.calls != 0 {
		t.Errorf("experience tier ran %d times and graph hook %d times, want 0 and 0", tier.calls, hook.calls)
	}
	for _, h := range hits {
		if h.Source != "semantic" {
			t.Errorf("hit from source %q survived sources:[semantic]", h.Source)
		}
	}
}

// --- DW-5.7: an adversarial filter value is data, never structure -----

// TestDW_5_7_AdversarialValueParameterizedIntoQueryBody: a filter value carrying
// OpenSearch DSL is placed in a clause STRUCTURE and marshaled, so it arrives at
// the cluster as an inert string that matches no keyword. The assertion is on
// the decoded body, not on its text: the value must be a leaf string equal, byte
// for byte, to what the caller sent — never parsed back into query structure.
func TestDW_5_7_AdversarialValueParameterizedIntoQueryBody(t *testing.T) {
	const evil = `x"}}]}},"query":{"match_all":{}},"z":{"script":{"source":"ctx._source.remove('acl')"`

	r, captured, _, _ := newFilterRetriever(t, emptyResults)
	_, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, retrieval.Filter{
		TenantID:   "t1",
		Predicates: []retrieval.Predicate{{Field: "kind", Op: "term", Value: evil}},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	raw := bodyFor(t, captured(), "ep-idx")

	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("emitted body is not valid JSON (interpolation would do exactly this): %v", err)
	}
	if _, hijacked := body["z"]; hijacked {
		t.Fatalf("the filter value injected a top-level key into the query body: %s", raw)
	}
	found := false
	for _, clause := range filterClausesOf(t, body) {
		term, ok := clause["term"].(map[string]any)
		if !ok {
			continue
		}
		if got, ok := term["kind"].(string); ok {
			found = true
			if got != evil {
				t.Errorf("term value mangled in transit:\n got: %q\nwant: %q", got, evil)
			}
		}
	}
	if !found {
		t.Fatalf("no term clause on kind in the emitted body: %s", raw)
	}
}

// filterClausesOf digs the bool filter clauses out of an emitted query body,
// whichever sub-query they hang under (BM25 or kNN).
func filterClausesOf(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	var out []map[string]any
	var walk func(v any)
	walk = func(v any) {
		switch node := v.(type) {
		case map[string]any:
			if f, ok := node["filter"].([]any); ok {
				for _, c := range f {
					if clause, ok := c.(map[string]any); ok {
						out = append(out, clause)
					}
				}
			}
			for _, child := range node {
				walk(child)
			}
		case []any:
			for _, child := range node {
				walk(child)
			}
		}
	}
	walk(body)
	return out
}

// --- the unfilterable-source contract (Phase 4 carry-forward) ---------

// TestFilteredSearchExcludesUnfilterableSources is the explicit decision this
// phase owes Phase 4's review: a registered tier source and a post-hook declare
// NO filterable fields, so they cannot honor a filter. Rather than letting their
// hits ride back unconstrained beside constrained ones — a kind="decision" search
// silently returning experience hits of every kind — a filtered search does not
// run them at all. Unfiltered, both still run: this narrows a filtered search,
// it does not disable a feature.
func TestFilteredSearchExcludesUnfilterableSources(t *testing.T) {
	t.Run("unfiltered: both run", func(t *testing.T) {
		r, _, tier, hook := newFilterRetriever(t, emptyResults)
		if _, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, retrieval.Filter{}); err != nil {
			t.Fatalf("Search: %v", err)
		}
		if tier.calls != 1 || hook.calls != 1 {
			t.Errorf("experience=%d graph=%d, want 1 and 1 — an unfiltered search must keep every source", tier.calls, hook.calls)
		}
	})
	t.Run("filtered: neither runs", func(t *testing.T) {
		r, captured, tier, hook := newFilterRetriever(t, emptyResults)
		_, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, retrieval.Filter{
			Predicates: []retrieval.Predicate{{Field: "kind", Op: "term", Value: "decision"}},
		})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if tier.calls != 0 || hook.calls != 0 {
			t.Errorf("experience=%d graph=%d, want 0 and 0 — an unfilterable source must not smuggle unconstrained hits into a filtered result", tier.calls, hook.calls)
		}
		// The filterable tiers still run: this excludes what cannot be filtered,
		// it does not turn the search off.
		if idx := searchedIndices(captured()); len(idx) != 2 {
			t.Errorf("searched indices = %v, want both built-in tiers", idx)
		}
	})
}

// TestFilteredSearchNamingAnUnfilterableSourceErrors: when the exclusion is
// implicit (Sources omitted) it is silent and documented. When the caller
// EXPLICITLY names a source that cannot be filtered and also passes a filter,
// the two requests contradict each other and the caller hears about it.
func TestFilteredSearchNamingAnUnfilterableSourceErrors(t *testing.T) {
	for _, source := range []string{"graph", "experience"} {
		t.Run(source, func(t *testing.T) {
			r, captured, _, _ := newFilterRetriever(t, emptyResults)
			_, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, retrieval.Filter{
				Sources:    []string{"semantic", source},
				Predicates: []retrieval.Predicate{{Field: "subject", Op: "term", Value: "orders-svc"}},
			})
			if err == nil {
				t.Fatalf("filtering %q silently succeeded — the source cannot honor the filter", source)
			}
			if !errors.Is(err, retrieval.ErrInvalidFilter) {
				t.Errorf("error does not wrap ErrInvalidFilter (the barricades map it to InvalidArgument): %v", err)
			}
			if !strings.Contains(err.Error(), source) {
				t.Errorf("error does not name the offending source %q: %v", source, err)
			}
			if n := len(captured()); n != 0 {
				t.Errorf("%d cluster round-trips on a rejected filter, want 0", n)
			}
		})
	}
}

// TestErrInvalidFilterWrapsCallerErrors: every caller-input rejection is
// classifiable as ErrInvalidFilter, which is what lets the gRPC and MCP
// barricades report a caller mistake as InvalidArgument rather than as an
// internal server fault.
func TestErrInvalidFilterWrapsCallerErrors(t *testing.T) {
	cases := map[string]retrieval.Filter{
		"unknown field":  {Predicates: []retrieval.Predicate{{Field: "nope", Op: "term", Value: "x"}}},
		"unsupported op": {Predicates: []retrieval.Predicate{{Field: "kind", Op: "range", Value: map[string]any{"gte": "a"}}}},
		"non-scalar value": {Predicates: []retrieval.Predicate{{Field: "kind", Op: "term",
			Value: map[string]any{"script": "evil"}}}},
		"unknown source": {Sources: []string{"nope"}},
		"empty sources":  {Sources: []string{}},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			r, _, _, _ := newFilterRetriever(t, emptyResults)
			_, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, f)
			if err == nil {
				t.Fatalf("no error")
			}
			if !errors.Is(err, retrieval.ErrInvalidFilter) {
				t.Errorf("error does not wrap ErrInvalidFilter: %v", err)
			}
		})
	}
}

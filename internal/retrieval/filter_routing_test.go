package retrieval_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/retrieval"
)

// --- helpers ----------------------------------------------------------

// bodyFor returns the single captured request body for the given index, or
// fails if that index was not searched exactly once.
func bodyFor(t *testing.T, reqs []reqCapture, index string) string {
	t.Helper()
	var found []string
	for _, c := range reqs {
		if strings.Contains(c.path, index) {
			found = append(found, c.raw)
		}
	}
	if len(found) != 1 {
		t.Fatalf("index %q searched %d times, want exactly 1", index, len(found))
	}
	return found[0]
}

// searchedIndices lists the indices the fake cluster saw a query for.
func searchedIndices(reqs []reqCapture) []string {
	out := make([]string, 0, len(reqs))
	for _, c := range reqs {
		out = append(out, strings.Trim(strings.TrimSuffix(c.path, "/_search"), "/"))
	}
	return out
}

// countingTier is a registered TierSource that records whether it was searched.
type countingTier struct {
	calls int
	hits  []retrieval.Hit
}

func (c *countingTier) Search(context.Context, auth.Identity, retrieval.Query) ([]retrieval.Hit, error) {
	c.calls++
	return c.hits, nil
}

// countingHook is a registered PostHook that records whether it ran.
type countingHook struct{ calls int }

func (c *countingHook) Apply(_ context.Context, _ auth.Identity, hits []retrieval.Hit) ([]retrieval.Hit, error) {
	c.calls++
	return hits, nil
}

// emptyResults answers every index with zero hits.
func emptyResults(string) (int, any) { return http.StatusOK, hitsBody() }

// newFilterRetriever wires a retriever over the fake cluster with a registered
// tier source ("experience") and post-hook ("graph") so Sources routing has a
// full namespace to resolve against.
func newFilterRetriever(t *testing.T, respond respondFunc) (*retrieval.MultiRetriever, func() []reqCapture, *countingTier, *countingHook) {
	t.Helper()
	srv, captured := newFakeSearchServer(t, respond)
	r := retrieval.NewOpenSearchRetriever(srv.Client(), srv.URL, embed.NewFakeEmbedder(4, nil),
		retrieval.WithIndices("ep-idx", "sem-idx"))
	tier := &countingTier{}
	hook := &countingHook{}
	r.RegisterTier("experience", tier)
	r.RegisterPostHook("graph", hook)
	return r, captured, tier, hook
}

// --- DW-4.2 -----------------------------------------------------------

// TestDW_4_2_KindPredicateRoutesToEpisodicOnly is the phase's core assertion:
// "kind" is declared by the episodic tier only, so a kind predicate must
// constrain the episodic query and leave the SEMANTIC query untouched. Under
// the naive shared-clause implementation this phase replaces, the semantic
// query would carry {"term":{"kind":...}} — a field the semantic index does not
// even map (templates/semantic.json is "dynamic":"strict") — and would return
// zero hits. The strongest form of the assertion is the byte comparison: the
// semantic body must equal the body it emits with NO predicates at all.
func TestDW_4_2_KindPredicateRoutesToEpisodicOnly(t *testing.T) {
	r, captured, _, _ := newFilterRetriever(t, emptyResults)
	q := retrieval.Query{Text: "orders-svc leak", K: 5}

	// Baseline: the same search with no predicates.
	if _, err := r.Search(context.Background(), q, retrieval.Filter{TenantID: "t1"}); err != nil {
		t.Fatalf("baseline Search: %v", err)
	}
	baselineSemantic := bodyFor(t, captured(), "sem-idx")

	r2, captured2, _, _ := newFilterRetriever(t, emptyResults)
	_, err := r2.Search(context.Background(), q, retrieval.Filter{
		TenantID:   "t1",
		Predicates: []retrieval.Predicate{{Field: "kind", Op: "term", Value: "decision"}},
	})
	if err != nil {
		t.Fatalf("Search with kind predicate: %v", err)
	}
	reqs := captured2()

	episodic := bodyFor(t, reqs, "ep-idx")
	if !strings.Contains(episodic, `{"term":{"kind":"decision"}}`) {
		t.Errorf("episodic query missing the kind predicate it declares: %s", episodic)
	}

	semantic := bodyFor(t, reqs, "sem-idx")
	if strings.Contains(semantic, "kind") {
		t.Errorf("semantic query carries a kind clause for a field it does not declare (this is the zeroing bug): %s", semantic)
	}
	if semantic != baselineSemantic {
		t.Errorf("semantic query changed under an episodic-only predicate\n got: %s\nwant: %s", semantic, baselineSemantic)
	}
}

// TestPredicateValidOnBothTiersReachesBoth: created_at is declared by BOTH
// tiers, so its clause must appear in both queries (routing is per-field, not
// per-tier-exclusive).
func TestPredicateValidOnBothTiersReachesBoth(t *testing.T) {
	r, captured, _, _ := newFilterRetriever(t, emptyResults)
	_, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, retrieval.Filter{
		Predicates: []retrieval.Predicate{{Field: "created_at", Op: "range", Value: map[string]any{"gte": "2026-01-01"}}},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, idx := range []string{"ep-idx", "sem-idx"} {
		if body := bodyFor(t, captured(), idx); !strings.Contains(body, `{"range":{"created_at":{"gte":"2026-01-01"}}}`) {
			t.Errorf("%s query missing the created_at range both tiers declare: %s", idx, body)
		}
	}
}

// TestSemanticOnlyPredicateLeavesEpisodicUnconstrained is DW-4.2's mirror:
// subject is semantic-only, so the episodic query must not carry it.
func TestSemanticOnlyPredicateLeavesEpisodicUnconstrained(t *testing.T) {
	r, captured, _, _ := newFilterRetriever(t, emptyResults)
	_, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, retrieval.Filter{
		Predicates: []retrieval.Predicate{{Field: "subject", Op: "term", Value: "orders-svc"}},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if body := bodyFor(t, captured(), "sem-idx"); !strings.Contains(body, `{"term":{"subject":"orders-svc"}}`) {
		t.Errorf("semantic query missing the subject predicate: %s", body)
	}
	if body := bodyFor(t, captured(), "ep-idx"); strings.Contains(body, "subject") {
		t.Errorf("episodic query carries a semantic-only field: %s", body)
	}
}

// --- DW-4.3 -----------------------------------------------------------

// TestDW_4_3_GoldenQueryBodyUnchangedWithoutFilters pins the no-predicate,
// no-Sources query body byte-for-byte. The want strings were captured from the
// UNMODIFIED retriever (before Predicates/Sources existed) for the same inputs
// — a golden-byte comparison, not a re-derivation of the code under test. Any
// change to clause construction, ordering, or the emitted body for a plain
// search fails here, which is the point: adding a filter surface must cost the
// existing query path exactly zero bytes.
func TestDW_4_3_GoldenQueryBodyUnchangedWithoutFilters(t *testing.T) {
	cases := []struct {
		name         string
		mode         retrieval.SearchMode
		filter       retrieval.Filter
		wantEpisodic string
		wantSemantic string
	}{
		{
			name:         "hybrid, zero filter",
			mode:         retrieval.ModeHybrid,
			filter:       retrieval.Filter{},
			wantEpisodic: `{"_source":{"excludes":["text_embedding","fact_embedding"]},"query":{"hybrid":{"queries":[{"match":{"text":"orders-svc leak"}},{"knn":{"text_embedding":{"k":5,"vector":[-0.32442936,-0.29370037,-0.77899903,-0.4490503]}}}]}},"size":5}`,
			wantSemantic: `{"_source":{"excludes":["text_embedding","fact_embedding"]},"query":{"hybrid":{"queries":[{"match":{"statement":"orders-svc leak"}},{"knn":{"fact_embedding":{"k":5,"vector":[-0.32442936,-0.29370037,-0.77899903,-0.4490503]}}}]}},"size":5}`,
		},
		{
			name:         "hybrid, tenancy + validity",
			mode:         retrieval.ModeHybrid,
			filter:       retrieval.Filter{TenantID: "t1", UserID: "agent-9", ValidOnly: true},
			wantEpisodic: `{"_source":{"excludes":["text_embedding","fact_embedding"]},"query":{"hybrid":{"queries":[{"bool":{"filter":[{"term":{"tenant_id":"t1"}},{"term":{"owner_agent_id":"agent-9"}}],"must":[{"match":{"text":"orders-svc leak"}}]}},{"knn":{"text_embedding":{"filter":{"bool":{"filter":[{"term":{"tenant_id":"t1"}},{"term":{"owner_agent_id":"agent-9"}}]}},"k":5,"vector":[-0.32442936,-0.29370037,-0.77899903,-0.4490503]}}}]}},"size":5}`,
			wantSemantic: `{"_source":{"excludes":["text_embedding","fact_embedding"]},"query":{"hybrid":{"queries":[{"bool":{"filter":[{"term":{"tenant_id":"t1"}},{"term":{"owner_agent_id":"agent-9"}},{"bool":{"minimum_should_match":1,"must_not":[{"exists":{"field":"expired_at"}}],"should":[{"bool":{"must_not":[{"exists":{"field":"invalid_at"}}]}},{"range":{"invalid_at":{"gt":"now"}}}]}}],"must":[{"match":{"statement":"orders-svc leak"}}]}},{"knn":{"fact_embedding":{"filter":{"bool":{"filter":[{"term":{"tenant_id":"t1"}},{"term":{"owner_agent_id":"agent-9"}},{"bool":{"minimum_should_match":1,"must_not":[{"exists":{"field":"expired_at"}}],"should":[{"bool":{"must_not":[{"exists":{"field":"invalid_at"}}]}},{"range":{"invalid_at":{"gt":"now"}}}]}}]}},"k":5,"vector":[-0.32442936,-0.29370037,-0.77899903,-0.4490503]}}}]}},"size":5}`,
		},
		{
			name:         "bm25 only, zero filter",
			mode:         retrieval.ModeBM25Only,
			filter:       retrieval.Filter{},
			wantEpisodic: `{"_source":{"excludes":["text_embedding","fact_embedding"]},"query":{"match":{"text":"orders-svc leak"}},"size":5}`,
			wantSemantic: `{"_source":{"excludes":["text_embedding","fact_embedding"]},"query":{"match":{"statement":"orders-svc leak"}},"size":5}`,
		},
		{
			name:         "bm25 only, tenancy + validity",
			mode:         retrieval.ModeBM25Only,
			filter:       retrieval.Filter{TenantID: "t1", UserID: "agent-9", ValidOnly: true},
			wantEpisodic: `{"_source":{"excludes":["text_embedding","fact_embedding"]},"query":{"bool":{"filter":[{"term":{"tenant_id":"t1"}},{"term":{"owner_agent_id":"agent-9"}}],"must":[{"match":{"text":"orders-svc leak"}}]}},"size":5}`,
			wantSemantic: `{"_source":{"excludes":["text_embedding","fact_embedding"]},"query":{"bool":{"filter":[{"term":{"tenant_id":"t1"}},{"term":{"owner_agent_id":"agent-9"}},{"bool":{"minimum_should_match":1,"must_not":[{"exists":{"field":"expired_at"}}],"should":[{"bool":{"must_not":[{"exists":{"field":"invalid_at"}}]}},{"range":{"invalid_at":{"gt":"now"}}}]}}],"must":[{"match":{"statement":"orders-svc leak"}}]}},"size":5}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, captured := newFakeSearchServer(t, emptyResults)
			r := retrieval.NewOpenSearchRetriever(srv.Client(), srv.URL, embed.NewFakeEmbedder(4, nil),
				retrieval.WithIndices("ep-idx", "sem-idx"), retrieval.WithMode(tc.mode))
			if _, err := r.Search(context.Background(), retrieval.Query{Text: "orders-svc leak", K: 5}, tc.filter); err != nil {
				t.Fatalf("Search: %v", err)
			}
			reqs := captured()
			if got := bodyFor(t, reqs, "ep-idx"); got != tc.wantEpisodic {
				t.Errorf("episodic body drifted from golden\n got: %s\nwant: %s", got, tc.wantEpisodic)
			}
			if got := bodyFor(t, reqs, "sem-idx"); got != tc.wantSemantic {
				t.Errorf("semantic body drifted from golden\n got: %s\nwant: %s", got, tc.wantSemantic)
			}
		})
	}
}

// --- DW-4.4 -----------------------------------------------------------

// TestDW_4_4_UnknownFieldErrorNamesValidFields: a predicate on a field no
// selected tier declares is a validation error whose message lists the valid
// filterable fields — an LLM caller must be able to self-correct from the error
// alone. It must also be raised BEFORE any cluster round-trip.
func TestDW_4_4_UnknownFieldErrorNamesValidFields(t *testing.T) {
	cases := []struct {
		name      string
		filter    retrieval.Filter
		wantNamed []string
		notNamed  []string
	}{
		{
			name:      "field declared by no tier",
			filter:    retrieval.Filter{Predicates: []retrieval.Predicate{{Field: "password", Op: "term", Value: "x"}}},
			wantNamed: []string{"kind", "subject", "predicate", "object", "extractor_version", "occurred_at", "valid_at"},
		},
		{
			name: "field declared only by an UNSELECTED tier",
			filter: retrieval.Filter{
				Sources:    []string{"semantic"},
				Predicates: []retrieval.Predicate{{Field: "kind", Op: "term", Value: "decision"}},
			},
			wantNamed: []string{"subject", "extractor_version"},
			notNamed:  []string{"kind"}, // episodic is not selected: its fields are not offered
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := failingHandler(t) // any HTTP call fails the test: validation precedes I/O
			r := retrieval.NewOpenSearchRetriever(srv.Client(), srv.URL, embed.NewFakeEmbedder(4, nil))
			_, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, tc.filter)
			if err == nil {
				t.Fatal("Search accepted a predicate on an unknown field, want a validation error")
			}
			for _, want := range tc.wantNamed {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not name valid field %q: %v", want, err)
				}
			}
			for _, notWant := range tc.notNamed {
				if strings.Contains(err.Error(), notWant) {
					// The offending field name itself is quoted in the message; make
					// sure the *valid fields* list is what we're checking.
					if !strings.Contains(err.Error(), `"`+notWant+`"`) {
						t.Errorf("error offers field %q from an unselected tier: %v", notWant, err)
					}
				}
			}
		})
	}
}

// TestUnsupportedOpForFieldTypeErrors: ops are declared per field type — a
// range on a keyword or a prefix on a date is a validation error naming the
// valid ops, not a silently-dropped or malformed clause.
func TestUnsupportedOpForFieldTypeErrors(t *testing.T) {
	cases := []struct {
		name string
		pred retrieval.Predicate
	}{
		{"range on a keyword field", retrieval.Predicate{Field: "kind", Op: "range", Value: map[string]any{"gte": "a"}}},
		{"prefix on a date field", retrieval.Predicate{Field: "valid_at", Op: "prefix", Value: "2026"}},
		{"unknown op entirely", retrieval.Predicate{Field: "subject", Op: "regexp", Value: ".*"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := failingHandler(t)
			r := retrieval.NewOpenSearchRetriever(srv.Client(), srv.URL, embed.NewFakeEmbedder(4, nil))
			_, err := r.Search(context.Background(), retrieval.Query{Text: "x"},
				retrieval.Filter{Predicates: []retrieval.Predicate{tc.pred}})
			if err == nil {
				t.Fatalf("Search accepted %s, want a validation error", tc.name)
			}
			if !strings.Contains(err.Error(), "valid ops") {
				t.Errorf("error does not name the valid ops: %v", err)
			}
		})
	}
}

// TestRangeBounds covers the gte-only / lte-only / both / neither edge cases.
func TestRangeBounds(t *testing.T) {
	cases := []struct {
		name    string
		value   any
		wantErr bool
		want    string
	}{
		{name: "gte only", value: map[string]any{"gte": "2026-01-01"}, want: `{"range":{"valid_at":{"gte":"2026-01-01"}}}`},
		{name: "lte only", value: map[string]any{"lte": "2026-12-31"}, want: `{"range":{"valid_at":{"lte":"2026-12-31"}}}`},
		{name: "both", value: map[string]any{"gte": "2026-01-01", "lte": "2026-12-31"}, want: `{"range":{"valid_at":{"gte":"2026-01-01","lte":"2026-12-31"}}}`},
		{name: "neither", value: map[string]any{}, wantErr: true},
		{name: "unrecognized bound only", value: map[string]any{"gt": "2026-01-01"}, wantErr: true},
		{name: "not a bounds map", value: "2026-01-01", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, captured := newFakeSearchServer(t, emptyResults)
			r := retrieval.NewOpenSearchRetriever(srv.Client(), srv.URL, embed.NewFakeEmbedder(4, nil),
				retrieval.WithIndices("ep-idx", "sem-idx"))
			_, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, retrieval.Filter{
				Sources:    []string{"semantic"},
				Predicates: []retrieval.Predicate{{Field: "valid_at", Op: "range", Value: tc.value}},
			})
			if tc.wantErr {
				if err == nil {
					t.Fatal("Search accepted an empty/invalid range, want a validation error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if body := bodyFor(t, captured(), "sem-idx"); !strings.Contains(body, tc.want) {
				t.Errorf("semantic query missing %s: %s", tc.want, body)
			}
		})
	}
}

// TestTooManyPredicatesRejected bounds caller-supplied input (the MaxK
// rationale, applied to predicate count).
func TestTooManyPredicatesRejected(t *testing.T) {
	srv := failingHandler(t)
	r := retrieval.NewOpenSearchRetriever(srv.Client(), srv.URL, embed.NewFakeEmbedder(4, nil))
	preds := make([]retrieval.Predicate, retrieval.MaxPredicates+1)
	for i := range preds {
		preds[i] = retrieval.Predicate{Field: "kind", Op: "term", Value: "decision"}
	}
	if _, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, retrieval.Filter{Predicates: preds}); err == nil {
		t.Fatal("Search accepted more than MaxPredicates predicates, want a validation error")
	}
}

// --- DW-4.5 / DW-4.7: Sources ----------------------------------------

// TestDW_4_5_SourcesSkipsEpisodicTierAndGraphPostHook: Sources:["semantic"]
// must skip the episodic tier (no HTTP query at all), the registered
// experience tier source, and the graph post-hook — all three of Search's
// fan-out/chain sites, not just whichever one someone remembered to gate.
func TestDW_4_5_SourcesSkipsEpisodicTierAndGraphPostHook(t *testing.T) {
	r, captured, tier, hook := newFilterRetriever(t, emptyResults)
	_, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, retrieval.Filter{Sources: []string{"semantic"}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if idx := searchedIndices(captured()); len(idx) != 1 || idx[0] != "sem-idx" {
		t.Errorf("searched %v, want only [sem-idx] — the episodic tier must not be queried at all", idx)
	}
	if tier.calls != 0 {
		t.Errorf("registered tier source ran %d times under Sources:[semantic], want 0", tier.calls)
	}
	if hook.calls != 0 {
		t.Errorf("graph post-hook ran %d times under Sources:[semantic], want 0", hook.calls)
	}
}

// TestSourcesSelectsRegisteredTierSourceAndPostHook: the Sources namespace
// spans registered tier sources and post-hooks too, and selecting them skips
// BOTH built-in tiers.
func TestSourcesSelectsRegisteredTierSourceAndPostHook(t *testing.T) {
	r, captured, tier, hook := newFilterRetriever(t, emptyResults)
	tier.hits = []retrieval.Hit{{ID: "x1", Score: 1, Source: "experience", Fields: map[string]any{"text": "t"}}}

	hits, err := r.Search(context.Background(), retrieval.Query{Text: "x"},
		retrieval.Filter{Sources: []string{"experience", "graph"}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if idx := searchedIndices(captured()); len(idx) != 0 {
		t.Errorf("built-in tiers were queried (%v) though neither was named in Sources", idx)
	}
	if tier.calls != 1 || hook.calls != 1 {
		t.Errorf("tier source ran %d times, post-hook %d times; want 1 and 1", tier.calls, hook.calls)
	}
	if len(hits) != 1 || hits[0].ID != "x1" {
		t.Errorf("got %v, want the registered tier source's single hit", hits)
	}
}

// TestDW_4_7_UnknownSourceErrorNamesValidSources: an unknown source name errors
// and the message lists the valid sources (DW-4.4's contract, for the Sources
// namespace). The empty-but-non-nil case is the edge the plan calls out: it must
// NOT silently mean "all".
func TestDW_4_7_UnknownSourceErrorNamesValidSources(t *testing.T) {
	t.Run("unknown name", func(t *testing.T) {
		r, captured, tier, hook := newFilterRetriever(t, emptyResults)
		_, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, retrieval.Filter{Sources: []string{"semantic", "epsiodic"}})
		if err == nil {
			t.Fatal("Search accepted an unknown source name, want a validation error")
		}
		for _, want := range []string{"episodic", "semantic", "graph", "experience"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error does not name valid source %q: %v", want, err)
			}
		}
		if len(captured()) != 0 || tier.calls != 0 || hook.calls != 0 {
			t.Error("an invalid Sources list still ran part of the search; validation must precede all I/O")
		}
	})

	t.Run("empty but non-nil is an error, not silent-all", func(t *testing.T) {
		r, captured, tier, hook := newFilterRetriever(t, emptyResults)
		_, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, retrieval.Filter{Sources: []string{}})
		if err == nil {
			t.Fatal("Search accepted an empty Sources list, want a validation error")
		}
		if len(captured()) != 0 || tier.calls != 0 || hook.calls != 0 {
			t.Error("an empty Sources list silently searched sources anyway")
		}
	})

	t.Run("nil means every source", func(t *testing.T) {
		r, captured, tier, hook := newFilterRetriever(t, emptyResults)
		if _, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, retrieval.Filter{Sources: nil}); err != nil {
			t.Fatalf("Search: %v", err)
		}
		if idx := searchedIndices(captured()); len(idx) != 2 {
			t.Errorf("searched %v, want both built-in tiers under nil Sources", idx)
		}
		if tier.calls != 1 || hook.calls != 1 {
			t.Errorf("tier source ran %d times, post-hook %d; want 1 and 1 under nil Sources", tier.calls, hook.calls)
		}
	})

	t.Run("duplicate names are idempotent", func(t *testing.T) {
		r, captured, _, _ := newFilterRetriever(t, emptyResults)
		if _, err := r.Search(context.Background(), retrieval.Query{Text: "x"},
			retrieval.Filter{Sources: []string{"semantic", "semantic"}}); err != nil {
			t.Fatalf("Search: %v", err)
		}
		if idx := searchedIndices(captured()); len(idx) != 1 || idx[0] != "sem-idx" {
			t.Errorf("searched %v, want sem-idx exactly once", idx)
		}
	})
}

// --- DW-4.6 -----------------------------------------------------------

// TestDW_4_6_ExtractorVersionOnSemanticHits: extractor_version is filterable on
// the semantic tier, so it must also survive the display projection — a caller
// that filters on it has to be able to see it on the hits that come back.
func TestDW_4_6_ExtractorVersionOnSemanticHits(t *testing.T) {
	srv, _ := newFakeSearchServer(t, func(idx string) (int, any) {
		if !strings.Contains(idx, "sem") {
			return http.StatusOK, hitsBody()
		}
		return http.StatusOK, hitsBody(map[string]any{
			"_id": "f1", "_score": 1.0,
			"_source": map[string]any{
				"statement": "orders-svc owns billing", "subject": "orders-svc",
				"extractor_version": "v3",
				"fact_embedding":    []float64{0.1}, // must still be dropped
			},
		})
	})
	r := retrieval.NewOpenSearchRetriever(srv.Client(), srv.URL, embed.NewFakeEmbedder(4, nil),
		retrieval.WithIndices("ep-idx", "sem-idx"))
	hits, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, retrieval.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(hits))
	}
	if got := hits[0].Fields["extractor_version"]; got != "v3" {
		t.Errorf("extractor_version = %v, want v3 (it is filterable, so it must be visible)", got)
	}
	if _, leaked := hits[0].Fields["fact_embedding"]; leaked {
		t.Error("fact_embedding leaked through the display projection")
	}
}

// TestExtractorVersionPredicateFiltersSemanticTier closes the DW-4.6 loop: the
// field is not just projected, it is filterable end to end.
func TestExtractorVersionPredicateFiltersSemanticTier(t *testing.T) {
	r, captured, _, _ := newFilterRetriever(t, emptyResults)
	_, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, retrieval.Filter{
		Predicates: []retrieval.Predicate{{Field: "extractor_version", Op: "term", Value: "v3"}},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if body := bodyFor(t, captured(), "sem-idx"); !strings.Contains(body, `{"term":{"extractor_version":"v3"}}`) {
		t.Errorf("semantic query missing the extractor_version predicate: %s", body)
	}
	if body := bodyFor(t, captured(), "ep-idx"); strings.Contains(body, "extractor_version") {
		t.Errorf("episodic query carries a semantic-only field: %s", body)
	}
}

// --- DW-4.8 -----------------------------------------------------------

// TestDW_4_8_InjectionShapedValueIsParameterized: a filter value carrying
// OpenSearch query DSL must land in the query body as a STRING LEAF inside a
// term clause — data, not structure. The assertion decodes the emitted body and
// walks to the clause: if the value had been interpolated into the query text,
// the DSL would have escaped its slot (and, in the worst case, replaced the
// caller's filters with a match_all). Instead it appears verbatim, escaped, as
// the term's value.
func TestDW_4_8_InjectionShapedValueIsParameterized(t *testing.T) {
	// Each of these, if concatenated into a JSON query string, would break out of
	// the value position and inject its own clause.
	injections := []string{
		`decision"}},{"match_all":{}},{"term":{"x":"`,
		`*`,
		`{"match_all":{}}`,
		`" OR 1==1 //`,
	}
	for _, inj := range injections {
		t.Run(inj, func(t *testing.T) {
			r, captured, _, _ := newFilterRetriever(t, emptyResults)
			_, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, retrieval.Filter{
				TenantID:   "t1",
				Predicates: []retrieval.Predicate{{Field: "kind", Op: "term", Value: inj}},
			})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			body := bodyFor(t, captured(), "ep-idx")

			// The body must still be well-formed JSON with the expected shape.
			var decoded map[string]any
			if err := json.Unmarshal([]byte(body), &decoded); err != nil {
				t.Fatalf("emitted body is not valid JSON (value escaped its slot): %v\n%s", err, body)
			}
			filters := episodicFilters(t, decoded)

			// The tenant clause and the kind clause, in that order, and NOTHING
			// else: no clause was injected into the filter array.
			if len(filters) != 2 {
				t.Fatalf("got %d filter clauses, want exactly 2 (tenant + kind) — a clause was injected: %s", len(filters), body)
			}
			term, ok := filters[1].(map[string]any)["term"].(map[string]any)
			if !ok {
				t.Fatalf("second filter clause is not a term clause: %s", body)
			}
			got, isString := term["kind"].(string)
			if !isString {
				t.Fatalf("kind value is %T, not a string leaf — the value became structure: %s", term["kind"], body)
			}
			if got != inj {
				t.Errorf("kind value = %q, want the caller's value verbatim %q", got, inj)
			}
		})
	}
}

// TestNonScalarPredicateValueRejected: the one way a value could become
// structure rather than data is a map/slice in the value slot. It is rejected at
// the barricade, so no nested DSL object can ever occupy a clause value.
func TestNonScalarPredicateValueRejected(t *testing.T) {
	cases := []struct {
		name  string
		field string
		op    string
		value any
	}{
		{"map value smuggling a DSL object", "kind", "term", map[string]any{"match_all": map[string]any{}}},
		{"slice value", "kind", "term", []any{"a", "b"}},
		{"nil value", "kind", "term", nil},
		// A range bound is not itself the top-level value (that's the bounds
		// map, checked separately) — it is the leaf under "gte"/"lte". A
		// caller could smuggle a DSL object one level deeper, into a bound,
		// e.g. {"gte": {"match_all": {}}}. validatePredicateValue's range
		// branch must reject that leaf too, not just the outer map shape.
		{"non-scalar range bound", "created_at", "range", map[string]any{"gte": map[string]any{"match_all": map[string]any{}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := failingHandler(t)
			r := retrieval.NewOpenSearchRetriever(srv.Client(), srv.URL, embed.NewFakeEmbedder(4, nil))
			_, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, retrieval.Filter{
				Predicates: []retrieval.Predicate{{Field: tc.field, Op: tc.op, Value: tc.value}},
			})
			if err == nil {
				t.Fatalf("Search accepted a non-scalar filter value (%s), want a validation error", tc.name)
			}
			if !strings.Contains(err.Error(), "scalar") {
				t.Errorf("error does not explain the scalar requirement: %v", err)
			}
		})
	}
}

// TestRangeBoundUnknownKeyRejected: an unrecognized range-bound key must
// error, even when a recognized bound is also present. Silently dropping the
// unknown key (e.g. an unsupported "gt") and compiling only the recognized
// bounds would emit a filter LOOSER than the caller asked for — the wrong
// failure direction for a filter barricade.
func TestRangeBoundUnknownKeyRejected(t *testing.T) {
	srv := failingHandler(t)
	r := retrieval.NewOpenSearchRetriever(srv.Client(), srv.URL, embed.NewFakeEmbedder(4, nil))
	_, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, retrieval.Filter{
		Predicates: []retrieval.Predicate{{
			Field: "created_at",
			Op:    "range",
			Value: map[string]any{"gte": "2026-01-01", "gt": "2027-01-01"},
		}},
	})
	if err == nil {
		t.Fatal("Search accepted a range bound with an unknown key (\"gt\") alongside a known one, want a validation error")
	}
	if !strings.Contains(err.Error(), `"gt"`) || !strings.Contains(err.Error(), "gte") || !strings.Contains(err.Error(), "lte") {
		t.Errorf("error does not name the unknown key and the valid bounds: %v", err)
	}
}

// episodicFilters walks a decoded hybrid query body to the BM25 sub-query's
// filter array.
func episodicFilters(t *testing.T, decoded map[string]any) []any {
	t.Helper()
	query, ok := decoded["query"].(map[string]any)
	if !ok {
		t.Fatalf("body has no query object: %v", decoded)
	}
	hybrid, ok := query["hybrid"].(map[string]any)
	if !ok {
		t.Fatalf("body has no hybrid clause: %v", query)
	}
	queries, ok := hybrid["queries"].([]any)
	if !ok || len(queries) == 0 {
		t.Fatalf("hybrid clause has no sub-queries: %v", hybrid)
	}
	bm25, ok := queries[0].(map[string]any)["bool"].(map[string]any)
	if !ok {
		t.Fatalf("first sub-query is not a bool query: %v", queries[0])
	}
	filters, ok := bm25["filter"].([]any)
	if !ok {
		t.Fatalf("bool query has no filter array: %v", bm25)
	}
	return filters
}

package retrieval_test

// Query.Expand: graph expansion is opt-in. These tests pin the halves of the
// contract — false skips the traversal itself (the hook is never CALLED, not
// merely filtered), true is today's behavior, post-hooks under any other name
// are not gated — plus the contradictory-request barricade.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/retrieval"
)

// appendingHook is a post-hook that counts its calls and appends one hit of
// its source — a stand-in for the graph expander (source "graph") or for a
// future non-expansion hook (any other source).
type appendingHook struct {
	calls  int
	source string
}

func (h *appendingHook) Apply(_ context.Context, _ auth.Identity, hits []retrieval.Hit) ([]retrieval.Hit, error) {
	h.calls++
	return append(hits, retrieval.Hit{ID: h.source + "-added", Score: 0.5, Source: h.source,
		Fields: map[string]any{"statement": "added by " + h.source}}), nil
}

// newExpandRetriever wires a retriever over the fake cluster whose single
// matched hit comes from a registered tier, with an appending hook registered
// as the graph expander and a second appending hook under another name.
func newExpandRetriever(t *testing.T) (r *retrieval.MultiRetriever, captured func() []reqCapture, graph, other *appendingHook) {
	t.Helper()
	srv, captured := newFakeSearchServer(t, emptyResults)
	r = retrieval.NewOpenSearchRetriever(srv.Client(), srv.URL, embed.NewFakeEmbedder(4, nil),
		retrieval.WithIndices("ep-idx", "sem-idx"))
	r.RegisterTier("experience", &countingTier{hits: []retrieval.Hit{
		{ID: "m1", Score: 1, Source: "experience", Fields: map[string]any{"text": "t"}},
	}})
	graph = &appendingHook{source: retrieval.ExpandedSource}
	other = &appendingHook{source: "other"}
	r.RegisterPostHook(retrieval.ExpandedSource, graph)
	r.RegisterPostHook("other", other)
	return r, captured, graph, other
}

// TestExpandGatesOnlyTheGraphPostHook: Expand=false never calls the graph hook
// and returns no expansions; Expand=true is the pre-Expand behavior. Either way
// the matched hits, the tier fan-out, and the non-graph hook are identical.
func TestExpandGatesOnlyTheGraphPostHook(t *testing.T) {
	for _, tc := range []struct {
		expand       bool
		wantGraph    int
		wantExpanded int
	}{
		{expand: false, wantGraph: 0, wantExpanded: 0},
		{expand: true, wantGraph: 1, wantExpanded: 1},
	} {
		r, captured, graph, other := newExpandRetriever(t)
		hits, err := r.Search(context.Background(), retrieval.Query{Text: "x", K: 5, Expand: tc.expand}, retrieval.Filter{})
		if err != nil {
			t.Fatalf("expand=%v: Search: %v", tc.expand, err)
		}
		if graph.calls != tc.wantGraph {
			t.Errorf("expand=%v: graph post-hook ran %d times, want %d — Expand=false must not start the traversal", tc.expand, graph.calls, tc.wantGraph)
		}
		if other.calls != 1 {
			t.Errorf("expand=%v: non-graph post-hook ran %d times, want 1 — Expand gates expansion only", tc.expand, other.calls)
		}
		if n := len(captured()); n != 2 {
			t.Errorf("expand=%v: %d built-in tier queries, want 2 — matched retrieval is unaffected", tc.expand, n)
		}
		matched, expanded := retrieval.SplitExpanded(hits, 5)
		if len(expanded) != tc.wantExpanded {
			t.Errorf("expand=%v: expanded = %+v, want %d hit(s)", tc.expand, expanded, tc.wantExpanded)
		}
		if tc.wantExpanded == 0 && expanded != nil {
			t.Errorf("expand=false: expanded must be nil (an absent block), got %#v", expanded)
		}
		// m1 (the tier's match) plus the non-graph hook's addition, which is a
		// match per SplitExpanded's contract, not an expansion.
		if len(matched) != 2 || matched[0].ID != "m1" || matched[1].Source != "other" {
			t.Errorf("expand=%v: matched = %+v, want [m1, other-added]", tc.expand, matched)
		}
	}
}

// TestNamingGraphSourceWithoutExpandErrors: Sources naming "graph" while
// Expand is false asks for expansions and declines them in one request. It is
// rejected as an invalid filter naming the fix, before any I/O — even for an
// empty query, the same barricade ordering as every other filter check.
func TestNamingGraphSourceWithoutExpandErrors(t *testing.T) {
	r, captured, tier, hook := newFilterRetriever(t, emptyResults)
	for _, q := range []retrieval.Query{{Text: "x"}, {}} {
		for _, sources := range [][]string{{"graph"}, {"semantic", "graph"}} {
			_, err := r.Search(context.Background(), q, retrieval.Filter{Sources: sources})
			if !errors.Is(err, retrieval.ErrInvalidFilter) {
				t.Fatalf("query=%q Sources=%v Expand=false: err = %v, want ErrInvalidFilter", q.Text, sources, err)
			}
			if !strings.Contains(err.Error(), "expand") {
				t.Errorf("error does not name the fix (expand): %v", err)
			}
		}
	}
	if len(captured()) != 0 || tier.calls != 0 || hook.calls != 0 {
		t.Error("a contradictory Sources/Expand request still ran part of the search")
	}
	// The same Sources with Expand=true is the honored, pre-Expand request.
	if _, err := r.Search(context.Background(), retrieval.Query{Text: "x", Expand: true},
		retrieval.Filter{Sources: []string{"semantic", "graph"}}); err != nil {
		t.Errorf("Sources=[semantic graph] Expand=true: %v, want success", err)
	}
	if hook.calls != 1 {
		t.Errorf("graph hook ran %d times with Expand=true and graph named, want 1", hook.calls)
	}
}

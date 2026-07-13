package server_test

// Phase-6 tests at the gRPC boundary: server.Search is where the retriever's
// single fused list becomes two labeled wire blocks. The fake retriever below
// reproduces exactly what MultiRetriever hands back — the truncated top-k with
// the graph post-hook's expansions APPENDED after it — so these tests exercise
// the split against the real shape, not a convenient one.

import (
	"context"
	"testing"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/retrieval"
	"github.com/ryanthedev/engram/internal/server"
)

// fusedHits builds the post-hook-inflated list MultiRetriever.Search returns:
// nMatched semantic hits (already truncated to k) followed by nExpanded graph
// hits the expander appended afterwards.
func fusedHits(nMatched, nExpanded int) []retrieval.Hit {
	hits := make([]retrieval.Hit, 0, nMatched+nExpanded)
	for i := 0; i < nMatched; i++ {
		hits = append(hits, retrieval.Hit{
			ID: "sem", Score: 1, Source: "semantic",
			Fields: map[string]any{"statement": "matched"},
		})
	}
	for i := 0; i < nExpanded; i++ {
		hits = append(hits, retrieval.Hit{
			ID: "edge", Score: 0.5, Source: retrieval.ExpandedSource,
			Fields: map[string]any{"statement": "expansion", "hop": 1},
		})
	}
	return hits
}

// searchReturning runs one Search against a retriever that returns hits.
func searchReturning(t *testing.T, hits []retrieval.Hit, k int32) *engrampb.SearchResponse {
	t.Helper()
	s := server.New(&fakeStore{}, &fakeRetriever{
		searchFn: func(context.Context, retrieval.Query, retrieval.Filter) ([]retrieval.Hit, error) {
			return hits, nil
		},
	})
	resp, err := s.Search(context.Background(), &engrampb.SearchRequest{Query: "q", K: k})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	return resp
}

// TestDW_6_1_SearchHitsExcludeGraphAndRespectK: the honest-k contract on the
// wire. The retriever returns 20 matched + 20 expansions (the k=20-returns-40
// bug); the response must carry exactly the 20 matched hits, none of them graph.
func TestDW_6_1_SearchHitsExcludeGraphAndRespectK(t *testing.T) {
	resp := searchReturning(t, fusedHits(20, 20), 20)

	if got := len(resp.GetHits()); got != 20 {
		t.Fatalf("len(hits) = %d, want 20 (<= k, nothing evicted)", got)
	}
	for _, h := range resp.GetHits() {
		if h.GetSource() == retrieval.ExpandedSource {
			t.Fatalf("hits carries a %q hit: an expansion was smuggled into the matched array", h.GetSource())
		}
	}
}

// TestDW_6_2_SearchExpandedBlockCarriesGraphHits: expansions ride in the
// `expanded` block, fully populated (id/score/source/fields all cross the wire),
// never mixed into hits.
func TestDW_6_2_SearchExpandedBlockCarriesGraphHits(t *testing.T) {
	resp := searchReturning(t, fusedHits(2, 3), 10)

	if got := len(resp.GetHits()); got != 2 {
		t.Fatalf("len(hits) = %d, want 2", got)
	}
	expanded := resp.GetExpanded()
	if len(expanded) != 3 {
		t.Fatalf("len(expanded) = %d, want 3", len(expanded))
	}
	for _, h := range expanded {
		if h.GetSource() != retrieval.ExpandedSource {
			t.Errorf("expanded hit source = %q, want %q", h.GetSource(), retrieval.ExpandedSource)
		}
		if h.GetId() == "" || h.GetScore() == 0 || h.GetFieldsJson() == "" {
			t.Errorf("expanded hit came across the wire hollow: %+v", h)
		}
	}
}

// TestDW_6_3_NoGraphHitsEmitsNoExpandedBlock: zero expansions means NO block —
// not an empty one. A present-but-empty list would still cost the caller tokens
// and would read as "expansion ran and found nothing" rather than "no block".
func TestDW_6_3_NoGraphHitsEmitsNoExpandedBlock(t *testing.T) {
	resp := searchReturning(t, fusedHits(3, 0), 10)

	if got := len(resp.GetHits()); got != 3 {
		t.Fatalf("len(hits) = %d, want 3", got)
	}
	if resp.GetExpanded() != nil {
		t.Fatalf("expanded = %v, want nil (absent block, not an empty one)", resp.GetExpanded())
	}
}

// TestSearchUnsetKStillBoundsMatchedHits: k=0 means the retriever's DefaultK,
// not zero. The split must apply the SAME clamp the retriever did, or an unset
// k either truncates everything away or bounds nothing at all.
func TestSearchUnsetKStillBoundsMatchedHits(t *testing.T) {
	resp := searchReturning(t, fusedHits(retrieval.DefaultK+5, 2), 0)

	if got := len(resp.GetHits()); got != retrieval.DefaultK {
		t.Fatalf("len(hits) = %d, want DefaultK (%d) for an unset k", got, retrieval.DefaultK)
	}
	if got := len(resp.GetExpanded()); got != 2 {
		t.Fatalf("len(expanded) = %d, want 2: expansions are not counted against k", got)
	}
}

// TestSearchNonExpanderPostHookHitsStayInHits: a registered post-hook that is
// not the expander emits hits under its own source. They are matches, not
// expansions — they must land in hits (and still respect k), never be misfiled
// into the expanded block.
func TestSearchNonExpanderPostHookHitsStayInHits(t *testing.T) {
	hits := []retrieval.Hit{
		{ID: "s1", Score: 1, Source: "semantic", Fields: map[string]any{"statement": "m"}},
		{ID: "x1", Score: 0.9, Source: "experience", Fields: map[string]any{"text": "x"}},
		{ID: "g1", Score: 0.5, Source: retrieval.ExpandedSource, Fields: map[string]any{"statement": "e"}},
	}
	resp := searchReturning(t, hits, 10)

	if len(resp.GetHits()) != 2 {
		t.Fatalf("len(hits) = %d, want 2 (semantic + experience)", len(resp.GetHits()))
	}
	if got := len(resp.GetExpanded()); got != 1 {
		t.Fatalf("len(expanded) = %d, want 1: only graph hits are expansions", got)
	}
	if src := resp.GetExpanded()[0].GetSource(); src != retrieval.ExpandedSource {
		t.Fatalf("expanded[0].source = %q, want %q — an experience hit was misfiled", src, retrieval.ExpandedSource)
	}
}

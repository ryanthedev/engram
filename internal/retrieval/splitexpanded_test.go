package retrieval_test

// Phase-6 tests for the honest-k contract: SplitExpanded is the one place that
// decides what "k" means. Post-hooks append to Search's already-truncated
// top-k (deliberately — an expansion must never evict a direct match), so the
// fused list can exceed k; this is the split that keeps the caller's `hits`
// array honest without touching the Retriever interface.

import (
	"testing"

	"github.com/ryanthedev/engram/internal/retrieval"
)

// srcHit builds a Hit with just the fields the split reads.
func srcHit(id, source string) retrieval.Hit {
	return retrieval.Hit{ID: id, Source: source, Score: 1}
}

// matchedAndExpanded builds n matched hits (episodic/semantic) followed by m
// graph expansions — the exact shape MultiRetriever.Search returns once the
// graph post-hook has appended to the truncated top-k.
func matchedAndExpanded(n, m int) []retrieval.Hit {
	hits := make([]retrieval.Hit, 0, n+m)
	for i := 0; i < n; i++ {
		hits = append(hits, srcHit(string(rune('a'+i)), "semantic"))
	}
	for i := 0; i < m; i++ {
		hits = append(hits, srcHit(string(rune('A'+i)), retrieval.ExpandedSource))
	}
	return hits
}

// TestDW_6_1_SplitExpandedCapsMatchedAtK: k means k for MATCHED hits. The
// retriever hands back k matched hits plus expansions appended after the
// truncation; the caller must get back exactly k, none of them a graph hit.
func TestDW_6_1_SplitExpandedCapsMatchedAtK(t *testing.T) {
	// 20 matched + 20 expansions = the 40-hits-for-k=20 bug this phase closes.
	matched, expanded := retrieval.SplitExpanded(matchedAndExpanded(20, 20), 20)

	if len(matched) > 20 {
		t.Fatalf("len(matched) = %d, want <= k (20): k is not honest", len(matched))
	}
	if len(matched) != 20 {
		t.Fatalf("len(matched) = %d, want 20: a matched hit was evicted by an expansion", len(matched))
	}
	for _, h := range matched {
		if h.Source == retrieval.ExpandedSource {
			t.Fatalf("matched hit %q has Source == %q: an expansion was smuggled into hits", h.ID, h.Source)
		}
	}
	if len(expanded) != 20 {
		t.Fatalf("len(expanded) = %d, want 20", len(expanded))
	}
}

// TestDW_6_1_SplitExpandedNormalizesKLikeTheRetriever: the cap is only honest
// if it uses the SAME clamp Search applied to Query.K. An unset k (0) means
// DefaultK, not zero — re-deriving that rule at the call site is exactly how
// "len(hits) <= k" turns into a lie.
func TestDW_6_1_SplitExpandedNormalizesKLikeTheRetriever(t *testing.T) {
	tests := []struct {
		name        string
		k           int
		matchedIn   int
		wantMatched int
	}{
		{"unset k means DefaultK, not 0", 0, retrieval.DefaultK + 5, retrieval.DefaultK},
		{"negative k means DefaultK, not a panic", -7, retrieval.DefaultK + 5, retrieval.DefaultK},
		{"k above MaxK is capped at MaxK", retrieval.MaxK + 50, retrieval.MaxK + 10, retrieval.MaxK},
		{"in-range k passes through", 3, 10, 3},
		{"fewer matched than k is not padded", 5, 2, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := make([]retrieval.Hit, 0, tc.matchedIn)
			for i := 0; i < tc.matchedIn; i++ {
				in = append(in, retrieval.Hit{ID: "m", Source: "semantic", Score: 1})
			}
			matched, _ := retrieval.SplitExpanded(in, tc.k)
			if len(matched) != tc.wantMatched {
				t.Fatalf("len(matched) = %d, want %d", len(matched), tc.wantMatched)
			}
		})
	}
}

// TestDW_6_2_SplitExpandedPartitionsByGraphSource: expansions land in their own
// block, never mixed into hits, and each block keeps its relative order (the
// expander emits nearest-hop-first, so order carries meaning).
func TestDW_6_2_SplitExpandedPartitionsByGraphSource(t *testing.T) {
	in := []retrieval.Hit{
		srcHit("s1", "semantic"),
		srcHit("g1", retrieval.ExpandedSource),
		srcHit("e1", "episodic"),
		srcHit("g2", retrieval.ExpandedSource),
		srcHit("s2", "semantic"),
	}
	matched, expanded := retrieval.SplitExpanded(in, 10)

	wantMatched := []string{"s1", "e1", "s2"}
	wantExpanded := []string{"g1", "g2"}
	if got := hitIDs(matched); !sameIDs(got, wantMatched) {
		t.Errorf("matched = %v, want %v (order-preserving, graph-free)", got, wantMatched)
	}
	if got := hitIDs(expanded); !sameIDs(got, wantExpanded) {
		t.Errorf("expanded = %v, want %v (order-preserving)", got, wantExpanded)
	}
}

// TestDW_6_3_SplitExpandedReturnsNilExpanded: zero expansions yields a NIL
// expanded block, not an empty one — so every layer above can gate the block on
// len/nil alone and an absent block stays absent on the wire.
func TestDW_6_3_SplitExpandedReturnsNilExpanded(t *testing.T) {
	matched, expanded := retrieval.SplitExpanded(matchedAndExpanded(3, 0), 10)
	if len(matched) != 3 {
		t.Fatalf("len(matched) = %d, want 3", len(matched))
	}
	if expanded != nil {
		t.Fatalf("expanded = %v, want nil (no expansions must mean NO block, not an empty one)", expanded)
	}
}

// TestSplitExpandedDoesNotMisfileNonGraphPostHookHits: a registered post-hook
// that is NOT the expander emits hits whose Source is its own, not "graph".
// Those are matches, not expansions: they belong in hits — and, because they
// too are appended AFTER the retriever's truncation, they must be capped at k
// on the way in rather than allowed to inflate it.
func TestSplitExpandedDoesNotMisfileNonGraphPostHookHits(t *testing.T) {
	in := []retrieval.Hit{
		srcHit("s1", "semantic"),
		srcHit("s2", "semantic"),
		srcHit("x1", "experience"), // a non-expander post-hook's addition
		srcHit("g1", retrieval.ExpandedSource),
	}
	matched, expanded := retrieval.SplitExpanded(in, 3)

	if got := hitIDs(expanded); !sameIDs(got, []string{"g1"}) {
		t.Fatalf("expanded = %v, want [g1]: only graph hits are expansions", got)
	}
	if got := hitIDs(matched); !sameIDs(got, []string{"s1", "s2", "x1"}) {
		t.Fatalf("matched = %v, want [s1 s2 x1]: a non-graph post-hook hit is a match, not an expansion", got)
	}

	// And the cap still binds when such a hook overflows k.
	matched, _ = retrieval.SplitExpanded(in, 2)
	if len(matched) != 2 {
		t.Fatalf("len(matched) = %d, want 2: a non-expander post-hook must not inflate k either", len(matched))
	}
}

// TestSplitExpandedEmptyInputs: nil and empty inputs are ordinary, not edge
// cases that panic — an empty query short-circuits Search to nil hits.
func TestSplitExpandedEmptyInputs(t *testing.T) {
	for _, in := range [][]retrieval.Hit{nil, {}} {
		matched, expanded := retrieval.SplitExpanded(in, 10)
		if matched != nil || expanded != nil {
			t.Fatalf("SplitExpanded(%v) = (%v, %v), want (nil, nil)", in, matched, expanded)
		}
	}
}

// TestSplitExpandedAllExpansions: a result of nothing but expansions (every
// match filtered away by the ACL re-check, say) yields an empty matched block —
// never a graph hit promoted into it to fill the gap.
func TestSplitExpandedAllExpansions(t *testing.T) {
	matched, expanded := retrieval.SplitExpanded(matchedAndExpanded(0, 4), 10)
	if len(matched) != 0 {
		t.Fatalf("matched = %v, want empty: a graph hit must never be promoted into hits", matched)
	}
	if len(expanded) != 4 {
		t.Fatalf("len(expanded) = %d, want 4", len(expanded))
	}
}

func hitIDs(hits []retrieval.Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.ID
	}
	return out
}

func sameIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

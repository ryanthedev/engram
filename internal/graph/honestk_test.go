package graph

// Phase-6: the honest-k contract proved against a REAL expansion — a real
// graph Store, a real Expander at depth 2, and a real retrieval.MultiRetriever
// with the expander registered as its "graph" post-hook. Nothing here fakes the
// thing under test: the expansions these tests split out are edges the traversal
// actually walked.

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/acl"
	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/retrieval"
)

// Seeds here use expand_test.go's tripleHit (subject+predicate+object), not the
// bare semanticHit: a real semantic hit carries its full triple, and without the
// predicate Phase 2's echo guard cannot fingerprint the seed's own edge — so the
// seed's edge would come back as a bogus "expansion" and these tests would be
// asserting against a shape production never produces.

// chainStore builds A -works_at-> B -located_in-> C, all owned by the reachable
// agent a1. A seed hit on the A/B fact anchors at A and B; its own A-B edge is
// suppressed by the echo guard (Phase 2), so a depth-2 traversal surfaces the
// B-C edge — a genuine expansion, not the seed handed back to itself.
func chainStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	store, _ := newTestStore(t)

	ids := map[string]string{}
	for _, name := range []string{"A", "B", "C"} {
		id, _, err := store.UpsertMention(ctx, Mention{
			TenantID: "t1", Scope: "private", OwnerAgentID: "a1",
			Name: name, Context: "chain", SourceID: "ev-1",
		})
		if err != nil {
			t.Fatalf("upsert %s: %v", name, err)
		}
		ids[name] = id
	}
	edges := []struct{ from, pred, to string }{
		{"A", "works_at", "B"},
		{"B", "located_in", "C"},
	}
	for _, e := range edges {
		if _, err := store.UpsertEdge(ctx, EdgeSpec{
			TenantID: "t1", Scope: "private", OwnerAgentID: "a1",
			FromEntityID: ids[e.from], ToEntityID: ids[e.to],
			Predicate: e.pred, Statement: e.from + " " + e.pred + " " + e.to,
			SourceID: "ev-1", ValidAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("edge %s-%s: %v", e.from, e.to, err)
		}
	}
	return store
}

// expandingRetriever wires the real expander (depth 2) into a real
// MultiRetriever as its "graph" post-hook. The built-in episodic/semantic tiers
// point at an unreachable loopback port so they contribute nothing; seeds is the
// registered tier that supplies the matched hits.
func expandingRetriever(t *testing.T, store *Store, seeds []retrieval.Hit) *retrieval.MultiRetriever {
	t.Helper()
	expander, err := NewExpander(store, 2, slog.Default()) // depth 2: expansion stays ON
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	aclFilter := acl.NewFilter(fakeEdgeSource{reach: map[string]acl.Reach{"u1": {Agents: []string{"a1"}}}}, slog.Default())
	r := retrieval.NewOpenSearchRetriever(
		&http.Client{Timeout: 300 * time.Millisecond}, "http://127.0.0.1:1",
		embed.NewFakeEmbedder(8, nil), retrieval.WithACL(aclFilter),
	)
	r.RegisterPostHook("graph", expander)
	r.RegisterTier("seed", &seedTier{hits: seeds})
	return r
}

// TestDW_6_1_HonestKAtDepth2 is the phase's headline case, end to end through
// the real pipeline: k=2, three matched hits available, and a live depth-2
// expansion appending graph hits AFTER the retriever's truncation. The caller
// must get back exactly 2 matched hits, none of them a graph hit — while the
// expansions survive in their own block rather than being evicted (DW-6.1,
// DW-6.2).
func TestDW_6_1_HonestKAtDepth2(t *testing.T) {
	ctx := context.Background()
	seeds := []retrieval.Hit{
		tripleHit("fact-ab", "A", "works_at", "B"), // anchors the traversal
		tripleHit("fact-2", "X", "likes", "Y"),
		tripleHit("fact-3", "X", "likes", "Z"),
	}
	r := expandingRetriever(t, chainStore(t), seeds)

	const k = 2
	hits, err := r.Search(ctx, retrieval.Query{Text: "q", K: k},
		retrieval.Filter{Identity: auth.Identity{TenantID: "t1", UserID: "u1", AgentID: "a1"}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	matched, expanded := retrieval.SplitExpanded(hits, k)

	// The precondition that makes this test meaningful: expansion really ran.
	// Without it, "no graph hits in matched" would pass vacuously.
	if len(expanded) == 0 {
		t.Fatalf("no expansions at depth 2 — the honest-k split is untested; fused hits = %+v", hits)
	}
	if len(matched) != k {
		t.Fatalf("len(matched) = %d, want %d: k is not honest", len(matched), k)
	}
	for _, h := range matched {
		if h.Source == retrieval.ExpandedSource {
			t.Fatalf("matched hit %q is a graph expansion: expansions leaked into hits", h.ID)
		}
	}
	for _, h := range expanded {
		if h.Source != retrieval.ExpandedSource {
			t.Fatalf("expanded hit %q has source %q, want %q", h.ID, h.Source, retrieval.ExpandedSource)
		}
	}
	// And the seed's own edge was not served back as a discovery (Phase 2's
	// echo guard) — what expanded carries is the B-C edge the traversal found.
	for _, h := range expanded {
		if stmt, _ := h.Fields["statement"].(string); stmt == "A works_at B" {
			t.Errorf("seed's own edge came back as an expansion: %+v", h)
		}
	}
}

// TestSourcesExcludingGraphYieldsNoExpansion: a caller whose sources exclude
// graph gets NO expanded block at all — the post-hook never runs, so there is
// nothing to split out (plan Edge case).
func TestSourcesExcludingGraphYieldsNoExpansion(t *testing.T) {
	ctx := context.Background()
	seeds := []retrieval.Hit{tripleHit("fact-ab", "A", "works_at", "B")}
	r := expandingRetriever(t, chainStore(t), seeds)

	hits, err := r.Search(ctx, retrieval.Query{Text: "q", K: 10}, retrieval.Filter{
		Identity: auth.Identity{TenantID: "t1", UserID: "u1", AgentID: "a1"},
		Sources:  []string{"seed"}, // graph deliberately not named
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	matched, expanded := retrieval.SplitExpanded(hits, 10)
	if len(expanded) != 0 {
		t.Fatalf("expanded = %+v, want none: sources excluded graph, so no block should exist", expanded)
	}
	if len(matched) != 1 {
		t.Fatalf("len(matched) = %d, want 1: excluding graph must not cost the caller its matches", len(matched))
	}
}

// TestDW_6_5_PostHookContractCommentIsCurrent: the comment that documented the
// OLD contract ("the retriever itself does not re-truncate post-hook additions
// (Phase 4 design)") justified expansions riding back inside the matched array.
// This phase reverses that decision, so the comment must state the new contract
// — a stale one here is what would send the next reader back to the old design.
func TestDW_6_5_PostHookContractCommentIsCurrent(t *testing.T) {
	src, err := os.ReadFile("expand.go")
	if err != nil {
		t.Fatalf("reading expand.go: %v", err)
	}
	text := string(src)

	if strings.Contains(text, "(Phase 4 design)") {
		t.Error("expand.go still carries the stale \"(Phase 4 design)\" note: it documents the reversed decision that expansions ride back inside the matched hits")
	}
	for _, want := range []string{"`expanded` block", "SplitExpanded"} {
		if !strings.Contains(text, want) {
			t.Errorf("expand.go does not mention %q: the new post-hook contract is undocumented where the old one lived", want)
		}
	}
}

package graph

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/acl"
	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/retrieval"
)

func semanticHit(id, tenant, scope, owner, team, subject, object string) retrieval.Hit {
	return retrieval.Hit{
		ID: id, Source: "semantic", Score: 10,
		Fields: map[string]any{
			"tenant_id": tenant, "scope": scope, "owner_agent_id": owner, "team_id": team,
			"subject": subject, "object": object,
		},
	}
}

// --- depth boundary (Test Plan: "depth 2 honored, 3 rejected") ---

func TestNewExpander_RejectsDepthAbove2(t *testing.T) {
	store, _ := newTestStore(t)
	if _, err := NewExpander(store, 3, nil); err != ErrDepthExceeded {
		t.Fatalf("NewExpander(depth=3) err = %v, want ErrDepthExceeded", err)
	}
	if _, err := NewExpander(store, -1, nil); err != ErrDepthExceeded {
		t.Fatalf("NewExpander(depth=-1) err = %v, want ErrDepthExceeded", err)
	}
}

func TestNewExpander_HonorsDepth2(t *testing.T) {
	store, _ := newTestStore(t)
	if _, err := NewExpander(store, 2, nil); err != nil {
		t.Fatalf("NewExpander(depth=2) should succeed: %v", err)
	}
	if _, err := NewExpander(store, 0, nil); err != nil {
		t.Fatalf("NewExpander(depth=0) should succeed (expansion disabled): %v", err)
	}
}

func TestExpand_RejectsDepthAbove2AtCallTime(t *testing.T) {
	store, _ := newTestStore(t)
	e, err := NewExpander(store, 2, nil)
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	if _, err := e.Expand(context.Background(), []retrieval.Hit{semanticHit("h1", "t1", "private", "a1", "", "A", "B")}, 3); err != ErrDepthExceeded {
		t.Fatalf("Expand(depth=3) err = %v, want ErrDepthExceeded", err)
	}
}

// TestExpand_ZeroHitsIsNoOp (edge case: expansion on zero hits is a no-op).
func TestExpand_ZeroHitsIsNoOp(t *testing.T) {
	store, _ := newTestStore(t)
	e, err := NewExpander(store, 2, nil)
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	out, err := e.Expand(context.Background(), nil, 2)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("Expand(zero hits) = %v, want empty", out)
	}
}

// TestExpand_DepthZeroIsNoOp: depth 0 returns hits unchanged.
func TestExpand_DepthZeroIsNoOp(t *testing.T) {
	store, _ := newTestStore(t)
	e, err := NewExpander(store, 2, nil)
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	seed := []retrieval.Hit{semanticHit("h1", "t1", "private", "a1", "", "A", "B")}
	out, err := e.Expand(context.Background(), seed, 0)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out) != 1 || out[0].ID != "h1" {
		t.Fatalf("Expand(depth=0) = %v, want the seed unchanged", out)
	}
}

// seedGraph ingests the fixture "A works_at B, B located_in C" chain
// directly through Store (bypassing the worker Stage — these tests exercise
// Expand, not ingestion), all owned by a1/private, tenant t1.
func seedConnectTheDotsGraph(t *testing.T, store *Store) (aID, bID, cID string) {
	t.Helper()
	ctx := context.Background()
	mustUpsert := func(name, context, src string) string {
		id, _, err := store.UpsertMention(ctx, Mention{TenantID: "t1", Scope: "private", OwnerAgentID: "a1", Name: name, Context: context, SourceID: src})
		if err != nil {
			t.Fatalf("UpsertMention(%s): %v", name, err)
		}
		return id
	}
	aID = mustUpsert("A", "A works_at B", "ev-1")
	bID = mustUpsert("B", "A works_at B", "ev-1")
	if _, err := store.UpsertEdge(ctx, EdgeSpec{TenantID: "t1", TeamID: "", Scope: "private", OwnerAgentID: "a1", FromEntityID: aID, ToEntityID: bID, Predicate: "works_at", Statement: "A works_at B", SourceID: "ev-1", ValidAt: time.Now().UTC()}); err != nil {
		t.Fatalf("UpsertEdge A-B: %v", err)
	}
	bID2 := mustUpsert("B", "B located_in C", "ev-2")
	cID = mustUpsert("C", "B located_in C", "ev-2")
	if bID2 != bID {
		t.Fatalf("B mention should resolve to the same entity across both facts, got %s and %s", bID, bID2)
	}
	if _, err := store.UpsertEdge(ctx, EdgeSpec{TenantID: "t1", TeamID: "", Scope: "private", OwnerAgentID: "a1", FromEntityID: bID, ToEntityID: cID, Predicate: "located_in", Statement: "B located_in C", SourceID: "ev-2", ValidAt: time.Now().UTC()}); err != nil {
		t.Fatalf("UpsertEdge B-C: %v", err)
	}
	return aID, bID, cID
}

// TestDW_6_2_TwoHopConnectTheDots: a seed hit about A (with object B) expands
// at depth 2 to surface the B->C edge — the documented A->B->C answer path —
// without the seed query ever mentioning C.
func TestDW_6_2_TwoHopConnectTheDots(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	seedConnectTheDotsGraph(t, store)

	e, err := NewExpander(store, 2, slog.Default())
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	seed := []retrieval.Hit{semanticHit("fact-ab", "t1", "private", "a1", "", "A", "B")}
	out, err := e.Expand(ctx, seed, 2)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out) <= len(seed) {
		t.Fatalf("Expand added nothing; got %v", out)
	}
	foundC := false
	for _, h := range out {
		if h.Source != "graph" {
			continue
		}
		if obj, _ := h.Fields["object"].(string); obj == "C" {
			foundC = true
		}
		if subj, _ := h.Fields["subject"].(string); subj == "C" {
			foundC = true
		}
	}
	if !foundC {
		t.Fatalf("expansion did not reach C via the B->C hop; hits: %+v", out)
	}
}

// TestDW_6_4_ExpansionACLBlocked: a hit reachable ONLY through an edge owned
// by an agent the caller cannot reach must be ABSENT from the final result —
// proven through the real retrieval.MultiRetriever + acl.Filter, not just
// Expand in isolation (the guarantee is the retriever's re-authorization of
// post-hook additions, which this phase adds zero code to).
func TestDW_6_4_ExpansionACLBlocked(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)

	// A (owned by a1, reachable) -> B (owned by a1) -> C (owned by a9, an
	// agent the caller CANNOT reach) — a dangling-to-the-caller edge.
	aID, _, err := store.UpsertMention(ctx, Mention{TenantID: "t1", Scope: "private", OwnerAgentID: "a1", Name: "A", Context: "A works_at B", SourceID: "ev-1"})
	if err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	bID, _, err := store.UpsertMention(ctx, Mention{TenantID: "t1", Scope: "private", OwnerAgentID: "a1", Name: "B", Context: "A works_at B", SourceID: "ev-1"})
	if err != nil {
		t.Fatalf("upsert B: %v", err)
	}
	if _, err := store.UpsertEdge(ctx, EdgeSpec{TenantID: "t1", TeamID: "", Scope: "private", OwnerAgentID: "a1", FromEntityID: aID, ToEntityID: bID, Predicate: "works_at", Statement: "A works_at B", SourceID: "ev-1", ValidAt: time.Now().UTC()}); err != nil {
		t.Fatalf("edge A-B: %v", err)
	}
	cID, _, err := store.UpsertMention(ctx, Mention{TenantID: "t1", Scope: "private", OwnerAgentID: "a9", Name: "C", Context: "B secretly_owns C", SourceID: "ev-2"})
	if err != nil {
		t.Fatalf("upsert C: %v", err)
	}
	// The connecting edge is itself owned by a9 (unreachable) — the
	// realistic shape of "reachable only through an unauthorized edge".
	if _, err := store.UpsertEdge(ctx, EdgeSpec{TenantID: "t1", TeamID: "", Scope: "private", OwnerAgentID: "a9", FromEntityID: bID, ToEntityID: cID, Predicate: "secretly_owns", Statement: "B secretly_owns C", SourceID: "ev-2", ValidAt: time.Now().UTC()}); err != nil {
		t.Fatalf("edge B-C: %v", err)
	}

	expander, err := NewExpander(store, 2, slog.Default())
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	edges := fakeEdgeSource{reach: map[string]acl.Reach{"u1": {Agents: []string{"a1"}}}} // u1 reaches only a1, never a9
	aclFilter := acl.NewFilter(edges, slog.Default())
	// A real MultiRetriever, built through the public constructor (no
	// internal/retrieval edits, no access to its unexported fields): the
	// built-in episodic/semantic tiers point at an unreachable loopback port
	// so they fail fast and contribute nothing, while the registered
	// seedTier supplies the seed hit — proving the FULL retriever pipeline
	// (built-in tiers + registered tier + registered post-hook + ACL
	// re-authorization), not just Expand in isolation.
	httpClient := &http.Client{Timeout: 300 * time.Millisecond}
	retriever := retrieval.NewOpenSearchRetriever(httpClient, "http://127.0.0.1:1", embed.NewFakeEmbedder(8, nil), retrieval.WithACL(aclFilter))
	retriever.RegisterPostHook(expander)
	retriever.RegisterTier(&seedTier{hits: []retrieval.Hit{semanticHit("fact-ab", "t1", "private", "a1", "", "A", "B")}})

	caller := auth.Identity{TenantID: "t1", UserID: "u1", AgentID: "a1"}
	hits, err := retriever.Search(ctx, retrieval.Query{Text: "q", K: 10}, retrieval.Filter{Identity: caller})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, h := range hits {
		if owner, _ := h.Fields["owner_agent_id"].(string); owner == "a9" {
			t.Fatalf("leaked a hit reachable only through an unauthorized (a9-owned) edge: %+v", h)
		}
	}
	// Sanity: the authorized A-B hit (and its expansion, both a1-owned) is
	// still present — proving this isn't just an empty/over-broad denial.
	var sawAB bool
	for _, h := range hits {
		if h.ID == "fact-ab" {
			sawAB = true
		}
	}
	if !sawAB {
		t.Fatalf("authorized seed hit was unexpectedly dropped too: %+v", hits)
	}
}

// --- DW-2.3: the seed-echo guard (visited set = fingerprints, not doc ids) ---

// tripleHit is semanticHit plus the predicate — the field the seed-echo guard
// needs to recompute the hit's own edge fingerprint. (semanticHit omits it,
// which is exactly why the pre-fix tests above never noticed the guard was
// inert.)
func tripleHit(id, subject, predicate, object string) retrieval.Hit {
	h := semanticHit(id, "t1", "private", "a1", "", subject, object)
	h.Fields["predicate"] = predicate
	return h
}

// TestDW_2_3_SeedEdgeNotReturnedAsExpandedHit: the seed fact "A works_at B"
// already told the caller about the A->B edge. Expansion must not hand that
// same edge back as a "discovery" — while still surfacing the genuinely new
// B->C hop.
//
// Before the fix, visitedEdges was seeded with the seed hits' SEMANTIC doc ids
// ("fact-ab") and compared against graph EDGE fingerprints — disjoint id spaces
// that can never collide, so the guard never fired once and A->B was always
// re-served.
func TestDW_2_3_SeedEdgeNotReturnedAsExpandedHit(t *testing.T) {
	ctx := context.Background()
	store, backend := newTestStore(t)
	aID, bID, _ := seedConnectTheDotsGraph(t, store)
	abEdgeID := edgeFingerprint("t1", aID, "works_at", bID)
	if _, ok, err := backend.GetEdge(ctx, "t1", abEdgeID); err != nil || !ok {
		t.Fatalf("precondition: the A->B edge must exist under its fingerprint (ok=%v err=%v)", ok, err)
	}

	e, err := NewExpander(store, 2, slog.Default())
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	seed := []retrieval.Hit{tripleHit("fact-ab", "A", "works_at", "B")}
	out, err := e.Expand(ctx, seed, 2)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}

	var sawC bool
	for _, h := range out {
		if h.Source != "graph" {
			continue
		}
		if h.ID == abEdgeID {
			t.Errorf("the seed fact's OWN edge came back as an expanded hit: %+v", h)
		}
		if obj, _ := h.Fields["object"].(string); obj == "C" {
			sawC = true
		}
	}
	if !sawC {
		t.Fatalf("the guard suppressed too much: the genuinely new B->C hop is missing; hits=%+v", out)
	}
}

// TestExpand_SeedWithoutPredicateStillExpands: a seed hit carrying no predicate
// (or none at all — the episodic tier has no subject/predicate/object fields)
// contributes no fingerprint, because it asserted no edge. It must still anchor
// expansion rather than being treated as suppressing something.
func TestExpand_SeedWithoutPredicateStillExpands(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	seedConnectTheDotsGraph(t, store)

	e, err := NewExpander(store, 2, slog.Default())
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	// An episodic-shaped hit (text only, no triple) alongside a predicate-less
	// semantic hit: the former must contribute nothing and crash nothing.
	episodic := retrieval.Hit{ID: "ep-1", Source: "episodic", Score: 5, Fields: map[string]any{"tenant_id": "t1", "text": "some raw event"}}
	seed := []retrieval.Hit{episodic, semanticHit("fact-ab", "t1", "private", "a1", "", "A", "B")}

	out, err := e.Expand(ctx, seed, 2)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out) <= len(seed) {
		t.Fatalf("a predicate-less seed must still anchor expansion, got no added hits: %+v", out)
	}
}

// fakeEdgeSource is a minimal acl.EdgeSource for wiring a real acl.Filter in
// this package's tests without depending on internal/store.
type fakeEdgeSource struct{ reach map[string]acl.Reach }

func (f fakeEdgeSource) Reachability(_ context.Context, id auth.Identity) (acl.Reach, error) {
	r := f.reach[id.UserID]
	if id.AgentID != "" {
		hasSelf := false
		for _, a := range r.Agents {
			if a == id.AgentID {
				hasSelf = true
			}
		}
		if !hasSelf {
			r.Agents = append(append([]string{}, r.Agents...), id.AgentID)
		}
	}
	return r, nil
}

// seedTier is a minimal retrieval.TierSource returning canned hits.
type seedTier struct{ hits []retrieval.Hit }

func (s *seedTier) Search(context.Context, auth.Identity, retrieval.Query) ([]retrieval.Hit, error) {
	return s.hits, nil
}

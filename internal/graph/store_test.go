package graph

import (
	"context"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/embed"
)

func newTestStore(t *testing.T) (*Store, *MemBackend) {
	t.Helper()
	backend := NewMemBackend()
	dedup := mustDeduper(t, RuleJudge{})
	embedder := embed.NewFakeEmbedder(8, nil)
	return NewStore(backend, dedup, embedder, slog.Default()), backend
}

// newNameKeyedTestStore is newTestStore with WithNameKeyedDedup enabled —
// the dev/e2e-only mode finding #2's fix introduces (DW-2.1/DW-2.2/DW-2.3).
func newNameKeyedTestStore(t *testing.T) (*Store, *MemBackend) {
	t.Helper()
	backend := NewMemBackend()
	dedup := mustDeduper(t, RuleJudge{})
	embedder := embed.NewFakeEmbedder(8, nil)
	return NewStore(backend, dedup, embedder, slog.Default(), WithNameKeyedDedup()), backend
}

// TestDW_6_1_IngestTouchesOnlyItsOwnEntities: ingesting one episode's
// mentions never recomputes or mutates an unrelated, already-landed entity —
// no batch recompute, ever (D2).
func TestDW_6_1_IngestTouchesOnlyItsOwnEntities(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)

	aID, _, err := store.UpsertMention(ctx, Mention{TenantID: "t1", Name: "service-a", Context: "service-a owns billing", SourceID: "ev-1"})
	if err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	before, ok, err := store.GetEntity(ctx, "t1", aID)
	if err != nil || !ok {
		t.Fatalf("get A before: ok=%v err=%v", ok, err)
	}

	// Ingest a wholly unrelated episode.
	_, _, err = store.UpsertMention(ctx, Mention{TenantID: "t1", Name: "service-z", Context: "service-z owns nothing related", SourceID: "ev-2"})
	if err != nil {
		t.Fatalf("upsert Z: %v", err)
	}

	after, ok, err := store.GetEntity(ctx, "t1", aID)
	if err != nil || !ok {
		t.Fatalf("get A after: ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("entity A was touched by an unrelated ingest:\nbefore=%+v\nafter=%+v", before, after)
	}
}

// TestDW_6_3_RepeatedIngestEntityCountStable: 10 re-ingests of the identical
// fact set (same mentions, same context) never grow the entity count past
// the first.
func TestDW_6_3_RepeatedIngestEntityCountStable(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)

	mention := Mention{TenantID: "t1", Name: "acme-svc", Context: "acme-svc owns checkout", SourceID: "ev-x"}
	var firstID string
	for i := 0; i < 10; i++ {
		id, dec, err := store.UpsertMention(ctx, mention)
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if i == 0 {
			firstID = id
			if dec.Merge {
				t.Fatal("first mention should create, not merge")
			}
		} else {
			if id != firstID {
				t.Fatalf("iteration %d: entity id changed (%s -> %s)", i, firstID, id)
			}
			if !dec.Merge {
				t.Fatalf("iteration %d: expected a merge decision, got new-entity", i)
			}
		}
		count, err := store.CountEntities(ctx, "t1")
		if err != nil {
			t.Fatalf("CountEntities: %v", err)
		}
		if count != 1 {
			t.Fatalf("iteration %d: entity count = %d, want 1 (stable)", i, count)
		}
	}
}

// TestHomonymDisambiguation_ThroughStore: two mentions sharing an exact name
// but unrelated context create TWO distinct entities, end to end through
// Store.UpsertMention (not just the Deduper in isolation).
func TestHomonymDisambiguation_ThroughStore(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)

	countryID, _, err := store.UpsertMention(ctx, Mention{
		TenantID: "t1", Name: "Jordan", Context: "Jordan is a country in the Middle East with capital Amman", SourceID: "ev-country",
	})
	if err != nil {
		t.Fatalf("country mention: %v", err)
	}
	athleteID, dec, err := store.UpsertMention(ctx, Mention{
		TenantID: "t1", Name: "Jordan", Context: "Jordan won six NBA championships playing basketball for Chicago", SourceID: "ev-athlete",
	})
	if err != nil {
		t.Fatalf("athlete mention: %v", err)
	}
	if dec.Merge {
		t.Fatalf("homonym mention merged into %q; want a distinct entity", dec.MatchID)
	}
	if countryID == athleteID {
		t.Fatal("two different Jordans resolved to the same entity id")
	}
	count, err := store.CountEntities(ctx, "t1")
	if err != nil {
		t.Fatalf("CountEntities: %v", err)
	}
	if count != 2 {
		t.Fatalf("entity count = %d, want 2 (disambiguated homonyms)", count)
	}
}

// TestDW_2_1_NameKeyedDedup_SameEntityDifferentFactContextMerges: under the
// deterministic dev embedder in name-keyed mode (WithNameKeyedDedup), the
// SAME entity mentioned in two facts with UNRELATED fact context resolves
// to exactly ONE entity doc — this is finding #2's dev-stack fragmentation,
// fixed. Before this option existed, this scenario (exercised without the
// option — see TestNameKeyedDedup_IsOptInOnly_DefaultStoreStillFragments
// below) produced TWO entity docs; WithNameKeyedDedup is the fix.
func TestDW_2_1_NameKeyedDedup_SameEntityDifferentFactContextMerges(t *testing.T) {
	ctx := context.Background()
	store, _ := newNameKeyedTestStore(t)

	id1, dec1, err := store.UpsertMention(ctx, Mention{TenantID: "t1", Scope: "private", Name: "Acme Corp", Context: "Acme Corp shipped the Q1 release", SourceID: "ev-1"})
	if err != nil {
		t.Fatalf("first mention: %v", err)
	}
	if dec1.Merge {
		t.Fatalf("first-ever mention of an entity should create, not merge: %+v", dec1)
	}
	id2, dec2, err := store.UpsertMention(ctx, Mention{TenantID: "t1", Scope: "private", Name: "Acme Corp", Context: "Acme Corp announced a merger with Widget Inc", SourceID: "ev-2"})
	if err != nil {
		t.Fatalf("second mention: %v", err)
	}
	if !dec2.Merge {
		t.Fatalf("second mention of the same entity (different fact context) did not merge: %+v", dec2)
	}
	if id1 != id2 {
		t.Fatalf("same entity across two facts resolved to two different entity ids: %s vs %s", id1, id2)
	}
	count, err := store.CountEntities(ctx, "t1")
	if err != nil {
		t.Fatalf("CountEntities: %v", err)
	}
	if count != 1 {
		t.Fatalf("entity count = %d, want 1 (same entity merged across facts, not fragmented)", count)
	}
}

// TestNameKeyedDedup_IsOptInOnly_DefaultStoreStillFragments pins the "before"
// half of DW-2.1's fails-before/passes-after proof: the DEFAULT Store (no
// WithNameKeyedDedup, i.e. every existing production/test construction path)
// still embeds full fact context, so the identical same-entity-two-facts
// scenario from TestDW_2_1_... still fragments into two entities under the
// fake embedder. This also documents that WithNameKeyedDedup is additive —
// it changes nothing for a caller who doesn't opt in.
func TestNameKeyedDedup_IsOptInOnly_DefaultStoreStillFragments(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t) // default: NOT name-keyed

	id1, _, err := store.UpsertMention(ctx, Mention{TenantID: "t1", Scope: "private", Name: "Acme Corp", Context: "Acme Corp shipped the Q1 release", SourceID: "ev-1"})
	if err != nil {
		t.Fatalf("first mention: %v", err)
	}
	id2, dec, err := store.UpsertMention(ctx, Mention{TenantID: "t1", Scope: "private", Name: "Acme Corp", Context: "Acme Corp announced a merger with Widget Inc", SourceID: "ev-2"})
	if err != nil {
		t.Fatalf("second mention: %v", err)
	}
	if dec.Merge || id1 == id2 {
		t.Fatalf("expected the DEFAULT (non-name-keyed) store to still fragment same-entity mentions under the fake embedder (id1=%s id2=%s merge=%v) — if this now merges, the production embedding contract has silently changed", id1, id2, dec.Merge)
	}
}

// TestDW_2_3_NameKeyedDedup_RepeatedIngestEntityCountStable: DW-6.3's
// stability contract (10x re-ingest of an identical mention never grows the
// entity count) still holds under the NEW name-keyed dev mode, not just the
// default store the existing DW-6.3 tests exercise.
func TestDW_2_3_NameKeyedDedup_RepeatedIngestEntityCountStable(t *testing.T) {
	ctx := context.Background()
	store, _ := newNameKeyedTestStore(t)

	mention := Mention{TenantID: "t1", Scope: "private", Name: "acme-svc", Context: "acme-svc owns checkout", SourceID: "ev-x"}
	var firstID string
	for i := 0; i < 10; i++ {
		id, dec, err := store.UpsertMention(ctx, mention)
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if i == 0 {
			firstID = id
		} else if id != firstID {
			t.Fatalf("iteration %d: entity id changed (%s -> %s)", i, firstID, id)
		} else if !dec.Merge {
			t.Fatalf("iteration %d: expected a merge decision, got new-entity", i)
		}
	}
	count, err := store.CountEntities(ctx, "t1")
	if err != nil {
		t.Fatalf("CountEntities: %v", err)
	}
	if count != 1 {
		t.Fatalf("entity count = %d, want 1 (stable under name-keyed dedup)", count)
	}
}

// TestScopePreFilter_PrivateAndTeamSameNameStaySeparate: the (tenant, scope)
// merge boundary (plan-review finding) — a private-scope entity and a
// team-scope entity sharing an exact name AND identical context in the SAME
// tenant are never merge candidates for each other, because UpsertMention
// pre-filters candidates by scope before Decide ever sees them (Decide's
// signature carries no Scope field and is not asked to enforce this).
// Without the pre-filter, identical name+context would clear the merge
// threshold easily (embed sim = lex sim = 1.0) — this test would fail.
func TestScopePreFilter_PrivateAndTeamSameNameStaySeparate(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	sameContext := "Acme is discussed in this note"

	privID, _, err := store.UpsertMention(ctx, Mention{TenantID: "t1", Scope: "private", OwnerAgentID: "a1", Name: "Acme", Context: sameContext, SourceID: "ev-1"})
	if err != nil {
		t.Fatalf("private mention: %v", err)
	}
	teamID, dec, err := store.UpsertMention(ctx, Mention{TenantID: "t1", Scope: "team", TeamID: "team-x", Name: "Acme", Context: sameContext, SourceID: "ev-2"})
	if err != nil {
		t.Fatalf("team mention: %v", err)
	}
	if dec.Merge {
		t.Fatalf("private-scope and team-scope same-name entities merged across the scope boundary: %+v", dec)
	}
	if privID == teamID {
		t.Fatal("private-scope and team-scope same-name entities resolved to the same entity id")
	}
	count, err := store.CountEntities(ctx, "t1")
	if err != nil {
		t.Fatalf("CountEntities: %v", err)
	}
	if count != 2 {
		t.Fatalf("entity count = %d, want 2 (scope-separated despite identical name+context)", count)
	}
}

// TestUpsertEdge_RepeatedIdenticalFactStaysOneDoc: DW-6.1/6.3's edge-side
// counterpart — the same (tenant, from, predicate, to) always upserts one
// doc.
func TestUpsertEdge_RepeatedIdenticalFactStaysOneDoc(t *testing.T) {
	ctx := context.Background()
	store, backend := newTestStore(t)
	validAt := time.Now().UTC()

	var lastID string
	for i := 0; i < 5; i++ {
		id, err := store.UpsertEdge(ctx, EdgeSpec{TenantID: "t1", TeamID: "", Scope: "private", OwnerAgentID: "a1", FromEntityID: "from-1", ToEntityID: "to-1", Predicate: "owns", Statement: "from-1 owns to-1", SourceID: "ev-1", ValidAt: validAt})
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if i > 0 && id != lastID {
			t.Fatalf("iteration %d: edge id changed", i)
		}
		lastID = id
	}
	edges, err := backend.Neighbors(ctx, "t1", "from-1")
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("edge count = %d, want 1 after 5 identical upserts", len(edges))
	}
	if len(edges[0].SourceIDs) != 1 {
		t.Errorf("SourceIDs = %v, want exactly one (same event replayed, not double-counted)", edges[0].SourceIDs)
	}
}

// TestUpsertMention_RequiresTenantAndName is the dirty/boundary input test.
func TestUpsertMention_RequiresTenantAndName(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	cases := []Mention{
		{TenantID: "", Name: "x"},
		{TenantID: "t1", Name: ""},
		{TenantID: "t1", Name: "   "},
	}
	for _, m := range cases {
		if _, _, err := store.UpsertMention(ctx, m); err == nil {
			t.Errorf("UpsertMention(%+v) should error", m)
		}
	}
}

// TestNeighbors_ExcludesSoftExpiredAndClosedEdges proves the dangling-edge
// exclusion happens at the store/backend layer too (Expander separately
// checks endpoint liveness).
func TestNeighbors_ExcludesSoftExpiredAndClosedEdges(t *testing.T) {
	ctx := context.Background()
	_, backend := newTestStore(t)
	now := time.Now().UTC()
	live := Edge{ID: "live", TenantID: "t1", FromEntityID: "a", ToEntityID: "b", Predicate: "p", ValidAt: now, CreatedAt: now}
	closed := Edge{ID: "closed", TenantID: "t1", FromEntityID: "a", ToEntityID: "c", Predicate: "p", ValidAt: now, CreatedAt: now, InvalidAt: &now}
	expired := Edge{ID: "expired", TenantID: "t1", FromEntityID: "a", ToEntityID: "d", Predicate: "p", ValidAt: now, CreatedAt: now, ExpiredAt: &now}
	for _, e := range []Edge{live, closed, expired} {
		if err := backend.PutEdge(ctx, e); err != nil {
			t.Fatalf("PutEdge: %v", err)
		}
	}
	edges, err := backend.Neighbors(ctx, "t1", "a")
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	if len(edges) != 1 || edges[0].ID != "live" {
		t.Fatalf("Neighbors returned %v, want only the live edge", edges)
	}
}

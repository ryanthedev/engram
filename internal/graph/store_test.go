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

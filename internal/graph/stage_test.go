package graph

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/ingest"
	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/worker"
)

// var _ worker.Stage confirms Stage satisfies the D20 seam at compile time
// without making the production package depend on internal/worker. It is the
// DW-1.1 assertion: the seam now carries []ingest.FactOutcome, and this fails
// to compile if Stage.Process drifts from it.
var _ worker.Stage = (*Stage)(nil)

// added wraps freshly-landed facts as OpAdd outcomes — the shape the worker
// hands the stage for new knowledge with no predecessor.
func added(facts ...memory.SemanticFact) []ingest.FactOutcome {
	out := make([]ingest.FactOutcome, 0, len(facts))
	for _, f := range facts {
		out = append(out, ingest.FactOutcome{Fact: f, Decision: ingest.OpAdd})
	}
	return out
}

func newTestStage(t *testing.T) (*Stage, *Store, *MemBackend) {
	t.Helper()
	store, backend := newTestStore(t)
	return NewStage(store, slog.Default()), store, backend
}

func fact(subject, predicate, object, statement string) memory.SemanticFact {
	return memory.SemanticFact{
		Subject: subject, Predicate: predicate, Object: object, Statement: statement,
		TenantID: "t1", OwnerAgentID: "a1", Scope: "private", ValidAt: time.Now().UTC(),
	}
}

// TestStage_UpsertsEntitiesAndEdgeFromOneFact (DW-6.1's ingest-side unit).
func TestStage_UpsertsEntitiesAndEdgeFromOneFact(t *testing.T) {
	ctx := context.Background()
	stage, store, backend := newTestStage(t)
	ev := memory.Episodic{EventID: "ev-1", TenantID: "t1", OwnerAgentID: "a1", Scope: "private"}
	facts := []memory.SemanticFact{fact("service-a", "owns", "billing-db", "service-a owns billing-db")}

	if err := stage.Process(ctx, ev, added(facts...)); err != nil {
		t.Fatalf("Process: %v", err)
	}
	count, err := store.CountEntities(ctx, "t1")
	if err != nil {
		t.Fatalf("CountEntities: %v", err)
	}
	if count != 2 {
		t.Fatalf("entity count = %d, want 2 (subject + object)", count)
	}
	cands, err := backend.CandidateEntities(ctx, "t1", "service-a")
	if err != nil || len(cands) != 1 {
		t.Fatalf("expected exactly one candidate for service-a: %v (err=%v)", cands, err)
	}
	edges, err := backend.Neighbors(ctx, "t1", cands[0].ID)
	if err != nil || len(edges) != 1 {
		t.Fatalf("expected exactly one edge from service-a: %v (err=%v)", edges, err)
	}
	if edges[0].Predicate != "owns" {
		t.Errorf("edge predicate = %q, want owns", edges[0].Predicate)
	}
}

// TestStage_RetractionFactYieldsNoEdge: an empty Object (retraction intent,
// per ingest.ParseExtraction) still creates the subject mention but no edge.
func TestStage_RetractionFactYieldsNoEdge(t *testing.T) {
	ctx := context.Background()
	stage, store, backend := newTestStage(t)
	ev := memory.Episodic{EventID: "ev-1", TenantID: "t1"}
	facts := []memory.SemanticFact{fact("service-a", "owns", "", "service-a owns: retracted")}

	if err := stage.Process(ctx, ev, added(facts...)); err != nil {
		t.Fatalf("Process: %v", err)
	}
	count, _ := store.CountEntities(ctx, "t1")
	if count != 1 {
		t.Fatalf("entity count = %d, want 1 (subject only, no object to connect)", count)
	}
	cands, _ := backend.CandidateEntities(ctx, "t1", "service-a")
	edges, _ := backend.Neighbors(ctx, "t1", cands[0].ID)
	if len(edges) != 0 {
		t.Fatalf("expected no edges for a retraction fact, got %v", edges)
	}
}

// TestStage_SkipsFactsWithNoSubject: a fact with an empty Subject (shouldn't
// occur post-ParseExtraction, but the stage stays defensive) is skipped, not
// an error.
func TestStage_SkipsFactsWithNoSubject(t *testing.T) {
	ctx := context.Background()
	stage, store, _ := newTestStage(t)
	ev := memory.Episodic{EventID: "ev-1", TenantID: "t1"}
	facts := []memory.SemanticFact{{Subject: "", Predicate: "p", Object: "o", TenantID: "t1"}}

	if err := stage.Process(ctx, ev, added(facts...)); err != nil {
		t.Fatalf("Process: %v", err)
	}
	count, _ := store.CountEntities(ctx, "t1")
	if count != 0 {
		t.Fatalf("entity count = %d, want 0", count)
	}
}

// TestStage_ReplayIsIdempotent: processing the SAME event+facts twice
// (at-least-once replay, e.g. after a crash before ledger-complete) never
// grows the entity or edge count (DW-6.1/6.3 at the stage level).
func TestStage_ReplayIsIdempotent(t *testing.T) {
	ctx := context.Background()
	stage, store, backend := newTestStage(t)
	ev := memory.Episodic{EventID: "ev-1", TenantID: "t1"}
	facts := []memory.SemanticFact{fact("service-a", "owns", "billing-db", "service-a owns billing-db")}

	for i := 0; i < 3; i++ {
		if err := stage.Process(ctx, ev, added(facts...)); err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
	}
	count, _ := store.CountEntities(ctx, "t1")
	if count != 2 {
		t.Fatalf("entity count after 3 replays = %d, want 2", count)
	}
	cands, _ := backend.CandidateEntities(ctx, "t1", "service-a")
	edges, _ := backend.Neighbors(ctx, "t1", cands[0].ID)
	if len(edges) != 1 {
		t.Fatalf("edge count after 3 replays = %d, want 1", len(edges))
	}
}

// TestStage_MultipleFactsShareEntities: two facts naming the same entity
// (as subject in one, object in another) resolve to one entity, forming a
// connected chain — the substrate DW-6.2's 2-hop test walks.
func TestStage_MultipleFactsShareEntities(t *testing.T) {
	ctx := context.Background()
	stage, store, backend := newTestStage(t)
	ev := memory.Episodic{EventID: "ev-1", TenantID: "t1"}
	facts := []memory.SemanticFact{
		fact("A", "works_at", "B", "A works_at B"),
		fact("B", "located_in", "C", "B located_in C"),
	}
	if err := stage.Process(ctx, ev, added(facts...)); err != nil {
		t.Fatalf("Process: %v", err)
	}
	count, _ := store.CountEntities(ctx, "t1")
	if count != 3 {
		t.Fatalf("entity count = %d, want 3 (A, B, C)", count)
	}
	bCands, _ := backend.CandidateEntities(ctx, "t1", "B")
	if len(bCands) != 1 {
		t.Fatalf("expected exactly one B entity, got %d", len(bCands))
	}
	edges, _ := backend.Neighbors(ctx, "t1", bCands[0].ID)
	if len(edges) != 2 {
		t.Fatalf("B should have 2 edges (A->B, B->C), got %d", len(edges))
	}
}

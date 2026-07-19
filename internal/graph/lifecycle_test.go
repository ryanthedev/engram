package graph

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/ingest"
	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/retrieval"
)

// Phase 2 — graph edge lifecycle (DW-2.1, DW-2.2, DW-2.4, DW-2.5) and the
// edge cases the phase's Scope names: the late-arrival gotcha, a predecessor
// that never had an edge, and replay.

// --- fixtures -------------------------------------------------------------

// newNameKeyedTestStage is newTestStage with WithNameKeyedDedup: every mention
// of one NAME resolves to one entity, which is what gives these tests the
// realistic topology a real (semantic) embedder produces — the same subject
// mentioned across a fact and its correction is ONE entity, so a supersession
// rewrites an edge on a shared node instead of fragmenting the graph. (The
// context-keyed production default is exercised separately by
// TestStage_ClosesPredecessorEdgeUnderContextKeyedDedup.)
func newNameKeyedTestStage(t *testing.T) (*Stage, *Store, *MemBackend) {
	t.Helper()
	store, backend := newNameKeyedTestStore(t)
	return NewStage(store, slog.Default()), store, backend
}

// factAt is fact(...) with an explicit valid time — supersession ordering is
// the whole point of these tests, so it can never be left to wall-clock luck.
func factAt(subject, predicate, object, statement string, validAt time.Time) memory.SemanticFact {
	f := fact(subject, predicate, object, statement)
	f.ValidAt = validAt
	return f
}

// superseded wraps a landed fact as the outcome of a supersession: the
// reconciler reported kind (OpUpdate or OpInvalidate) against predecessor.
func superseded(kind ingest.OpKind, f, predecessor memory.SemanticFact) []ingest.FactOutcome {
	return []ingest.FactOutcome{{Fact: f, Decision: kind, Predecessor: &predecessor}}
}

// entityIDNamed returns the id of the live entity canonically named name
// (CandidateEntities is a deliberately fuzzy superset, so the exact match is
// filtered here rather than trusted from the backend).
func entityIDNamed(t *testing.T, b *MemBackend, name string) string {
	t.Helper()
	cands, err := b.CandidateEntities(context.Background(), "t1", name)
	if err != nil {
		t.Fatalf("CandidateEntities(%q): %v", name, err)
	}
	for _, c := range cands {
		if normalizeName(c.Name) == normalizeName(name) {
			return c.ID
		}
	}
	t.Fatalf("no live entity named %q (candidates: %v)", name, cands)
	return ""
}

// edgeNamed fetches the edge the triple (subject, predicate, object) produces,
// recomputing its fingerprint exactly as the store keys it.
func edgeNamed(t *testing.T, b *MemBackend, subject, predicate, object string) (Edge, bool) {
	t.Helper()
	id := edgeFingerprint("t1", entityIDNamed(t, b, subject), predicate, entityIDNamed(t, b, object))
	e, ok, err := b.GetEdge(context.Background(), "t1", id)
	if err != nil {
		t.Fatalf("GetEdge(%s): %v", id, err)
	}
	return e, ok
}

// landAndSupersede runs the canonical two-event story these tests share:
// ev-1 lands "service-a owns billing-db"; ev-2 supersedes it with a fact whose
// object is newObject (empty ⇒ a retraction, i.e. OpInvalidate). It returns the
// stage's backend, ready to assert on.
func landAndSupersede(t *testing.T, kind ingest.OpKind, newObject string) *MemBackend {
	t.Helper()
	ctx := context.Background()
	stage, _, backend := newNameKeyedTestStage(t)

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	old := factAt("service-a", "owns", "billing-db", "service-a owns billing-db", t0)
	if err := stage.Process(ctx, memory.Episodic{EventID: "ev-1", TenantID: "t1"}, added(old)); err != nil {
		t.Fatalf("Process(ev-1): %v", err)
	}
	if e, ok := edgeNamed(t, backend, "service-a", "owns", "billing-db"); !ok || !e.Live() {
		t.Fatalf("precondition: the original edge should exist and be live (ok=%v edge=%+v)", ok, e)
	}

	statement := "service-a owns " + newObject
	if newObject == "" {
		statement = "service-a owns: retracted"
	}
	next := factAt("service-a", "owns", newObject, statement, t0.Add(time.Hour))
	if err := stage.Process(ctx, memory.Episodic{EventID: "ev-2", TenantID: "t1"}, superseded(kind, next, old)); err != nil {
		t.Fatalf("Process(ev-2): %v", err)
	}
	return backend
}

// --- DW-2.1: supersession closes the predecessor's edge --------------------

// TestDW_2_1_UpdateClosesPredecessorEdge: an UPDATE stamps InvalidAt on the
// edge of the fact it superseded, so that edge is no longer Live().
func TestDW_2_1_UpdateClosesPredecessorEdge(t *testing.T) {
	backend := landAndSupersede(t, ingest.OpUpdate, "billing-db-v2")

	closed, ok := edgeNamed(t, backend, "service-a", "owns", "billing-db")
	if !ok {
		t.Fatal("the predecessor's edge was removed, not closed — closing must be soft (bi-temporal), never a delete")
	}
	if closed.InvalidAt == nil {
		t.Fatalf("predecessor edge InvalidAt = nil, want a close stamp; edge=%+v", closed)
	}
	if closed.Live() {
		t.Fatalf("predecessor edge is still Live() after being superseded; edge=%+v", closed)
	}

	// The correction itself is live — this is a supersession, not a wipe.
	fresh, ok := edgeNamed(t, backend, "service-a", "owns", "billing-db-v2")
	if !ok || !fresh.Live() {
		t.Fatalf("the superseding fact's own edge should be live (ok=%v edge=%+v)", ok, fresh)
	}
}

// TestDW_2_1_InvalidateClosesPredecessorEdge: a retraction (empty Object ⇒
// OpInvalidate) writes NO edge of its own — the retraction convention — yet
// still closes the edge it retracted. This is the case a naive "close the
// predecessor only when we wrote a replacement" implementation would miss.
func TestDW_2_1_InvalidateClosesPredecessorEdge(t *testing.T) {
	backend := landAndSupersede(t, ingest.OpInvalidate, "")

	closed, ok := edgeNamed(t, backend, "service-a", "owns", "billing-db")
	if !ok {
		t.Fatal("retracted edge was removed, not closed")
	}
	if closed.InvalidAt == nil || closed.Live() {
		t.Fatalf("retracted edge should be closed and not Live(); edge=%+v", closed)
	}
}

// TestStage_ClosesPredecessorEdgeUnderContextKeyedDedup proves the phase's
// load-bearing assumption on the PRODUCTION dedup path (context embeddings, not
// the name-keyed dev mode): the predecessor's triple alone is enough to recover
// its edge fingerprint. Re-resolving the predecessor's mentions with its own
// Statement as context re-finds the very entities it landed on, so the edge is
// found and closed even though the superseding fact's different context makes
// its own subject mention resolve elsewhere.
func TestStage_ClosesPredecessorEdgeUnderContextKeyedDedup(t *testing.T) {
	ctx := context.Background()
	store, backend := newTestStore(t) // context-keyed: the production configuration
	stage := NewStage(store, slog.Default())

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	old := factAt("service-a", "owns", "billing-db", "service-a owns billing-db", t0)
	if err := stage.Process(ctx, memory.Episodic{EventID: "ev-1", TenantID: "t1"}, added(old)); err != nil {
		t.Fatalf("Process(ev-1): %v", err)
	}
	before, ok := edgeNamed(t, backend, "service-a", "owns", "billing-db")
	if !ok || !before.Live() {
		t.Fatalf("precondition: original edge must be live; ok=%v edge=%+v", ok, before)
	}

	next := factAt("service-a", "owns", "billing-db-v2", "service-a owns billing-db-v2", t0.Add(time.Hour))
	if err := stage.Process(ctx, memory.Episodic{EventID: "ev-2", TenantID: "t1"}, superseded(ingest.OpUpdate, next, old)); err != nil {
		t.Fatalf("Process(ev-2): %v", err)
	}

	closed, ok, err := backend.GetEdge(ctx, "t1", before.ID)
	if err != nil || !ok {
		t.Fatalf("GetEdge(%s): ok=%v err=%v", before.ID, ok, err)
	}
	if closed.Live() {
		t.Fatalf("predecessor edge still live under context-keyed dedup — the fingerprint was not recovered; edge=%+v", closed)
	}
}

// --- DW-2.2: a closed edge disappears from reads ---------------------------

// TestDW_2_2_SupersededEdgeAbsentFromNeighbors: Neighbors is the traversal
// primitive every read path (and the expander) is built on — a superseded edge
// must not come back from it.
func TestDW_2_2_SupersededEdgeAbsentFromNeighbors(t *testing.T) {
	ctx := context.Background()
	backend := landAndSupersede(t, ingest.OpUpdate, "billing-db-v2")

	stale := entityIDNamed(t, backend, "billing-db")
	subject := entityIDNamed(t, backend, "service-a")

	edges, err := backend.Neighbors(ctx, "t1", subject)
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("service-a should have exactly 1 live edge after the correction, got %d: %+v", len(edges), edges)
	}
	if edges[0].ToEntityID == stale {
		t.Fatalf("Neighbors still serves the superseded edge to billing-db: %+v", edges[0])
	}

	// The stale endpoint is reachable from neither direction.
	if edges, err := backend.Neighbors(ctx, "t1", stale); err != nil || len(edges) != 0 {
		t.Fatalf("the superseded object should have no live edges left, got %d (err=%v): %+v", len(edges), err, edges)
	}
}

// TestDW_2_2_SupersededEdgeAbsentFromSearchResults: the same guarantee at the
// level a caller actually sees — a superseded relation is never served as an
// expansion hit alongside the correction that replaced it (the zombie-edge bug
// this phase exists to kill).
func TestDW_2_2_SupersededEdgeAbsentFromSearchResults(t *testing.T) {
	ctx := context.Background()
	backend := landAndSupersede(t, ingest.OpUpdate, "billing-db-v2")
	store := NewStore(backend, mustDeduper(t, RuleJudge{}), nil, slog.Default(), WithNameKeyedDedup())

	expander, err := NewExpander(store, 2, slog.Default())
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	// Seed on the OBJECT that was superseded away, so the stale edge is the one
	// thing expansion would surface if it were still live.
	seed := []retrieval.Hit{semanticHit("fact-x", "t1", "private", "a1", "", "billing-db", "")}
	out, err := expander.Expand(ctx, seed, 2)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	for _, h := range out {
		if h.Source != "graph" {
			continue
		}
		if obj, _ := h.Fields["object"].(string); obj == "billing-db" {
			t.Fatalf("search results still carry the superseded billing-db edge: %+v", h)
		}
	}
}

// --- DW-2.4: closing is idempotent ----------------------------------------

// TestDW_2_4_CloseEdgeIsIdempotent: closing the same edge repeatedly neither
// errors nor re-stamps InvalidAt — a replayed close must not drift the closing
// time forward.
func TestDW_2_4_CloseEdgeIsIdempotent(t *testing.T) {
	ctx := context.Background()
	backend := landAndSupersede(t, ingest.OpUpdate, "billing-db-v2")
	store := NewStore(backend, mustDeduper(t, RuleJudge{}), nil, slog.Default())

	closed, ok := edgeNamed(t, backend, "service-a", "owns", "billing-db")
	if !ok || closed.Live() {
		t.Fatalf("precondition: the edge should already be closed; ok=%v edge=%+v", ok, closed)
	}
	first := *closed.InvalidAt

	for i := 0; i < 3; i++ {
		if err := store.CloseEdge(ctx, "t1", closed.ID); err != nil {
			t.Fatalf("CloseEdge (re-close %d) errored: %v", i, err)
		}
	}
	again, _, err := backend.GetEdge(ctx, "t1", closed.ID)
	if err != nil {
		t.Fatalf("GetEdge: %v", err)
	}
	if again.InvalidAt == nil || !again.InvalidAt.Equal(first) {
		t.Fatalf("re-closing rewrote InvalidAt: was %v, now %v", first, again.InvalidAt)
	}
}

// TestDW_2_4_CloseEdgeUnknownIDIsNoOp: an edge id nothing exists under — the
// predecessor whose edge was never written (retraction fact) or whose entity
// was soft-expired — is a silent no-op, never ErrNotFound.
func TestDW_2_4_CloseEdgeUnknownIDIsNoOp(t *testing.T) {
	ctx := context.Background()
	store, backend := newTestStore(t)

	if err := store.CloseEdge(ctx, "t1", "no-such-edge"); err != nil {
		t.Fatalf("CloseEdge(unknown id) = %v, want nil (idempotent, not an error)", err)
	}
	// A cross-tenant id is equally a no-op — and must not close the other
	// tenant's edge.
	aID, _, err := store.UpsertMention(ctx, Mention{TenantID: "t1", Scope: "private", Name: "A", Context: "A owns B", SourceID: "ev-1"})
	if err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	bID, _, err := store.UpsertMention(ctx, Mention{TenantID: "t1", Scope: "private", Name: "B", Context: "A owns B", SourceID: "ev-1"})
	if err != nil {
		t.Fatalf("upsert B: %v", err)
	}
	edgeID, err := store.UpsertEdge(ctx, EdgeSpec{TenantID: "t1", Scope: "private", FromEntityID: aID, ToEntityID: bID, Predicate: "owns", Statement: "A owns B", SourceID: "ev-1", ValidAt: time.Now().UTC()})
	if err != nil {
		t.Fatalf("UpsertEdge: %v", err)
	}
	if err := store.CloseEdge(ctx, "t2", edgeID); err != nil {
		t.Fatalf("CloseEdge(other tenant) = %v, want nil", err)
	}
	e, _, _ := backend.GetEdge(ctx, "t1", edgeID)
	if !e.Live() {
		t.Fatalf("a close issued under tenant t2 closed t1's edge — tenant isolation breached: %+v", e)
	}
}

// TestDW_2_4_ReplayedOutcomeDoesNotClose: OpReplayed means the write already
// landed on an earlier attempt and its decision is unrecoverable — the stage
// must treat it as already-accounted-for and never re-apply a supersession side
// effect. Replaying the whole event is likewise a no-op on the graph.
func TestDW_2_4_ReplayedOutcomeDoesNotClose(t *testing.T) {
	ctx := context.Background()
	stage, _, backend := newNameKeyedTestStage(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	old := factAt("service-a", "owns", "billing-db", "service-a owns billing-db", t0)
	if err := stage.Process(ctx, memory.Episodic{EventID: "ev-1", TenantID: "t1"}, added(old)); err != nil {
		t.Fatalf("Process(ev-1): %v", err)
	}

	// A replayed fact carries a nil Predecessor by invariant — nothing to close.
	replay := []ingest.FactOutcome{{Fact: old, Decision: ingest.OpReplayed}}
	for i := 0; i < 3; i++ {
		if err := stage.Process(ctx, memory.Episodic{EventID: "ev-1", TenantID: "t1"}, replay); err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
	}
	e, ok := edgeNamed(t, backend, "service-a", "owns", "billing-db")
	if !ok || !e.Live() {
		t.Fatalf("a replayed outcome closed a live edge; ok=%v edge=%+v", ok, e)
	}
}

// TestStage_SupersedingEventReplayIsIdempotent: re-processing the SAME
// supersession event (at-least-once delivery, e.g. a crash before
// ledger-complete) closes the same edge again without erroring and without
// disturbing the live one.
func TestStage_SupersedingEventReplayIsIdempotent(t *testing.T) {
	ctx := context.Background()
	stage, store, backend := newNameKeyedTestStage(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	old := factAt("service-a", "owns", "billing-db", "service-a owns billing-db", t0)
	if err := stage.Process(ctx, memory.Episodic{EventID: "ev-1", TenantID: "t1"}, added(old)); err != nil {
		t.Fatalf("Process(ev-1): %v", err)
	}
	next := factAt("service-a", "owns", "billing-db-v2", "service-a owns billing-db-v2", t0.Add(time.Hour))
	ev2 := memory.Episodic{EventID: "ev-2", TenantID: "t1"}
	for i := 0; i < 3; i++ {
		if err := stage.Process(ctx, ev2, superseded(ingest.OpUpdate, next, old)); err != nil {
			t.Fatalf("supersession replay %d: %v", i, err)
		}
	}

	stale, _ := edgeNamed(t, backend, "service-a", "owns", "billing-db")
	if stale.Live() {
		t.Fatalf("superseded edge is live after replays: %+v", stale)
	}
	fresh, ok := edgeNamed(t, backend, "service-a", "owns", "billing-db-v2")
	if !ok || !fresh.Live() {
		t.Fatalf("the correction's edge must survive every replay; ok=%v edge=%+v", ok, fresh)
	}
	if n, _ := store.CountEntities(ctx, "t1"); n != 3 {
		t.Fatalf("entity count = %d, want 3 (service-a, billing-db, billing-db-v2) — replay must not grow the graph", n)
	}
}

// TestDW_2_4_ReplayOfSupersededEventDoesNotResurrectEdge is the mirror of
// "replay must not double-close": replay must not RE-OPEN either.
//
// An edge's doc id is a pure function of its triple, so re-ingesting the
// original event lands an upsert on the very doc the correction closed. Because
// UpsertEdge rebuilds the Edge struct, it used to drop InvalidAt — and
// at-least-once redelivery of the superseded event silently resurrected the
// zombie edge, handing the stale relation straight back to search and undoing
// the supersession entirely. The close survives the replay.
func TestDW_2_4_ReplayOfSupersededEventDoesNotResurrectEdge(t *testing.T) {
	ctx := context.Background()
	stage, _, backend := newNameKeyedTestStage(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ev1 := memory.Episodic{EventID: "ev-1", TenantID: "t1"}

	old := factAt("service-a", "owns", "billing-db", "service-a owns billing-db", t0)
	if err := stage.Process(ctx, ev1, added(old)); err != nil {
		t.Fatalf("Process(ev-1): %v", err)
	}
	next := factAt("service-a", "owns", "billing-db-v2", "service-a owns billing-db-v2", t0.Add(time.Hour))
	if err := stage.Process(ctx, memory.Episodic{EventID: "ev-2", TenantID: "t1"}, superseded(ingest.OpUpdate, next, old)); err != nil {
		t.Fatalf("Process(ev-2): %v", err)
	}

	// ev-1 is redelivered AFTER the correction landed (at-least-once delivery).
	// It replays as OpReplayed and re-upserts the identical triple.
	for i := 0; i < 3; i++ {
		if err := stage.Process(ctx, ev1, []ingest.FactOutcome{{Fact: old, Decision: ingest.OpReplayed}}); err != nil {
			t.Fatalf("redelivery %d: %v", i, err)
		}
	}

	stale, ok := edgeNamed(t, backend, "service-a", "owns", "billing-db")
	if !ok {
		t.Fatal("the closed edge vanished")
	}
	if stale.Live() {
		t.Fatalf("replaying the superseded event RESURRECTED its closed edge — the zombie relation is live again: %+v", stale)
	}
	subject := entityIDNamed(t, backend, "service-a")
	edges, err := backend.Neighbors(ctx, "t1", subject)
	if err != nil || len(edges) != 1 {
		t.Fatalf("service-a should still have exactly 1 live edge after the redeliveries, got %d (err=%v): %+v", len(edges), err, edges)
	}
}

// TestUpsertEdge_ReAssertionRevivesClosedEdge is the other side of that
// discriminator, and the reason the close is not simply frozen forever: a
// relation that is retracted and then GENUINELY re-asserted later is true
// again, and its edge must come back. The re-assertion is distinguishable from
// a replay by exactly one thing — a strictly newer valid time (a replay can only
// ever carry the valid time already recorded).
func TestUpsertEdge_ReAssertionRevivesClosedEdge(t *testing.T) {
	ctx := context.Background()
	store, backend := newTestStore(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	aID, _, err := store.UpsertMention(ctx, Mention{TenantID: "t1", Scope: "private", Name: "A", Context: "A owns B", SourceID: "ev-1"})
	if err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	bID, _, err := store.UpsertMention(ctx, Mention{TenantID: "t1", Scope: "private", Name: "B", Context: "A owns B", SourceID: "ev-1"})
	if err != nil {
		t.Fatalf("upsert B: %v", err)
	}
	spec := EdgeSpec{TenantID: "t1", Scope: "private", FromEntityID: aID, ToEntityID: bID, Predicate: "owns", Statement: "A owns B", SourceID: "ev-1", ValidAt: t0}
	edgeID, err := store.UpsertEdge(ctx, spec)
	if err != nil {
		t.Fatalf("UpsertEdge: %v", err)
	}
	if err := store.CloseEdge(ctx, "t1", edgeID); err != nil {
		t.Fatalf("CloseEdge: %v", err)
	}

	// Same valid time (a replay) must NOT revive it.
	if _, err := store.UpsertEdge(ctx, spec); err != nil {
		t.Fatalf("UpsertEdge (replay): %v", err)
	}
	if e, _, _ := backend.GetEdge(ctx, "t1", edgeID); e.Live() {
		t.Fatalf("a same-valid-time replay revived the closed edge: %+v", e)
	}

	// A strictly newer assertion — the relation is true again — MUST revive it.
	reassert := spec
	reassert.ValidAt, reassert.SourceID = t0.Add(48*time.Hour), "ev-9"
	if _, err := store.UpsertEdge(ctx, reassert); err != nil {
		t.Fatalf("UpsertEdge (re-assertion): %v", err)
	}
	e, ok, err := backend.GetEdge(ctx, "t1", edgeID)
	if err != nil || !ok {
		t.Fatalf("GetEdge: ok=%v err=%v", ok, err)
	}
	if !e.Live() {
		t.Fatalf("a genuinely re-asserted (newer) relation stayed dead: %+v", e)
	}
	if !e.ValidAt.Equal(reassert.ValidAt) {
		t.Errorf("reviving should advance ValidAt to the re-assertion's: got %v, want %v", e.ValidAt, reassert.ValidAt)
	}
	if edges, err := backend.Neighbors(ctx, "t1", aID); err != nil || len(edges) != 1 {
		t.Fatalf("the revived edge should be traversable again, got %d edges (err=%v)", len(edges), err)
	}
}

// --- DW-2.5: an UPDATE must not close the edge it just wrote ---------------

// TestDW_2_5_UpdateToSameEntityDoesNotCloseItsOwnEdge: a correction that only
// RESTATES its object ("billing-db" → "Billing-DB") is a genuine semantic
// UPDATE (different content key ⇒ a predecessor to supersede), but both objects
// dedup to the SAME entity — so the predecessor's triple fingerprints to the
// exact edge this call just upserted. Closing it would leave the relation dead
// with no replacement: the correction would silently erase itself.
func TestDW_2_5_UpdateToSameEntityDoesNotCloseItsOwnEdge(t *testing.T) {
	ctx := context.Background()
	stage, _, backend := newNameKeyedTestStage(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	old := factAt("service-a", "owns", "billing-db", "service-a owns billing-db", t0)
	if err := stage.Process(ctx, memory.Episodic{EventID: "ev-1", TenantID: "t1"}, added(old)); err != nil {
		t.Fatalf("Process(ev-1): %v", err)
	}
	subjID := entityIDNamed(t, backend, "service-a")
	objID := entityIDNamed(t, backend, "billing-db")
	edgeID := edgeFingerprint("t1", subjID, "owns", objID)

	// Same entity, different surface form ⇒ the same edge fingerprint.
	next := factAt("service-a", "owns", "Billing-DB", "service-a owns Billing-DB", t0.Add(time.Hour))
	if err := stage.Process(ctx, memory.Episodic{EventID: "ev-2", TenantID: "t1"}, superseded(ingest.OpUpdate, next, old)); err != nil {
		t.Fatalf("Process(ev-2): %v", err)
	}

	if got := entityIDNamed(t, backend, "Billing-DB"); got != objID {
		t.Fatalf("fixture precondition failed: %q resolved to a NEW entity (%s != %s), so this is not the same-entity case", "Billing-DB", got, objID)
	}
	e, ok, err := backend.GetEdge(ctx, "t1", edgeID)
	if err != nil || !ok {
		t.Fatalf("GetEdge(%s): ok=%v err=%v", edgeID, ok, err)
	}
	if !e.Live() {
		t.Fatalf("the UPDATE closed the very edge it just wrote — the relation is now dead with no replacement: %+v", e)
	}
	edges, err := backend.Neighbors(ctx, "t1", subjID)
	if err != nil || len(edges) != 1 {
		t.Fatalf("service-a should still have exactly 1 live edge, got %d (err=%v): %+v", len(edges), err, edges)
	}
}

// --- Scope edge cases ------------------------------------------------------

// TestStage_LateArrivalDoesNotCloseLiveEdge is the Phase-1 gotcha, and the most
// dangerous case in this phase: ingest's historical-insert path reports
// OpUpdate with a non-nil Predecessor yet deliberately does NOT close it,
// because the fact that just landed is OLDER than the one already live. Acting
// on that Predecessor would retire a relation that is still true. The stage
// detects it exactly as ingest.FactOutcome documents:
// Fact.ValidAt.Before(Predecessor.ValidAt).
func TestStage_LateArrivalDoesNotCloseLiveEdge(t *testing.T) {
	ctx := context.Background()
	stage, _, backend := newNameKeyedTestStage(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// The CURRENT truth landed first and is the live head.
	current := factAt("service-a", "owns", "billing-db-v2", "service-a owns billing-db-v2", t0)
	if err := stage.Process(ctx, memory.Episodic{EventID: "ev-1", TenantID: "t1"}, added(current)); err != nil {
		t.Fatalf("Process(ev-1): %v", err)
	}

	// A backfilled, OLDER fact arrives late. The writer reports OpUpdate with
	// `current` as Predecessor — but bounded it at index time instead of
	// closing it.
	late := factAt("service-a", "owns", "billing-db", "service-a owns billing-db", t0.Add(-24*time.Hour))
	if err := stage.Process(ctx, memory.Episodic{EventID: "ev-2", TenantID: "t1"}, superseded(ingest.OpUpdate, late, current)); err != nil {
		t.Fatalf("Process(ev-2): %v", err)
	}

	stillLive, ok := edgeNamed(t, backend, "service-a", "owns", "billing-db-v2")
	if !ok {
		t.Fatal("the live head's edge vanished entirely")
	}
	if !stillLive.Live() {
		t.Fatalf("a LATE-ARRIVING (older) fact closed the edge of the still-live head — it retired a fact that is currently true: %+v", stillLive)
	}
}

// TestStage_PredecessorWithNoEdgeIsNoOp: a predecessor that itself was a
// retraction (empty Object) never produced an edge. Closing must find nothing
// and shrug — not error, and not close some other edge of the same subject.
func TestStage_PredecessorWithNoEdgeIsNoOp(t *testing.T) {
	ctx := context.Background()
	stage, _, backend := newNameKeyedTestStage(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// An unrelated live edge on the same subject, which must survive untouched.
	other := factAt("service-a", "hosts", "api-gw", "service-a hosts api-gw", t0)
	if err := stage.Process(ctx, memory.Episodic{EventID: "ev-0", TenantID: "t1"}, added(other)); err != nil {
		t.Fatalf("Process(ev-0): %v", err)
	}

	retraction := factAt("service-a", "owns", "", "service-a owns: retracted", t0)
	next := factAt("service-a", "owns", "billing-db", "service-a owns billing-db", t0.Add(time.Hour))
	if err := stage.Process(ctx, memory.Episodic{EventID: "ev-1", TenantID: "t1"}, superseded(ingest.OpUpdate, next, retraction)); err != nil {
		t.Fatalf("superseding an edgeless predecessor must not error: %v", err)
	}

	if e, ok := edgeNamed(t, backend, "service-a", "hosts", "api-gw"); !ok || !e.Live() {
		t.Fatalf("an unrelated edge on the same subject was closed; ok=%v edge=%+v", ok, e)
	}
	if e, ok := edgeNamed(t, backend, "service-a", "owns", "billing-db"); !ok || !e.Live() {
		t.Fatalf("the new fact's own edge should be live; ok=%v edge=%+v", ok, e)
	}
}

// TestStage_SupersessionDoesNotInflateMentionCounts: recovering the
// predecessor's entities is a READ. If it went through UpsertMention it would
// bump MentionCount/SourceIDs on entities the event never actually mentioned,
// corrupting the entity-stability metric.
func TestStage_SupersessionDoesNotInflateMentionCounts(t *testing.T) {
	ctx := context.Background()
	stage, store, backend := newNameKeyedTestStage(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	old := factAt("service-a", "owns", "billing-db", "service-a owns billing-db", t0)
	if err := stage.Process(ctx, memory.Episodic{EventID: "ev-1", TenantID: "t1"}, added(old)); err != nil {
		t.Fatalf("Process(ev-1): %v", err)
	}
	staleID := entityIDNamed(t, backend, "billing-db")
	before, _, err := store.GetEntity(ctx, "t1", staleID)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}

	next := factAt("service-a", "owns", "billing-db-v2", "service-a owns billing-db-v2", t0.Add(time.Hour))
	if err := stage.Process(ctx, memory.Episodic{EventID: "ev-2", TenantID: "t1"}, superseded(ingest.OpUpdate, next, old)); err != nil {
		t.Fatalf("Process(ev-2): %v", err)
	}

	after, _, err := store.GetEntity(ctx, "t1", staleID)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if after.MentionCount != before.MentionCount {
		t.Errorf("closing an edge inflated the stale object's MentionCount: %d -> %d (the close must be a READ of the predecessor, not a re-mention)",
			before.MentionCount, after.MentionCount)
	}
	if len(after.SourceIDs) != len(before.SourceIDs) {
		t.Errorf("closing an edge appended a source id to an entity ev-2 never mentioned: %v -> %v", before.SourceIDs, after.SourceIDs)
	}
}

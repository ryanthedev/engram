package worker_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/worker"
)

// TestDW_2_8_CrashBetweenCreateAndCloseSweepConverges: a worker killed after
// indexing the new fact but before closing the predecessor leaves two live
// versions of one chain. The repair sweep completes the close via the
// durable supersedes link (rule a), resumes the incomplete ledger from its
// cached extraction (rule c — the LLM is not re-called), and exactly one
// live head remains.
func TestDW_2_8_CrashBetweenCreateAndCloseSweepConverges(t *testing.T) {
	f := newFakeStore()
	f.ledgerLease = -time.Second // the crashed run's ledger lease is expired
	w, ex := newTestWorker(f, "v1")
	ctx := context.Background()

	mustProcess(t, f, w, event("ev-1", factLine("svc", "owner", "bob", tv0)))
	predID, _ := singleLiveHead(t, f, "svc", "owner")

	crash := errors.New("simulated crash between create and close")
	f.failOnce("update", crash)
	ev := event("ev-2", factLine("svc", "owner", "ana", tv1))
	if _, err := f.Append(ctx, ev); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.ProcessEvent(ctx, ev); !errors.Is(err, crash) {
		t.Fatalf("crashed ProcessEvent error = %v, want the injected crash", err)
	}

	// The documented transient state: two live versions of one chain.
	if n := len(f.liveFacts()); n != 2 {
		t.Fatalf("live facts after crash = %d, want the documented transient 2", n)
	}
	callsBefore := ex.Calls()

	sweeper := &worker.Sweeper{Store: f, Worker: w, Logger: quietLogger}
	rep, err := sweeper.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rep.ClosesCompleted != 1 {
		t.Errorf("sweep closes completed = %d, want 1", rep.ClosesCompleted)
	}
	if rep.LedgersResumed != 1 {
		t.Errorf("sweep ledgers resumed = %d, want 1", rep.LedgersResumed)
	}
	if ex.Calls() != callsBefore {
		t.Errorf("sweep re-called the extractor (%d → %d) — resume must use the cached extraction", callsBefore, ex.Calls())
	}

	pred := f.allFacts()[predID]
	if pred.InvalidAt == nil || !pred.InvalidAt.Equal(tv1) {
		t.Fatalf("predecessor invalid_at = %v, want the successor's valid_at %v", pred.InvalidAt, tv1)
	}
	_, head := singleLiveHead(t, f, "svc", "owner")
	if head.Object != "ana" {
		t.Errorf("live head = %+v, want ana", head)
	}
	if rec, _ := f.eventRow("ev-2"); rec.ProcessedAt == nil {
		t.Fatal("sweep resume must complete the event")
	}
}

// TestDW_2_8_CrashBeforeLedgerClaimOutboxRetries: a worker killed before the
// ledger claim leaves nothing behind but the outbox lease; after it expires
// the event is reclaimed and processed cleanly, with exactly one extraction.
func TestDW_2_8_CrashBeforeLedgerClaimOutboxRetries(t *testing.T) {
	f := newFakeStore()
	w, ex := newTestWorker(f, "v1")
	ctx := context.Background()
	if _, err := f.Append(ctx, event("ev-1", factLine("svc", "owner", "ana", tv1))); err != nil {
		t.Fatalf("Append: %v", err)
	}

	crash := errors.New("simulated crash before ledger claim")
	f.failOnce("claimledger", crash)
	if n, err := w.Tick(ctx); err != nil || n != 1 {
		t.Fatalf("Tick = (%d, %v); event errors are retried, not tick failures", n, err)
	}
	if ex.Calls() != 0 {
		t.Fatalf("extractor called before the ledger claim: %d", ex.Calls())
	}
	if n := len(f.allFacts()); n != 0 {
		t.Fatalf("facts after pre-claim crash = %d, want 0", n)
	}

	time.Sleep(25 * time.Millisecond) // outbox lease (20ms) expires
	if n, err := w.Tick(ctx); err != nil || n != 1 {
		t.Fatalf("retry Tick = (%d, %v), want (1, nil)", n, err)
	}
	if ex.Calls() != 1 {
		t.Errorf("extractor calls = %d, want exactly 1", ex.Calls())
	}
	singleLiveHead(t, f, "svc", "owner")
	if rec, _ := f.eventRow("ev-1"); rec.ProcessedAt == nil || rec.Attempts != 2 {
		t.Fatalf("event = %+v, want processed on the second attempt", rec)
	}
}

// seedLiveFact plants a fact directly (bypassing the worker) for sweep-only
// scenarios.
func seedLiveFact(t *testing.T, f *fakeStore, subject, predicate, object, supersedes string, validAt time.Time) string {
	t.Helper()
	fact := memory.SemanticFact{
		Subject: subject, Predicate: predicate, Object: object,
		Statement:  subject + " " + predicate + " " + object,
		TenantID:   "t1",
		ContentKey: memory.ContentKey("t1", subject, predicate, object),
		Supersedes: supersedes,
		ValidAt:    validAt,
		CreatedAt:  time.Now().UTC(),
	}
	id := memory.FactDocID(fact.ContentKey, fact.ValidAt)
	if err := f.Create(context.Background(), id, fact); err != nil {
		t.Fatalf("seeding fact %s: %v", id, err)
	}
	return id
}

// TestDW_2_5_SweepConvergesDivergentUpdates isolates rule (a'): two
// divergent successors of one predecessor (both live, different content
// keys — the state two racing UPDATE writers leave) converge to a single
// live head, chained in valid-time order.
func TestDW_2_5_SweepConvergesDivergentUpdates(t *testing.T) {
	f := newFakeStore()
	w, _ := newTestWorker(f, "v1")
	ctx := context.Background()

	predID := seedLiveFact(t, f, "svc", "owner", "bob", "", tv0)
	aID := seedLiveFact(t, f, "svc", "owner", "ana", predID, tv1)
	bID := seedLiveFact(t, f, "svc", "owner", "cid", predID, tv2)

	sweeper := &worker.Sweeper{Store: f, Worker: w, Logger: quietLogger}
	rep, err := sweeper.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rep.ClosesCompleted != 1 || rep.SiblingsClosed != 1 {
		t.Errorf("report = %+v, want 1 close completed + 1 sibling closed", rep)
	}

	all := f.allFacts()
	if pred := all[predID]; pred.InvalidAt == nil || !pred.InvalidAt.Equal(tv1) {
		t.Errorf("predecessor invalid_at = %v, want the earliest successor's %v", pred.InvalidAt, tv1)
	}
	if a := all[aID]; a.InvalidAt == nil || !a.InvalidAt.Equal(tv2) {
		t.Errorf("earlier sibling invalid_at = %v, want the later sibling's %v", a.InvalidAt, tv2)
	}
	liveID, _ := singleLiveHead(t, f, "svc", "owner")
	if liveID != bID {
		t.Errorf("surviving head = %s, want the latest-by-valid-time %s", liveID, bID)
	}
}

// TestSweepClosesDuplicateLiveContentKeys isolates rule (b): duplicate live
// copies of one content key keep the earliest and close the rest with empty
// intervals.
func TestSweepClosesDuplicateLiveContentKeys(t *testing.T) {
	f := newFakeStore()
	w, _ := newTestWorker(f, "v1")
	ctx := context.Background()

	keepID := seedLiveFact(t, f, "db", "tier", "gold", "", tv0)
	dupID := seedLiveFact(t, f, "db", "tier", "gold", "", tv1) // same content key, later valid_at

	sweeper := &worker.Sweeper{Store: f, Worker: w, Logger: quietLogger}
	rep, err := sweeper.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rep.DuplicatesClosed != 1 {
		t.Errorf("duplicates closed = %d, want 1", rep.DuplicatesClosed)
	}
	all := f.allFacts()
	if keep := all[keepID]; keep.InvalidAt != nil {
		t.Errorf("earliest duplicate was closed; want it kept live")
	}
	dup := all[dupID]
	if dup.InvalidAt == nil || !dup.InvalidAt.Equal(dup.ValidAt) {
		t.Errorf("duplicate interval = [%v, %v), want the empty interval [v, v)", dup.ValidAt, dup.InvalidAt)
	}
	if keys, _ := f.DuplicateLiveContentKeys(ctx, 10); len(keys) != 0 {
		t.Errorf("duplicate live content keys after sweep = %v, want none", keys)
	}
}

// TestSweepSkipsForeignExtractorVersions: rule (c) must not resume ledger
// entries owned by a different pipeline version.
func TestSweepSkipsForeignExtractorVersions(t *testing.T) {
	f := newFakeStore()
	f.ledgerLease = -time.Second
	ctx := context.Background()

	// A crashed v1 run left an incomplete ledger entry.
	v1, ex1 := newTestWorker(f, "v1")
	crash := errors.New("boom")
	f.failOnce("create", crash)
	ev := event("ev-1", factLine("svc", "owner", "ana", tv1))
	if _, err := f.Append(ctx, ev); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := v1.ProcessEvent(ctx, ev); !errors.Is(err, crash) {
		t.Fatalf("ProcessEvent error = %v, want crash", err)
	}

	// A v2 sweeper must leave it alone.
	v2, ex2 := newTestWorker(f, "v2")
	rep, err := (&worker.Sweeper{Store: f, Worker: v2, Logger: quietLogger}).Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rep.LedgersResumed != 0 {
		t.Errorf("v2 sweep resumed %d v1 ledgers, want 0", rep.LedgersResumed)
	}
	if ex2.Calls() != 0 {
		t.Errorf("v2 extractor calls = %d, want 0", ex2.Calls())
	}

	// The owning version converges it.
	rep, err = (&worker.Sweeper{Store: f, Worker: v1, Logger: quietLogger}).Sweep(ctx)
	if err != nil {
		t.Fatalf("v1 sweep: %v", err)
	}
	if rep.LedgersResumed != 1 {
		t.Errorf("v1 sweep resumed %d, want 1", rep.LedgersResumed)
	}
	if ex1.Calls() != 1 {
		t.Errorf("v1 extractor calls = %d, want 1 (cached extraction)", ex1.Calls())
	}
	singleLiveHead(t, f, "svc", "owner")
}

// TestDW_2_8_CrashWindowLateArrivalSweepStaysDisjoint is the regression for
// review Issue 2, interleaving (a): a worker crashes between creating
// cid@tv2 (supersedes ana@tv1) and closing ana — DW-2.8's documented
// transient — and a late arrival eve@tv1+6h lands INSIDE that window (its
// valid-time predecessor ana is live, so its trim is correctly skipped).
// The sweep's completing close must be neighbor-aware: ana closes at eve's
// valid_at (tv1+6h), NOT at cid's (tv2), or ana ⊇ eve overlap permanently.
func TestDW_2_8_CrashWindowLateArrivalSweepStaysDisjoint(t *testing.T) {
	f := newFakeStore()
	f.ledgerLease = -time.Second
	w, _ := newTestWorker(f, "v1")
	ctx := context.Background()
	tvMid := tv1.Add(6 * time.Hour)

	mustProcess(t, f, w, event("ev-1", factLine("svc", "owner", "ana", tv1)))
	anaID, _ := singleLiveHead(t, f, "svc", "owner")

	crash := errors.New("simulated crash between create and close")
	f.failOnce("update", crash)
	ev2 := event("ev-2", factLine("svc", "owner", "cid", tv2))
	if _, err := f.Append(ctx, ev2); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.ProcessEvent(ctx, ev2); !errors.Is(err, crash) {
		t.Fatalf("crashed ProcessEvent error = %v, want the injected crash", err)
	}

	// Late arrival inside the crash window (ana and cid both live).
	mustProcess(t, f, w, event("ev-3", factLine("svc", "owner", "eve", tvMid)))

	sweeper := &worker.Sweeper{Store: f, Worker: w, Logger: quietLogger}
	rep, err := sweeper.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rep.ClosesCompleted != 1 {
		t.Errorf("sweep report = %+v, want exactly one completed close", rep)
	}

	all := f.allFacts()
	ana := all[anaID]
	if ana.InvalidAt == nil || !ana.InvalidAt.Equal(tvMid) {
		t.Fatalf("ana interval = [%v, %v), want the NEIGHBOR-AWARE close [%v, %v) — not the requested cid bound %v", ana.ValidAt, ana.InvalidAt, tv1, tvMid, tv2)
	}
	var eve memory.SemanticFact
	for _, fc := range all {
		if fc.Object == "eve" {
			eve = fc
		}
	}
	if eve.InvalidAt == nil || !eve.InvalidAt.Equal(tv2) || !eve.ValidAt.Equal(tvMid) {
		t.Fatalf("eve interval = [%v, %v), want [%v, %v)", eve.ValidAt, eve.InvalidAt, tvMid, tv2)
	}
	if _, head := singleLiveHead(t, f, "svc", "owner"); head.Object != "cid" {
		t.Fatalf("live head = %+v, want cid", head)
	}
	assertDisjointChain(t, f, "svc", "owner")
	if rec, _ := f.eventRow("ev-2"); rec.ProcessedAt == nil {
		t.Fatal("sweep rule (c) must complete the crashed event")
	}
}

// TestDW_2_5_DivergenceWindowLateArrivalSweepStaysDisjoint is the regression
// for review Issue 2, interleaving (b): two divergent concurrent UPDATEs of
// one predecessor (ana@tv1 and cid@tv3, both live, both supersede bob@tv0)
// plus a late arrival eve@tv2 between their valid times. Sweep rule (a')'s
// sibling chain-close must be neighbor-aware: ana closes at eve's valid_at
// (tv2), NOT at its sibling cid's (tv3) — the whole timeline converges to
// bob=[tv0,tv1), ana=[tv1,tv2), eve=[tv2,tv3), cid=[tv3,∞).
func TestDW_2_5_DivergenceWindowLateArrivalSweepStaysDisjoint(t *testing.T) {
	f := newFakeStore()
	w, _ := newTestWorker(f, "v1")
	ctx := context.Background()

	bobID := seedLiveFact(t, f, "svc", "owner", "bob", "", tv0)
	anaID := seedLiveFact(t, f, "svc", "owner", "ana", bobID, tv1)
	cidID := seedLiveFact(t, f, "svc", "owner", "cid", bobID, tv3)

	// Late arrival between the divergent successors' valid times.
	mustProcess(t, f, w, event("ev-late", factLine("svc", "owner", "eve", tv2)))

	sweeper := &worker.Sweeper{Store: f, Worker: w, Logger: quietLogger}
	rep, err := sweeper.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rep.ClosesCompleted != 1 || rep.SiblingsClosed != 1 {
		t.Errorf("report = %+v, want 1 close completed + 1 sibling closed", rep)
	}

	all := f.allFacts()
	if bob := all[bobID]; bob.InvalidAt == nil || !bob.InvalidAt.Equal(tv1) {
		t.Errorf("bob interval = [%v, %v), want [%v, %v)", bob.ValidAt, bob.InvalidAt, tv0, tv1)
	}
	if ana := all[anaID]; ana.InvalidAt == nil || !ana.InvalidAt.Equal(tv2) {
		t.Errorf("ana interval = [%v, %v), want the NEIGHBOR-AWARE [%v, %v) — not the sibling bound %v", ana.ValidAt, ana.InvalidAt, tv1, tv2, tv3)
	}
	var eve memory.SemanticFact
	for _, fc := range all {
		if fc.Object == "eve" {
			eve = fc
		}
	}
	if eve.InvalidAt == nil || !eve.InvalidAt.Equal(tv3) || !eve.ValidAt.Equal(tv2) {
		t.Errorf("eve interval = [%v, %v), want [%v, %v)", eve.ValidAt, eve.InvalidAt, tv2, tv3)
	}
	if liveID, _ := singleLiveHead(t, f, "svc", "owner"); liveID != cidID {
		t.Errorf("live head = %s, want cid %s", liveID, cidID)
	}
	assertDisjointChain(t, f, "svc", "owner")
}

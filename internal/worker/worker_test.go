package worker_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/ingest"
	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/worker"
)

var (
	tv0 = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	tv1 = tv0.Add(24 * time.Hour)
	tv2 = tv0.Add(48 * time.Hour)
	tv3 = tv0.Add(72 * time.Hour)
)

// quietLogger keeps expected-failure noise out of test output.
var quietLogger = slog.New(slog.NewTextHandler(discard{}, &slog.HandlerOptions{Level: slog.LevelError + 4}))

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// newTestWorker wires a Worker over the fake store with the deterministic
// extractor/reconciler and test-friendly timings.
func newTestWorker(f *fakeStore, version string) (*worker.Worker, *ingest.RuleExtractor) {
	ex := &ingest.RuleExtractor{}
	cfg := worker.Config{
		ExtractorVersion: version,
		MaxAttempts:      2,
		ClaimLease:       20 * time.Millisecond,
		PollInterval:     5 * time.Millisecond,
	}
	return worker.New(f, ex, ingest.RuleReconciler{}, nil, cfg, quietLogger), ex
}

// event builds an episodic fixture whose Text carries extractor directives.
func event(id, text string) memory.Episodic {
	return memory.Episodic{
		EventID:    id,
		TenantID:   "t1",
		TeamID:     "team-a",
		Scope:      "team",
		Kind:       "test",
		Text:       text,
		OccurredAt: tv0,
		CreatedAt:  time.Now().UTC(),
	}
}

func factLine(s, p, o string, at time.Time) string {
	return fmt.Sprintf("fact: %s | %s | %s @ %s", s, p, o, at.Format(time.RFC3339))
}

// mustProcess appends ev and processes it through w.
func mustProcess(t *testing.T, f *fakeStore, w *worker.Worker, ev memory.Episodic) {
	t.Helper()
	if _, err := f.Append(context.Background(), ev); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.ProcessEvent(context.Background(), ev); err != nil {
		t.Fatalf("ProcessEvent(%s): %v", ev.EventID, err)
	}
}

// singleLiveHead asserts exactly one live fact exists for subject+predicate
// and returns its doc id + fact.
func singleLiveHead(t *testing.T, f *fakeStore, subject, predicate string) (string, memory.SemanticFact) {
	t.Helper()
	var id string
	var found []string
	var head memory.SemanticFact
	for did, fc := range f.liveFacts() {
		if fc.Subject == subject && fc.Predicate == predicate {
			found = append(found, did)
			id, head = did, fc
		}
	}
	if len(found) != 1 {
		t.Fatalf("live heads for %s/%s = %v, want exactly 1", subject, predicate, found)
	}
	return id, head
}

// TestDW_2_1_KillBeforeClaimEventStillProcessed: the append IS the enqueue —
// an event ingested before any worker existed (service killed before the
// claim) is processed once a worker starts.
func TestDW_2_1_KillBeforeClaimEventStillProcessed(t *testing.T) {
	f := newFakeStore()
	ctx := context.Background()
	if _, err := f.Append(ctx, event("ev-1", factLine("svc", "owner", "ana", tv1))); err != nil {
		t.Fatalf("Append: %v", err)
	}

	w, _ := newTestWorker(f, "v1") // "restart": a fresh worker over the durable log
	n, err := w.Tick(ctx)
	if err != nil || n != 1 {
		t.Fatalf("Tick = (%d, %v), want (1, nil)", n, err)
	}
	if rec, _ := f.eventRow("ev-1"); rec.ProcessedAt == nil {
		t.Fatal("event not processed after worker restart")
	}
	singleLiveHead(t, f, "svc", "owner")
}

// TestDW_2_1_AbandonedClaimReclaimedAfterLease: a worker that claims and
// dies (lease held, never completed) loses the event to the next worker
// after lease expiry; attempts count the retries.
func TestDW_2_1_AbandonedClaimReclaimedAfterLease(t *testing.T) {
	f := newFakeStore()
	ctx := context.Background()
	if _, err := f.Append(ctx, event("ev-1", factLine("svc", "owner", "ana", tv1))); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Dead worker: claims, then vanishes.
	claimed, err := f.ClaimBatch(ctx, 10, 20*time.Millisecond)
	if err != nil || len(claimed) != 1 || claimed[0].Attempts != 1 {
		t.Fatalf("first claim = (%+v, %v), want one event attempts=1", claimed, err)
	}

	// While the lease is live nobody can steal it.
	if again, _ := f.ClaimBatch(ctx, 10, time.Minute); len(again) != 0 {
		t.Fatalf("claim during a live lease returned %d events, want 0", len(again))
	}

	time.Sleep(30 * time.Millisecond) // lease expires
	w, _ := newTestWorker(f, "v1")
	if n, err := w.Tick(ctx); err != nil || n != 1 {
		t.Fatalf("Tick after lease expiry = (%d, %v), want (1, nil)", n, err)
	}
	rec, _ := f.eventRow("ev-1")
	if rec.ProcessedAt == nil || rec.Attempts != 2 {
		t.Fatalf("event = processed=%v attempts=%d, want processed attempts=2", rec.ProcessedAt != nil, rec.Attempts)
	}
}

// TestDW_2_1_CrashAfterLedgerCompleteBeforeOutboxComplete closes the last
// crash window: killed between marking the ledger complete and marking the
// outbox event processed, the retry short-circuits at the ledger (no
// re-extraction, no duplicate writes) and finishes the outbox bookkeeping.
func TestDW_2_1_CrashAfterLedgerCompleteBeforeOutboxComplete(t *testing.T) {
	f := newFakeStore()
	w, ex := newTestWorker(f, "v1")
	ctx := context.Background()
	ev := event("ev-1", factLine("svc", "owner", "ana", tv1))
	if _, err := f.Append(ctx, ev); err != nil {
		t.Fatalf("Append: %v", err)
	}

	crash := errors.New("simulated crash before outbox complete")
	f.failOnce("complete", crash)
	if err := w.ProcessEvent(ctx, ev); !errors.Is(err, crash) {
		t.Fatalf("crashed ProcessEvent error = %v, want the injected crash", err)
	}
	if rec, _ := f.eventRow("ev-1"); rec.ProcessedAt != nil {
		t.Fatal("event marked processed despite the crash")
	}

	if err := w.ProcessEvent(ctx, ev); err != nil { // outbox retry
		t.Fatalf("retry ProcessEvent: %v", err)
	}
	if ex.Calls() != 1 {
		t.Errorf("extractor calls = %d, want 1 (ledger short-circuit)", ex.Calls())
	}
	if n := len(f.allFacts()); n != 1 {
		t.Errorf("facts = %d, want 1 (no duplicate writes on retry)", n)
	}
	if rec, _ := f.eventRow("ev-1"); rec.ProcessedAt == nil {
		t.Fatal("event not completed after retry")
	}
}

// TestDW_2_2_WorkerExtractsAndRetrievesCandidates: processing an event
// extracts facts and retrieves top-k candidates from semantic — and the
// candidates are actually consumed (the pre-existing head gets superseded).
func TestDW_2_2_WorkerExtractsAndRetrievesCandidates(t *testing.T) {
	f := newFakeStore()
	w, ex := newTestWorker(f, "v1")
	mustProcess(t, f, w, event("ev-seed", factLine("svc", "owner", "bob", tv0)))
	predID, _ := singleLiveHead(t, f, "svc", "owner")

	f.mu.Lock()
	f.candidateCalls, f.lastCandidateK = 0, 0
	f.mu.Unlock()

	mustProcess(t, f, w, event("ev-2", factLine("svc", "owner", "ana", tv1)))
	if ex.Calls() != 2 {
		t.Errorf("extractor calls = %d, want 2", ex.Calls())
	}
	f.mu.Lock()
	calls, k := f.candidateCalls, f.lastCandidateK
	f.mu.Unlock()
	if calls < 1 {
		t.Fatal("worker never retrieved candidates from semantic")
	}
	if k != 10 {
		t.Errorf("candidate top-k = %d, want the default 10", k)
	}
	_, head := singleLiveHead(t, f, "svc", "owner")
	if head.Supersedes != predID {
		t.Errorf("new head supersedes %q, want the retrieved candidate %q", head.Supersedes, predID)
	}
}

// TestDW_2_3_UpdateWritesNewFactAndClosesPrior is the bi-temporal UPDATE
// contract: a new SemanticFact is indexed, the prior fact's invalid_at is
// set to the new fact's valid_at with the invalidated_tx_at audit stamp, all
// four timestamps are present, and nothing is ever hard-deleted.
func TestDW_2_3_UpdateWritesNewFactAndClosesPrior(t *testing.T) {
	f := newFakeStore()
	w, _ := newTestWorker(f, "v1")
	mustProcess(t, f, w, event("ev-1", factLine("svc", "owner", "bob", tv0)))
	oldID, _ := singleLiveHead(t, f, "svc", "owner")

	mustProcess(t, f, w, event("ev-2", factLine("svc", "owner", "ana", tv1)))

	all := f.allFacts()
	if len(all) != 2 {
		t.Fatalf("total facts = %d, want 2 (never hard-delete)", len(all))
	}
	old := all[oldID]
	if old.InvalidAt == nil || !old.InvalidAt.Equal(tv1) {
		t.Fatalf("prior invalid_at = %v, want new.valid_at %v", old.InvalidAt, tv1)
	}
	if old.InvalidatedTxAt == nil {
		t.Error("prior invalidated_tx_at missing — the sanctioned mutation must be audit-stamped")
	}
	if old.CreatedAt.IsZero() || old.ValidAt.IsZero() {
		t.Error("prior fact missing valid_at/created_at")
	}
	if old.ExpiredAt != nil {
		t.Error("prior expired_at set — a valid-time close is not a record retraction")
	}

	newID, head := singleLiveHead(t, f, "svc", "owner")
	if head.Object != "ana" || !head.ValidAt.Equal(tv1) || head.InvalidAt != nil || head.CreatedAt.IsZero() {
		t.Errorf("new head = %+v, want live ana @ %v with created_at", head, tv1)
	}
	if head.Supersedes != oldID {
		t.Errorf("supersedes = %q, want %q", head.Supersedes, oldID)
	}
	if newID != memory.FactDocID(head.ContentKey, head.ValidAt) {
		t.Errorf("doc id %q is not content-addressed (D11)", newID)
	}
}

// TestDW_2_3_InvalidateRetractsWithoutSuccessorTruth: a retraction closes
// the head and indexes the retraction record (empty object) without
// asserting a new truth.
func TestDW_2_3_InvalidateRetractsWithoutSuccessorTruth(t *testing.T) {
	f := newFakeStore()
	w, _ := newTestWorker(f, "v1")
	mustProcess(t, f, w, event("ev-1", factLine("svc", "deprecated", "false", tv0)))
	oldID, _ := singleLiveHead(t, f, "svc", "deprecated")

	mustProcess(t, f, w, event("ev-2", fmt.Sprintf("retract: svc | deprecated @ %s", tv1.Format(time.RFC3339))))

	old := f.allFacts()[oldID]
	if old.InvalidAt == nil || !old.InvalidAt.Equal(tv1) {
		t.Fatalf("retracted fact invalid_at = %v, want %v", old.InvalidAt, tv1)
	}
	_, head := singleLiveHead(t, f, "svc", "deprecated")
	if head.Object != "" || head.Supersedes != oldID {
		t.Errorf("retraction record = %+v, want empty object superseding %s", head, oldID)
	}
}

// TestDW_2_4_ReplayShortCircuitsAtLedger: reprocessing the same event_id
// stops at the completed ledger entry — the LLM is not re-called and no new
// facts appear.
func TestDW_2_4_ReplayShortCircuitsAtLedger(t *testing.T) {
	f := newFakeStore()
	w, ex := newTestWorker(f, "v1")
	ev := event("ev-1", factLine("svc", "owner", "ana", tv1))
	mustProcess(t, f, w, ev)

	if err := w.ProcessEvent(context.Background(), ev); err != nil {
		t.Fatalf("replay ProcessEvent: %v", err)
	}
	if ex.Calls() != 1 {
		t.Fatalf("extractor calls after replay = %d, want 1 (ledger short-circuit)", ex.Calls())
	}
	if n := len(f.allFacts()); n != 1 {
		t.Fatalf("facts after replay = %d, want 1", n)
	}
}

// TestDW_2_4_CrashResumeUsesCachedExtraction: a worker that crashed after
// caching the extraction but before writing resumes from the ledger cache —
// verified by extractor call count staying at 1 (idempotency is mechanical,
// not LLM-behavioral).
func TestDW_2_4_CrashResumeUsesCachedExtraction(t *testing.T) {
	f := newFakeStore()
	w, ex := newTestWorker(f, "v1")
	ev := event("ev-1", factLine("svc", "owner", "ana", tv1))
	if _, err := f.Append(context.Background(), ev); err != nil {
		t.Fatalf("Append: %v", err)
	}

	crash := errors.New("simulated crash at semantic write")
	f.failOnce("create", crash)
	if err := w.ProcessEvent(context.Background(), ev); !errors.Is(err, crash) {
		t.Fatalf("crashed ProcessEvent error = %v, want the injected crash", err)
	}
	if ex.Calls() != 1 {
		t.Fatalf("extractor calls after crash = %d, want 1", ex.Calls())
	}

	if err := w.ProcessEvent(context.Background(), ev); err != nil { // restart
		t.Fatalf("resumed ProcessEvent: %v", err)
	}
	if ex.Calls() != 1 {
		t.Fatalf("extractor calls after resume = %d, want 1 — the cached extraction must be used", ex.Calls())
	}
	singleLiveHead(t, f, "svc", "owner")
	if rec, _ := f.eventRow("ev-1"); rec.ProcessedAt == nil {
		t.Fatal("event not completed after resume")
	}
}

// TestDW_2_4_ExtractorVersionBumpNoDuplicateLiveFacts: bumping
// extractor_version deliberately re-extracts under a new ledger identity,
// but content-key dedup (the content-addressed doc id + 409 → re-reconcile
// → NOOP) prevents duplicate live facts.
func TestDW_2_4_ExtractorVersionBumpNoDuplicateLiveFacts(t *testing.T) {
	f := newFakeStore()
	w1, _ := newTestWorker(f, "v1")
	ev := event("ev-1", factLine("svc", "owner", "ana", tv1))
	mustProcess(t, f, w1, ev)

	w2, ex2 := newTestWorker(f, "v2")
	if err := w2.ProcessEvent(context.Background(), ev); err != nil {
		t.Fatalf("v2 reprocess: %v", err)
	}
	if ex2.Calls() != 1 {
		t.Fatalf("v2 extractor calls = %d, want 1 (a version bump re-extracts)", ex2.Calls())
	}
	if n := len(f.allFacts()); n != 1 {
		t.Fatalf("facts after version bump = %d, want 1 (no duplicates, live or otherwise)", n)
	}
	singleLiveHead(t, f, "svc", "owner")
	keys, _ := f.DuplicateLiveContentKeys(context.Background(), 10)
	if len(keys) != 0 {
		t.Fatalf("duplicate live content keys = %v, want none", keys)
	}
}

// TestDW_2_5_DuplicateAddConflictReReconciles pins the duplicate-ADD race
// deterministically. The 409 window is real concurrency: the worker's
// candidate read (search + realtime get) finds nothing, and a concurrent
// identical extraction lands the same content-addressed doc id before this
// worker's create — injected here between reconcile and create. The create
// 409s, the re-read + re-reconcile sees the winner's doc, and the decision
// lands NOOP: one fact, no duplicates.
func TestDW_2_5_DuplicateAddConflictReReconciles(t *testing.T) {
	f := newFakeStore()
	w, _ := newTestWorker(f, "v1")

	// The concurrent winner's fact: identical triple + valid_at ⇒ identical
	// content-addressed doc id (D11).
	winner := memory.SemanticFact{
		Subject: "svc", Predicate: "owner", Object: "ana",
		Statement: "svc owner ana", TenantID: "t1",
		ContentKey: memory.ContentKey("t1", "svc", "owner", "ana"),
		ValidAt:    tv1, CreatedAt: time.Now().UTC(),
	}
	docID := memory.FactDocID(winner.ContentKey, winner.ValidAt)
	f.hookOnce("create", func() {
		f.facts[docID] = &factRow{fact: winner, seqNo: 1, primaryTerm: 1}
	})

	mustProcess(t, f, w, event("ev-1", factLine("svc", "owner", "ana", tv1)))
	if w.Metrics.CreateConflicts.Load() != 1 {
		t.Errorf("create conflicts = %d, want 1 (the 409 path)", w.Metrics.CreateConflicts.Load())
	}
	if n := len(f.allFacts()); n != 1 {
		t.Fatalf("facts = %d, want 1", n)
	}
	singleLiveHead(t, f, "svc", "owner")
}

// TestDW_2_5_GuardedCloseConflictNoLostUpdate pins the overlapping-close
// race deterministically: a concurrent worker closes the predecessor between
// this worker's read and its guarded write. The guard loses (409), the
// re-read sees the fact closed, and the winner's close is preserved — no
// lost update, no blind overwrite.
func TestDW_2_5_GuardedCloseConflictNoLostUpdate(t *testing.T) {
	f := newFakeStore()
	w, _ := newTestWorker(f, "v1")
	mustProcess(t, f, w, event("ev-1", factLine("svc", "owner", "bob", tv0)))
	predID, _ := singleLiveHead(t, f, "svc", "owner")

	winnerClose := tv0.Add(time.Hour)
	f.hookOnce("update", func() { f.closeFactDirectly(predID, winnerClose) })

	mustProcess(t, f, w, event("ev-2", factLine("svc", "owner", "ana", tv1)))
	if w.Metrics.CloseConflicts.Load() != 1 {
		t.Errorf("close conflicts = %d, want 1", w.Metrics.CloseConflicts.Load())
	}
	pred := f.allFacts()[predID]
	if pred.InvalidAt == nil || !pred.InvalidAt.Equal(winnerClose) {
		t.Fatalf("predecessor invalid_at = %v, want the concurrent winner's %v preserved", pred.InvalidAt, winnerClose)
	}
	if rec, _ := f.eventRow("ev-2"); rec.ProcessedAt == nil {
		t.Fatal("losing worker must still complete its event")
	}
}

// TestDW_2_5_ParallelWorkersConverge is the concurrency test: N parallel
// workers process duplicate ADDs and divergent UPDATEs of one predecessor
// concurrently; after the repair sweep there are no duplicate live content
// keys, the predecessor is closed exactly once (no lost update), and exactly
// one live head remains.
func TestDW_2_5_ParallelWorkersConverge(t *testing.T) {
	f := newFakeStore()
	f.ledgerLease = -time.Second // sweep may resume anything unfinished
	seedWorker, _ := newTestWorker(f, "v1")
	mustProcess(t, f, seedWorker, event("ev-seed", factLine("svc", "owner", "bob", tv0)))
	predID, _ := singleLiveHead(t, f, "svc", "owner")

	// 4 duplicate ADDs of one identical fact + 4 divergent UPDATEs of the
	// seeded predecessor, all racing on their own workers.
	var events []memory.Episodic
	for i := 0; i < 4; i++ {
		events = append(events, event(fmt.Sprintf("ev-dup-%d", i), factLine("db", "tier", "gold", tv1)))
	}
	for i := 0; i < 4; i++ {
		events = append(events, event(fmt.Sprintf("ev-upd-%d", i), factLine("svc", "owner", fmt.Sprintf("eng-%d", i), tv1.Add(time.Duration(i)*time.Hour))))
	}
	ctx := context.Background()
	for _, ev := range events {
		if _, err := f.Append(ctx, ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	var wg sync.WaitGroup
	workers := make([]*worker.Worker, len(events))
	for i := range events {
		workers[i], _ = newTestWorker(f, "v1")
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Convergence may need a bounded retry (as the outbox provides
			// in production): losing every optimistic race in one pass is
			// legal, abandoning the event is not.
			for attempt := 0; attempt < 5; attempt++ {
				if err := workers[i].ProcessEvent(ctx, events[i]); err == nil {
					return
				}
			}
			t.Errorf("event %s never settled", events[i].EventID)
		}(i)
	}
	wg.Wait()

	sweeper := &worker.Sweeper{Store: f, Worker: seedWorker, Logger: quietLogger}
	for i := 0; i < 3; i++ { // the sweep is convergent, not single-pass
		if _, err := sweeper.Sweep(ctx); err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
	}

	if keys, _ := f.DuplicateLiveContentKeys(ctx, 10); len(keys) != 0 {
		t.Fatalf("duplicate live content keys after sweep = %v, want none", keys)
	}
	pred := f.allFacts()[predID]
	if pred.InvalidAt == nil {
		t.Fatal("predecessor still live — a close was lost")
	}
	_, head := singleLiveHead(t, f, "svc", "owner")
	if !strings.HasPrefix(head.Object, "eng-") {
		t.Errorf("surviving head = %+v, want one of the divergent updates", head)
	}
	singleLiveHead(t, f, "db", "tier")
}

// TestDW_2_7_ContradictoryBatchResolvesDeterministically: one event
// asserting two contradictory facts resolves by valid-time order regardless
// of extraction order — run twice (directive order flipped), identical final
// state.
func TestDW_2_7_ContradictoryBatchResolvesDeterministically(t *testing.T) {
	lineA := factLine("svc", "owner", "ana", tv1)
	lineB := factLine("svc", "owner", "bob", tv2)

	type outcome struct {
		headObject string
		headValid  time.Time
		closedAt   time.Time
		total      int
	}
	run := func(text string) outcome {
		f := newFakeStore()
		w, _ := newTestWorker(f, "v1")
		mustProcess(t, f, w, event("ev-1", text))
		var out outcome
		out.total = len(f.allFacts())
		for _, fc := range f.allFacts() {
			if fc.InvalidAt == nil {
				out.headObject, out.headValid = fc.Object, fc.ValidAt
			} else {
				out.closedAt = *fc.InvalidAt
			}
		}
		return out
	}

	first := run(lineA + "\n" + lineB)
	second := run(lineB + "\n" + lineA)
	if first != second {
		t.Fatalf("contradictory batch resolved differently by input order:\n%+v\n%+v", first, second)
	}
	if first.headObject != "bob" || !first.headValid.Equal(tv2) {
		t.Errorf("head = %s @ %v, want the later-by-valid-time bob @ %v", first.headObject, first.headValid, tv2)
	}
	if !first.closedAt.Equal(tv2) {
		t.Errorf("earlier fact closed at %v, want the later fact's valid_at %v", first.closedAt, tv2)
	}
	if first.total != 2 {
		t.Errorf("total facts = %d, want 2", first.total)
	}
}

// TestDW_2_7_MalformedExtractionRejectedNotIndexed: malformed LLM output
// never indexes anything; the event retries via the lease and dead-letters
// after MaxAttempts.
func TestDW_2_7_MalformedExtractionRejectedNotIndexed(t *testing.T) {
	f := newFakeStore()
	f.ledgerLease = -time.Second // crashed claims are immediately adoptable
	w, ex := newTestWorker(f, "v1")
	ctx := context.Background()
	if _, err := f.Append(ctx, event("ev-bad", "!malformed")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	for i := 0; i < 10; i++ { // MaxAttempts=2 → two failures, then dead-letter
		if _, err := w.Tick(ctx); err != nil {
			t.Fatalf("Tick %d: %v", i, err)
		}
		if rec, _ := f.eventRow("ev-bad"); rec.DeadLettered {
			break
		}
		time.Sleep(25 * time.Millisecond) // outlive the claim lease
	}

	rec, _ := f.eventRow("ev-bad")
	if !rec.DeadLettered || !strings.Contains(rec.DeadLetterReason, "attempts exhausted") {
		t.Fatalf("event = %+v, want dead-lettered with an attempts reason", rec)
	}
	if rec.ProcessedAt != nil {
		t.Error("a dead-lettered event must not be marked processed")
	}
	if n := len(f.allFacts()); n != 0 {
		t.Fatalf("facts indexed from malformed extraction = %d, want 0", n)
	}
	if ex.Calls() != 2 {
		t.Errorf("extraction attempts = %d, want MaxAttempts=2", ex.Calls())
	}
	if w.Metrics.EventsDeadLettered.Load() != 1 {
		t.Errorf("dead-letter metric = %d, want 1", w.Metrics.EventsDeadLettered.Load())
	}
}

// TestFactFreeEventCompletesWithoutIndexing: an event whose extraction is
// legitimately empty (ErrNoFacts) completes — rejected from indexing but not
// poisoned into retries.
func TestFactFreeEventCompletesWithoutIndexing(t *testing.T) {
	f := newFakeStore()
	w, _ := newTestWorker(f, "v1")
	mustProcess(t, f, w, event("ev-empty", "just some prose with no facts"))
	if n := len(f.allFacts()); n != 0 {
		t.Fatalf("facts = %d, want 0", n)
	}
	if rec, _ := f.eventRow("ev-empty"); rec.ProcessedAt == nil {
		t.Fatal("fact-free event must complete")
	}
}

// assertDisjointChain asserts pairwise non-overlap of valid-time intervals
// across ALL versions — live and closed — of one subject+predicate chain
// (the bi-temporal invariant D10's neighbor-aware insertion must preserve:
// no point in valid time is covered by two records). Open intervals extend
// to +inf; empty intervals [v, v) cover nothing and never overlap.
func assertDisjointChain(t *testing.T, f *fakeStore, subject, predicate string) {
	t.Helper()
	type span struct {
		id         string
		start, end time.Time
		open       bool
	}
	var spans []span
	for id, fc := range f.allFacts() {
		if fc.Subject != subject || fc.Predicate != predicate || fc.ExpiredAt != nil {
			continue
		}
		sp := span{id: id, start: fc.ValidAt, open: fc.InvalidAt == nil}
		if !sp.open {
			sp.end = *fc.InvalidAt
			if sp.end.Before(sp.start) {
				t.Errorf("inverted interval on %s: [%v, %v)", id, sp.start, sp.end)
			}
		}
		spans = append(spans, sp)
	}
	for i := 0; i < len(spans); i++ {
		for j := i + 1; j < len(spans); j++ {
			a, b := spans[i], spans[j]
			aBeforeB := !a.open && !a.end.After(b.start) // a ends at/before b starts
			bBeforeA := !b.open && !b.end.After(a.start)
			aEmpty := !a.open && a.start.Equal(a.end)
			bEmpty := !b.open && b.start.Equal(b.end)
			if !aBeforeB && !bBeforeA && !aEmpty && !bEmpty {
				t.Errorf("overlapping valid-time intervals: %s [%v, %v open=%v) and %s [%v, %v open=%v)",
					a.id, a.start, a.end, a.open, b.id, b.start, b.end, b.open)
			}
		}
	}
}

// TestDW_2_8_LateArrivalMultiHopChainBoundsAtTrueNeighbor is the regression
// for the review's Issue 1: with a chain of ≥2 prior versions —
// bob=[tv0,tv1) closed, ana=[tv1,tv2) closed, cid=[tv2,∞) live — a late
// arrival eve@tv0+12h must be bounded by its TRUE nearest valid-time
// successor (ana → invalid_at = tv1), NOT the live head (cid → tv2, which
// overlaps ana), and the containing closed neighbor bob must be trimmed to
// [tv0, tv0+12h) so the whole chain stays pairwise disjoint.
func TestDW_2_8_LateArrivalMultiHopChainBoundsAtTrueNeighbor(t *testing.T) {
	f := newFakeStore()
	w, _ := newTestWorker(f, "v1")
	tvMid := tv0.Add(12 * time.Hour)

	mustProcess(t, f, w, event("ev-1", factLine("svc", "owner", "bob", tv0)))
	bobID, _ := singleLiveHead(t, f, "svc", "owner")
	mustProcess(t, f, w, event("ev-2", factLine("svc", "owner", "ana", tv1)))
	anaID, _ := singleLiveHead(t, f, "svc", "owner")
	mustProcess(t, f, w, event("ev-3", factLine("svc", "owner", "cid", tv2)))
	cidID, _ := singleLiveHead(t, f, "svc", "owner")

	bobTxBefore := f.allFacts()[bobID].InvalidatedTxAt

	mustProcess(t, f, w, event("ev-4", factLine("svc", "owner", "eve", tvMid))) // late arrival

	all := f.allFacts()
	var eveID string
	for id, fc := range all {
		if fc.Object == "eve" {
			eveID = id
		}
	}
	eve := all[eveID]
	if eve.InvalidAt == nil || !eve.InvalidAt.Equal(tv1) {
		t.Fatalf("eve interval = [%v, %v), want bounded by the TRUE successor ana: [%v, %v)", eve.ValidAt, eve.InvalidAt, tvMid, tv1)
	}
	if eve.Supersedes != "" {
		t.Error("late arrival must not carry a supersedes link")
	}
	bob := all[bobID]
	if bob.InvalidAt == nil || !bob.InvalidAt.Equal(tvMid) {
		t.Fatalf("bob interval = [%v, %v), want trimmed to [%v, %v) by the insertion", bob.ValidAt, bob.InvalidAt, tv0, tvMid)
	}
	if bob.InvalidatedTxAt == nil || (bobTxBefore != nil && !bob.InvalidatedTxAt.After(*bobTxBefore)) {
		t.Error("bob's trim must re-stamp invalidated_tx_at (the audited sanctioned mutation)")
	}
	if ana := all[anaID]; ana.InvalidAt == nil || !ana.InvalidAt.Equal(tv2) || !ana.ValidAt.Equal(tv1) {
		t.Fatalf("ana interval = [%v, %v), must stay untouched at [%v, %v)", ana.ValidAt, ana.InvalidAt, tv1, tv2)
	}
	if cid := all[cidID]; cid.InvalidAt != nil {
		t.Fatal("live head cid must stay untouched by a late arrival")
	}
	if liveID, _ := singleLiveHead(t, f, "svc", "owner"); liveID != cidID {
		t.Fatalf("live head = %s, want cid %s", liveID, cidID)
	}
	assertDisjointChain(t, f, "svc", "owner")

	// The converged state needs no repair.
	rep, err := (&worker.Sweeper{Store: f, Worker: w, Logger: quietLogger}).Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rep != (worker.SweepReport{}) {
		t.Errorf("sweep after the insertion converged %+v, want nothing to do", rep)
	}
	assertDisjointChain(t, f, "svc", "owner")
}

// TestDW_2_8_LateArrivalInBatchPairStaysDisjoint: two historical facts in
// ONE batch insert disjointly — the second late arrival's true predecessor
// is the first (written moments ago, realtime-merged), whose fresh bound is
// trimmed accordingly: x=[tv0,tv1), y=[tv1,tv3) under a live head @tv3.
func TestDW_2_8_LateArrivalInBatchPairStaysDisjoint(t *testing.T) {
	f := newFakeStore()
	w, _ := newTestWorker(f, "v1")

	mustProcess(t, f, w, event("ev-1", factLine("svc", "owner", "zed", tv3)))
	headID, _ := singleLiveHead(t, f, "svc", "owner")

	mustProcess(t, f, w, event("ev-2", factLine("svc", "owner", "x", tv0)+"\n"+factLine("svc", "owner", "y", tv1)))

	all := f.allFacts()
	byObject := map[string]memory.SemanticFact{}
	for _, fc := range all {
		byObject[fc.Object] = fc
	}
	x, y := byObject["x"], byObject["y"]
	if x.InvalidAt == nil || !x.InvalidAt.Equal(tv1) {
		t.Fatalf("x interval = [%v, %v), want [%v, %v) — trimmed by its in-batch successor", x.ValidAt, x.InvalidAt, tv0, tv1)
	}
	if y.InvalidAt == nil || !y.InvalidAt.Equal(tv3) {
		t.Fatalf("y interval = [%v, %v), want [%v, %v)", y.ValidAt, y.InvalidAt, tv1, tv3)
	}
	if head := all[headID]; head.InvalidAt != nil {
		t.Fatal("live head must stay untouched")
	}
	assertDisjointChain(t, f, "svc", "owner")
}

// TestDW_2_8_LateArrivalCrashBetweenTrimAndCreateConverges: the historical
// path trims BEFORE creating precisely so a crash between the two leaves a
// valid-time GAP (no query ever sees two truths), never a permanent overlap
// — and the cached ledger extraction fills the gap on resume.
func TestDW_2_8_LateArrivalCrashBetweenTrimAndCreateConverges(t *testing.T) {
	f := newFakeStore()
	w, ex := newTestWorker(f, "v1")
	ctx := context.Background()
	tvMid := tv0.Add(12 * time.Hour)

	mustProcess(t, f, w, event("ev-1", factLine("svc", "owner", "bob", tv0)))
	bobID, _ := singleLiveHead(t, f, "svc", "owner")
	mustProcess(t, f, w, event("ev-2", factLine("svc", "owner", "ana", tv1)))

	crash := errors.New("simulated crash between trim and create")
	f.failOnce("create", crash)
	late := event("ev-3", factLine("svc", "owner", "eve", tvMid))
	if _, err := f.Append(ctx, late); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.ProcessEvent(ctx, late); !errors.Is(err, crash) {
		t.Fatalf("crashed ProcessEvent error = %v, want the injected crash", err)
	}

	// Crash state: bob already trimmed, eve missing — a gap, never an overlap.
	if bob := f.allFacts()[bobID]; bob.InvalidAt == nil || !bob.InvalidAt.Equal(tvMid) {
		t.Fatalf("bob after crash = [%v, %v), want trimmed to %v before the create", bob.ValidAt, bob.InvalidAt, tvMid)
	}
	assertDisjointChain(t, f, "svc", "owner")

	if err := w.ProcessEvent(ctx, late); err != nil { // resume (outbox retry)
		t.Fatalf("resumed ProcessEvent: %v", err)
	}
	if ex.Calls() != 3 {
		t.Errorf("extractor calls = %d, want 3 (resume uses the cached extraction)", ex.Calls())
	}
	var eve memory.SemanticFact
	for _, fc := range f.allFacts() {
		if fc.Object == "eve" {
			eve = fc
		}
	}
	if eve.InvalidAt == nil || !eve.InvalidAt.Equal(tv1) || !eve.ValidAt.Equal(tvMid) {
		t.Fatalf("eve after resume = [%v, %v), want [%v, %v)", eve.ValidAt, eve.InvalidAt, tvMid, tv1)
	}
	assertDisjointChain(t, f, "svc", "owner")
	singleLiveHead(t, f, "svc", "owner")
}

// TestDW_2_8_LateArrivalBoundedIntervalPredecessorUntouched: a historical
// fact (valid before the live head) inserts with a bounded interval
// [its valid_at, head.valid_at), carries NO supersedes link, and leaves the
// predecessor untouched — and the sweep has nothing to converge afterwards.
func TestDW_2_8_LateArrivalBoundedIntervalPredecessorUntouched(t *testing.T) {
	f := newFakeStore()
	w, _ := newTestWorker(f, "v1")
	mustProcess(t, f, w, event("ev-1", factLine("svc", "owner", "ana", tv2)))
	headID, _ := singleLiveHead(t, f, "svc", "owner")
	seqBefore := f.factSeqNo(headID)

	mustProcess(t, f, w, event("ev-2", factLine("svc", "owner", "bob", tv0))) // historical

	head := f.allFacts()[headID]
	if head.InvalidAt != nil || f.factSeqNo(headID) != seqBefore {
		t.Fatalf("predecessor touched by a late arrival: invalid_at=%v seq %d→%d", head.InvalidAt, seqBefore, f.factSeqNo(headID))
	}
	var hist memory.SemanticFact
	for id, fc := range f.allFacts() {
		if id != headID {
			hist = fc
		}
	}
	if hist.InvalidAt == nil || !hist.InvalidAt.Equal(tv2) || !hist.ValidAt.Equal(tv0) {
		t.Fatalf("historical fact interval = [%v, %v), want [%v, %v)", hist.ValidAt, hist.InvalidAt, tv0, tv2)
	}
	if hist.Supersedes != "" {
		t.Fatal("late arrival must not carry a supersedes link (the sweep would close the predecessor)")
	}
	if hist.InvalidAt.Before(hist.ValidAt) {
		t.Fatal("inverted interval")
	}

	sweeper := &worker.Sweeper{Store: f, Worker: w, Logger: quietLogger}
	rep, err := sweeper.Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rep != (worker.SweepReport{}) {
		t.Errorf("sweep after a late arrival converged %+v, want nothing to do", rep)
	}
	if fc := f.allFacts()[headID]; fc.InvalidAt != nil {
		t.Fatal("sweep closed the predecessor of a late arrival")
	}
}

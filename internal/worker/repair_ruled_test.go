package worker_test

import (
	"context"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/worker"
)

// closedFact builds a valid-time-closed semantic fact row for chain
// svc/owner: interval [validAt, invalidAt), live on the transaction axis.
func closedFact(object string, validAt, invalidAt time.Time) memory.SemanticFact {
	inv := invalidAt
	now := time.Now().UTC()
	return memory.SemanticFact{
		Subject:    "svc",
		Predicate:  "owner",
		Object:     object,
		Statement:  "svc owner " + object,
		TenantID:   "t1",
		ContentKey: memory.ContentKey("t1", "svc", "owner", object),
		ValidAt:    validAt,
		InvalidAt:  &inv,
		CreatedAt:  now,
	}
}

// liveFact builds a live (open valid-time) fact row for chain svc/owner.
func liveFact(object string, validAt time.Time) memory.SemanticFact {
	return memory.SemanticFact{
		Subject:    "svc",
		Predicate:  "owner",
		Object:     object,
		Statement:  "svc owner " + object,
		TenantID:   "t1",
		ContentKey: memory.ContentKey("t1", "svc", "owner", object),
		ValidAt:    validAt,
		CreatedAt:  time.Now().UTC(),
	}
}

// TestDW_RuleD_ClosedClosedOverlapTrimmed is the direct regression for the
// Phase-2 review's Issue 3: two per-document-OCC closes race into write skew
// and leave two closed records covering the same valid-time span —
// ana=[tv1, tv2) ⊇ eve=[tv1+6h, tv2), with cid live @tv2. No prior sweep rule
// examines closed-closed overlaps, so this state was permanent. Rule (d)
// trims the earlier record (ana) down to its nearest successor (eve@tv1+6h),
// converging the chain to pairwise-disjoint intervals. The state is seeded
// directly because the review adjudicated it unreachable through any sequence
// of protocol steps (it requires true read-before-write simultaneity).
func TestDW_RuleD_ClosedClosedOverlapTrimmed(t *testing.T) {
	f := newFakeStore()
	w, _ := newTestWorker(f, "v1")
	ctx := context.Background()
	tvMid := tv1.Add(6 * time.Hour)

	f.seedFact("ana", closedFact("ana", tv1, tv2))   // overlaps eve
	f.seedFact("eve", closedFact("eve", tvMid, tv2)) // late arrival, closed at tv2
	f.seedFact("cid", liveFact("cid", tv2))          // live head

	// Precondition: the chain is genuinely overlapping before the sweep.
	assertOverlapExists(t, f, "svc", "owner")

	rep, err := (&worker.Sweeper{Store: f, Worker: w, Logger: quietLogger}).Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rep.OverlapsTrimmed != 1 {
		t.Errorf("OverlapsTrimmed = %d, want 1", rep.OverlapsTrimmed)
	}

	// ana must now end exactly at eve's valid_at; the chain is disjoint.
	ana := f.allFacts()["ana"]
	if ana.InvalidAt == nil || !ana.InvalidAt.Equal(tvMid) {
		t.Fatalf("ana.invalid_at = %v, want the successor's valid_at %v", ana.InvalidAt, tvMid)
	}
	if ana.InvalidatedTxAt == nil {
		t.Error("trim must re-stamp invalidated_tx_at (audit of the in-place mutation)")
	}
	assertDisjointChain(t, f, "svc", "owner")
	_, head := singleLiveHead(t, f, "svc", "owner")
	if head.Object != "cid" {
		t.Errorf("live head = %q, want cid (rule d must never touch the live head)", head.Object)
	}

	// Idempotent: a second sweep is a no-op (monotone trim already within bound).
	rep2, err := (&worker.Sweeper{Store: f, Worker: w, Logger: quietLogger}).Sweep(ctx)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if rep2.OverlapsTrimmed != 0 {
		t.Errorf("second sweep OverlapsTrimmed = %d, want 0 (already converged)", rep2.OverlapsTrimmed)
	}
}

// TestDW_RuleD_DivergenceWindowChainConverges reproduces the review's
// divergence variant: an earlier record closed far past several later
// versions (old=[tv0, tv3) closed) with two later closed records and a live
// head between tv0 and tv3. Rule (d) walks the chain and trims every closed
// record to its nearest successor, so a multi-record overlap collapses to a
// disjoint timeline in one pass.
func TestDW_RuleD_DivergenceWindowChainConverges(t *testing.T) {
	f := newFakeStore()
	w, _ := newTestWorker(f, "v1")
	ctx := context.Background()

	f.seedFact("old", closedFact("old", tv0, tv3)) // overlaps ana AND eve
	f.seedFact("ana", closedFact("ana", tv1, tv3)) // overlaps eve
	f.seedFact("eve", closedFact("eve", tv2, tv3)) // overlaps live head
	f.seedFact("cid", liveFact("cid", tv3))        // live head

	rep, err := (&worker.Sweeper{Store: f, Worker: w, Logger: quietLogger}).Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// old and ana each overlap a later record and are trimmed to their nearest
	// successor; eve=[tv2, tv3) already abuts the live head cid@tv3, so it is
	// left untouched — rule (d) trims the *earlier* record, never the boundary.
	if rep.OverlapsTrimmed != 2 {
		t.Errorf("OverlapsTrimmed = %d, want 2 (old, ana)", rep.OverlapsTrimmed)
	}
	assertDisjointChain(t, f, "svc", "owner")
	if old := f.allFacts()["old"]; old.InvalidAt == nil || !old.InvalidAt.Equal(tv1) {
		t.Errorf("old.invalid_at = %v, want tv1", old.InvalidAt)
	}
	if ana := f.allFacts()["ana"]; ana.InvalidAt == nil || !ana.InvalidAt.Equal(tv2) {
		t.Errorf("ana.invalid_at = %v, want tv2", ana.InvalidAt)
	}
	if eve := f.allFacts()["eve"]; eve.InvalidAt == nil || !eve.InvalidAt.Equal(tv3) {
		t.Errorf("eve.invalid_at = %v, want tv3 (untouched)", eve.InvalidAt)
	}
}

// TestDW_RuleD_NoopWhenDisjoint proves rule (d) never touches an
// already-disjoint chain and never closes a live record: a normal chain
// (bob=[tv0,tv1) closed, ana live @tv1) sweeps to zero trims.
func TestDW_RuleD_NoopWhenDisjoint(t *testing.T) {
	f := newFakeStore()
	w, _ := newTestWorker(f, "v1")
	ctx := context.Background()

	f.seedFact("bob", closedFact("bob", tv0, tv1))
	f.seedFact("ana", liveFact("ana", tv1))

	rep, err := (&worker.Sweeper{Store: f, Worker: w, Logger: quietLogger}).Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rep.OverlapsTrimmed != 0 {
		t.Errorf("OverlapsTrimmed = %d, want 0 on a disjoint chain", rep.OverlapsTrimmed)
	}
	if bob := f.allFacts()["bob"]; bob.InvalidAt == nil || !bob.InvalidAt.Equal(tv1) {
		t.Errorf("bob.invalid_at = %v, want tv1 (untouched)", bob.InvalidAt)
	}
	_, head := singleLiveHead(t, f, "svc", "owner")
	if head.Object != "ana" {
		t.Errorf("live head = %q, want ana (unchanged)", head.Object)
	}
}

// assertOverlapExists fails if no two non-expired records for subject/predicate
// cover a common valid-time instant — a guard that the regression's seeded
// precondition is genuinely overlapping (so the sweep has something to fix).
func assertOverlapExists(t *testing.T, f *fakeStore, subject, predicate string) {
	t.Helper()
	type span struct {
		start, end time.Time
		open       bool
	}
	var spans []span
	for _, fc := range f.allFacts() {
		if fc.Subject != subject || fc.Predicate != predicate || fc.ExpiredAt != nil {
			continue
		}
		sp := span{start: fc.ValidAt, open: fc.InvalidAt == nil}
		if !sp.open {
			sp.end = *fc.InvalidAt
		}
		spans = append(spans, sp)
	}
	for i := 0; i < len(spans); i++ {
		for j := i + 1; j < len(spans); j++ {
			a, b := spans[i], spans[j]
			aEnd := a.end
			bEnd := b.end
			aAfterBStart := a.open || aEnd.After(b.start)
			bAfterAStart := b.open || bEnd.After(a.start)
			if aAfterBStart && bAfterAStart {
				return // found a covering overlap
			}
		}
	}
	t.Fatal("seeded state is not overlapping; the regression precondition is wrong")
}

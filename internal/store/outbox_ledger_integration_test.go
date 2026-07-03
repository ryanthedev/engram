//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/store"
	"github.com/ryanthedev/engram/internal/testutil"
)

// liveOutboxStore is liveStore plus a scratch ledger index and a short
// ledger lease, for the Phase-2 outbox/ledger mechanics.
func liveOutboxStore(t *testing.T, name string, ledgerLease time.Duration) (s *store.OpenSearchStore, base, epIdx string) {
	t.Helper()
	base = testutil.OpenSearchURL()
	if _, err := store.Apply(context.Background(), testutil.HTTPClient, base); err != nil {
		t.Fatalf("applying cluster contract: %v", err)
	}
	epIdx = testutil.ScratchIndexName("episodic", name)
	semIdx := testutil.ScratchIndexName("semantic", name)
	ledIdx := testutil.ScratchIndexName("ledger", name)
	for _, idx := range []string{epIdx, semIdx, ledIdx} {
		testutil.DeleteIndex(t, base, idx)
		t.Cleanup(func(idx string) func() { return func() { testutil.DeleteIndex(t, base, idx) } }(idx))
		testutil.CreateScratchIndex(t, base, idx)
	}
	return store.NewOpenSearchStore(testutil.HTTPClient, base,
		store.WithEpisodicIndex(epIdx), store.WithSemanticIndex(semIdx),
		store.WithLedgerIndex(ledIdx), store.WithLedgerLease(ledgerLease)), base, epIdx
}

// TestDW_2_1_LiveOutboxClaimCompleteDeadLetter proves the scan-and-claim
// outbox (D12) on the real cluster: claims lease the oldest unprocessed
// events and count attempts, a live lease blocks re-claim, Complete and
// DeadLetter both remove events from the scan, and an expired lease makes
// the event reclaimable (the kill-and-restart handoff).
func TestDW_2_1_LiveOutboxClaimCompleteDeadLetter(t *testing.T) {
	s, base, epIdx := liveOutboxStore(t, t.Name(), store.DefaultLedgerLease)
	ctx := context.Background()

	base3 := time.Now().UTC().Add(-time.Minute)
	for i, id := range []string{"ev-a", "ev-b", "ev-c"} {
		_, err := s.Append(ctx, memory.Episodic{
			EventID: id, TenantID: "t1", Text: "outbox " + id,
			CreatedAt: base3.Add(time.Duration(i) * time.Second),
		})
		if err != nil {
			t.Fatalf("Append %s: %v", id, err)
		}
	}
	testutil.RefreshIndex(t, base, epIdx)

	claimed, err := s.ClaimBatch(ctx, 2, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("ClaimBatch: %v", err)
	}
	if len(claimed) != 2 || claimed[0].EventID != "ev-a" || claimed[1].EventID != "ev-b" {
		t.Fatalf("claimed %+v, want the two oldest (ev-a, ev-b)", claimed)
	}
	for _, c := range claimed {
		if c.Attempts != 1 || c.ClaimLeaseUntil == nil {
			t.Errorf("claimed %s attempts=%d lease=%v, want attempts=1 with a lease", c.EventID, c.Attempts, c.ClaimLeaseUntil)
		}
	}

	// A live lease blocks re-claim; the unclaimed third event is available.
	rest, err := s.ClaimBatch(ctx, 10, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("second ClaimBatch: %v", err)
	}
	if len(rest) != 1 || rest[0].EventID != "ev-c" {
		t.Fatalf("second claim = %+v, want only ev-c", rest)
	}

	// Complete one, dead-letter another; both leave the outbox for good.
	if err := s.Complete(ctx, "ev-a"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := s.DeadLetter(ctx, "ev-c", "poison fixture"); err != nil {
		t.Fatalf("DeadLetter: %v", err)
	}

	// ev-b's abandoned lease expires → reclaimable with attempts=2 (the
	// kill-and-restart path: no handoff was lost).
	time.Sleep(600 * time.Millisecond)
	reclaimed, err := s.ClaimBatch(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("reclaim ClaimBatch: %v", err)
	}
	if len(reclaimed) != 1 || reclaimed[0].EventID != "ev-b" || reclaimed[0].Attempts != 2 {
		t.Fatalf("reclaim = %+v, want ev-b at attempts=2 only", reclaimed)
	}
}

// TestLiveLedgerClaimResumeAndScan proves the claim-first ledger (D13) on
// the real cluster: the first claim wins via op_type=create, cached state
// round-trips to a resuming claimant (including the extraction bytes), and
// an incomplete entry surfaces in ScanIncomplete once its lease expires —
// then disappears when marked complete.
func TestLiveLedgerClaimResumeAndScan(t *testing.T) {
	s, _, _ := liveOutboxStore(t, t.Name(), 300*time.Millisecond)
	ctx := context.Background()
	key := memory.LedgerKey{TenantID: "t1", EventID: "ev-led-1", ExtractorVersion: "v1"}

	first, err := s.ClaimLedger(ctx, key)
	if err != nil {
		t.Fatalf("first ClaimLedger: %v", err)
	}
	if !first.Claimed || first.State.Phase != store.LedgerClaimed {
		t.Fatalf("first claim = %+v, want Claimed=true phase=claimed", first)
	}

	extraction := []byte(`[{"subject":"svc","predicate":"owner","object":"ana"}]`)
	if err := s.UpdateLedger(ctx, key, store.LedgerState{Phase: store.LedgerExtracted, Extraction: extraction, CompletedActions: []string{"doc-1"}}); err != nil {
		t.Fatalf("UpdateLedger: %v", err)
	}

	resumed, err := s.ClaimLedger(ctx, key)
	if err != nil {
		t.Fatalf("resume ClaimLedger: %v", err)
	}
	if resumed.Claimed || resumed.State.Phase != store.LedgerExtracted || string(resumed.State.Extraction) != string(extraction) {
		t.Fatalf("resume = %+v, want Claimed=false with the cached extraction", resumed)
	}

	time.Sleep(400 * time.Millisecond) // outlive the 300ms lease
	incomplete, err := s.ScanIncomplete(ctx)
	if err != nil {
		t.Fatalf("ScanIncomplete: %v", err)
	}
	if len(incomplete) != 1 || incomplete[0].Key != key {
		t.Fatalf("ScanIncomplete = %+v, want the one expired incomplete entry", incomplete)
	}

	if err := s.UpdateLedger(ctx, key, store.LedgerState{Phase: store.LedgerComplete, Extraction: extraction, CompletedActions: []string{"doc-1"}}); err != nil {
		t.Fatalf("completing UpdateLedger: %v", err)
	}
	time.Sleep(400 * time.Millisecond)
	incomplete, err = s.ScanIncomplete(ctx)
	if err != nil {
		t.Fatalf("ScanIncomplete after complete: %v", err)
	}
	if len(incomplete) != 0 {
		t.Fatalf("ScanIncomplete after complete = %+v, want empty", incomplete)
	}
}

//go:build integration

package store_test

// Live-cluster proof of PurgeEvent's per-tier semantics against real
// OpenSearch: hard delete on episodic + ledger, expired_at tombstone on
// semantic, replay duplicates taken together, dry run that mutates nothing,
// and the two "absence is not an error" cases (no matches, no index).
// Each test owns scratch indices named after itself so parallel runs on the
// shared dev cluster cannot collide.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/store"
	"github.com/ryanthedev/engram/internal/testutil"
)

// livePurgeStore provisions scratch episodic/semantic/ledger indices (each
// inheriting the real template via CreateScratchIndex) and returns a store
// bound to them plus their names, for direct assertions.
func livePurgeStore(t *testing.T) (s *store.OpenSearchStore, base, epIdx, semIdx, ledIdx string) {
	t.Helper()
	base = testutil.OpenSearchURL()
	if _, err := store.Apply(context.Background(), testutil.HTTPClient, base); err != nil {
		t.Fatalf("applying cluster contract: %v", err)
	}
	epIdx = testutil.ScratchIndexName("episodic", t.Name())
	semIdx = testutil.ScratchIndexName("semantic", t.Name())
	ledIdx = testutil.ScratchIndexName("ledger", t.Name())
	for _, idx := range []string{epIdx, semIdx, ledIdx} {
		testutil.DeleteIndex(t, base, idx)
		t.Cleanup(func(idx string) func() { return func() { testutil.DeleteIndex(t, base, idx) } }(idx))
		testutil.CreateScratchIndex(t, base, idx)
	}
	s = store.NewOpenSearchStore(testutil.HTTPClient, base,
		store.WithEpisodicIndex(epIdx), store.WithSemanticIndex(semIdx), store.WithLedgerIndex(ledIdx))
	return s, base, epIdx, semIdx, ledIdx
}

// countIndex refreshes then counts an index directly, bypassing the store —
// the independent check that a hard delete really removed rows.
func countIndex(t *testing.T, base, index string) int64 {
	t.Helper()
	testutil.RefreshIndex(t, base, index)
	status, decoded := testutil.Call(t, http.MethodGet, base+"/"+index+"/_count", nil)
	if status != http.StatusOK {
		t.Fatalf("counting %s: status %d: %v", index, status, decoded)
	}
	count, _ := decoded["count"].(float64)
	return int64(count)
}

// seedFact writes one live semantic fact sourced from eventID and returns its
// doc id, so a test can read it back and inspect expired_at.
func seedFact(t *testing.T, s *store.OpenSearchStore, tenantID, eventID, subject string) string {
	t.Helper()
	validAt := time.Now().UTC().Truncate(time.Second)
	key := memory.ContentKey(tenantID, subject, "prefers", "dark-mode")
	id := memory.FactDocID(key, validAt)
	fact := memory.SemanticFact{
		Subject: subject, Predicate: "prefers", Object: "dark-mode",
		Statement: subject + " prefers dark-mode", ContentKey: key,
		ExtractorVersion: "test-v1", TenantID: tenantID, SourceIDs: []string{eventID},
		ValidAt: validAt, CreatedAt: time.Now().UTC(),
	}
	if err := s.Create(context.Background(), id, fact); err != nil {
		t.Fatalf("seeding fact %s: %v", id, err)
	}
	return id
}

// seedLedger claims a complete ledger entry for (tenant, event) — the row
// that would short-circuit a re-ingest if a purge left it behind.
func seedLedger(t *testing.T, s *store.OpenSearchStore, tenantID, eventID string) memory.LedgerKey {
	t.Helper()
	key := memory.LedgerKey{TenantID: tenantID, EventID: eventID, ExtractorVersion: "test-v1"}
	if _, err := s.ClaimLedger(context.Background(), key); err != nil {
		t.Fatalf("claiming ledger for %s: %v", eventID, err)
	}
	if err := s.UpdateLedger(context.Background(), key, store.LedgerState{Phase: store.LedgerComplete}); err != nil {
		t.Fatalf("completing ledger for %s: %v", eventID, err)
	}
	return key
}

// TestLivePurgeEventRemovesReplayDuplicatesAndTombstonesFacts is the core
// contract, end to end on real indices:
//
//   - BOTH episodic docs sharing one event_id go (event_id does not
//     deduplicate the raw log — the exact hazard purge exists for);
//   - the ledger row goes, so a corrected re-ingest re-extracts;
//   - the semantic fact is tombstoned, not deleted: still in the index, but
//     carrying expired_at, which every live read path already excludes;
//   - a second event's data, and another tenant's data under the SAME event
//     id, are untouched.
func TestLivePurgeEventRemovesReplayDuplicatesAndTombstonesFacts(t *testing.T) {
	s, base, epIdx, semIdx, ledIdx := livePurgeStore(t)
	ctx := context.Background()

	// Two appends of the SAME event id: the replay duplicate D13 does not
	// prevent, since idempotency lives on the ledger, not the raw append.
	for i := 0; i < 2; i++ {
		if _, err := s.Append(ctx, memory.Episodic{
			EventID: "ev-purge", TenantID: "t1", Text: "note to purge",
			OccurredAt: time.Now().UTC(), CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	// A neighbour event, and the same event id under a DIFFERENT tenant.
	if _, err := s.Append(ctx, memory.Episodic{EventID: "ev-keep", TenantID: "t1", Text: "keep me", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("append neighbour: %v", err)
	}
	if _, err := s.Append(ctx, memory.Episodic{EventID: "ev-purge", TenantID: "t2", Text: "other tenant", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("append cross-tenant: %v", err)
	}
	seedLedger(t, s, "t1", "ev-purge")
	seedLedger(t, s, "t1", "ev-keep")
	purgedFact := seedFact(t, s, "t1", "ev-purge", "alice")
	keptFact := seedFact(t, s, "t1", "ev-keep", "bob")
	testutil.RefreshIndex(t, base, epIdx)
	testutil.RefreshIndex(t, base, semIdx)
	testutil.RefreshIndex(t, base, ledIdx)

	counts, err := s.PurgeEvent(ctx, "t1", "ev-purge", false)
	if err != nil {
		t.Fatalf("PurgeEvent: %v", err)
	}
	if counts.Episodic != 2 {
		t.Errorf("Episodic = %d, want 2 (both replay duplicates)", counts.Episodic)
	}
	if counts.Ledger != 1 {
		t.Errorf("Ledger = %d, want 1", counts.Ledger)
	}
	if counts.Semantic != 1 {
		t.Errorf("Semantic = %d, want 1", counts.Semantic)
	}

	// Episodic: only ev-keep (t1) and ev-purge (t2) remain.
	if n := countIndex(t, base, epIdx); n != 2 {
		t.Errorf("episodic docs remaining = %d, want 2 (ev-keep and the other tenant's ev-purge)", n)
	}
	// FindByEventID is tenant-blind by design (it is the repair sweep's
	// lookup), so it is the sharpest available check that the purge removed
	// t1's rows and ONLY t1's: exactly the t2 doc must be left.
	remaining, err := s.FindByEventID(ctx, "ev-purge")
	if err != nil {
		t.Fatalf("re-reading purged event: %v", err)
	}
	if len(remaining) != 1 || remaining[0].TenantID != "t2" {
		t.Errorf("episodic docs for ev-purge after purge = %+v, want only the t2 row", remaining)
	}
	kept, err := s.FindByEventID(ctx, "ev-keep")
	if err != nil || len(kept) != 1 {
		t.Errorf("the neighbour event was removed (got %d docs, err=%v)", len(kept), err)
	}

	// Ledger: the purged event's row is gone, the neighbour's survives.
	if n := countIndex(t, base, ledIdx); n != 1 {
		t.Errorf("ledger rows remaining = %d, want 1 (ev-keep only)", n)
	}
	// Semantic: tombstoned in place, not deleted. The doc is still there for
	// Audit; it just carries expired_at now.
	testutil.RefreshIndex(t, base, semIdx)
	if n := countIndex(t, base, semIdx); n != 2 {
		t.Errorf("semantic docs = %d, want 2 (soft delete keeps the row)", n)
	}
	vf, ok, err := s.GetFact(ctx, purgedFact)
	if err != nil || !ok {
		t.Fatalf("reading the purged fact: ok=%v err=%v", ok, err)
	}
	if vf.Fact.ExpiredAt == nil {
		t.Errorf("purged fact carries no expired_at — it was not tombstoned")
	}
	vf, ok, err = s.GetFact(ctx, keptFact)
	if err != nil || !ok {
		t.Fatalf("reading the neighbour fact: ok=%v err=%v", ok, err)
	}
	if vf.Fact.ExpiredAt != nil {
		t.Errorf("the neighbour fact was tombstoned too (expired_at=%v)", vf.Fact.ExpiredAt)
	}

	// Re-purging is idempotent AND does not re-stamp the tombstone: the
	// second call matches nothing at all.
	again, err := s.PurgeEvent(ctx, "t1", "ev-purge", false)
	if err != nil {
		t.Fatalf("re-purging: %v", err)
	}
	if again != (store.PurgeCounts{}) {
		t.Errorf("re-purge counts = %+v, want all zero", again)
	}

	// Last, because it WRITES: re-claiming the purged ledger key must win a
	// fresh claim. That is the operative consequence of removing the row — a
	// corrected re-ingest re-extracts instead of short-circuiting on a
	// LedgerComplete entry (D13).
	entry, err := s.ClaimLedger(ctx, memory.LedgerKey{TenantID: "t1", EventID: "ev-purge", ExtractorVersion: "test-v1"})
	if err != nil {
		t.Fatalf("re-claiming the purged ledger key: %v", err)
	}
	if !entry.Claimed {
		t.Errorf("re-claiming ev-purge did not win a fresh claim — the ledger row survived, so a re-ingest would short-circuit")
	}
}

// TestLivePurgeEventDryRunCountsWithoutMutating: the default mode reports the
// exact same numbers a real purge would, and changes nothing anywhere.
func TestLivePurgeEventDryRunCountsWithoutMutating(t *testing.T) {
	s, base, epIdx, semIdx, ledIdx := livePurgeStore(t)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, err := s.Append(ctx, memory.Episodic{EventID: "ev-dry", TenantID: "t1", Text: "x", CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	seedLedger(t, s, "t1", "ev-dry")
	factID := seedFact(t, s, "t1", "ev-dry", "carol")
	testutil.RefreshIndex(t, base, epIdx)
	testutil.RefreshIndex(t, base, semIdx)
	testutil.RefreshIndex(t, base, ledIdx)

	dry, err := s.PurgeEvent(ctx, "t1", "ev-dry", true)
	if err != nil {
		t.Fatalf("PurgeEvent(dryRun): %v", err)
	}
	want := store.PurgeCounts{Episodic: 2, Ledger: 1, Semantic: 1}
	if dry != want {
		t.Errorf("dry-run counts = %+v, want %+v", dry, want)
	}
	if n := countIndex(t, base, epIdx); n != 2 {
		t.Errorf("episodic docs after dry run = %d, want 2 (nothing removed)", n)
	}
	if n := countIndex(t, base, ledIdx); n != 1 {
		t.Errorf("ledger rows after dry run = %d, want 1", n)
	}
	vf, ok, err := s.GetFact(ctx, factID)
	if err != nil || !ok {
		t.Fatalf("reading fact after dry run: ok=%v err=%v", ok, err)
	}
	if vf.Fact.ExpiredAt != nil {
		t.Errorf("dry run tombstoned a fact (expired_at=%v)", vf.Fact.ExpiredAt)
	}

	// The real purge that follows must land exactly the numbers the dry run
	// promised — that equivalence is the whole value of the preview.
	real, err := s.PurgeEvent(ctx, "t1", "ev-dry", false)
	if err != nil {
		t.Fatalf("PurgeEvent(confirm): %v", err)
	}
	if real != want {
		t.Errorf("confirmed purge counts = %+v, want the dry run's %+v", real, want)
	}
}

// TestLivePurgeEventZeroMatchesIsNotError: purging an event that was never
// ingested is a successful purge of nothing. Reporting NOT_FOUND instead
// would break idempotent re-runs of a batch and hand callers an existence
// oracle.
func TestLivePurgeEventZeroMatchesIsNotError(t *testing.T) {
	s, _, _, _, _ := livePurgeStore(t)
	counts, err := s.PurgeEvent(context.Background(), "t1", "ev-never-ingested", false)
	if err != nil {
		t.Fatalf("PurgeEvent on an unknown event: %v", err)
	}
	if counts != (store.PurgeCounts{}) {
		t.Errorf("counts = %+v, want all zero", counts)
	}
}

// TestLivePurgeEventMissingIndexIsNotError: a tier whose index has never been
// created is empty, not broken — the house isIndexNotFound-as-empty rule. A
// fresh deployment purging before its first write must not error.
func TestLivePurgeEventMissingIndexIsNotError(t *testing.T) {
	base := testutil.OpenSearchURL()
	s := store.NewOpenSearchStore(testutil.HTTPClient, base,
		store.WithEpisodicIndex("engram-episodic-purge-absent-xyz"),
		store.WithSemanticIndex("engram-semantic-purge-absent-xyz"),
		store.WithLedgerIndex("engram-ledger-purge-absent-xyz"))
	for _, dryRun := range []bool{true, false} {
		counts, err := s.PurgeEvent(context.Background(), "t1", "ev-1", dryRun)
		if err != nil {
			t.Fatalf("PurgeEvent(dryRun=%v) against absent indices: %v", dryRun, err)
		}
		if counts != (store.PurgeCounts{}) {
			t.Errorf("counts = %+v, want all zero", counts)
		}
	}
}

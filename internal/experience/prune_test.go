package experience

import (
	"context"
	"testing"
)

// TestDW_5_5_PruneSoftExpiresRecoverable: the prune job soft-expires a low-Φ,
// low-retrieval experience — it disappears from retrieval but is NOT deleted:
// Recover still returns it (bi-temporal recoverability). A high-Φ experience is
// untouched.
func TestDW_5_5_PruneSoftExpiresRecoverable(t *testing.T) {
	backend := NewMemBackend()
	store, _ := NewStore(RuleGatekeeper{}, backend, nil)
	ctx := context.Background()

	low := admittableExp("rarely useful trick")
	low.Utility = 0.1
	high := admittableExp("very useful pattern")
	high.Utility = 0.9
	for _, e := range []Experience{low, high} {
		if v, err := store.Admit(ctx, e); err != nil || v != Admit {
			t.Fatalf("Admit(%s) => %q, %v", e.Task, v, err)
		}
	}
	lowFP := Fingerprint("t1", "rarely useful trick")
	highFP := Fingerprint("t1", "very useful pattern")

	// Prune: utility <= 0.2 AND retrieval_count <= 0.
	pruned, err := store.Prune(ctx, 0.2, 0)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("pruned %d, want exactly 1 (only the low-Φ record)", pruned)
	}

	// The low-Φ record is gone from retrieval...
	hits, _ := store.SearchAdmitted(ctx, "t1", "trick pattern", 10)
	for _, h := range hits {
		if h.Fingerprint == lowFP {
			t.Fatalf("soft-expired record still retrievable")
		}
	}
	// ...but recoverable (never deleted — D3).
	rec, err := store.Recover(ctx, "t1", lowFP)
	if err != nil {
		t.Fatalf("Recover soft-expired: %v", err)
	}
	if rec.ExpiredAt == nil {
		t.Fatalf("recovered record has no expired_at stamp; soft-expire didn't record")
	}
	if rec.Live() {
		t.Fatalf("recovered record still reports Live()")
	}

	// The high-Φ record is untouched and still retrievable.
	if h, err := store.Recover(ctx, "t1", highFP); err != nil || !h.Live() {
		t.Fatalf("high-Φ record disturbed by prune: live=%v err=%v", h.Live(), err)
	}
}

// TestDW_5_5_PruneRespectsRetrievalCount: a low-Φ but frequently-retrieved
// experience is NOT pruned (retrieval count keeps it alive).
func TestDW_5_5_PruneRespectsRetrievalCount(t *testing.T) {
	backend := NewMemBackend()
	store, _ := NewStore(RuleGatekeeper{}, backend, nil)
	ctx := context.Background()

	e := admittableExp("popular low-utility")
	e.Utility = 0.1
	if _, err := store.Admit(ctx, e); err != nil {
		t.Fatal(err)
	}
	fp := Fingerprint("t1", "popular low-utility")
	_ = backend.BumpRetrieval(ctx, "t1", fp) // retrieval_count = 1

	pruned, err := store.Prune(ctx, 0.2, 0) // retrievalMax 0 excludes it
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 0 {
		t.Fatalf("pruned %d; a retrieved record should survive retrievalMax=0", pruned)
	}
}

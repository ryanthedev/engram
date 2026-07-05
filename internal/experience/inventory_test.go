package experience

import (
	"context"
	"testing"
)

// TestDW_3_1_InventoryCountsSurviveRestart is the Phase-3 regression: it
// simulates a server restart by seeding a Backend directly (bypassing
// Store.Admit, exactly as OpenSearch data would already exist before a
// process even starts) and then constructing a BRAND NEW Store over that
// same backend. The new Store's in-process VerdictCounts must read 0,0,0
// (a genuine restart resets them — that is expected and documented), while
// InventoryCounts must reflect the durable backend state, proving the
// durable gauges do not depend on any in-process bookkeeping.
func TestDW_3_1_InventoryCountsSurviveRestart(t *testing.T) {
	ctx := context.Background()
	backend := NewMemBackend()

	// Seed the backend directly — this is the "already-durable" state a
	// restarted process finds waiting in OpenSearch.
	for _, task := range []string{"task-a", "task-b", "task-c"} {
		exp := admittableExp(task)
		exp.Fingerprint = Fingerprint(exp.TenantID, exp.Task)
		if err := backend.PutAdmitted(ctx, exp); err != nil {
			t.Fatalf("seeding admitted %s: %v", task, err)
		}
	}
	for _, task := range []string{"q-a", "q-b"} {
		q := Quarantined{Experience: admittableExp(task), Verdict: Quarantine, Reason: "seed"}
		q.Experience.Fingerprint = Fingerprint(q.Experience.TenantID, q.Experience.Task)
		if err := backend.PutQuarantine(ctx, q); err != nil {
			t.Fatalf("seeding quarantine %s: %v", task, err)
		}
	}

	// Simulate a restart: a fresh Store over the SAME backend. Its
	// in-process counters start at zero regardless of the seeded data.
	fresh, err := NewStore(RuleGatekeeper{}, backend, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	admitted, quarantined, err := fresh.InventoryCounts(ctx)
	if err != nil {
		t.Fatalf("InventoryCounts: %v", err)
	}
	if admitted != 3 {
		t.Errorf("InventoryCounts admitted = %d, want 3 (durable state, not 0)", admitted)
	}
	if quarantined != 2 {
		t.Errorf("InventoryCounts quarantined = %d, want 2 (durable state, not 0)", quarantined)
	}

	gotAdmit, gotQuarantine, gotReject := fresh.VerdictCounts()
	if gotAdmit != 0 || gotQuarantine != 0 || gotReject != 0 {
		t.Errorf("VerdictCounts on a freshly-restarted store = (%d,%d,%d), want (0,0,0) — "+
			"the in-process rate legitimately resets on restart", gotAdmit, gotQuarantine, gotReject)
	}
}

// TestDW_3_1_InventoryExcludesSoftExpired proves InventoryCounts' admitted
// figure is LIVE inventory, not a raw document count: a soft-expired
// (pruned) document is excluded, matching SearchAdmitted's live-only
// contract and the plan's "current live inventory" framing.
func TestDW_3_1_InventoryExcludesSoftExpired(t *testing.T) {
	ctx := context.Background()
	backend := NewMemBackend()
	store, err := NewStore(RuleGatekeeper{}, backend, nil)
	if err != nil {
		t.Fatal(err)
	}

	exp := admittableExp("prunable")
	if v, err := store.Admit(ctx, exp); err != nil || v != Admit {
		t.Fatalf("Admit => %v, %v; want Admit", v, err)
	}
	fp := Fingerprint(exp.TenantID, exp.Task)

	if admitted, _, err := store.InventoryCounts(ctx); err != nil || admitted != 1 {
		t.Fatalf("InventoryCounts before prune => %d, %v; want 1", admitted, err)
	}
	if _, err := store.Prune(ctx, 1.0, 1000); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if admitted, _, err := store.InventoryCounts(ctx); err != nil || admitted != 0 {
		t.Fatalf("InventoryCounts after prune => %d, %v; want 0 (soft-expired excluded)", admitted, err)
	}
	// Recover proves the doc still physically exists (D3: never delete) even
	// though it's excluded from the live inventory count.
	if _, err := store.Recover(ctx, exp.TenantID, fp); err != nil {
		t.Fatalf("Recover pruned doc: %v", err)
	}
}

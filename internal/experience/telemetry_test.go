package experience

import (
	"context"
	"testing"
)

// TestStore_VerdictCounts_TracksLiveAdmitQuarantineReject is the Phase-7
// gate-verdict-rate telemetry source's unit test (DW-7.8): each Admit call
// bumps exactly one of the three cumulative counters, matching the verdict
// it actually returned.
func TestStore_VerdictCounts_TracksLiveAdmitQuarantineReject(t *testing.T) {
	store, err := NewStore(RuleGatekeeper{}, NewMemBackend(), nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ctx := context.Background()

	if admit, quarantine, reject := store.VerdictCounts(); admit != 0 || quarantine != 0 || reject != 0 {
		t.Fatalf("fresh store VerdictCounts = (%d,%d,%d), want all zero", admit, quarantine, reject)
	}

	// A well-evidenced success admits.
	good := Experience{TenantID: "t", OwnerAgentID: "a", Scope: "private",
		Task: "fix flaky test", DistilledSkill: "add retry", Utility: 0.8,
		Outcome: Outcome{Success: true, Evidence: []string{"ci green"}, Signals: []string{"tests_passed"}}}
	if v, err := store.Admit(ctx, good); err != nil || v != Admit {
		t.Fatalf("Admit(good) = (%v, %v), want (Admit, nil)", v, err)
	}

	// Self-reported success with no evidence quarantines (RuleGatekeeper).
	unproven := Experience{TenantID: "t", OwnerAgentID: "a", Scope: "private",
		Task: "claims a fix", DistilledSkill: "trust me", Utility: 0.5,
		Outcome: Outcome{Success: true}}
	if v, err := store.Admit(ctx, unproven); err != nil || v != Quarantine {
		t.Fatalf("Admit(unproven) = (%v, %v), want (Quarantine, nil)", v, err)
	}

	// No tenant is a hard reject before the gate even runs.
	if v, err := store.Admit(ctx, Experience{Task: "no tenant"}); err == nil || v != Reject {
		t.Fatalf("Admit(no tenant) = (%v, %v), want (Reject, error)", v, err)
	}

	admit, quarantine, reject := store.VerdictCounts()
	if admit != 1 || quarantine != 1 || reject != 1 {
		t.Fatalf("VerdictCounts = (%d,%d,%d), want (1,1,1)", admit, quarantine, reject)
	}

	// A second admit of a different good record accumulates rather than resets.
	good2 := good
	good2.Task = "a different fix"
	if v, _ := store.Admit(ctx, good2); v != Admit {
		t.Fatalf("second Admit = %v, want Admit", v)
	}
	if admit, _, _ := store.VerdictCounts(); admit != 2 {
		t.Fatalf("admit count after second Admit = %d, want 2 (cumulative)", admit)
	}
}

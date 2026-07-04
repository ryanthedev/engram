//go:build integration

package experience

import (
	"context"
	"testing"

	"github.com/ryanthedev/engram/internal/testutil"
)

// scratchBackend applies the T3 templates and creates a pair of scratch indices
// (matching the template patterns) so an integration test exercises the real
// OpenSearch mappings without touching production indices.
func scratchBackend(t *testing.T) (*OpenSearchBackend, func()) {
	t.Helper()
	base := testutil.OpenSearchURL()
	ctx := context.Background()
	if err := Apply(ctx, testutil.HTTPClient, base); err != nil {
		t.Fatalf("apply experience templates: %v", err)
	}
	admitted := "engram-experience-000001-" + sanitize(t.Name())
	quarantine := "engram-experience-quarantine-000001-" + sanitize(t.Name())
	testutil.CreateScratchIndex(t, base, admitted)
	testutil.CreateScratchIndex(t, base, quarantine)
	backend := NewOpenSearchBackend(testutil.HTTPClient, base, WithIndices(admitted, quarantine))
	return backend, func() {
		testutil.DeleteIndex(t, base, admitted)
		testutil.DeleteIndex(t, base, quarantine)
	}
}

func sanitize(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r+('a'-'A'))
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}

// TestIntegration_StoreRoundTrip drives the full gated store against the live
// pinned cluster: admit → retrievable, quarantine → listed + releasable, prune →
// soft-expired but recoverable. It proves the OpenSearch backend honors the same
// contract the in-memory backend does.
func TestIntegration_StoreRoundTrip(t *testing.T) {
	backend, cleanup := scratchBackend(t)
	defer cleanup()
	store, err := NewStore(RuleGatekeeper{}, backend, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Admit an evidence-backed success.
	good := Experience{
		TenantID: "ten", OwnerAgentID: "ag", Scope: "private",
		Task: "optimize live search", DistilledSkill: "warm the pipeline cache", Utility: 0.8,
		Outcome: Outcome{Success: true, Evidence: []string{"p95 down"}},
	}
	if v, err := store.Admit(ctx, good); err != nil || v != Admit {
		t.Fatalf("Admit(good) => %q, %v; want Admit", v, err)
	}
	hits, err := store.SearchAdmitted(ctx, "ten", "search cache", 10)
	if err != nil || len(hits) != 1 {
		t.Fatalf("SearchAdmitted => %d, %v; want 1", len(hits), err)
	}

	// Quarantine a self-reported success with no evidence, then release it.
	bad := good
	bad.Task = "unproven claim"
	bad.Outcome = Outcome{Success: true}
	if v, err := store.Admit(ctx, bad); err != nil || v != Quarantine {
		t.Fatalf("Admit(bad) => %q, %v; want Quarantine", v, err)
	}
	badFP := Fingerprint("ten", "unproven claim")
	list, err := store.ListQuarantine(ctx, "ten")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListQuarantine => %d, %v; want 1", len(list), err)
	}
	if err := store.Release(ctx, "ten", badFP); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if list, _ := store.ListQuarantine(ctx, "ten"); len(list) != 0 {
		t.Fatalf("quarantine not cleared after release; %d remain", len(list))
	}

	// Prune a low-Φ record; it becomes unretrievable but recoverable.
	low := good
	low.Task = "rare trick"
	low.DistilledSkill = "rarely helps"
	low.Utility = 0.05
	if _, err := store.Admit(ctx, low); err != nil {
		t.Fatal(err)
	}
	lowFP := Fingerprint("ten", "rare trick")
	if pruned, err := store.Prune(ctx, 0.1, 0); err != nil || pruned < 1 {
		t.Fatalf("Prune => %d, %v; want >= 1", pruned, err)
	}
	rec, err := store.Recover(ctx, "ten", lowFP)
	if err != nil || rec.ExpiredAt == nil {
		t.Fatalf("Recover pruned => %+v, %v; want a soft-expired record (never deleted)", rec, err)
	}
}

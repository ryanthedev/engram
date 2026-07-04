package experience

import (
	"context"
	"testing"

	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/retrieval"
)

// TestTier_ReturnsAdmittedWithProvenance: the tier surfaces admitted experiences
// as hits carrying the provenance/scope fields the MultiRetriever needs to
// re-verify them through the ACL.
func TestTier_ReturnsAdmittedWithProvenance(t *testing.T) {
	store, _ := NewStore(RuleGatekeeper{}, NewMemBackend(), nil)
	ctx := context.Background()
	e := admittableExp("cache the embeddings")
	if _, err := store.Admit(ctx, e); err != nil {
		t.Fatal(err)
	}
	tier := NewTier(store, nil)
	hits, err := tier.Search(ctx, auth.Identity{TenantID: "t1", UserID: "u1", AgentID: "a1"},
		retrieval.Query{Text: "cache embeddings", K: 10})
	if err != nil || len(hits) != 1 {
		t.Fatalf("tier.Search => %d hits, %v; want 1", len(hits), err)
	}
	h := hits[0]
	if h.Source != "experience" {
		t.Fatalf("hit source = %q, want experience", h.Source)
	}
	for _, k := range []string{"tenant_id", "scope", "owner_agent_id", "distilled_skill"} {
		if _, ok := h.Fields[k]; !ok {
			t.Fatalf("hit missing field %q needed for ACL re-verify / display", k)
		}
	}
	if h.Fields["tenant_id"] != "t1" || h.Fields["owner_agent_id"] != "a1" {
		t.Fatalf("hit provenance = %v, want tenant t1 / owner a1", h.Fields)
	}
}

// TestTier_ExcludesQuarantinedAndPruned: the tier never surfaces a quarantined
// or soft-expired experience.
func TestTier_ExcludesQuarantinedAndPruned(t *testing.T) {
	backend := NewMemBackend()
	store, _ := NewStore(RuleGatekeeper{}, backend, nil)
	ctx := context.Background()

	// Quarantined (no evidence).
	q := admittableExp("quarantined skill")
	q.Outcome = Outcome{Success: true}
	_, _ = store.Admit(ctx, q)
	// Admitted then pruned.
	p := admittableExp("pruned skill")
	p.Utility = 0.05
	_, _ = store.Admit(ctx, p)
	if _, err := store.Prune(ctx, 0.1, 0); err != nil {
		t.Fatal(err)
	}

	tier := NewTier(store, nil)
	hits, _ := tier.Search(ctx, auth.Identity{TenantID: "t1", UserID: "u1", AgentID: "a1"},
		retrieval.Query{Text: "skill", K: 10})
	if len(hits) != 0 {
		t.Fatalf("tier returned %d hits; quarantined+pruned must be invisible", len(hits))
	}
}

// TestTier_BumpsRetrievalCount: retrieving an experience increments its
// retrieval count (the prune/following signal).
func TestTier_BumpsRetrievalCount(t *testing.T) {
	backend := NewMemBackend()
	store, _ := NewStore(RuleGatekeeper{}, backend, nil)
	ctx := context.Background()
	_, _ = store.Admit(ctx, admittableExp("bump me"))
	fp := Fingerprint("t1", "bump me")

	tier := NewTier(store, nil)
	id := auth.Identity{TenantID: "t1", UserID: "u1", AgentID: "a1"}
	_, _ = tier.Search(ctx, id, retrieval.Query{Text: "bump", K: 10})
	_, _ = tier.Search(ctx, id, retrieval.Query{Text: "bump", K: 10})

	got, _, _ := backend.GetAdmitted(ctx, "t1", fp, false)
	if got.RetrievalCount != 2 {
		t.Fatalf("retrieval_count = %d after 2 retrievals, want 2", got.RetrievalCount)
	}
}

// TestTier_NoTenantReturnsNothing: an identity with no tenant retrieves nothing
// (fail-closed).
func TestTier_NoTenantReturnsNothing(t *testing.T) {
	store, _ := NewStore(RuleGatekeeper{}, NewMemBackend(), nil)
	_, _ = store.Admit(context.Background(), admittableExp("secret"))
	tier := NewTier(store, nil)
	hits, err := tier.Search(context.Background(), auth.Identity{}, retrieval.Query{Text: "secret", K: 10})
	if err != nil || len(hits) != 0 {
		t.Fatalf("no-tenant search => %d hits, %v; want 0", len(hits), err)
	}
}

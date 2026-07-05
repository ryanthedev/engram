package graph

import (
	"context"
	"testing"
)

// TestDW_3_2_CountAllEntitiesSpansTenants is the Phase-3 regression:
// CountEntities is per-tenant scoped, but the graph telemetry gauge needs an
// all-tenant figure (the DW-6.3 stability signal rendered on /metrics).
// CountAllEntities must count live entities across every tenant, and must
// increment as entities are added (the gauge "tracks the entity count"
// requirement from DW-3.2), while CountEntities keeps its narrower
// per-tenant scope unchanged.
func TestDW_3_2_CountAllEntitiesSpansTenants(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)

	if count, err := store.CountAllEntities(ctx); err != nil || count != 0 {
		t.Fatalf("CountAllEntities on an empty store => (%d, %v), want (0, nil)", count, err)
	}

	if _, _, err := store.UpsertMention(ctx, Mention{TenantID: "t1", Name: "acme", Context: "acme ships widgets", SourceID: "ev-1"}); err != nil {
		t.Fatalf("upsert t1/acme: %v", err)
	}
	if count, err := store.CountAllEntities(ctx); err != nil || count != 1 {
		t.Fatalf("CountAllEntities after 1 entity => (%d, %v), want (1, nil)", count, err)
	}
	// Per-tenant count for t1 must be 1; the OTHER tenant's count must be 0.
	if count, err := store.CountEntities(ctx, "t1"); err != nil || count != 1 {
		t.Fatalf("CountEntities(t1) => (%d, %v), want (1, nil)", count, err)
	}
	if count, err := store.CountEntities(ctx, "t2"); err != nil || count != 0 {
		t.Fatalf("CountEntities(t2) => (%d, %v), want (0, nil)", count, err)
	}

	// A DIFFERENT tenant's entity must still add to the ALL-tenant total —
	// this is exactly what distinguishes CountAllEntities from CountEntities.
	if _, _, err := store.UpsertMention(ctx, Mention{TenantID: "t2", Name: "globex", Context: "globex ships gadgets", SourceID: "ev-2"}); err != nil {
		t.Fatalf("upsert t2/globex: %v", err)
	}
	if count, err := store.CountAllEntities(ctx); err != nil || count != 2 {
		t.Fatalf("CountAllEntities after 2 tenants' entities => (%d, %v), want (2, nil)", count, err)
	}
	if count, err := store.CountEntities(ctx, "t1"); err != nil || count != 1 {
		t.Fatalf("CountEntities(t1) unaffected by t2 => (%d, %v), want (1, nil)", count, err)
	}
}

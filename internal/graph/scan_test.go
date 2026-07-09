package graph

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"
)

// withScanBatchSize shrinks the package-level scanBatchSize var for the
// duration of a test, then restores it — the mechanism DW-1.2's tests use
// to exercise real multi-page pagination without inserting hundreds of
// fixture records.
func withScanBatchSize(t *testing.T, n int) {
	t.Helper()
	old := scanBatchSize
	scanBatchSize = n
	t.Cleanup(func() { scanBatchSize = old })
}

func mkEntity(id, tenantID string) Entity {
	now := time.Now().UTC()
	return Entity{ID: id, NameKey: Fingerprint(tenantID, id), TenantID: tenantID, Scope: "private", Name: id, MentionCount: 1, ValidAt: now, CreatedAt: now}
}

func mkEdge(id, tenantID, from, to string) Edge {
	now := time.Now().UTC()
	return Edge{ID: id, TenantID: tenantID, Scope: "private", FromEntityID: from, ToEntityID: to, Predicate: "relates_to", Statement: from + " relates_to " + to, ValidAt: now, CreatedAt: now}
}

// scanAllEntities drains ScanEntities to exhaustion via the store, guarding
// against an infinite loop (a next cursor that never zeroes) with a hard
// iteration cap well beyond any legitimate test fixture size.
func scanAllEntities(t *testing.T, store *Store, tenantID string) []Entity {
	t.Helper()
	var out []Entity
	cursor := Cursor{}
	for i := 0; i < 10_000; i++ {
		page, next, err := store.ScanEntities(context.Background(), tenantID, cursor)
		if err != nil {
			t.Fatalf("ScanEntities: %v", err)
		}
		out = append(out, page...)
		if next == (Cursor{}) {
			return out
		}
		cursor = next
	}
	t.Fatal("ScanEntities: did not exhaust within iteration cap — possible infinite pagination loop")
	return nil
}

func scanAllEdges(t *testing.T, store *Store, tenantID string) []Edge {
	t.Helper()
	var out []Edge
	cursor := Cursor{}
	for i := 0; i < 10_000; i++ {
		page, next, err := store.ScanEdges(context.Background(), tenantID, cursor)
		if err != nil {
			t.Fatalf("ScanEdges: %v", err)
		}
		out = append(out, page...)
		if next == (Cursor{}) {
			return out
		}
		cursor = next
	}
	t.Fatal("ScanEdges: did not exhaust within iteration cap — possible infinite pagination loop")
	return nil
}

// TestDW_1_1_ScanEntities_ReturnsLiveEntities: the Backend/Store method
// exists and, for a small tenant well under one batch, returns exactly the
// live entities inserted.
func TestDW_1_1_ScanEntities_ReturnsLiveEntities(t *testing.T) {
	ctx := context.Background()
	store, backend := newTestStore(t)
	for _, id := range []string{"e1", "e2", "e3"} {
		if err := backend.PutEntity(ctx, mkEntity(id, "t1")); err != nil {
			t.Fatalf("PutEntity %s: %v", id, err)
		}
	}
	got := scanAllEntities(t, store, "t1")
	if len(got) != 3 {
		t.Fatalf("scanned %d entities, want 3: %+v", len(got), got)
	}
	gotIDs := map[string]bool{}
	for _, e := range got {
		gotIDs[e.ID] = true
	}
	for _, want := range []string{"e1", "e2", "e3"} {
		if !gotIDs[want] {
			t.Errorf("missing entity %q in scan result", want)
		}
	}
}

// TestDW_1_1_ScanEdges_ReturnsLiveEdges is ScanEntities' edge-tier
// counterpart.
func TestDW_1_1_ScanEdges_ReturnsLiveEdges(t *testing.T) {
	ctx := context.Background()
	store, backend := newTestStore(t)
	for _, id := range []string{"ed1", "ed2"} {
		if err := backend.PutEdge(ctx, mkEdge(id, "t1", "a", "b")); err != nil {
			t.Fatalf("PutEdge %s: %v", id, err)
		}
	}
	got := scanAllEdges(t, store, "t1")
	if len(got) != 2 {
		t.Fatalf("scanned %d edges, want 2: %+v", len(got), got)
	}
}

// TestDW_1_1_ScanQueryShape_UsesSortAndSearchAfter pins the OpenSearch
// query shape scanQuery builds: sort by id ascending is always present;
// search_after is present only when resuming (cursor.after != "").
func TestDW_1_1_ScanQueryShape_UsesSortAndSearchAfter(t *testing.T) {
	start := scanQuery("t1", Cursor{}, []any{map[string]any{"exists": map[string]any{"field": "expired_at"}}})
	if _, ok := start["search_after"]; ok {
		t.Errorf("start-of-scan query should not carry search_after: %+v", start)
	}
	sortClause, ok := start["sort"].([]any)
	if !ok || len(sortClause) == 0 {
		t.Fatalf("query missing sort clause: %+v", start)
	}

	resume := scanQuery("t1", Cursor{after: "abc123"}, []any{map[string]any{"exists": map[string]any{"field": "expired_at"}}})
	sa, ok := resume["search_after"].([]any)
	if !ok || len(sa) != 1 || sa[0] != "abc123" {
		t.Errorf("resume query search_after = %+v, want [\"abc123\"]", resume["search_after"])
	}
}

// TestDW_1_2_ScanEntities_PaginatesAcrossMultipleBatches proves the primitive
// never truncates: inserting more entities than one batch, draining via
// the cursor loop returns every one of them exactly once.
func TestDW_1_2_ScanEntities_PaginatesAcrossMultipleBatches(t *testing.T) {
	withScanBatchSize(t, 3)
	ctx := context.Background()
	store, backend := newTestStore(t)

	const total = 10 // > one batch (3), not a clean multiple
	want := map[string]bool{}
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("e-%02d", i)
		if err := backend.PutEntity(ctx, mkEntity(id, "t1")); err != nil {
			t.Fatalf("PutEntity %s: %v", id, err)
		}
		want[id] = true
	}

	got := scanAllEntities(t, store, "t1")
	if len(got) != total {
		t.Fatalf("scanned %d entities across pages, want %d", len(got), total)
	}
	seen := map[string]bool{}
	for _, e := range got {
		if seen[e.ID] {
			t.Errorf("entity %q returned twice across pages — pagination duplicated a record", e.ID)
		}
		seen[e.ID] = true
		if !want[e.ID] {
			t.Errorf("unexpected entity %q in scan result", e.ID)
		}
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("entity %q never surfaced — pagination truncated the tier", id)
		}
	}
}

// TestDW_1_2_ScanEdges_PaginatesAcrossMultipleBatches is the edge-tier
// counterpart.
func TestDW_1_2_ScanEdges_PaginatesAcrossMultipleBatches(t *testing.T) {
	withScanBatchSize(t, 4)
	ctx := context.Background()
	store, backend := newTestStore(t)

	const total = 13
	want := map[string]bool{}
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("ed-%02d", i)
		if err := backend.PutEdge(ctx, mkEdge(id, "t1", "a", "b")); err != nil {
			t.Fatalf("PutEdge %s: %v", id, err)
		}
		want[id] = true
	}

	got := scanAllEdges(t, store, "t1")
	if len(got) != total {
		t.Fatalf("scanned %d edges across pages, want %d", len(got), total)
	}
	seen := map[string]bool{}
	for _, e := range got {
		seen[e.ID] = true
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("edge %q never surfaced — pagination truncated the tier", id)
		}
	}
}

// TestBoundary_ScanEntities_ExactBatchMultiple_ExhaustsCleanly covers the
// plan's "page boundary exactly on batch size" edge case: when the live
// count is an exact multiple of the batch size, the follow-up call must
// return an empty page and a zero (exhausted) cursor, not loop forever or
// error.
func TestBoundary_ScanEntities_ExactBatchMultiple_ExhaustsCleanly(t *testing.T) {
	withScanBatchSize(t, 5)
	ctx := context.Background()
	store, backend := newTestStore(t)
	for i := 0; i < 5; i++ { // exactly one batch
		if err := backend.PutEntity(ctx, mkEntity(fmt.Sprintf("e-%d", i), "t1")); err != nil {
			t.Fatalf("PutEntity: %v", err)
		}
	}

	page1, next1, err := store.ScanEntities(ctx, "t1", Cursor{})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(page1) != 5 {
		t.Fatalf("first page len = %d, want 5", len(page1))
	}
	if next1 == (Cursor{}) {
		t.Fatal("first page's next cursor is zero after a full batch — cannot tell caller to check for more")
	}

	page2, next2, err := store.ScanEntities(ctx, "t1", next1)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(page2) != 0 {
		t.Fatalf("second page len = %d, want 0 (exact-multiple boundary)", len(page2))
	}
	if next2 != (Cursor{}) {
		t.Fatal("second (empty) page's next cursor is non-zero — scan never reports exhaustion")
	}
}

// TestDW_1_3_ScanEntities_ExcludesExpired: only Entity.Live() (ExpiredAt ==
// nil) entities surface.
func TestDW_1_3_ScanEntities_ExcludesExpired(t *testing.T) {
	ctx := context.Background()
	store, backend := newTestStore(t)
	now := time.Now().UTC()

	live := mkEntity("live", "t1")
	expired := mkEntity("expired", "t1")
	expired.ExpiredAt = &now
	for _, e := range []Entity{live, expired} {
		if err := backend.PutEntity(ctx, e); err != nil {
			t.Fatalf("PutEntity: %v", err)
		}
	}

	got := scanAllEntities(t, store, "t1")
	if len(got) != 1 || got[0].ID != "live" {
		t.Fatalf("ScanEntities = %+v, want only the live entity", got)
	}
}

// TestDW_1_3_ScanEdges_ExcludesClosedAndExpired: edges require BOTH
// InvalidAt==nil AND ExpiredAt==nil (Edge.Live()) — a superseded-but-not-
// expired edge (InvalidAt set) is excluded too, distinct from the entity
// test above (entities have no InvalidAt field at all).
func TestDW_1_3_ScanEdges_ExcludesClosedAndExpired(t *testing.T) {
	ctx := context.Background()
	store, backend := newTestStore(t)
	now := time.Now().UTC()

	live := mkEdge("live", "t1", "a", "b")
	closed := mkEdge("closed", "t1", "a", "c")
	closed.InvalidAt = &now
	expired := mkEdge("expired", "t1", "a", "d")
	expired.ExpiredAt = &now
	for _, e := range []Edge{live, closed, expired} {
		if err := backend.PutEdge(ctx, e); err != nil {
			t.Fatalf("PutEdge: %v", err)
		}
	}

	got := scanAllEdges(t, store, "t1")
	if len(got) != 1 || got[0].ID != "live" {
		t.Fatalf("ScanEdges = %+v, want only the live edge", got)
	}
}

// TestDW_1_4_ScanEntities_TenantScoped: tenant A's scan never surfaces
// tenant B's entities.
func TestDW_1_4_ScanEntities_TenantScoped(t *testing.T) {
	ctx := context.Background()
	store, backend := newTestStore(t)
	if err := backend.PutEntity(ctx, mkEntity("a1", "tenant-a")); err != nil {
		t.Fatalf("PutEntity a1: %v", err)
	}
	if err := backend.PutEntity(ctx, mkEntity("b1", "tenant-b")); err != nil {
		t.Fatalf("PutEntity b1: %v", err)
	}

	got := scanAllEntities(t, store, "tenant-a")
	if len(got) != 1 || got[0].ID != "a1" {
		t.Fatalf("tenant-a scan = %+v, want only a1 (no cross-tenant leak)", got)
	}
}

// TestDW_1_4_ScanEdges_TenantScoped is the edge-tier counterpart.
func TestDW_1_4_ScanEdges_TenantScoped(t *testing.T) {
	ctx := context.Background()
	store, backend := newTestStore(t)
	if err := backend.PutEdge(ctx, mkEdge("ea", "tenant-a", "x", "y")); err != nil {
		t.Fatalf("PutEdge ea: %v", err)
	}
	if err := backend.PutEdge(ctx, mkEdge("eb", "tenant-b", "x", "y")); err != nil {
		t.Fatalf("PutEdge eb: %v", err)
	}

	got := scanAllEdges(t, store, "tenant-a")
	if len(got) != 1 || got[0].ID != "ea" {
		t.Fatalf("tenant-a scan = %+v, want only ea (no cross-tenant leak)", got)
	}
}

// TestDW_1_4_ScanEntities_EmptyTenant_NoError: a tenant with zero entities
// (index exists, but nothing for this tenant, or the index has never been
// created at all — MemBackend models both as "no matching docs") returns a
// clean empty result, not an error.
func TestDW_1_4_ScanEntities_EmptyTenant_NoError(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)

	items, next, err := store.ScanEntities(ctx, "no-such-tenant", Cursor{})
	if err != nil {
		t.Fatalf("ScanEntities on empty tenant returned an error: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("ScanEntities on empty tenant returned %d items, want 0", len(items))
	}
	if next != (Cursor{}) {
		t.Fatalf("ScanEntities on empty tenant returned a non-zero next cursor: %+v", next)
	}
}

// TestDW_1_4_Cursor_FromEmptiedIndex_ExhaustsCleanly: a cursor whose
// after-id doesn't (or no longer) match anything in the tier — e.g. every
// record was expired/removed since the cursor was issued — still resolves
// to an empty page and a zero next cursor, never an error or a crash.
func TestDW_1_4_Cursor_FromEmptiedIndex_ExhaustsCleanly(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)

	stale := Cursor{after: "some-id-that-was-once-the-last-page-boundary"}
	items, next, err := store.ScanEntities(ctx, "t1", stale)
	if err != nil {
		t.Fatalf("ScanEntities with a stale/emptied-index cursor errored: %v", err)
	}
	if len(items) != 0 || next != (Cursor{}) {
		t.Fatalf("ScanEntities(stale cursor) = items=%v next=%+v, want empty/zero", items, next)
	}
}

// TestEdgeCase_EntityWithAllEdgesExpired: an entity with live existence but
// whose only edge is expired still surfaces from ScanEntities; ScanEdges
// correctly surfaces no edges for it — no crash, no dangling reference
// handling required at this layer (Scan is tier-independent; it doesn't
// dereference entity<->edge relationships).
func TestEdgeCase_EntityWithAllEdgesExpired(t *testing.T) {
	ctx := context.Background()
	store, backend := newTestStore(t)
	now := time.Now().UTC()

	if err := backend.PutEntity(ctx, mkEntity("lonely", "t1")); err != nil {
		t.Fatalf("PutEntity: %v", err)
	}
	expiredEdge := mkEdge("only-edge", "t1", "lonely", "other")
	expiredEdge.ExpiredAt = &now
	if err := backend.PutEdge(ctx, expiredEdge); err != nil {
		t.Fatalf("PutEdge: %v", err)
	}

	entities := scanAllEntities(t, store, "t1")
	if len(entities) != 1 || entities[0].ID != "lonely" {
		t.Fatalf("ScanEntities = %+v, want the entity to surface despite its only edge being expired", entities)
	}
	edges := scanAllEdges(t, store, "t1")
	if len(edges) != 0 {
		t.Fatalf("ScanEdges = %+v, want none (the only edge is expired)", edges)
	}
}

// TestScanEntities_ResumesFromArbitraryCursor_NoDuplicatesOrGaps is an
// extra robustness check beyond the DW floor: resuming from a cursor
// pointing between two existing ids (not necessarily one previously
// returned by this backend) still walks the remainder of the tier exactly
// once, with correct ordering — the scanPage binary-search resume logic
// working correctly on an arbitrary boundary, not just ones this test
// happened to produce itself.
func TestScanEntities_ResumesFromArbitraryCursor_NoDuplicatesOrGaps(t *testing.T) {
	ctx := context.Background()
	store, backend := newTestStore(t)
	ids := []string{"a", "b", "c", "d", "e"}
	for _, id := range ids {
		if err := backend.PutEntity(ctx, mkEntity(id, "t1")); err != nil {
			t.Fatalf("PutEntity %s: %v", id, err)
		}
	}

	// "bb" sorts strictly between "b" and "c" but matches no real id.
	got, next, err := store.ScanEntities(ctx, "t1", Cursor{after: "bb"})
	if err != nil {
		t.Fatalf("ScanEntities: %v", err)
	}
	if next != (Cursor{}) {
		t.Fatalf("expected exhaustion in one page (default batch size), got next=%+v", next)
	}
	gotIDs := make([]string, len(got))
	for i, e := range got {
		gotIDs[i] = e.ID
	}
	sort.Strings(gotIDs)
	want := []string{"c", "d", "e"}
	if fmt.Sprint(gotIDs) != fmt.Sprint(want) {
		t.Fatalf("resume from arbitrary cursor = %v, want %v", gotIDs, want)
	}
}

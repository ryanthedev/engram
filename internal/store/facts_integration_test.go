//go:build integration

package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/store"
	"github.com/ryanthedev/engram/internal/testutil"
)

// TestScanLiveFacts_Integration_PaginatesRealClusterAndExcludesSuperseded is
// the live-cluster proof behind Phase 3's riskiest new logic: the
// (created_at, content_key) search_after pagination (facts.go's ScanLiveFacts
// doc comment explains why _id sorting was rejected) actually works against
// real OpenSearch, and the live-only filter genuinely excludes a superseded
// fact — not just against the hermetic httptest fake in facts_test.go.
//
// Uses a scratch semantic index (never the fixed production name) so this
// test cannot disturb any other data on a shared dev cluster.
func TestScanLiveFacts_Integration_PaginatesRealClusterAndExcludesSuperseded(t *testing.T) {
	base := testutil.OpenSearchURL()
	if _, err := store.Apply(context.Background(), testutil.HTTPClient, base); err != nil {
		t.Fatalf("applying cluster contract: %v", err)
	}
	semIdx := testutil.ScratchIndexName("semantic", t.Name())
	testutil.DeleteIndex(t, base, semIdx)
	t.Cleanup(func() { testutil.DeleteIndex(t, base, semIdx) })

	s := store.NewOpenSearchStore(testutil.HTTPClient, base, store.WithSemanticIndex(semIdx))
	ctx := context.Background()
	t0 := time.Now().UTC()

	// Three live facts, landed one millisecond apart so (created_at,
	// content_key) ordering is unambiguous.
	live := []memory.SemanticFact{
		{Subject: "A", Predicate: "p", Object: "1", ContentKey: "ck-a", TenantID: "t1", ValidAt: t0, CreatedAt: t0},
		{Subject: "B", Predicate: "p", Object: "2", ContentKey: "ck-b", TenantID: "t1", ValidAt: t0.Add(time.Millisecond), CreatedAt: t0.Add(time.Millisecond)},
		{Subject: "C", Predicate: "p", Object: "3", ContentKey: "ck-c", TenantID: "t1", ValidAt: t0.Add(2 * time.Millisecond), CreatedAt: t0.Add(2 * time.Millisecond)},
	}
	for i, f := range live {
		if err := s.Create(ctx, fmt.Sprintf("live-fact-%d", i), f); err != nil {
			t.Fatalf("Create live fact %d: %v", i, err)
		}
	}
	// A superseded fact (invalid_at set) — must NEVER appear in a
	// ScanLiveFacts page, on a real cluster's live-filter query exactly as
	// asserted at the hermetic level in facts_test.go.
	closedAt := t0.Add(-time.Second)
	superseded := memory.SemanticFact{
		Subject: "Z", Predicate: "p", Object: "9", ContentKey: "ck-z", TenantID: "t1",
		ValidAt: t0.Add(-time.Hour), CreatedAt: t0.Add(-time.Hour), InvalidAt: &closedAt,
	}
	if err := s.Create(ctx, "superseded-fact", superseded); err != nil {
		t.Fatalf("Create superseded fact: %v", err)
	}
	testutil.RefreshIndex(t, base, semIdx)

	// Page through with size=2 so a real multi-page search_after round trip
	// is exercised, not just a single page.
	var got []memory.SemanticFact
	var cursor store.FactCursor
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("ScanLiveFacts did not terminate within 10 pages — pagination is looping")
		}
		facts, next, err := s.ScanLiveFacts(ctx, "t1", cursor, 2)
		if err != nil {
			t.Fatalf("ScanLiveFacts: %v", err)
		}
		for _, vf := range facts {
			got = append(got, vf.Fact)
		}
		if next == (store.FactCursor{}) {
			break
		}
		cursor = next
	}

	if len(got) != len(live) {
		t.Fatalf("ScanLiveFacts returned %d facts, want %d (live only, superseded excluded)", len(got), len(live))
	}
	seen := map[string]bool{}
	for _, f := range got {
		seen[f.Subject] = true
		if f.Subject == "Z" {
			t.Fatal("ScanLiveFacts returned the superseded fact (Z) — the live filter did not exclude it")
		}
	}
	for _, want := range []string{"A", "B", "C"} {
		if !seen[want] {
			t.Errorf("ScanLiveFacts never returned live fact %q across %d page(s)", want, len(got))
		}
	}
}

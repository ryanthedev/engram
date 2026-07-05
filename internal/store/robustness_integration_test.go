//go:build integration

package store_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/store"
	"github.com/ryanthedev/engram/internal/testutil"
)

// TestDW_1_2_FreshSemanticIndexReconcilesAndRetrieves is the end-to-end proof
// on the live cluster: with an overridden, never-before-seen semantic index
// name and NO manual index PUT anywhere, the reconcile order (candidate search
// → ADD) runs to completion and the derived fact is retrievable. This exercises
// the 404-as-empty belt alone — EnsureIndices is deliberately NOT called — so
// the candidate search on the not-yet-created index must be empty (before the
// fix it 404s and the first fact retries forever).
func TestDW_1_2_FreshSemanticIndexReconcilesAndRetrieves(t *testing.T) {
	base := testutil.OpenSearchURL()
	// Apply the contract (index templates + default indices) exactly as the
	// server does on boot. Crucially we never create the fresh semantic index.
	if _, err := store.Apply(context.Background(), testutil.HTTPClient, base); err != nil {
		t.Fatalf("applying cluster contract: %v", err)
	}
	semIdx := testutil.ScratchIndexName("semantic", t.Name())
	epIdx := testutil.ScratchIndexName("episodic", t.Name())
	// Guarantee both indices are absent — no PUT of the semantic index occurs
	// in this test (that is the whole point).
	for _, idx := range []string{semIdx, epIdx} {
		testutil.DeleteIndex(t, base, idx)
		t.Cleanup(func(idx string) func() { return func() { testutil.DeleteIndex(t, base, idx) } }(idx))
	}

	s := store.NewOpenSearchStore(testutil.HTTPClient, base,
		store.WithSemanticIndex(semIdx), store.WithEpisodicIndex(epIdx))
	ctx := context.Background()
	f := memory.SemanticFact{
		Subject: "svc", Predicate: "owner", Object: "ana",
		Statement: "svc owner ana", ContentKey: "ck-fresh-1", TenantID: "t1",
		ValidAt: time.Now().UTC(),
	}

	// Reconcile step 1: the candidate search on the not-yet-created semantic
	// index. Empty (not a 404 error) is what lets the worker proceed to ADD.
	cands, err := s.Candidates(ctx, f, 10)
	if err != nil {
		t.Fatalf("Candidates on a fresh semantic index = %v, want empty + nil (the bug)", err)
	}
	if len(cands) != 0 {
		t.Fatalf("Candidates on a fresh semantic index = %d, want 0", len(cands))
	}

	// Reconcile step 2 (ADD): Create writes the fact, auto-creating the index,
	// which inherits the engram-semantic* template Apply PUT above.
	const id = "fact-fresh-1"
	if err := s.Create(ctx, id, f); err != nil {
		t.Fatalf("Create (ADD) on the fresh index: %v", err)
	}
	testutil.RefreshIndex(t, base, semIdx)

	// The derived fact is retrievable with no manual index PUT anywhere.
	cands, err = s.Candidates(ctx, f, 10)
	if err != nil {
		t.Fatalf("Candidates after ADD: %v", err)
	}
	found := false
	for _, c := range cands {
		if c.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("derived fact not retrievable after fresh-index ingest: %+v", cands)
	}
}

// TestDW_1_3_EnsureIndicesCreatesConfiguredAndIdempotent proves the boot-ensure
// suspenders: EnsureIndices materializes the configured episodic/semantic/
// ledger override names (which then exist on the cluster), and a re-run is a
// pure no-op — every index reports "unchanged".
func TestDW_1_3_EnsureIndicesCreatesConfiguredAndIdempotent(t *testing.T) {
	base := testutil.OpenSearchURL()
	// EnsureIndices assumes the templates are already applied (server boot runs
	// Apply first) so the created indices inherit the engram-<tier>* mapping.
	if _, err := store.Apply(context.Background(), testutil.HTTPClient, base); err != nil {
		t.Fatalf("applying cluster contract: %v", err)
	}
	epIdx := testutil.ScratchIndexName("episodic", t.Name())
	semIdx := testutil.ScratchIndexName("semantic", t.Name())
	ledIdx := testutil.ScratchIndexName("ledger", t.Name())
	configured := []string{epIdx, semIdx, ledIdx}
	for _, idx := range configured {
		testutil.DeleteIndex(t, base, idx)
		t.Cleanup(func(idx string) func() { return func() { testutil.DeleteIndex(t, base, idx) } }(idx))
	}

	s := store.NewOpenSearchStore(testutil.HTTPClient, base,
		store.WithEpisodicIndex(epIdx), store.WithSemanticIndex(semIdx), store.WithLedgerIndex(ledIdx))
	ctx := context.Background()

	first, err := s.EnsureIndices(ctx)
	if err != nil {
		t.Fatalf("first EnsureIndices: %v", err)
	}
	for _, idx := range configured {
		if first[idx] != "created" {
			t.Errorf("first EnsureIndices: %s action = %q, want created", idx, first[idx])
		}
		if status, _ := testutil.Call(t, http.MethodHead, base+"/"+idx, nil); status != http.StatusOK {
			t.Errorf("index %s not present after EnsureIndices (HEAD = %d)", idx, status)
		}
	}

	second, err := s.EnsureIndices(ctx)
	if err != nil {
		t.Fatalf("second EnsureIndices: %v", err)
	}
	for _, idx := range configured {
		if second[idx] != "unchanged" {
			t.Errorf("re-run EnsureIndices: %s action = %q, want unchanged (idempotent)", idx, second[idx])
		}
	}
}

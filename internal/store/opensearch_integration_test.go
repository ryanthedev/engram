//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/store"
	"github.com/ryanthedev/engram/internal/testutil"
)

// liveStore applies the cluster contract and returns an OpenSearchStore
// pointed at fresh scratch indices unique to name, so this test can't
// collide with any other package's integration test sharing the cluster.
func liveStore(t *testing.T, name string) (s *store.OpenSearchStore, base, epIdx, semIdx string) {
	t.Helper()
	base = testutil.OpenSearchURL()
	if _, err := store.Apply(context.Background(), testutil.HTTPClient, base); err != nil {
		t.Fatalf("applying cluster contract: %v", err)
	}
	epIdx = testutil.ScratchIndexName("episodic", name)
	semIdx = testutil.ScratchIndexName("semantic", name)
	for _, idx := range []string{epIdx, semIdx} {
		testutil.DeleteIndex(t, base, idx)
		t.Cleanup(func(idx string) func() { return func() { testutil.DeleteIndex(t, base, idx) } }(idx))
		testutil.CreateScratchIndex(t, base, idx)
	}
	return store.NewOpenSearchStore(testutil.HTTPClient, base, store.WithEpisodicIndex(epIdx), store.WithSemanticIndex(semIdx)), base, epIdx, semIdx
}

// TestOpenSearchStoreLiveAppendCreateUpdate proves the write-protocol
// primitives the Phase-0 spikes validated raw (op_type=create 409, guarded
// update 409) behave identically through OpenSearchStore against the real
// pinned cluster.
func TestOpenSearchStoreLiveAppendCreateUpdate(t *testing.T) {
	s, base, _, semIdx := liveStore(t, t.Name())
	ctx := context.Background()

	id, err := s.Append(ctx, memory.Episodic{EventID: "ev-live-1", Text: "live append test", TenantID: "t1", CreatedAt: time.Now()})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if id == "" {
		t.Fatal("Append returned empty id")
	}

	fact := memory.SemanticFact{Statement: "v1", ContentKey: "ck-live-1", ValidAt: time.Now()}
	if err := s.Create(ctx, "fact-live-1", fact); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Create(ctx, "fact-live-1", fact); !errors.Is(err, store.ErrConflict) {
		t.Errorf("duplicate Create = %v, want ErrConflict", err)
	}

	seqNo, primaryTerm := testutil.GetSeqNo(t, base, semIdx, "fact-live-1")
	if err := s.Update(ctx, "fact-live-1", fact, seqNo, primaryTerm); err != nil {
		t.Fatalf("guarded Update with fresh tokens: %v", err)
	}
	if err := s.Update(ctx, "fact-live-1", fact, seqNo, primaryTerm); !errors.Is(err, store.ErrConflict) {
		t.Errorf("stale-guard Update = %v, want ErrConflict", err)
	}
}

// TestOpenSearchStoreLiveEnrichmentPlumbing proves FindUnembedded/
// SetTextEmbedding work against the real cluster: a text-only append is
// found pending, and disappears from the pending scan once embedded.
func TestOpenSearchStoreLiveEnrichmentPlumbing(t *testing.T) {
	s, base, epIdx, _ := liveStore(t, t.Name())
	ctx := context.Background()

	id, err := s.Append(ctx, memory.Episodic{EventID: "ev-enrich-1", Text: "needs an embedding", CreatedAt: time.Now()})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	testutil.RefreshIndex(t, base, epIdx)

	pending, err := s.FindUnembedded(ctx, 10)
	if err != nil {
		t.Fatalf("FindUnembedded: %v", err)
	}
	found := false
	for _, p := range pending {
		if p.DocID == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("FindUnembedded = %+v, want an entry for %s", pending, id)
	}

	vec := make([]float32, store.EmbeddingDim)
	vec[0] = 1
	if err := s.SetTextEmbedding(ctx, id, vec); err != nil {
		t.Fatalf("SetTextEmbedding: %v", err)
	}
	testutil.RefreshIndex(t, base, epIdx)

	pending, err = s.FindUnembedded(ctx, 10)
	if err != nil {
		t.Fatalf("FindUnembedded after fill: %v", err)
	}
	for _, p := range pending {
		if p.DocID == id {
			t.Errorf("doc %s still reported unembedded after SetTextEmbedding", id)
		}
	}
}

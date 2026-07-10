//go:build integration

package retrieval_test

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/retrieval"
	"github.com/ryanthedev/engram/internal/store"
	"github.com/ryanthedev/engram/internal/testutil"
)

func applyLive(t *testing.T) string {
	t.Helper()
	base := testutil.OpenSearchURL()
	if _, err := store.Apply(context.Background(), testutil.HTTPClient, base); err != nil {
		t.Fatalf("applying cluster contract: %v", err)
	}
	return base
}

// scratchIndices creates and registers cleanup for a fresh episodic +
// semantic scratch index pair unique to name.
func scratchIndices(t *testing.T, base, name string) (epIdx, semIdx string) {
	t.Helper()
	epIdx = testutil.ScratchIndexName("episodic", name)
	semIdx = testutil.ScratchIndexName("semantic", name)
	for _, idx := range []string{epIdx, semIdx} {
		testutil.DeleteIndex(t, base, idx)
		t.Cleanup(func(idx string) func() { return func() { testutil.DeleteIndex(t, base, idx) } }(idx))
		testutil.CreateScratchIndex(t, base, idx)
	}
	return epIdx, semIdx
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// TestDW_1_2_HybridFusionOutranksSingleSignal is the Retriever-level version
// of the Phase-0 RRF spike: a doc matching both the BM25 and kNN clauses
// must fuse to rank 1, ahead of docs matching only one signal, and the fused
// list must be in descending score order (DW-1.2).
func TestDW_1_2_HybridFusionOutranksSingleSignal(t *testing.T) {
	base := applyLive(t)
	epIdx, semIdx := scratchIndices(t, base, t.Name())

	queryText := "orders-svc connection pool leak"
	fixtures := map[string]string{queryText: "vk-both"}
	embedder := embed.NewFakeEmbedder(store.EmbeddingDim, fixtures)

	bothVec, err := embedder.Embed(context.Background(), []string{"vk-both"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	otherVec, err := embedder.Embed(context.Background(), []string{"vk-other"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}

	docs := []struct {
		id, statement string
		vec           []float32
	}{
		{"both", "orders-svc connection pool leak incident postmortem", bothVec[0]},
		{"bm25-only", "orders-svc connection pool leak in the incident", otherVec[0]},
		{"knn-only", "checkout latency regression during deploy", bothVec[0]},
		{"neither", "quarterly finance report for the platform org", otherVec[0]},
	}
	for _, d := range docs {
		testutil.IndexDoc(t, base, semIdx, d.id, map[string]any{
			"statement":      d.statement,
			"fact_embedding": d.vec,
			"tenant_id":      "dw12",
			"content_key":    "ck-" + d.id,
			"valid_at":       nowRFC3339(),
			"created_at":     nowRFC3339(),
		})
	}
	testutil.RefreshIndex(t, base, semIdx)

	r := retrieval.NewOpenSearchRetriever(testutil.HTTPClient, base, embedder, retrieval.WithIndices(epIdx, semIdx))
	hits, err := r.Search(context.Background(), retrieval.Query{Text: queryText, K: 4}, retrieval.Filter{TenantID: "dw12"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("hybrid search returned no hits")
	}
	if hits[0].ID != "both" {
		t.Errorf("fused rank 1 = %s, want both", hits[0].ID)
	}
	for i := 1; i < len(hits); i++ {
		if hits[i].Score > hits[i-1].Score {
			t.Errorf("fused scores not descending at %d: %+v", i, hits)
		}
	}
	t.Logf("finding: fused order=%v", hitIDs(hits))
}

func hitIDs(hits []retrieval.Hit) []string {
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.ID
	}
	return ids
}

// TestDW_1_4_ScopeAndValidityFiltersEnforced covers the correctness half of
// DW-1.4: tenant scoping never leaks another tenant's doc, ValidOnly excludes
// an invalidated fact while keeping the live one, and Filter.UserID (mapped
// onto owner_agent_id) scopes correctly.
func TestDW_1_4_ScopeAndValidityFiltersEnforced(t *testing.T) {
	base := applyLive(t)
	epIdx, semIdx := scratchIndices(t, base, t.Name())

	past := time.Now().UTC().Add(-time.Hour)
	closed := past.Add(time.Minute)
	testutil.IndexDoc(t, base, semIdx, "valid-a", map[string]any{
		"statement": "scope test alpha", "tenant_id": "tenant-a", "owner_agent_id": "agent-1",
		"content_key": "ck-valid-a", "valid_at": past.Format(time.RFC3339Nano), "created_at": past.Format(time.RFC3339Nano),
	})
	testutil.IndexDoc(t, base, semIdx, "invalid-a", map[string]any{
		"statement": "scope test alpha", "tenant_id": "tenant-a", "owner_agent_id": "agent-1",
		"content_key": "ck-invalid-a", "valid_at": past.Format(time.RFC3339Nano),
		"invalid_at": closed.Format(time.RFC3339Nano), "created_at": past.Format(time.RFC3339Nano),
	})
	testutil.IndexDoc(t, base, semIdx, "valid-b", map[string]any{
		"statement": "scope test alpha", "tenant_id": "tenant-b", "owner_agent_id": "agent-2",
		"content_key": "ck-valid-b", "valid_at": past.Format(time.RFC3339Nano), "created_at": past.Format(time.RFC3339Nano),
	})
	testutil.RefreshIndex(t, base, semIdx)

	embedder := embed.NewFakeEmbedder(store.EmbeddingDim, nil)
	r := retrieval.NewOpenSearchRetriever(testutil.HTTPClient, base, embedder, retrieval.WithIndices(epIdx, semIdx))
	ctx := context.Background()

	hits, err := r.Search(ctx, retrieval.Query{Text: "scope test alpha", K: 10}, retrieval.Filter{TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("Search(tenant-a): %v", err)
	}
	for _, h := range hits {
		if h.ID == "valid-b" {
			t.Error("tenant filter leaked tenant-b's doc")
		}
	}

	hits, err = r.Search(ctx, retrieval.Query{Text: "scope test alpha", K: 10}, retrieval.Filter{TenantID: "tenant-a", ValidOnly: true})
	if err != nil {
		t.Fatalf("Search(ValidOnly): %v", err)
	}
	var foundValid bool
	for _, h := range hits {
		if h.ID == "invalid-a" {
			t.Error("ValidOnly leaked an invalidated fact")
		}
		if h.ID == "valid-a" {
			foundValid = true
		}
	}
	if !foundValid {
		t.Errorf("ValidOnly excluded the still-valid fact; got %v", hitIDs(hits))
	}

	hits, err = r.Search(ctx, retrieval.Query{Text: "scope test alpha", K: 10}, retrieval.Filter{UserID: "agent-2"})
	if err != nil {
		t.Fatalf("Search(UserID): %v", err)
	}
	for _, h := range hits {
		if h.ID != "valid-b" {
			t.Errorf("UserID filter returned %s, want only valid-b", h.ID)
		}
	}
}

type selectivityDoc struct {
	id, tenant string
	vec        []float32
}

// TestDW_1_4_FilteredKNNRecallAtSelectivities is the Retriever-level version
// of the Phase-0 filtered-kNN spike (efficient_filter, filter inside the knn
// clause): recall against local brute-force truth must not collapse at
// three filter selectivities (DW-1.4).
func TestDW_1_4_FilteredKNNRecallAtSelectivities(t *testing.T) {
	base := applyLive(t)
	epIdx, semIdx := scratchIndices(t, base, t.Name())

	embedder := embed.NewFakeEmbedder(store.EmbeddingDim, nil)
	const n = 300
	const k = 10
	docs := make([]selectivityDoc, n)
	for i := 0; i < n; i++ {
		vecs, err := embedder.Embed(context.Background(), []string{fmt.Sprintf("vec-seed-%d", 1000+i)})
		if err != nil {
			t.Fatalf("embed: %v", err)
		}
		tenant := "rest"
		switch {
		case i%50 == 0:
			tenant = "rare" // 2%
		case i%10 < 1:
			tenant = "uncommon" // ~8%
		case i%2 == 0:
			tenant = "common" // ~44%
		}
		docs[i] = selectivityDoc{id: fmt.Sprintf("d%d", i), tenant: tenant, vec: vecs[0]}
		testutil.IndexDoc(t, base, semIdx, docs[i].id, map[string]any{
			"statement":      "synthetic selectivity doc",
			"fact_embedding": docs[i].vec,
			"tenant_id":      tenant,
			"content_key":    "ck-" + docs[i].id,
			"valid_at":       nowRFC3339(),
			"created_at":     nowRFC3339(),
		})
	}
	testutil.RefreshIndex(t, base, semIdx)

	queryVecs, err := embedder.Embed(context.Background(), []string{"vec-seed-7"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	queryVec := queryVecs[0]

	r := retrieval.NewOpenSearchRetriever(testutil.HTTPClient, base, embedder,
		retrieval.WithIndices(epIdx, semIdx), retrieval.WithMode(retrieval.ModeKNNOnly))

	for _, tenant := range []string{"common", "uncommon", "rare"} {
		matching := 0
		for _, d := range docs {
			if d.tenant == tenant {
				matching++
			}
		}
		truth := bruteForceTopK(docs, tenant, queryVec, k)

		hits, err := r.Search(context.Background(), retrieval.Query{Text: "vec-seed-7", K: k}, retrieval.Filter{TenantID: tenant})
		if err != nil {
			t.Fatalf("Search(%s): %v", tenant, err)
		}
		// Post-Search hits are projected (tenant_id stripped), so the leak
		// check goes through the seeded id->tenant mapping.
		tenantByID := make(map[string]string, len(docs))
		for _, d := range docs {
			tenantByID[d.id] = d.tenant
		}
		got := map[string]bool{}
		for _, h := range hits {
			got[h.ID] = true
			if tid := tenantByID[h.ID]; tid != tenant {
				t.Errorf("filter leak: hit %s was seeded with tenant_id=%q, want %s", h.ID, tid, tenant)
			}
		}
		found := 0
		for _, id := range truth {
			if got[id] {
				found++
			}
		}
		want := min(k, matching)
		recall := float64(found) / float64(want)
		selectivity := float64(matching) / float64(n)
		t.Logf("finding: tenant=%s selectivity=%.1f%% (%d/%d docs) recall@%d=%.2f (%d/%d)",
			tenant, 100*selectivity, matching, n, k, recall, found, want)
		if recall < 0.9 {
			t.Errorf("filtered-kNN recall collapsed at %.1f%% selectivity: %.2f < 0.9", 100*selectivity, recall)
		}
	}
}

func bruteForceTopK(docs []selectivityDoc, tenant string, q []float32, k int) []string {
	type scored struct {
		id string
		s  float64
	}
	var all []scored
	for _, d := range docs {
		if d.tenant == tenant {
			all = append(all, scored{d.id, dot(q, d.vec)})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].s > all[j].s })
	if len(all) > k {
		all = all[:k]
	}
	ids := make([]string, len(all))
	for i, s := range all {
		ids[i] = s.id
	}
	return ids
}

func dot(a, b []float32) float64 {
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

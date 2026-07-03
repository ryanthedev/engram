//go:build integration

package spike

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/ryanthedev/engram/internal/store"
)

// TestDW_0_9b_RRFPipelineShape is spike 2: the exact hybrid-query + RRF
// pipeline shape Phase 1 will use, against the live cluster. It indexes docs
// into an index cut from the real semantic template (the engram-semantic*
// pattern), runs a hybrid query (BM25 clause + kNN clause) through the
// engram-rrf pipeline, and asserts one fused, descending-ranked list where a
// doc matching BOTH signals outranks single-signal docs.
func TestDW_0_9b_RRFPipelineShape(t *testing.T) {
	base := cluster(t)
	idx := "engram-semantic-spike" // matches the engram-semantic* template pattern
	deleteIndex(t, base, idx)
	defer deleteIndex(t, base, idx)
	if status, body := call(t, http.MethodPut, base+"/"+idx, nil); status != http.StatusOK {
		t.Fatalf("creating templated spike index: %d %v", status, body)
	}

	queryVec := vec(1)
	docs := []struct {
		id        string
		statement string
		vecSeed   int64
		vecBlend  float64 // 1.0 = identical to query vector
	}{
		{"both", "orders-svc connection pool leak incident postmortem", 1, 0.95},
		{"bm25-only", "orders-svc connection pool leak in the incident", 99, 0.0},
		{"knn-only", "checkout latency regression during deploy", 1, 0.90},
		{"neither", "quarterly finance report for the platform org", 42, 0.0},
	}
	for _, d := range docs {
		v := vec(d.vecSeed)
		if d.vecBlend > 0 {
			for i := range v {
				v[i] = float32(d.vecBlend)*queryVec[i] + float32(1-d.vecBlend)*v[i]
			}
		}
		indexDoc(t, base, idx, d.id, map[string]any{
			"statement":      d.statement,
			"fact_embedding": v,
			"tenant_id":      "t1",
			"content_key":    "ck-" + d.id,
		})
	}
	refresh(t, base, idx)

	status, body := call(t, http.MethodPost,
		fmt.Sprintf("%s/%s/_search?search_pipeline=%s", base, idx, store.RRFPipelineName),
		map[string]any{
			"size": 4,
			"query": map[string]any{
				"hybrid": map[string]any{
					"queries": []any{
						map[string]any{"match": map[string]any{"statement": "orders-svc connection pool leak"}},
						map[string]any{"knn": map[string]any{"fact_embedding": map[string]any{"vector": queryVec, "k": 4}}},
					},
				},
			},
		})
	if status != http.StatusOK {
		t.Fatalf("hybrid RRF search: %d %v", status, body)
	}

	hits, _ := dig(body, "hits", "hits").([]any)
	if len(hits) == 0 {
		t.Fatal("hybrid RRF search returned no hits")
	}
	var order []string
	var scores []float64
	for _, h := range hits {
		hm := h.(map[string]any)
		order = append(order, hm["_id"].(string))
		scores = append(scores, hm["_score"].(float64))
	}
	t.Logf("finding: fused list order=%v scores=%v", order, scores)

	for i := 1; i < len(scores); i++ {
		if scores[i] > scores[i-1] {
			t.Errorf("fused scores not descending at %d: %v", i, scores)
		}
	}
	if order[0] != "both" {
		t.Errorf("doc matching both signals should fuse to rank 1, got %v", order)
	}
	// RRF scores are rank-based: with rank_constant=60 the max per signal is
	// 1/61 ≈ 0.0164; a doc topping both lists scores ≈ 2/61 ≈ 0.0328.
	if scores[0] > 2.0/60.0 || scores[0] < 1.0/62.0 {
		t.Errorf("top RRF score %v outside rank-based range — pipeline may not be applying RRF", scores[0])
	}
	t.Logf("finding: top score %.5f consistent with rank-based RRF (rank_constant=60: both-signals ≈ 2/61 = %.5f)",
		scores[0], 2.0/61.0)
}

//go:build integration

package spike

import (
	"fmt"
	"net/http"
	"sort"
	"testing"

	"github.com/ryanthedev/engram/internal/store"
)

// TestDW_0_9c_FilteredKNNRecall is spike 3: filtered-kNN recall on the exact
// Faiss HNSW + SQfp16 config the templates pin, at three filter
// selectivities. Post-filtering collapses recall as filters tighten; the
// filter-inside-knn-clause form (efficient filtering) must not. Ground truth
// is local brute-force inner product over the matching docs.
func TestDW_0_9c_FilteredKNNRecall(t *testing.T) {
	base := cluster(t)
	idx := "engram-spike-knn"
	deleteIndex(t, base, idx)
	defer deleteIndex(t, base, idx)

	// Same vector config as the checked-in templates (1024-dim, Faiss HNSW
	// m=16/ef_construction=128, SQfp16), plus a keyword field to filter on.
	status, body := call(t, http.MethodPut, base+"/"+idx, map[string]any{
		"settings": map[string]any{"index.knn": true, "number_of_shards": 1, "number_of_replicas": 0},
		"mappings": map[string]any{
			"properties": map[string]any{
				"bucket": map[string]any{"type": "keyword"},
				"v": map[string]any{
					"type": "knn_vector", "dimension": store.EmbeddingDim,
					"method": map[string]any{
						"name": "hnsw", "engine": "faiss", "space_type": "innerproduct",
						"parameters": map[string]any{
							"m": 16, "ef_construction": 128,
							"encoder": map[string]any{"name": "sq", "parameters": map[string]any{"type": "fp16"}},
						},
					},
				},
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("creating knn spike index: %d %v", status, body)
	}

	// 300 docs; bucket assignment yields ~50%, ~10%, ~2% selectivities.
	const n = 300
	const k = 10
	vecs := make([][]float32, n)
	buckets := make([]string, n)
	for i := 0; i < n; i++ {
		vecs[i] = vec(int64(1000 + i))
		switch {
		case i%50 == 0:
			buckets[i] = "rare" // 2%
		case i%10 < 1:
			buckets[i] = "uncommon" // ~8%
		case i%2 == 0:
			buckets[i] = "common" // ~44%
		default:
			buckets[i] = "rest"
		}
		indexDoc(t, base, idx, fmt.Sprintf("d%d", i), map[string]any{"v": vecs[i], "bucket": buckets[i]})
	}
	refresh(t, base, idx)

	queryVec := vec(7)
	for _, bucket := range []string{"common", "uncommon", "rare"} {
		matching := 0
		for _, b := range buckets {
			if b == bucket {
				matching++
			}
		}
		truth := bruteForceTopK(vecs, buckets, bucket, queryVec, k)

		status, body := call(t, http.MethodPost, base+"/"+idx+"/_search", map[string]any{
			"size": k,
			"query": map[string]any{
				"knn": map[string]any{
					"v": map[string]any{
						"vector": queryVec,
						"k":      k,
						"filter": map[string]any{"term": map[string]any{"bucket": bucket}},
					},
				},
			},
		})
		if status != http.StatusOK {
			t.Fatalf("filtered kNN (%s): %d %v", bucket, status, body)
		}
		hits, _ := dig(body, "hits", "hits").([]any)
		got := map[string]bool{}
		for _, h := range hits {
			hm := h.(map[string]any)
			got[hm["_id"].(string)] = true
			if b := dig(hm, "_source", "bucket"); b != bucket {
				t.Errorf("filter leak: hit %v has bucket %v, want %v", hm["_id"], b, bucket)
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
		t.Logf("finding: bucket=%s selectivity=%.1f%% (%d/%d docs) recall@%d=%.2f (%d/%d)",
			bucket, 100*selectivity, matching, n, k, recall, found, want)
		if recall < 0.9 {
			t.Errorf("filtered-kNN recall collapsed at %.1f%% selectivity: %.2f < 0.9", 100*selectivity, recall)
		}
	}
}

// bruteForceTopK computes exact top-k doc ids by inner product among docs in
// the bucket.
func bruteForceTopK(vecs [][]float32, buckets []string, bucket string, q []float32, k int) []string {
	type scored struct {
		id string
		s  float64
	}
	var all []scored
	for i, v := range vecs {
		if buckets[i] == bucket {
			all = append(all, scored{fmt.Sprintf("d%d", i), dot(q, v)})
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

package retrieval

import "testing"

// TestDW_1_3_BuildQueryGoldenMatrix is the behavior-preservation guardrail for
// the buildQuery positional-params -> options-struct refactor. Every `want`
// body below was CAPTURED from the unmodified, pre-refactor buildQuery for the
// same inputs (2026-07-23), and `wantPipeline` pins the second return value —
// this is a golden-byte comparison against the old implementation, not a
// re-derivation of the one under test.
//
// Matrix: {BM25Only, KNNOnly, Hybrid} x {filters nil/set} x {sort nil/set},
// plus three edge cells (hybrid/knn without a vector, empty-text filter-only).
func TestDW_1_3_BuildQueryGoldenMatrix(t *testing.T) {
	vec := []float32{0.1, 0.25, -0.5}
	filters := []any{map[string]any{"term": map[string]any{"source": "arxiv"}}}
	sortKeys := []any{map[string]any{"published": map[string]any{"order": "desc"}}}

	cases := []struct {
		name         string
		mode         SearchMode
		text         string
		vec          []float32
		filters      []any
		sort         []any
		want         string
		wantPipeline bool
	}{
		{
			name: "BM25Only/no filters/no sort", mode: ModeBM25Only,
			want: `{"_source":{"excludes":["text_embedding","fact_embedding"]},"query":{"match":{"abstract":"quantum cache"}},"size":7}`,
		},
		{
			name: "BM25Only/no filters/sort", mode: ModeBM25Only, sort: sortKeys,
			want: `{"_source":{"excludes":["text_embedding","fact_embedding"]},"query":{"match":{"abstract":"quantum cache"}},"size":7,"sort":[{"published":{"order":"desc"}}]}`,
		},
		{
			name: "BM25Only/filters/no sort", mode: ModeBM25Only, filters: filters,
			want: `{"_source":{"excludes":["text_embedding","fact_embedding"]},"query":{"bool":{"filter":[{"term":{"source":"arxiv"}}],"must":[{"match":{"abstract":"quantum cache"}}]}},"size":7}`,
		},
		{
			name: "BM25Only/filters/sort", mode: ModeBM25Only, filters: filters, sort: sortKeys,
			want: `{"_source":{"excludes":["text_embedding","fact_embedding"]},"query":{"bool":{"filter":[{"term":{"source":"arxiv"}}],"must":[{"match":{"abstract":"quantum cache"}}]}},"size":7,"sort":[{"published":{"order":"desc"}}]}`,
		},
		{
			name: "KNNOnly/no filters/no sort", mode: ModeKNNOnly, vec: vec,
			want: `{"_source":{"excludes":["text_embedding","fact_embedding"]},"query":{"knn":{"text_embedding":{"k":7,"vector":[0.1,0.25,-0.5]}}},"size":7}`,
		},
		{
			name: "KNNOnly/no filters/sort", mode: ModeKNNOnly, vec: vec, sort: sortKeys,
			want: `{"_source":{"excludes":["text_embedding","fact_embedding"]},"query":{"knn":{"text_embedding":{"k":7,"vector":[0.1,0.25,-0.5]}}},"size":7,"sort":[{"published":{"order":"desc"}}]}`,
		},
		{
			name: "KNNOnly/filters/no sort", mode: ModeKNNOnly, vec: vec, filters: filters,
			want: `{"_source":{"excludes":["text_embedding","fact_embedding"]},"query":{"knn":{"text_embedding":{"filter":{"bool":{"filter":[{"term":{"source":"arxiv"}}]}},"k":7,"vector":[0.1,0.25,-0.5]}}},"size":7}`,
		},
		{
			name: "KNNOnly/filters/sort", mode: ModeKNNOnly, vec: vec, filters: filters, sort: sortKeys,
			want: `{"_source":{"excludes":["text_embedding","fact_embedding"]},"query":{"knn":{"text_embedding":{"filter":{"bool":{"filter":[{"term":{"source":"arxiv"}}]}},"k":7,"vector":[0.1,0.25,-0.5]}}},"size":7,"sort":[{"published":{"order":"desc"}}]}`,
		},
		{
			name: "Hybrid/no filters/no sort", mode: ModeHybrid, vec: vec, wantPipeline: true,
			want: `{"_source":{"excludes":["text_embedding","fact_embedding"]},"query":{"hybrid":{"queries":[{"match":{"abstract":"quantum cache"}},{"knn":{"text_embedding":{"k":7,"vector":[0.1,0.25,-0.5]}}}]}},"size":7}`,
		},
		{
			name: "Hybrid/no filters/sort", mode: ModeHybrid, vec: vec, sort: sortKeys, wantPipeline: true,
			want: `{"_source":{"excludes":["text_embedding","fact_embedding"]},"query":{"hybrid":{"queries":[{"match":{"abstract":"quantum cache"}},{"knn":{"text_embedding":{"k":7,"vector":[0.1,0.25,-0.5]}}}]}},"size":7,"sort":[{"published":{"order":"desc"}}]}`,
		},
		{
			name: "Hybrid/filters/no sort", mode: ModeHybrid, vec: vec, filters: filters, wantPipeline: true,
			want: `{"_source":{"excludes":["text_embedding","fact_embedding"]},"query":{"hybrid":{"queries":[{"bool":{"filter":[{"term":{"source":"arxiv"}}],"must":[{"match":{"abstract":"quantum cache"}}]}},{"knn":{"text_embedding":{"filter":{"bool":{"filter":[{"term":{"source":"arxiv"}}]}},"k":7,"vector":[0.1,0.25,-0.5]}}}]}},"size":7}`,
		},
		{
			name: "Hybrid/filters/sort", mode: ModeHybrid, vec: vec, filters: filters, sort: sortKeys, wantPipeline: true,
			want: `{"_source":{"excludes":["text_embedding","fact_embedding"]},"query":{"hybrid":{"queries":[{"bool":{"filter":[{"term":{"source":"arxiv"}}],"must":[{"match":{"abstract":"quantum cache"}}]}},{"knn":{"text_embedding":{"filter":{"bool":{"filter":[{"term":{"source":"arxiv"}}]}},"k":7,"vector":[0.1,0.25,-0.5]}}}]}},"size":7,"sort":[{"published":{"order":"desc"}}]}`,
		},
		{
			name: "Hybrid without vector falls back to BM25, no pipeline", mode: ModeHybrid,
			want: `{"_source":{"excludes":["text_embedding","fact_embedding"]},"query":{"match":{"abstract":"quantum cache"}},"size":7}`,
		},
		{
			name: "KNNOnly without vector falls back to BM25, no pipeline", mode: ModeKNNOnly,
			want: `{"_source":{"excludes":["text_embedding","fact_embedding"]},"query":{"match":{"abstract":"quantum cache"}},"size":7}`,
		},
		{
			name: "empty text is filter-only match_all", mode: ModeBM25Only, text: "-", filters: filters, sort: sortKeys,
			want: `{"_source":{"excludes":["text_embedding","fact_embedding"]},"query":{"bool":{"filter":[{"term":{"source":"arxiv"}}],"must":[{"match_all":{}}]}},"size":7,"sort":[{"published":{"order":"desc"}}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := "quantum cache"
			if tc.text == "-" { // sentinel: the empty-text cell
				text = ""
			}
			body, usePipeline := buildQuery(queryOpts{
				mode: tc.mode, textField: "abstract", vectorField: "text_embedding",
				text: text, vec: tc.vec, k: 7, filters: tc.filters, sort: tc.sort,
			})
			if string(body) != tc.want {
				t.Errorf("buildQuery body =\n%s\nwant (pre-refactor golden)\n%s", body, tc.want)
			}
			if usePipeline != tc.wantPipeline {
				t.Errorf("buildQuery usePipeline = %v, want %v (pre-refactor golden)", usePipeline, tc.wantPipeline)
			}
		})
	}
}

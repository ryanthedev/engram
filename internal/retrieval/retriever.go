// Package retrieval defines the Retriever seam: hybrid BM25 + kNN search
// fused by RRF (D1), with validity and tenancy filters applied inside the
// query (never post-filtered). Phase 1 implements it over OpenSearch.
package retrieval

import "context"

// Query is one retrieval request. Text is embedded at query time (D15) for
// the kNN clause and searched verbatim for the BM25 clause; K bounds the
// fused result count.
type Query struct {
	Text string
	K    int
}

// Filter scopes a search. TenantID and UserID narrow by tenancy (D16);
// ValidOnly restricts to currently-valid facts (invalid_at == null AND
// expired_at == null — the plan's current-state query).
type Filter struct {
	TenantID string
	UserID   string
	// ValidOnly applies the bi-temporal current-state filter.
	ValidOnly bool
}

// Hit is one fused result. Source names the index tier the hit came from
// (e.g. "episodic", "semantic"); Fields carries the stored document fields.
type Hit struct {
	ID     string
	Score  float64
	Source string
	Fields map[string]any
}

// Retriever is the read seam Phases 1–2 and the eval harness consume: one
// hybrid query (BM25 clause + kNN clause → RRF pipeline) returning a single
// fused, ranked list. An empty query or zero matches returns an empty slice,
// not an error.
type Retriever interface {
	// Search runs the hybrid query under the filter and returns fused hits
	// in descending score order, at most q.K of them.
	Search(ctx context.Context, q Query, f Filter) ([]Hit, error)
}

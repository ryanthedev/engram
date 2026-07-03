package retrieval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/store"
)

// SearchMode selects which signal(s) a search issues. Production traffic
// always uses ModeHybrid; ModeBM25Only and ModeKNNOnly exist so the eval
// harness can measure each signal in isolation (DW-1.3's non-inferiority
// gate needs all three numbers on the same data).
type SearchMode int

const (
	// ModeHybrid runs the BM25 clause and the kNN clause through the RRF
	// pipeline and returns the fused list (D1). The production default.
	ModeHybrid SearchMode = iota
	// ModeBM25Only runs only the BM25 clause.
	ModeBM25Only
	// ModeKNNOnly runs only the kNN clause.
	ModeKNNOnly
)

// DefaultK is the fused result count used when Query.K is unset (<=0).
const DefaultK = 10

// DefaultEmbedTimeout bounds the query-time embedding call: D15's co-located
// budget is <=50ms inside the overall read SLA.
const DefaultEmbedTimeout = 50 * time.Millisecond

// Option configures a Retriever built by NewOpenSearchRetriever.
type Option func(*config)

type config struct {
	episodicIndex string
	semanticIndex string
	mode          SearchMode
	embedTimeout  time.Duration
	logger        *slog.Logger
}

// WithIndices overrides the episodic/semantic index names searched.
// Defaults to the production indices (store.EpisodicIndex,
// store.SemanticIndex); tests point at scratch indices matching the
// template patterns so they can't collide with other tests or production
// data on the same cluster.
func WithIndices(episodic, semantic string) Option {
	return func(c *config) { c.episodicIndex, c.semanticIndex = episodic, semantic }
}

// WithMode overrides the search mode. Defaults to ModeHybrid.
func WithMode(m SearchMode) Option {
	return func(c *config) { c.mode = m }
}

// WithEmbedTimeout overrides the query-time embedding budget. Defaults to
// DefaultEmbedTimeout.
func WithEmbedTimeout(d time.Duration) Option {
	return func(c *config) { c.embedTimeout = d }
}

// WithLogger overrides the logger used to flag degraded searches (embedding
// timeout -> BM25-only fallback). Defaults to slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(c *config) { c.logger = l }
}

// NewOpenSearchRetriever returns the hybrid Retriever over OpenSearch: one
// Search call fans out to the episodic and semantic tiers concurrently,
// fuses each tier's own BM25+kNN+RRF result server-side, then merges the two
// fused lists by score (D1). client must not be nil; embedder supplies the
// query-time vector for the kNN clause (fixture-keyed fake or a real
// TEI-style HTTP client — see internal/embed).
func NewOpenSearchRetriever(client *http.Client, baseURL string, embedder embed.Embedder, opts ...Option) *MultiRetriever {
	cfg := config{
		episodicIndex: store.EpisodicIndex,
		semanticIndex: store.SemanticIndex,
		mode:          ModeHybrid,
		embedTimeout:  DefaultEmbedTimeout,
		logger:        slog.Default(),
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	base := strings.TrimRight(baseURL, "/")
	tiers := []Retriever{
		&tierRetriever{
			client: client, baseURL: base, index: cfg.episodicIndex,
			textField: "text", vectorField: "text_embedding", source: "episodic",
			supportsValidity: false,
			embedder:         embedder, embedTimeout: cfg.embedTimeout, mode: cfg.mode, logger: cfg.logger,
		},
		&tierRetriever{
			client: client, baseURL: base, index: cfg.semanticIndex,
			textField: "statement", vectorField: "fact_embedding", source: "semantic",
			supportsValidity: true,
			embedder:         embedder, embedTimeout: cfg.embedTimeout, mode: cfg.mode, logger: cfg.logger,
		},
	}
	return &MultiRetriever{tiers: tiers}
}

// MultiRetriever fuses hits from several per-tier retrievers into one ranked
// list. It is Retriever's cross-tier read path: today episodic + semantic,
// and how future tiers (experience, graph) compose in without changing the
// Search contract.
type MultiRetriever struct {
	tiers []Retriever
}

var _ Retriever = (*MultiRetriever)(nil)

// Search implements Retriever across every configured tier: an empty query
// short-circuits to an empty result (no HTTP calls); otherwise every tier is
// queried concurrently, and the merged list is sorted by score descending
// and truncated to K. If every tier errors, Search errors; if at least one
// tier succeeds, its hits are returned and the failure(s) are logged.
func (m *MultiRetriever) Search(ctx context.Context, q Query, f Filter) ([]Hit, error) {
	if q.Text == "" {
		return nil, nil
	}
	if q.K <= 0 {
		q.K = DefaultK
	}

	type outcome struct {
		hits []Hit
		err  error
	}
	results := make([]outcome, len(m.tiers))
	var wg sync.WaitGroup
	for i, tier := range m.tiers {
		wg.Add(1)
		go func(i int, tier Retriever) {
			defer wg.Done()
			hits, err := tier.Search(ctx, q, f)
			results[i] = outcome{hits, err}
		}(i, tier)
	}
	wg.Wait()

	var merged []Hit
	var errs []error
	for _, r := range results {
		if r.err != nil {
			errs = append(errs, r.err)
			continue
		}
		merged = append(merged, r.hits...)
	}
	if merged == nil && len(errs) > 0 {
		return nil, fmt.Errorf("retrieval: all tiers failed: %w", errors.Join(errs...))
	}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].Score > merged[j].Score })
	if len(merged) > q.K {
		merged = merged[:q.K]
	}
	return merged, nil
}

// tierRetriever implements Retriever for exactly one OpenSearch index, using
// that tier's own text/vector field names (episodic and semantic name their
// text and vector fields differently, so a single cross-index query can't
// use one field name for both — see the Phase-1 design notes).
type tierRetriever struct {
	client           *http.Client
	baseURL          string
	index            string
	textField        string
	vectorField      string
	source           string
	supportsValidity bool
	embedder         embed.Embedder
	embedTimeout     time.Duration
	mode             SearchMode
	logger           *slog.Logger
}

var _ Retriever = (*tierRetriever)(nil)

// Search implements Retriever for this tier.
func (t *tierRetriever) Search(ctx context.Context, q Query, f Filter) ([]Hit, error) {
	if q.Text == "" {
		return nil, nil
	}
	k := q.K
	if k <= 0 {
		k = DefaultK
	}

	vec, degraded := t.embed(ctx, q.Text)
	mode := t.mode
	if degraded {
		switch mode {
		case ModeKNNOnly:
			t.logger.WarnContext(ctx, "search degraded: embedding unavailable, kNN-only mode has no results", "tier", t.source)
			return nil, nil
		case ModeHybrid:
			t.logger.WarnContext(ctx, "hybrid search degraded to BM25-only: embedding unavailable", "tier", t.source)
			mode = ModeBM25Only
		}
	}

	filters := t.filterClauses(f)
	body, usePipeline := buildQuery(mode, t.textField, t.vectorField, q.Text, vec, k, filters)

	url := t.baseURL + "/" + t.index + "/_search"
	if usePipeline {
		url += "?search_pipeline=" + store.RRFPipelineName
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("retrieval: building search request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("retrieval: searching %s: %w", t.index, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("retrieval: reading search response from %s: %w", t.index, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("retrieval: search on %s returned %s: %s", t.index, resp.Status, string(raw))
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("retrieval: decoding search response from %s: %w", t.index, err)
	}
	return parseHits(decoded, t.source), nil
}

// embed returns the query-time vector for text, bounded by embedTimeout
// (D15). degraded is true when the vector is unavailable (timeout, error, or
// ModeBM25Only, which never needs one) — the caller falls back accordingly.
func (t *tierRetriever) embed(ctx context.Context, text string) (vec []float32, degraded bool) {
	if t.mode == ModeBM25Only {
		return nil, false
	}
	ectx, cancel := context.WithTimeout(ctx, t.embedTimeout)
	defer cancel()
	vecs, err := t.embedder.Embed(ectx, []string{text})
	if err != nil || len(vecs) == 0 {
		return nil, true
	}
	return vecs[0], false
}

// filterClauses builds the tenancy and validity filter query clauses,
// applied inside both the BM25 and kNN sub-queries (never post-filtered —
// the filtered-kNN recall collapse the Phase-0 spike found only when
// filtering after the fact, not inside the knn clause).
func (t *tierRetriever) filterClauses(f Filter) []any {
	var clauses []any
	if f.TenantID != "" {
		clauses = append(clauses, map[string]any{"term": map[string]any{"tenant_id": f.TenantID}})
	}
	if f.UserID != "" {
		// Filter.UserID is the retrieval-side scope name; it maps onto the
		// stored owner_agent_id field (memory.Episodic/SemanticFact have no
		// separate "user_id" — owner_agent_id is the closest provenance
		// field, D16).
		clauses = append(clauses, map[string]any{"term": map[string]any{"owner_agent_id": f.UserID}})
	}
	if f.ValidOnly && t.supportsValidity {
		clauses = append(clauses, map[string]any{
			"bool": map[string]any{
				"must_not": []any{map[string]any{"exists": map[string]any{"field": "expired_at"}}},
				"should": []any{
					map[string]any{"bool": map[string]any{"must_not": []any{map[string]any{"exists": map[string]any{"field": "invalid_at"}}}}},
					map[string]any{"range": map[string]any{"invalid_at": map[string]any{"gt": "now"}}},
				},
				"minimum_should_match": 1,
			},
		})
	}
	return clauses
}

// buildQuery constructs the OpenSearch request body for one tier's search.
// usePipeline reports whether the caller must attach the RRF search_pipeline
// query param (only hybrid mode with a usable vector fuses two clauses).
func buildQuery(mode SearchMode, textField, vectorField, text string, vec []float32, k int, filters []any) (body []byte, usePipeline bool) {
	bm25Query := map[string]any{"match": map[string]any{textField: text}}
	var bm25 any = bm25Query
	if len(filters) > 0 {
		bm25 = map[string]any{"bool": map[string]any{"must": []any{bm25Query}, "filter": filters}}
	}

	var knn any
	if vec != nil {
		inner := map[string]any{"vector": vec, "k": k}
		if len(filters) > 0 {
			inner["filter"] = map[string]any{"bool": map[string]any{"filter": filters}}
		}
		knn = map[string]any{"knn": map[string]any{vectorField: inner}}
	}

	var query map[string]any
	switch {
	case mode == ModeBM25Only || knn == nil:
		query = map[string]any{"size": k, "query": bm25}
	case mode == ModeKNNOnly:
		query = map[string]any{"size": k, "query": knn}
	default: // ModeHybrid with a usable vector.
		query = map[string]any{"size": k, "query": map[string]any{"hybrid": map[string]any{"queries": []any{bm25, knn}}}}
		usePipeline = true
	}
	body, _ = json.Marshal(query)
	return body, usePipeline
}

// parseHits decodes an OpenSearch _search response into fused Hits, tagged
// with the tier's Source.
func parseHits(decoded map[string]any, source string) []Hit {
	hitsField, _ := decoded["hits"].(map[string]any)
	rawHits, _ := hitsField["hits"].([]any)
	out := make([]Hit, 0, len(rawHits))
	for _, rh := range rawHits {
		hm, ok := rh.(map[string]any)
		if !ok {
			continue
		}
		id, _ := hm["_id"].(string)
		score, _ := hm["_score"].(float64)
		fields, _ := hm["_source"].(map[string]any)
		out = append(out, Hit{ID: id, Score: score, Source: source, Fields: fields})
	}
	return out
}

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

	"github.com/ryanthedev/engram/internal/acl"
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
	acl           ACLFilter
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
// timeout -> BM25-only fallback) and ACL denials. Defaults to slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(c *config) { c.logger = l }
}

// WithACL enables query-time ACL enforcement (Phase 4): the compiled filter is
// applied inside every tier query, and tier/expanded hits are re-verified
// through the ACL predicate. Without it (eval harness, unit tests) the
// Retriever performs no scope enforcement. A nil filter is ignored.
func WithACL(f ACLFilter) Option {
	return func(c *config) {
		if f != nil {
			c.acl = f
		}
	}
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
	tiers := []*tierRetriever{
		{
			client: client, baseURL: base, index: cfg.episodicIndex,
			textField: "text", vectorField: "text_embedding", source: "episodic",
			supportsValidity: false,
			embedder:         embedder, embedTimeout: cfg.embedTimeout, mode: cfg.mode, logger: cfg.logger,
		},
		{
			client: client, baseURL: base, index: cfg.semanticIndex,
			textField: "statement", vectorField: "fact_embedding", source: "semantic",
			supportsValidity: true,
			embedder:         embedder, embedTimeout: cfg.embedTimeout, mode: cfg.mode, logger: cfg.logger,
		},
	}
	return &MultiRetriever{tiers: tiers, acl: cfg.acl, logger: cfg.logger}
}

// MultiRetriever fuses hits from several per-tier retrievers into one ranked
// list. It is Retriever's cross-tier read path: episodic + semantic built-in
// tiers, plus registered tier sources (Phase 5) and post-hooks (Phase 6), all
// composed behind the unchanged Search contract. When built WithACL it is the
// enforcement point: the compiled filter goes inside every built-in query, and
// tier/expanded hits are re-verified — no caller can bypass scope.
type MultiRetriever struct {
	tiers     []*tierRetriever
	acl       ACLFilter
	tierSrcs  []TierSource
	postHooks []PostHook
	logger    *slog.Logger
}

var _ Retriever = (*MultiRetriever)(nil)

// RegisterTier adds a retrieval tier source (Phase 4 seam; P5 experience tier).
// Call it at wiring time before serving; not safe to call concurrently with
// active searches. Registered sources are searched with the caller's Identity
// and their hits re-verified through the ACL predicate.
func (m *MultiRetriever) RegisterTier(src TierSource) {
	m.tierSrcs = append(m.tierSrcs, src)
}

// RegisterPostHook adds a post-fusion hook (Phase 4 seam; P6 graph expansion).
// Call it at wiring time before serving. Hooks receive the caller's Identity;
// any hits they add are re-verified through the ACL predicate before return.
func (m *MultiRetriever) RegisterPostHook(h PostHook) {
	m.postHooks = append(m.postHooks, h)
}

// Search implements Retriever across every configured tier under the ACL. An
// empty query short-circuits to an empty result (no HTTP calls). With ACL
// enabled, the filter is compiled ONCE from f.Identity and enforced two ways:
// its OpenSearch clause goes inside every built-in tier query (efficient,
// preserves filtered-kNN recall), and its predicate re-verifies hits that came
// from registered tier sources or post-hooks. It is fail-closed: an ACL
// compile error returns zero results and logs a denial — the query never runs
// unfiltered. Built-in tiers run concurrently; if every tier errors Search
// errors, otherwise partial failures are logged.
func (m *MultiRetriever) Search(ctx context.Context, q Query, f Filter) ([]Hit, error) {
	if q.Text == "" {
		return nil, nil
	}
	if q.K <= 0 {
		q.K = DefaultK
	}

	var enf acl.Enforcer
	var aclClause map[string]any
	if m.acl != nil {
		e, err := m.acl.Enforce(ctx, f.Identity)
		if err != nil {
			// Fail-closed: never run a query we couldn't authorize.
			m.logger.WarnContext(ctx, "retrieval: ACL denial, returning zero results (fail-closed)",
				"identity", f.Identity.String(), "err", err)
			return nil, nil
		}
		enf = e
		aclClause = e.Clause()
	}

	type outcome struct {
		hits []Hit
		err  error
	}
	results := make([]outcome, len(m.tiers)+len(m.tierSrcs))
	var wg sync.WaitGroup
	for i, tier := range m.tiers {
		wg.Add(1)
		go func(i int, tier *tierRetriever) {
			defer wg.Done()
			hits, err := tier.search(ctx, q, f, aclClause)
			results[i] = outcome{hits, err}
		}(i, tier)
	}
	for j, src := range m.tierSrcs {
		wg.Add(1)
		go func(i int, src TierSource) {
			defer wg.Done()
			hits, err := src.Search(ctx, f.Identity, q)
			results[i] = outcome{hits, err}
		}(len(m.tiers)+j, src)
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

	// Authorize BEFORE the top-k truncation. Registered tier sources deliver
	// their hits unfiltered (they enforce on their own index, but we do not
	// trust that); if an unauthorized high-scoring tier hit survived into the
	// sort it could crowd an authorized built-in hit out of the top-k and then
	// be dropped by a later re-filter — yielding a deficient authorized result.
	// Filtering first guarantees no authorized hit is lost to truncation by an
	// unauthorized one. Built-in hits already passed the query clause, so this
	// only removes tier-source leakage (and is a cheap, equivalent re-check).
	if m.acl != nil {
		merged = filterAuthorized(merged, enf)
	}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].Score > merged[j].Score })
	if len(merged) > q.K {
		merged = merged[:q.K]
	}

	// Post-hooks (e.g. graph expansion) run with the Identity on the authorized
	// top-k; they may add hits reached through other documents. Re-authorize
	// their output so an expansion cannot introduce a fact the caller may not
	// read (defense in depth; the authorized top-k above is unaffected).
	for _, h := range m.postHooks {
		expanded, err := h.Apply(ctx, f.Identity, merged)
		if err != nil {
			return nil, fmt.Errorf("retrieval: post-hook: %w", err)
		}
		merged = expanded
	}
	if m.acl != nil && len(m.postHooks) > 0 {
		merged = filterAuthorized(merged, enf)
	}
	return merged, nil
}

// filterAuthorized drops hits the Enforcer does not authorize (fail-closed:
// a hit whose fields cannot be read as an ACL record is dropped).
func filterAuthorized(hits []Hit, enf acl.Enforcer) []Hit {
	out := hits[:0:0]
	for _, h := range hits {
		if enf.Authorize(recordFromHit(h)) {
			out = append(out, h)
		}
	}
	return out
}

// recordFromHit extracts the ACL-relevant provenance fields from a hit's stored
// document. Missing fields read as empty strings (which the ACL treats as
// deny-worthy for team/org and private-to-a-blank-owner), staying fail-closed.
func recordFromHit(h Hit) acl.Record {
	str := func(k string) string { s, _ := h.Fields[k].(string); return s }
	return acl.Record{
		TenantID:     str("tenant_id"),
		TeamID:       str("team_id"),
		Scope:        str("scope"),
		OwnerAgentID: str("owner_agent_id"),
	}
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

// Search implements Retriever for this tier with no ACL clause (used by the
// eval harness and tests that construct a tier directly). The MultiRetriever
// calls search with the compiled ACL clause.
func (t *tierRetriever) Search(ctx context.Context, q Query, f Filter) ([]Hit, error) {
	return t.search(ctx, q, f, nil)
}

// search runs this tier's query with an optional ACL clause ANDed into both
// the BM25 and kNN sub-queries (inside the knn clause, so filtered-kNN recall
// does not collapse — DW-4.5). A match_none aclClause makes the tier return
// nothing (fail-closed deny-all).
func (t *tierRetriever) search(ctx context.Context, q Query, f Filter, aclClause map[string]any) ([]Hit, error) {
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

	filters := t.filterClauses(f, aclClause)
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

// filterClauses builds the tenancy, validity, and ACL filter query clauses,
// applied inside both the BM25 and kNN sub-queries (never post-filtered —
// the filtered-kNN recall collapse the Phase-0 spike found only when
// filtering after the fact, not inside the knn clause). The ACL clause, when
// present, is the query-time scope barricade (Phase 4).
func (t *tierRetriever) filterClauses(f Filter, aclClause map[string]any) []any {
	var clauses []any
	if aclClause != nil {
		clauses = append(clauses, aclClause)
	}
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

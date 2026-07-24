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
	"slices"
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

// MaxK caps the fused result count (and each per-tier query size). K arrives
// from the MCP caller across a process boundary — external input — so it is
// clamped, never trusted: a runaway K would drag full result pages through
// every tier for a response the caller cannot consume anyway.
const MaxK = 100

// fallbackScore marks a hit whose backend supplied no score (e.g. a response
// missing _score). It is assigned AFTER the merged sort and truncation, so it
// can never reorder results — it only upholds the Search contract that every
// returned hit carries a non-zero Score. Deliberately tiny: an unscored hit
// should rank below any genuinely scored one if anything downstream re-sorts.
const fallbackScore = 1e-9

// clampK normalizes an externally supplied result count into [1, MaxK]:
// unset/non-positive becomes DefaultK, anything over MaxK is capped, and
// in-range values pass through unchanged.
func clampK(k int) int {
	switch {
	case k <= 0:
		return DefaultK
	case k > MaxK:
		return MaxK
	default:
		return k
	}
}

// ExpandedSource is the Hit.Source that marks a hit as a graph EXPANSION
// rather than a query match: it was reached by traversing edges out of a
// matched hit, not by matching the query itself. It is the sole discriminator
// SplitExpanded partitions on.
const ExpandedSource = "graph"

// SplitExpanded partitions one Search result into the k matched hits the
// caller asked for and the graph expansions that rode along beside them —
// the "honest k" contract:
//
//	len(matched) <= clampK(k), and no matched hit has Source == ExpandedSource.
//
// Post-hooks run AFTER Search's top-k truncation and append hits to the list
// (see Search), so the returned slice can exceed k. That is deliberate —
// expansion is bonus context and must never evict a direct match — but it
// means a caller asking for k=20 can be handed 40 hits with no way to tell
// which 20 it actually asked for. Splitting here restores that distinction
// without touching the Retriever interface (eval.NullRetriever implements it
// too), because the two blocks are fully derivable from Hit.Source.
//
// k is normalized with the SAME clamp Search applied to Query.K, so an unset
// (0) or over-MaxK k yields the identical bound the retriever actually used;
// re-deriving that rule at the call site is how "len(hits) <= k" becomes a
// lie for an unset k. The cap is enforced rather than assumed: today only the
// graph expander appends post-truncation, but a future non-graph post-hook's
// hits are NOT expansions — they belong in matched — and must not be allowed
// to inflate k on their way there.
//
// Relative order is preserved within each block. expanded is nil (never an
// empty non-nil slice) when there are no expansions, so a caller can emit the
// block conditionally on len/nil alone.
func SplitExpanded(hits []Hit, k int) (matched, expanded []Hit) {
	for _, h := range hits {
		if h.Source == ExpandedSource {
			expanded = append(expanded, h)
			continue
		}
		matched = append(matched, h)
	}
	if limit := clampK(k); len(matched) > limit {
		matched = matched[:limit]
	}
	return matched, expanded
}

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
			supportsValidity: false, filterable: episodicFilterable,
			embedder: embedder, embedTimeout: cfg.embedTimeout, mode: cfg.mode, logger: cfg.logger,
		},
		{
			client: client, baseURL: base, index: cfg.semanticIndex,
			textField: "statement", vectorField: "fact_embedding", source: "semantic",
			supportsValidity: true, filterable: semanticFilterable,
			embedder: embedder, embedTimeout: cfg.embedTimeout, mode: cfg.mode, logger: cfg.logger,
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
	tierSrcs  []namedTierSource
	postHooks []namedPostHook
	logger    *slog.Logger
}

// namedTierSource / namedPostHook carry the registration name alongside the
// registered implementation. The name lives HERE rather than on the TierSource
// and PostHook interfaces deliberately: a Name() method would force every
// implementor (internal/experience, internal/graph) to change, for a fact only
// the registry needs to know.
type namedTierSource struct {
	name string
	src  TierSource
}

type namedPostHook struct {
	name string
	hook PostHook
}

var _ Retriever = (*MultiRetriever)(nil)

// RegisterTier adds a retrieval tier source under name (P5 experience tier).
// name is the token callers pass in Filter.Sources to select — or skip — this
// source; it must be non-empty and unique across all registered tiers, tier
// sources and post-hooks. Call it at wiring time before serving; not safe to
// call concurrently with active searches. Registered sources are searched with
// the caller's Identity and their hits re-verified through the ACL predicate.
func (m *MultiRetriever) RegisterTier(name string, src TierSource) {
	m.checkSourceName(name)
	m.tierSrcs = append(m.tierSrcs, namedTierSource{name: name, src: src})
}

// RegisterPostHook adds a post-fusion hook under name (P6 graph expansion).
// name shares one namespace with the tiers — a caller asking for "graph" need
// not know it names a post-hook rather than a tier. Same rules as RegisterTier:
// non-empty, unique, wiring-time only. Hooks receive the caller's Identity; any
// hits they add are re-verified through the ACL predicate before return.
func (m *MultiRetriever) RegisterPostHook(name string, h PostHook) {
	m.checkSourceName(name)
	m.postHooks = append(m.postHooks, namedPostHook{name: name, hook: h})
}

// checkSourceName flags a wiring-time registration bug loudly rather than
// letting it fail silently later: an empty name makes a source unselectable
// (Filter.Sources could never name it), and a duplicate name makes selection
// ambiguous (one name would select two sources). Both are programmer errors at
// startup, not runtime conditions — but this package never panics, so they are
// logged at ERROR and the registration proceeds, leaving the source reachable
// through the default (nil Sources) path rather than dropping it on the floor.
func (m *MultiRetriever) checkSourceName(name string) {
	log := m.logger
	if log == nil {
		log = slog.Default()
	}
	if name == "" {
		log.Error("retrieval: source registered with an empty name; it cannot be selected via Filter.Sources")
		return
	}
	if slices.Contains(m.sourceNames(), name) {
		log.Error("retrieval: duplicate source name registered; Filter.Sources selection is ambiguous", "name", name)
	}
}

// Search implements Retriever across every configured tier under the ACL. An
// empty query short-circuits to an empty result (no HTTP calls).
//
// f.Sources and f.Predicates are validated FIRST, before any authorization or
// HTTP work: an unknown source, an empty (non-nil) source list, an unknown
// filterable field, a bad op, or a non-scalar filter value returns an error
// naming the valid vocabulary — no cluster round-trip, no partial search. The
// surviving sources decide which tiers, tier sources and post-hooks run, and
// each tier compiles only the predicates it declares (routing, not zeroing).
//
// With ACL enabled, the filter is compiled ONCE from f.Identity and enforced
// two ways:
// its OpenSearch clause goes inside every built-in tier query (efficient,
// preserves filtered-kNN recall), and its predicate re-verifies hits that came
// from registered tier sources or post-hooks. It is fail-closed: an ACL
// compile error returns zero results and logs a denial — the query never runs
// unfiltered. Built-in tiers run concurrently; if every tier errors Search
// errors, otherwise partial failures are logged. Returned hits are shaped:
// K is clamped to [1, MaxK], each hit's Fields are projected to its source's
// display allowlist (no embeddings, no ACL provenance), and every hit carries
// a non-zero Score.
func (m *MultiRetriever) Search(ctx context.Context, q Query, f Filter) ([]Hit, error) {
	// The filter barricade: caller-supplied Sources/Predicates are external
	// input (they arrive from the MCP caller across a process boundary), so
	// they are validated here, once, before anything else runs — including the
	// empty-text short-circuit below. An invalid filter (unknown source,
	// unfilterable field, malformed predicate) must error regardless of query
	// text (DW-4.7); it must never be silently skipped because the query
	// happened to be empty. Everything downstream — including every tier's
	// clause compiler — may assume the filter is well-formed and that each
	// predicate is owned by at least one selected tier.
	sel, err := m.resolveSources(f.Sources)
	if err != nil {
		return nil, err
	}
	if err := m.validatePredicates(f.Predicates, sel); err != nil {
		return nil, err
	}
	if err := m.validateFilterableSources(f.Predicates, sel); err != nil {
		return nil, err
	}
	if q.Text == "" {
		return nil, nil
	}
	q.K = clampK(q.K)

	// A filtered search runs only sources that can honor the filter: registered
	// tier sources and post-hooks declare no filterable fields, so their hits
	// would ride back unconstrained beside constrained ones (see selectSources).
	tiers, tierSrcs, postHooks := m.selectSources(sel, len(f.Predicates) > 0)

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
	results := make([]outcome, len(tiers)+len(tierSrcs))
	var wg sync.WaitGroup
	for i, tier := range tiers {
		wg.Add(1)
		go func(i int, tier *tierRetriever) {
			defer wg.Done()
			hits, err := tier.search(ctx, q, f, aclClause)
			results[i] = outcome{hits, err}
		}(i, tier)
	}
	for j, src := range tierSrcs {
		wg.Add(1)
		go func(i int, src TierSource) {
			defer wg.Done()
			hits, err := src.Search(ctx, f.Identity, q)
			results[i] = outcome{hits, err}
		}(len(tiers)+j, src.src)
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
	for _, h := range postHooks {
		expanded, err := h.hook.Apply(ctx, f.Identity, merged)
		if err != nil {
			return nil, fmt.Errorf("retrieval: post-hook %q: %w", h.name, err)
		}
		merged = expanded
	}
	if m.acl != nil && len(postHooks) > 0 {
		merged = filterAuthorized(merged, enf)
	}

	// Shape the response LAST, after every authorization pass and post-hook:
	// filterAuthorized/recordFromHit read tenant_id/team_id/scope/owner_agent_id
	// from Hit.Fields fail-closed (stripping them earlier would deny everything),
	// and the graph post-hook reads tenant_id/subject/object from seed hits.
	// Only now is it safe to reduce each hit to its display allowlist and
	// guarantee a populated score (post-sort, so ranking is never affected).
	for i := range merged {
		merged[i].Fields = projectFields(merged[i].Source, merged[i].Fields)
		if merged[i].Score == 0 {
			merged[i].Score = fallbackScore
		}
	}
	return merged, nil
}

// allowedFields maps each hit source to the display fields Search returns for
// it — everything else (embeddings, ACL provenance, internal bookkeeping) is
// dropped at the retrieval boundary so it never crosses the gRPC wire.
var allowedFields = map[string][]string{
	"episodic": {"text", "kind", "occurred_at", "event_id", "source_ids"},
	// extractor_version is projected because it is filterable: a caller that can
	// narrow by extractor_version must be able to SEE which version a hit came
	// from, or the filter is unverifiable from the response alone.
	"semantic": {"statement", "subject", "predicate", "object", "valid_at", "source_ids", "extractor_version"},
	"graph":    {"statement", "subject", "predicate", "object", "hop"},
}

// defaultAllowed is the safe-default projection for sources without an entry
// in allowedFields (registered tier sources, e.g. "experience"): keep the
// recognizable content fields if present, drop everything else.
var defaultAllowed = []string{"text", "statement", "subject", "predicate", "object"}

// projectFields reduces a hit's stored document to source's display allowlist,
// returning a NEW map (the input — possibly shared with a tier source or
// post-hook — is never mutated). Absent and nil-valued fields are omitted, not
// invented; a nil input stays nil. Cross-tier score normalization is a
// documented non-goal (v1 leaves fusion vs hop scores as-is).
func projectFields(source string, fields map[string]any) map[string]any {
	if fields == nil {
		return nil
	}
	allowed, ok := allowedFields[source]
	if !ok {
		allowed = defaultAllowed
	}
	out := make(map[string]any, len(allowed))
	for _, k := range allowed {
		if v, present := fields[k]; present && v != nil {
			out[k] = v
		}
	}
	return out
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
	// filterable declares which Filter.Predicates fields this tier owns and
	// how they compile. A predicate on a field absent from this map belongs to
	// another tier and never touches this tier's query.
	filterable   FilterableFields
	embedder     embed.Embedder
	embedTimeout time.Duration
	mode         SearchMode
	logger       *slog.Logger
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
	k := clampK(q.K)

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

	filters, err := t.filterClauses(f, aclClause)
	if err != nil {
		return nil, err
	}
	// Memory tiers never sort (relevance/RRF order only) — nil is the only
	// value any memory caller ever passes, which is exactly the value
	// buildQuery treats as "omit the sort key entirely" (DW-5.1).
	body, usePipeline := buildQuery(queryOpts{
		mode: mode, textField: t.textField, vectorField: t.vectorField,
		text: q.Text, vec: vec, k: k, filters: filters,
	})

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

// filterClauses builds the tenancy, validity, ACL and predicate filter query
// clauses, applied inside both the BM25 and kNN sub-queries (never
// post-filtered — the filtered-kNN recall collapse the Phase-0 spike found
// only when filtering after the fact, not inside the knn clause). The ACL
// clause, when present, is the query-time scope barricade (Phase 4). Caller
// predicates come LAST, appended only for the fields this tier declares — so a
// Filter with no predicates yields byte-for-byte the clause list it always did
// (DW-4.3), and a predicate another tier owns leaves this tier's query
// unconstrained rather than zeroing it (DW-4.2).
//
// An error can only mean an unvalidated predicate reached a tier (Search
// validates every one of them first). Failing closed here rather than emitting
// an unchecked clause is deliberate: a filter compiler is not the place to
// guess.
func (t *tierRetriever) filterClauses(f Filter, aclClause map[string]any) ([]any, error) {
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
	for _, p := range f.Predicates {
		clause, declared, err := t.filterable.clauseFor(p)
		if err != nil {
			return nil, err
		}
		if !declared {
			continue // owned by another tier: route, don't fail — and don't zero this one.
		}
		clauses = append(clauses, clause)
	}
	return clauses, nil
}

// queryOpts carries every input to buildQuery. Its ZERO VALUE is safe: an
// unset field means "off", so a caller setting only the core search fields
// (mode..sort) gets byte-for-byte the query the old 8-positional-parameter
// buildQuery produced — the memory path relies on that (DW-1.3's golden
// matrix in buildquery_golden_test.go pins it against pre-refactor bytes).
type queryOpts struct {
	mode        SearchMode
	textField   string
	vectorField string
	text        string
	vec         []float32
	k           int
	filters     []any
	sort        []any

	// Knowledge-search seams, declared here so Phases 2–3 extend the query
	// without another signature change. All are INERT this phase — buildQuery
	// does not read them yet — and zero-value-off by design: memory callers
	// leave them unset and must keep producing identical bodies when they are
	// wired in.
	//
	// offset is the paging start ("from", Phase 3); 0 means first page and
	// emits no "from" key. fragmentSize/numberOfFragments size the highlight
	// clause (Phase 2), already fallback-resolved by the caller via
	// knowledge.CollectionSpec.FragmentSizing — 0 here means highlighting off.
	// highlightPreTag/highlightPostTag wrap fragment matches; both empty means
	// markers off (the default — never a hardcoded marker pair).
	offset            int
	fragmentSize      int
	numberOfFragments int
	highlightPreTag   string
	highlightPostTag  string
}

// buildQuery constructs the OpenSearch request body for one tier's search.
// usePipeline reports whether the caller must attach the RRF search_pipeline
// query param (only hybrid mode with a usable vector fuses two clauses).
//
// sort is additive (Phase 5, knowledge retriever): a nil/empty sort omits
// the "sort" key entirely, so every existing memory caller — which always
// passes nil — gets byte-identical output (DW-5.1, proven by
// TestBuildQueryMemoryPathByteIdenticalWhenSortNil in knowledge_test.go).
// A non-nil sort is appended after size/query so relevance order stays the
// default when the caller doesn't ask for a field sort.
//
// text == "" (only reachable via the knowledge retriever's filter-only
// search — both memory callers short-circuit before text can ever be empty
// here) falls back to match_all so filters alone can still select documents,
// rather than a "match" query on an empty string, which OpenSearch analyzes
// to zero terms and use.
func buildQuery(opts queryOpts) (body []byte, usePipeline bool) {
	bm25Query := map[string]any{"match_all": map[string]any{}}
	if opts.text != "" {
		bm25Query = map[string]any{"match": map[string]any{opts.textField: opts.text}}
	}
	var bm25 any = bm25Query
	if len(opts.filters) > 0 {
		bm25 = map[string]any{"bool": map[string]any{"must": []any{bm25Query}, "filter": opts.filters}}
	}

	var knn any
	if opts.vec != nil {
		inner := map[string]any{"vector": opts.vec, "k": opts.k}
		if len(opts.filters) > 0 {
			inner["filter"] = map[string]any{"bool": map[string]any{"filter": opts.filters}}
		}
		knn = map[string]any{"knn": map[string]any{opts.vectorField: inner}}
	}

	var query map[string]any
	switch {
	case opts.mode == ModeBM25Only || knn == nil:
		query = map[string]any{"size": opts.k, "query": bm25}
	case opts.mode == ModeKNNOnly:
		query = map[string]any{"size": opts.k, "query": knn}
	default: // ModeHybrid with a usable vector.
		query = map[string]any{"size": opts.k, "query": map[string]any{"hybrid": map[string]any{"queries": []any{bm25, knn}}}}
		usePipeline = true
	}
	// Never fetch embedding vectors back: they are query-time inputs, not
	// results, and dominate response size (~95% of a raw hit). Excluding a
	// field an index doesn't have is a no-op, so both names apply to both tiers.
	excludes := []string{"text_embedding", "fact_embedding"}
	// Highlighting on (numberOfFragments > 0, the knowledge path's
	// fragments-default) means fragments REPLACE the body: the highlight
	// clause and the text-field suppression are one decision, gated on one
	// knob, so a caller can never get fragments plus the body it was meant to
	// spare, or a suppressed body with no fragments to stand in for it.
	// pre/post tags are emitted as-is — [""] when unset, which is OpenSearch's
	// markers-off escape from its <em> default; a non-empty pair comes from
	// the collection's opt-in tag fields, never a hardcoded marker.
	if opts.numberOfFragments > 0 {
		excludes = append(excludes, opts.textField)
		query["highlight"] = map[string]any{
			"fields": map[string]any{opts.textField: map[string]any{
				"fragment_size":       opts.fragmentSize,
				"number_of_fragments": opts.numberOfFragments,
				"pre_tags":            []string{opts.highlightPreTag},
				"post_tags":           []string{opts.highlightPostTag},
			}},
		}
	}
	query["_source"] = map[string]any{"excludes": excludes}
	if len(opts.sort) > 0 {
		query["sort"] = opts.sort
	}
	body, _ = json.Marshal(query)
	return body, usePipeline
}

// parseHits decodes an OpenSearch _search response into fused Hits, tagged
// with the tier's Source. A hit's "highlight" section (knowledge path only —
// buildQuery requests exactly ONE highlighted field, and memory queries
// request none, so the key is simply absent there) is flattened into
// Hit.Fragments.
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
		out = append(out, Hit{ID: id, Score: score, Source: source, Fields: fields, Fragments: parseFragments(hm)})
	}
	return out
}

// parseFragments reads one raw hit's highlight section into a fragment list.
// Only one field is ever requested for highlighting (the collection's text
// field), so every fragment under the section belongs to it regardless of
// key name; nil when the section is absent (every memory hit) or empty.
func parseFragments(hit map[string]any) []string {
	highlight, _ := hit["highlight"].(map[string]any)
	var out []string
	for _, v := range highlight {
		frags, _ := v.([]any)
		for _, f := range frags {
			if s, ok := f.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

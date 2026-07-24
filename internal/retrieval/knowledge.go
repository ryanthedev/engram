package retrieval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ryanthedev/engram/internal/knowledge"
)

// Predicate is one generic knowledge-search filter: Field must name a
// filterable entry in the target collection's Mappings. Op is "term" |
// "range" | "prefix". Value carries a scalar for term/prefix; for range it
// is a map[string]any with "gte" and/or "lte" scalar bounds (either may be
// absent, but at least one must be present). Mirrors mcp.Predicate — Phase 6
// translates at the seam.
type Predicate struct {
	Field string
	Op    string
	Value any
}

// SortKey orders knowledge-search results by one field: Field must name a
// sortable entry in the target collection's Mappings; Order is "asc" |
// "desc". Mirrors mcp.SortKey.
type SortKey struct {
	Field string
	Order string
}

// CollectionMeta is one Collections entry: a collection's document count and
// staleness. NewestHarvestedAt is the max harvested_at across the
// collection's documents; NewestDocDate is the max value across every
// date-typed field declared in the collection's Mappings (a collection with
// no declared date field reports nil — undated, not an error). Nil/zero
// values mean the collection is empty or not yet provisioned. Mirrors the
// staleness fields of mcp.CollectionInfo — Phase 6 zips this with a
// registry-fetched CollectionSpec.
type CollectionMeta struct {
	Name              string
	Count             int64
	NewestHarvestedAt *time.Time
	NewestDocDate     *time.Time
}

// knowledgeIndexNameRE is the safe-path-segment grammar for a knowledge
// collection's physical index/alias name embedded in an OpenSearch REST
// path — the same grammar store.indexNameRE uses (unexported there, so
// redeclared locally rather than exported cross-package for one caller).
var knowledgeIndexNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,254}$`)

// validateKnowledgeIndex is the barricade every code path that embeds a
// CollectionSpec.Index into an OpenSearch request path MUST pass the value
// through first. spec.Index is registry-assigned in the normal Create/Update
// flow, but Search/Collections receive a caller-supplied CollectionSpec —
// "internal team API is still external" (cc-defensive-programming) — so it
// is validated here too, mirroring the Phase-3 SECURITY LESSON.
func validateKnowledgeIndex(index string) error {
	if !knowledgeIndexNameRE.MatchString(index) || strings.Contains(index, "..") {
		return fmt.Errorf("retrieval: invalid knowledge collection index %q: must match %s and not contain \"..\"", index, knowledgeIndexNameRE)
	}
	return nil
}

// KnowledgeRetriever is the BM25-only read path for Engram's document
// knowledge platform (Phase 5): generic, registry-validated field filters
// and sort over one collection at a time (Search), plus per-collection
// staleness (Collections). It never touches the embedder or the memory
// tiers (MultiRetriever) — knowledge search is a distinct actor from memory
// reconciliation (see the plan's Rejected Approach C). It holds a
// knowledge.CollectionRegistry only to enumerate collections for
// Collections — Search receives an already-resolved CollectionSpec from its
// caller (the Phase-6 barricade), so a plain search costs no extra registry
// round-trip.
type KnowledgeRetriever struct {
	client   *http.Client
	baseURL  string
	registry knowledge.CollectionRegistry
}

// NewKnowledgeRetriever returns a KnowledgeRetriever over the OpenSearch
// cluster at baseURL, using registry to enumerate collections for
// Collections. client and registry must not be nil.
func NewKnowledgeRetriever(client *http.Client, baseURL string, registry knowledge.CollectionRegistry) *KnowledgeRetriever {
	return &KnowledgeRetriever{client: client, baseURL: strings.TrimRight(baseURL, "/"), registry: registry}
}

// Search runs one BM25 query over spec's collection, applying filters and
// sort (both generic, validated against spec.Mappings) and returning up to k
// hits ranked by relevance (or by sort, when given). query may be empty when
// filters alone should select documents (filter-only search). It issues
// zero embedding calls (KnowledgeRetriever holds no embed.Embedder — a
// structural guarantee, not just a tested one) and never registers with
// MultiRetriever/the RRF pipeline.
//
// By default each hit carries highlight-extracted Fragments (sized by
// spec.FragmentSizing, wrapped in the spec's opt-in marker tags — both empty
// means clean extraction) and its Fields OMIT the text field: fragments
// replace the body, and GetDocument is the drill-down for the whole
// document. fullBody=true restores the pre-fragment behavior byte-for-byte:
// no highlighting, body inline in Fields. A filter-only (empty-query) search
// under the default matches no terms, so its hits carry scalars only —
// neither fragments nor body — which is expected, not an error.
func (r *KnowledgeRetriever) Search(ctx context.Context, spec knowledge.CollectionSpec, query string, filters []Predicate, sortKeys []SortKey, k int, fullBody bool) ([]Hit, error) {
	if err := validateKnowledgeIndex(spec.Index); err != nil {
		return nil, err
	}
	if spec.TextField == "" {
		return nil, fmt.Errorf("retrieval: collection %q has no text field configured", spec.Name)
	}
	filterClauses, err := buildFilterClauses(spec, filters)
	if err != nil {
		return nil, err
	}
	sortClauses, err := buildSortClauses(spec, sortKeys)
	if err != nil {
		return nil, err
	}
	opts := queryOpts{
		mode: ModeBM25Only, textField: spec.TextField, text: query,
		k: clampK(k), filters: filterClauses, sort: sortClauses,
	}
	if !fullBody {
		opts.fragmentSize, opts.numberOfFragments = spec.FragmentSizing()
		opts.highlightPreTag, opts.highlightPostTag = spec.HighlightPreTag, spec.HighlightPostTag
	}
	body, _ := buildQuery(opts)

	status, decoded, err := postSearch(ctx, r.client, r.baseURL+"/"+spec.Index+"/_search", body)
	if err != nil {
		return nil, fmt.Errorf("retrieval: searching knowledge collection %q: %w", spec.Name, err)
	}
	if isKnowledgeIndexNotFound(status, decoded) {
		return nil, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("retrieval: searching knowledge collection %q: unexpected status %d: %v", spec.Name, status, decoded)
	}
	return parseHits(decoded, spec.Name), nil
}

// GetDocument fetches one knowledge document by doc id via realtime GET —
// the memory_read drill-down for a search hit whose body was suppressed in
// favor of fragments. It returns the FULL stored _source (body and harvest
// provenance included); ok=false means no such doc, which also covers a
// registered-but-unprovisioned index (the house index-not-found-as-empty
// rule). id is caller/harvester-chosen external input embedded in a REST
// path, so it is path-escaped, never interpolated raw; authorization is the
// caller's job (server/read.go authorizes BEFORE fetching).
func (r *KnowledgeRetriever) GetDocument(ctx context.Context, spec knowledge.CollectionSpec, id string) (map[string]any, bool, error) {
	if err := validateKnowledgeIndex(spec.Index); err != nil {
		return nil, false, err
	}
	if id == "" {
		return nil, false, nil // an empty id can address nothing; opaque miss
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL+"/"+spec.Index+"/_doc/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, false, fmt.Errorf("retrieval: building knowledge get request: %w", err)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("retrieval: getting knowledge doc %q from %q: %w", id, spec.Name, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, fmt.Errorf("retrieval: reading knowledge doc %q from %q: %w", id, spec.Name, err)
	}
	var decoded map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &decoded)
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		doc, _ := decoded["_source"].(map[string]any)
		if doc == nil {
			return nil, false, fmt.Errorf("retrieval: knowledge doc %q from %q has no _source", id, spec.Name)
		}
		return doc, true, nil
	case resp.StatusCode == http.StatusNotFound:
		// Both a missing doc ({"found": false}) and a missing index read as
		// "no such doc" — absence, not infrastructure failure.
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("retrieval: getting knowledge doc %q from %q: unexpected status %d: %v", id, spec.Name, resp.StatusCode, decoded)
	}
}

// Collections reports document count and staleness for every collection the
// registry knows about (unfiltered by access — Phase 6's barricade applies
// read authorization after this). A collection whose live index isn't
// provisioned yet reads as zero/undated, not an error (the house
// index-not-found-as-empty rule; matches Phase 3's documented Create-partial-
// failure repair scenario).
func (r *KnowledgeRetriever) Collections(ctx context.Context) ([]CollectionMeta, error) {
	summaries, err := r.registry.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("retrieval: listing knowledge collections: %w", err)
	}
	metas := make([]CollectionMeta, 0, len(summaries))
	for _, s := range summaries {
		spec, err := r.registry.Get(ctx, s.Name)
		if err != nil {
			if errors.Is(err, knowledge.ErrNotFound) {
				continue // deleted between List and Get; skip rather than fail the whole page
			}
			return nil, fmt.Errorf("retrieval: resolving knowledge collection %q: %w", s.Name, err)
		}
		meta, err := r.collectionMeta(ctx, spec)
		if err != nil {
			return nil, err
		}
		metas = append(metas, meta)
	}
	return metas, nil
}

// collectionMeta runs one staleness aggregation for spec: a "size":0 query
// with an exact total-hit count (track_total_hits, so Count is never a
// silently-capped estimate) plus a max aggregation over harvested_at and
// every date-typed Mappings field.
func (r *KnowledgeRetriever) collectionMeta(ctx context.Context, spec knowledge.CollectionSpec) (CollectionMeta, error) {
	if err := validateKnowledgeIndex(spec.Index); err != nil {
		return CollectionMeta{}, err
	}
	dateFields := dateMappingFields(spec)
	aggs := map[string]any{"newest_harvested_at": map[string]any{"max": map[string]any{"field": "harvested_at"}}}
	for i, f := range dateFields {
		aggs[docDateAggKey(i)] = map[string]any{"max": map[string]any{"field": f}}
	}
	body, _ := json.Marshal(map[string]any{"size": 0, "track_total_hits": true, "aggs": aggs})

	status, decoded, err := postSearch(ctx, r.client, r.baseURL+"/"+spec.Index+"/_search", body)
	if err != nil {
		return CollectionMeta{}, fmt.Errorf("retrieval: aggregating knowledge collection %q: %w", spec.Name, err)
	}
	if isKnowledgeIndexNotFound(status, decoded) {
		return CollectionMeta{Name: spec.Name}, nil
	}
	if status != http.StatusOK {
		return CollectionMeta{}, fmt.Errorf("retrieval: aggregating knowledge collection %q: unexpected status %d: %v", spec.Name, status, decoded)
	}

	meta := CollectionMeta{Name: spec.Name, Count: totalHits(decoded)}
	aggsOut, _ := decoded["aggregations"].(map[string]any)
	meta.NewestHarvestedAt = maxDateFromAgg(aggsOut, "newest_harvested_at")
	for i := range dateFields {
		t := maxDateFromAgg(aggsOut, docDateAggKey(i))
		if t != nil && (meta.NewestDocDate == nil || t.After(*meta.NewestDocDate)) {
			meta.NewestDocDate = t
		}
	}
	return meta, nil
}

// docDateAggKey names the per-date-field aggregation bucket ("newest_doc_date"
// is reserved for the memory-comparable name in CollectionMeta itself, so the
// per-field aggs are indexed to avoid colliding with it or each other).
func docDateAggKey(i int) string { return fmt.Sprintf("newest_doc_date_%d", i) }

// dateMappingFields returns the sorted names of spec's date-typed Mappings
// fields — the generic (never-hardcoded) definition of "this collection's
// doc-date signal" (Design Decision, discovery doc).
func dateMappingFields(spec knowledge.CollectionSpec) []string {
	var out []string
	for name, f := range spec.Mappings {
		if f.Type == "date" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// totalHits reads the exact hit count from a track_total_hits:true response.
func totalHits(decoded map[string]any) int64 {
	hitsField, _ := decoded["hits"].(map[string]any)
	total, _ := hitsField["total"].(map[string]any)
	v, _ := total["value"].(float64)
	return int64(v)
}

// maxDateFromAgg reads a "max" aggregation's numeric value (epoch millis) and
// converts it to a UTC time. A missing/null value (no documents, or none
// with that field set) returns nil — read directly from the numeric "value"
// rather than "value_as_string" so parsing doesn't depend on a field's
// configured date format.
func maxDateFromAgg(aggs map[string]any, key string) *time.Time {
	agg, _ := aggs[key].(map[string]any)
	v, ok := agg["value"].(float64)
	if !ok {
		return nil
	}
	t := time.UnixMilli(int64(v)).UTC()
	return &t
}

// filterOpValidity documents Predicate.Op's vocabulary for error messages.
const filterOpValidity = "term, range, prefix"

// filterableFields returns the sorted names of spec's filterable Mappings
// fields, for a self-correcting "unknown field" error message.
func filterableFields(spec knowledge.CollectionSpec) []string {
	var out []string
	for name, f := range spec.Mappings {
		if f.Filterable {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// sortableFields returns the sorted names of spec's sortable Mappings
// fields, for a self-correcting "unknown field" error message.
func sortableFields(spec knowledge.CollectionSpec) []string {
	var out []string
	for name, f := range spec.Mappings {
		if f.Sortable {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// fieldListOrNone renders a field list for an error message, so "no fields
// declared" reads as an explicit statement rather than a blank tail.
func fieldListOrNone(fields []string) string {
	if len(fields) == 0 {
		return "(none declared)"
	}
	return strings.Join(fields, ", ")
}

// buildFilterClauses validates each predicate's Field against spec.Mappings
// (must exist and be Filterable) and builds the corresponding OpenSearch
// filter clause. An unknown/unfilterable field or malformed predicate
// returns a validation error naming the offending value and, for field
// errors, the valid filterable fields (self-correcting for the LLM caller,
// DW-5.2).
func buildFilterClauses(spec knowledge.CollectionSpec, filters []Predicate) ([]any, error) {
	if len(filters) == 0 {
		return nil, nil
	}
	clauses := make([]any, 0, len(filters))
	for _, p := range filters {
		fs, ok := spec.Mappings[p.Field]
		if !ok || !fs.Filterable {
			return nil, fmt.Errorf("retrieval: unknown or unfilterable field %q on collection %q; valid filterable fields: %s",
				p.Field, spec.Name, fieldListOrNone(filterableFields(spec)))
		}
		clause, err := filterClause(p.Field, p.Op, p.Value)
		if err != nil {
			return nil, err
		}
		clauses = append(clauses, clause)
	}
	return clauses, nil
}

// filterClause builds one OpenSearch filter clause for a single validated
// predicate field.
func filterClause(field, op string, value any) (any, error) {
	switch op {
	case "term":
		return map[string]any{"term": map[string]any{field: value}}, nil
	case "prefix":
		return map[string]any{"prefix": map[string]any{field: value}}, nil
	case "range":
		bounds, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("retrieval: range filter on field %q requires a value of gte/lte bounds, got %T", field, value)
		}
		rng := make(map[string]any, 2)
		for _, b := range [2]string{"gte", "lte"} {
			if v, present := bounds[b]; present {
				rng[b] = v
			}
		}
		if len(rng) == 0 {
			return nil, fmt.Errorf("retrieval: range filter on field %q must set at least one of \"gte\"/\"lte\"", field)
		}
		return map[string]any{"range": map[string]any{field: rng}}, nil
	default:
		return nil, fmt.Errorf("retrieval: unsupported filter op %q on field %q; valid ops: %s", op, field, filterOpValidity)
	}
}

// buildSortClauses validates each sort key's Field against spec.Mappings
// (must exist and be Sortable) and Order ("asc"|"desc"), returning the
// OpenSearch sort array. An unknown/unsortable field or invalid order
// returns a self-correcting validation error (DW-5.3).
func buildSortClauses(spec knowledge.CollectionSpec, sortKeys []SortKey) ([]any, error) {
	if len(sortKeys) == 0 {
		return nil, nil
	}
	clauses := make([]any, 0, len(sortKeys))
	for _, s := range sortKeys {
		fs, ok := spec.Mappings[s.Field]
		if !ok || !fs.Sortable {
			return nil, fmt.Errorf("retrieval: unknown or unsortable field %q on collection %q; valid sortable fields: %s",
				s.Field, spec.Name, fieldListOrNone(sortableFields(spec)))
		}
		if s.Order != "asc" && s.Order != "desc" {
			return nil, fmt.Errorf("retrieval: invalid sort order %q on field %q; valid orders: asc, desc", s.Order, s.Field)
		}
		clauses = append(clauses, map[string]any{s.Field: map[string]any{"order": s.Order}})
	}
	return clauses, nil
}

// isKnowledgeIndexNotFound mirrors store.isIndexNotFound (unexported there,
// redeclared here for the same cross-package reason as postSearch): a
// not-yet-provisioned collection index reads as empty, not broken.
func isKnowledgeIndexNotFound(status int, decoded map[string]any) bool {
	if status != http.StatusNotFound {
		return false
	}
	errObj, _ := decoded["error"].(map[string]any)
	typ, _ := errObj["type"].(string)
	return typ == "index_not_found_exception"
}

// postSearch issues a raw OpenSearch _search POST and returns the status
// code and decoded body — the retrieval-package sibling of store.doJSON
// (unexported there; a cross-package export for one caller isn't worth it,
// mirroring Phase 4's doNDJSON precedent). tierRetriever.search keeps its
// own inline copy of this plumbing untouched: DW-5.1 pins the memory path
// byte-for-byte, so refactoring it here for deduplication would be needless
// regression risk for no behavioral gain.
func postSearch(ctx context.Context, client *http.Client, url string, body []byte) (int, map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("retrieval: building search request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("retrieval: search request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("retrieval: reading search response: %w", err)
	}
	var decoded map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &decoded)
	}
	return resp.StatusCode, decoded, nil
}

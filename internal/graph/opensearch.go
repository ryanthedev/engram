package graph

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Resource names for the T4 indices. Both key docs by their deterministic
// id (Entity.ID / edgeFingerprint), so upserts are idempotent.
const (
	// EntityTemplateName / EntityIndex name the entity template and its
	// concrete index.
	EntityTemplateName = "engram-graph-entities"
	EntityIndex        = "engram-graph-entities-000001"
	// EdgeTemplateName / EdgeIndex name the edge template and its concrete
	// index.
	EdgeTemplateName = "engram-graph-edges"
	EdgeIndex        = "engram-graph-edges-000001"
)

//go:embed templates/entities.json
var entityTemplateJSON []byte

//go:embed templates/edges.json
var edgeTemplateJSON []byte

// Apply idempotently applies the T4 cluster contract (entity + edge index
// templates and their concrete indices) to the OpenSearch at baseURL. It
// mirrors store.Apply's and experience.Apply's upsert idiom — re-running is
// a no-op — and keeps the graph indices' contract self-contained in this
// package (Phase 6's file scope), like Phase 5 did for T3.
func Apply(ctx context.Context, client *http.Client, baseURL string) error {
	if client == nil {
		client = http.DefaultClient
	}
	base := strings.TrimRight(baseURL, "/")
	steps := []struct {
		method, path string
		body         []byte
	}{
		{http.MethodPut, "/_index_template/" + EntityTemplateName, entityTemplateJSON},
		{http.MethodPut, "/_index_template/" + EdgeTemplateName, edgeTemplateJSON},
	}
	for _, s := range steps {
		if err := osDo(ctx, client, s.method, base+s.path, s.body); err != nil {
			return fmt.Errorf("graph: applying %s: %w", s.path, err)
		}
	}
	for _, idx := range []string{EntityIndex, EdgeIndex} {
		err := osDo(ctx, client, http.MethodPut, base+"/"+idx, nil)
		if err != nil && !strings.Contains(err.Error(), "resource_already_exists_exception") {
			return fmt.Errorf("graph: creating index %s: %w", idx, err)
		}
	}
	return nil
}

// OpenSearchBackend is the production Backend over the entity/edge indices.
type OpenSearchBackend struct {
	client      *http.Client
	baseURL     string
	entityIndex string
	edgeIndex   string
}

var _ Backend = (*OpenSearchBackend)(nil)

// BackendOption configures an OpenSearchBackend (tests point at scratch
// indices).
type BackendOption func(*OpenSearchBackend)

// WithIndices overrides the entity/edge index names.
func WithIndices(entity, edge string) BackendOption {
	return func(b *OpenSearchBackend) { b.entityIndex, b.edgeIndex = entity, edge }
}

// NewOpenSearchBackend returns a Backend over the cluster at baseURL. client
// must not be nil.
func NewOpenSearchBackend(client *http.Client, baseURL string, opts ...BackendOption) *OpenSearchBackend {
	b := &OpenSearchBackend{client: client, baseURL: strings.TrimRight(baseURL, "/"), entityIndex: EntityIndex, edgeIndex: EdgeIndex}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// CandidateEntities implements Backend: a fuzzy multi_match over name +
// aliases, tenant-filtered, live-only, top 20 — real BM25-scored recall
// (Deduper.Decide runs its OWN self-contained lexical/embedding scoring on
// top of whatever this returns, so the exact ranking OpenSearch applies here
// does not matter — only that true near-duplicates are IN the candidate
// set). operator=and requires EVERY query token to match (fuzzily, so typos
// still tolerate) — without it, two otherwise-unrelated entities that merely
// share a common word (e.g. two different chain ids sharing a test-name
// prefix) would both qualify on a single shared token, flooding the
// candidate set with irrelevant recall.
func (b *OpenSearchBackend) CandidateEntities(ctx context.Context, tenantID, name string) ([]Entity, error) {
	q := map[string]any{
		"size": 20,
		"query": map[string]any{"bool": map[string]any{
			"filter": []any{map[string]any{"term": map[string]any{"tenant_id": tenantID}}},
			"must": []any{map[string]any{"multi_match": map[string]any{
				"query": name, "fields": []string{"name", "aliases"}, "fuzziness": "AUTO", "operator": "and",
			}}},
			"must_not": []any{map[string]any{"exists": map[string]any{"field": "expired_at"}}},
		}},
	}
	body, err := json.Marshal(q)
	if err != nil {
		return nil, fmt.Errorf("graph: encoding candidate query: %w", err)
	}
	status, decoded, err := osJSON(ctx, b.client, http.MethodPost, b.baseURL+"/"+b.entityIndex+"/_search", body)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("graph: candidate search returned status %d: %v", status, decoded)
	}
	var out []Entity
	for _, hit := range osSearchHits(decoded) {
		var e Entity
		if err := osDecodeSource(hit, &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// PutEntity implements Backend.
func (b *OpenSearchBackend) PutEntity(ctx context.Context, e Entity) error {
	body, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("graph: encoding entity: %w", err)
	}
	url := fmt.Sprintf("%s/%s/_doc/%s?refresh=true", b.baseURL, b.entityIndex, e.ID)
	return osDo(ctx, b.client, http.MethodPut, url, body)
}

// GetEntity implements Backend.
func (b *OpenSearchBackend) GetEntity(ctx context.Context, tenantID, id string) (Entity, bool, error) {
	status, decoded, err := osJSON(ctx, b.client, http.MethodGet, fmt.Sprintf("%s/%s/_doc/%s", b.baseURL, b.entityIndex, id), nil)
	if err != nil {
		return Entity{}, false, err
	}
	if status == http.StatusNotFound {
		return Entity{}, false, nil
	}
	if status != http.StatusOK {
		return Entity{}, false, fmt.Errorf("graph: get entity %s status %d", id, status)
	}
	var e Entity
	if err := osDecodeSource(decoded, &e); err != nil {
		return Entity{}, false, err
	}
	if e.TenantID != tenantID {
		return Entity{}, false, nil // cross-tenant id collision guard (fail-closed)
	}
	return e, true, nil
}

// CountEntities implements Backend via _count, live-only. A missing index
// (e.g. a fresh/scratch instance whose first entity hasn't landed yet)
// reads 0, not an error — the same index_not_found_exception guard Phase 1
// established for internal/store's read paths.
func (b *OpenSearchBackend) CountEntities(ctx context.Context, tenantID string) (int, error) {
	q := map[string]any{"query": map[string]any{"bool": map[string]any{
		"filter":   []any{map[string]any{"term": map[string]any{"tenant_id": tenantID}}},
		"must_not": []any{map[string]any{"exists": map[string]any{"field": "expired_at"}}},
	}}}
	body, _ := json.Marshal(q)
	status, decoded, err := osJSON(ctx, b.client, http.MethodPost, b.baseURL+"/"+b.entityIndex+"/_count", body)
	if err != nil {
		return 0, err
	}
	if isIndexNotFound(status, decoded) {
		return 0, nil // index not created yet: count 0
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("graph: count entities status %d: %v", status, decoded)
	}
	count, _ := decoded["count"].(float64)
	return int(count), nil
}

// CountAllEntities implements Backend via a live-only _count with no tenant
// filter — Phase 3's all-tenant graph telemetry signal (the DW-6.3
// stability metric wired onto /metrics). A missing index reads 0, not an
// error (same guard as CountEntities).
func (b *OpenSearchBackend) CountAllEntities(ctx context.Context) (int, error) {
	q := map[string]any{"query": map[string]any{"bool": map[string]any{
		"must_not": []any{map[string]any{"exists": map[string]any{"field": "expired_at"}}},
	}}}
	body, _ := json.Marshal(q)
	status, decoded, err := osJSON(ctx, b.client, http.MethodPost, b.baseURL+"/"+b.entityIndex+"/_count", body)
	if err != nil {
		return 0, err
	}
	if isIndexNotFound(status, decoded) {
		return 0, nil // index not created yet: count 0
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("graph: count all entities status %d: %v", status, decoded)
	}
	count, _ := decoded["count"].(float64)
	return int(count), nil
}

// PutEdge implements Backend.
func (b *OpenSearchBackend) PutEdge(ctx context.Context, e Edge) error {
	body, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("graph: encoding edge: %w", err)
	}
	url := fmt.Sprintf("%s/%s/_doc/%s?refresh=true", b.baseURL, b.edgeIndex, e.ID)
	return osDo(ctx, b.client, http.MethodPut, url, body)
}

// GetEdge implements Backend.
func (b *OpenSearchBackend) GetEdge(ctx context.Context, tenantID, id string) (Edge, bool, error) {
	status, decoded, err := osJSON(ctx, b.client, http.MethodGet, fmt.Sprintf("%s/%s/_doc/%s", b.baseURL, b.edgeIndex, id), nil)
	if err != nil {
		return Edge{}, false, err
	}
	if status == http.StatusNotFound {
		return Edge{}, false, nil
	}
	if status != http.StatusOK {
		return Edge{}, false, fmt.Errorf("graph: get edge %s status %d", id, status)
	}
	var e Edge
	if err := osDecodeSource(decoded, &e); err != nil {
		return Edge{}, false, err
	}
	if e.TenantID != tenantID {
		return Edge{}, false, nil
	}
	return e, true, nil
}

// Neighbors implements Backend: every live edge touching entityID, either
// direction, tenant-scoped.
func (b *OpenSearchBackend) Neighbors(ctx context.Context, tenantID, entityID string) ([]Edge, error) {
	q := map[string]any{
		"size": 1000,
		"query": map[string]any{"bool": map[string]any{
			"filter": []any{
				map[string]any{"term": map[string]any{"tenant_id": tenantID}},
				map[string]any{"bool": map[string]any{"should": []any{
					map[string]any{"term": map[string]any{"from_entity_id": entityID}},
					map[string]any{"term": map[string]any{"to_entity_id": entityID}},
				}, "minimum_should_match": 1}},
			},
			"must_not": []any{
				map[string]any{"exists": map[string]any{"field": "expired_at"}},
				map[string]any{"exists": map[string]any{"field": "invalid_at"}},
			},
		}},
	}
	body, _ := json.Marshal(q)
	status, decoded, err := osJSON(ctx, b.client, http.MethodPost, b.baseURL+"/"+b.edgeIndex+"/_search", body)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("graph: neighbors search status %d: %v", status, decoded)
	}
	var out []Edge
	for _, hit := range osSearchHits(decoded) {
		var e Edge
		if err := osDecodeSource(hit, &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// ScanEntities implements Backend: one page of live, tenant-scoped entities
// sorted ascending by id, using OpenSearch's search_after over that sort —
// a deterministic total order, so repeated calls with the advancing cursor
// paginate the whole tier completely and stably (no duplicate/skipped docs
// across pages, even as the underlying index keeps accepting writes
// elsewhere — this is a snapshot-per-page, not a point-in-time snapshot of
// the whole tier; see the plan's "acceptable that concurrent writes during
// export may be missed" note). A missing index (isIndexNotFound) reads as
// an empty, exhausted page rather than an error — the same guard
// CountEntities uses.
func (b *OpenSearchBackend) ScanEntities(ctx context.Context, tenantID string, cursor Cursor) ([]Entity, Cursor, error) {
	q := scanQuery(tenantID, cursor, []any{map[string]any{"exists": map[string]any{"field": "expired_at"}}})
	body, err := json.Marshal(q)
	if err != nil {
		return nil, Cursor{}, fmt.Errorf("graph: encoding scan-entities query: %w", err)
	}
	status, decoded, err := osJSON(ctx, b.client, http.MethodPost, b.baseURL+"/"+b.entityIndex+"/_search", body)
	if err != nil {
		return nil, Cursor{}, err
	}
	if isIndexNotFound(status, decoded) {
		return nil, Cursor{}, nil
	}
	if status != http.StatusOK {
		return nil, Cursor{}, fmt.Errorf("graph: scan entities returned status %d: %v", status, decoded)
	}
	hits := osSearchHits(decoded)
	out := make([]Entity, 0, len(hits))
	for _, hit := range hits {
		var e Entity
		if err := osDecodeSource(hit, &e); err != nil {
			return nil, Cursor{}, err
		}
		out = append(out, e)
	}
	return out, nextScanCursor(len(out), func() string { return out[len(out)-1].ID }), nil
}

// ScanEdges is ScanEntities' edge-tier counterpart. The live filter differs
// from the entity tier per the plan constraint: an edge must have neither
// expired_at nor invalid_at set (Edge.Live()), whereas an entity checks
// expired_at alone.
func (b *OpenSearchBackend) ScanEdges(ctx context.Context, tenantID string, cursor Cursor) ([]Edge, Cursor, error) {
	q := scanQuery(tenantID, cursor, []any{
		map[string]any{"exists": map[string]any{"field": "expired_at"}},
		map[string]any{"exists": map[string]any{"field": "invalid_at"}},
	})
	body, err := json.Marshal(q)
	if err != nil {
		return nil, Cursor{}, fmt.Errorf("graph: encoding scan-edges query: %w", err)
	}
	status, decoded, err := osJSON(ctx, b.client, http.MethodPost, b.baseURL+"/"+b.edgeIndex+"/_search", body)
	if err != nil {
		return nil, Cursor{}, err
	}
	if isIndexNotFound(status, decoded) {
		return nil, Cursor{}, nil
	}
	if status != http.StatusOK {
		return nil, Cursor{}, fmt.Errorf("graph: scan edges returned status %d: %v", status, decoded)
	}
	hits := osSearchHits(decoded)
	out := make([]Edge, 0, len(hits))
	for _, hit := range hits {
		var e Edge
		if err := osDecodeSource(hit, &e); err != nil {
			return nil, Cursor{}, err
		}
		out = append(out, e)
	}
	return out, nextScanCursor(len(out), func() string { return out[len(out)-1].ID }), nil
}

// scanQuery builds the shared shape Scan{Entities,Edges} sends: tenant-term
// filter, size-bounded, sorted ascending by id (the search_after tie-break
// field), must_not the tier's own liveness-exclusion clauses, and
// search_after set from cursor.after when resuming.
func scanQuery(tenantID string, cursor Cursor, liveExclusions []any) map[string]any {
	q := map[string]any{
		"size": scanBatchSize,
		"sort": []any{map[string]any{"id": "asc"}},
		"query": map[string]any{"bool": map[string]any{
			"filter":   []any{map[string]any{"term": map[string]any{"tenant_id": tenantID}}},
			"must_not": liveExclusions,
		}},
	}
	if cursor.after != "" {
		q["search_after"] = []any{cursor.after}
	}
	return q
}

// nextScanCursor derives the returned next Cursor from a page's hit count:
// a full page (== scanBatchSize) may have more behind it, so next resumes
// after lastID(); a short page is the tier's last page, so next is zero
// (exhausted). A tenant whose live-record count is an exact multiple of
// scanBatchSize costs one extra, empty round trip to discover exhaustion —
// an accepted, standard pagination trade.
func nextScanCursor(hitCount int, lastID func() string) Cursor {
	if hitCount == scanBatchSize {
		return Cursor{after: lastID()}
	}
	return Cursor{}
}

// --- small OpenSearch HTTP helpers (self-contained; internal/store's are
// unexported, so graph carries its own thin copies, matching experience's
// osDo/osJSON/osSearchHits/osDecodeSource precedent) ---

// isIndexNotFound reports whether an OpenSearch response is the specific
// index_not_found_exception (HTTP 404 whose error.type is exactly that
// literal) — a read that reached an index which does not exist yet. The
// entity-count reads translate this ONE shape to an empty result: a
// not-yet-created entity index has 0 entities, not an error. Mirrors
// internal/store's and internal/experience's isIndexNotFound; this package
// keeps its own copy per the self-contained-helpers convention above.
func isIndexNotFound(status int, decoded map[string]any) bool {
	if status != http.StatusNotFound {
		return false
	}
	errObj, _ := decoded["error"].(map[string]any)
	typ, _ := errObj["type"].(string)
	return typ == "index_not_found_exception"
}

func osDo(ctx context.Context, client *http.Client, method, url string, body []byte) error {
	status, decoded, err := osJSON(ctx, client, method, url, body)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("%s %s returned %d: %v", method, url, status, decoded)
	}
	return nil
}

func osJSON(ctx context.Context, client *http.Client, method, url string, body []byte) (int, map[string]any, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	var decoded map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &decoded)
	}
	return resp.StatusCode, decoded, nil
}

func osSearchHits(decoded map[string]any) []map[string]any {
	hitsField, _ := decoded["hits"].(map[string]any)
	rawHits, _ := hitsField["hits"].([]any)
	out := make([]map[string]any, 0, len(rawHits))
	for _, rh := range rawHits {
		if hm, ok := rh.(map[string]any); ok {
			out = append(out, hm)
		}
	}
	return out
}

func osDecodeSource(hit map[string]any, dst any) error {
	src, ok := hit["_source"]
	if !ok {
		return fmt.Errorf("graph: response carries no _source")
	}
	raw, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, dst)
}

// DefaultTimeout is a sane default for callers building an *http.Client for
// this package (mirrors store.DefaultTimeout).
const DefaultTimeout = 30 * time.Second

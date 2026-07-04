package experience

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

// Resource names for the T3 indices. The admitted index holds retrievable
// experiences; the quarantine index holds parked ones (never retrievable). Both
// key docs by the experience Fingerprint (which already encodes the tenant), so
// _id is globally unique and grant/dedup are idempotent upserts.
const (
	// ExperienceTemplateName / ExperienceIndex name the admitted T3 template and
	// its concrete index.
	ExperienceTemplateName = "engram-experience"
	ExperienceIndex        = "engram-experience-000001"
	// QuarantineTemplateName / QuarantineIndex name the quarantine template and
	// its concrete index. Its pattern outranks the admitted template (priority
	// 200 > 100) so the quarantine index gets the quarantine mapping.
	QuarantineTemplateName = "engram-experience-quarantine"
	QuarantineIndex        = "engram-experience-quarantine-000001"
)

//go:embed templates/experience.json
var experienceTemplateJSON []byte

//go:embed templates/quarantine.json
var quarantineTemplateJSON []byte

// Apply idempotently applies the T3 cluster contract (admitted + quarantine
// index templates and their concrete indices) to the OpenSearch at baseURL. It
// mirrors store.Apply's upsert idiom so re-running is a no-op, and is called
// from the server wiring alongside store.Apply. Keeping the experience indices'
// contract in this package (not internal/store) keeps Phase 5's file scope
// self-contained and makes the experience module own its own storage.
func Apply(ctx context.Context, client *http.Client, baseURL string) error {
	if client == nil {
		client = http.DefaultClient
	}
	base := strings.TrimRight(baseURL, "/")
	steps := []struct {
		method, path string
		body         []byte
	}{
		{http.MethodPut, "/_index_template/" + QuarantineTemplateName, quarantineTemplateJSON},
		{http.MethodPut, "/_index_template/" + ExperienceTemplateName, experienceTemplateJSON},
	}
	for _, s := range steps {
		if err := osDo(ctx, client, s.method, base+s.path, s.body); err != nil {
			return fmt.Errorf("experience: applying %s: %w", s.path, err)
		}
	}
	for _, idx := range []string{ExperienceIndex, QuarantineIndex} {
		err := osDo(ctx, client, http.MethodPut, base+"/"+idx, nil)
		if err != nil && !strings.Contains(err.Error(), "resource_already_exists_exception") {
			return fmt.Errorf("experience: creating index %s: %w", idx, err)
		}
	}
	return nil
}

// OpenSearchBackend is the production Backend over the two T3 indices. It
// performs NO gating (the gate lives in Store); it is pure storage.
type OpenSearchBackend struct {
	client        *http.Client
	baseURL       string
	admittedIndex string
	quarantineIdx string
}

var (
	_ Backend         = (*OpenSearchBackend)(nil)
	_ retrievalBumper = (*OpenSearchBackend)(nil)
)

// BackendOption configures an OpenSearchBackend (tests point at scratch indices).
type BackendOption func(*OpenSearchBackend)

// WithIndices overrides the admitted/quarantine index names.
func WithIndices(admitted, quarantine string) BackendOption {
	return func(b *OpenSearchBackend) { b.admittedIndex, b.quarantineIdx = admitted, quarantine }
}

// NewOpenSearchBackend returns a Backend over the cluster at baseURL. client
// must not be nil.
func NewOpenSearchBackend(client *http.Client, baseURL string, opts ...BackendOption) *OpenSearchBackend {
	b := &OpenSearchBackend{
		client:        client,
		baseURL:       strings.TrimRight(baseURL, "/"),
		admittedIndex: ExperienceIndex,
		quarantineIdx: QuarantineIndex,
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// PutAdmitted implements Backend: upsert into the admitted index by fingerprint.
func (b *OpenSearchBackend) PutAdmitted(ctx context.Context, exp Experience) error {
	body, err := json.Marshal(exp)
	if err != nil {
		return fmt.Errorf("experience: encoding admitted doc: %w", err)
	}
	url := fmt.Sprintf("%s/%s/_doc/%s?refresh=true", b.baseURL, b.admittedIndex, exp.Fingerprint)
	return osDo(ctx, b.client, http.MethodPut, url, body)
}

// GetAdmitted implements Backend, fail-closed on a cross-tenant id collision.
func (b *OpenSearchBackend) GetAdmitted(ctx context.Context, tenantID, fingerprint string, includeExpired bool) (Experience, bool, error) {
	status, decoded, err := osJSON(ctx, b.client, http.MethodGet, fmt.Sprintf("%s/%s/_doc/%s", b.baseURL, b.admittedIndex, fingerprint), nil)
	if err != nil {
		return Experience{}, false, err
	}
	if status == http.StatusNotFound {
		return Experience{}, false, nil
	}
	if status != http.StatusOK {
		return Experience{}, false, fmt.Errorf("experience: get admitted %s: status %d", fingerprint, status)
	}
	var exp Experience
	if err := osDecodeSource(decoded, &exp); err != nil {
		return Experience{}, false, err
	}
	if exp.TenantID != tenantID {
		return Experience{}, false, nil // cross-tenant id collision guard (fail-closed)
	}
	if !includeExpired && !exp.Live() {
		return Experience{}, false, nil
	}
	return exp, true, nil
}

// SearchAdmitted implements Backend: live, tenant-scoped multi_match, top-k.
func (b *OpenSearchBackend) SearchAdmitted(ctx context.Context, tenantID, query string, k int) ([]Experience, error) {
	filters := []any{
		map[string]any{"term": map[string]any{"tenant_id": tenantID}},
	}
	q := map[string]any{
		"size": k,
		"query": map[string]any{"bool": map[string]any{
			"filter":   filters,
			"must_not": []any{map[string]any{"exists": map[string]any{"field": "expired_at"}}},
		}},
	}
	if strings.TrimSpace(query) != "" {
		q["query"].(map[string]any)["bool"].(map[string]any)["must"] = []any{
			map[string]any{"multi_match": map[string]any{
				"query":  query,
				"fields": []string{"distilled_skill", "task", "context"},
			}},
		}
	}
	return b.search(ctx, q)
}

// ScanPrunable implements Backend: live docs at/below both prune ceilings.
func (b *OpenSearchBackend) ScanPrunable(ctx context.Context, phiMax float64, retrievalMax int) ([]Experience, error) {
	q := map[string]any{
		"size": 1000,
		"query": map[string]any{"bool": map[string]any{
			"filter": []any{
				map[string]any{"range": map[string]any{"utility": map[string]any{"lte": phiMax}}},
				map[string]any{"range": map[string]any{"retrieval_count": map[string]any{"lte": retrievalMax}}},
			},
			"must_not": []any{map[string]any{"exists": map[string]any{"field": "expired_at"}}},
		}},
	}
	return b.search(ctx, q)
}

func (b *OpenSearchBackend) search(ctx context.Context, query map[string]any) ([]Experience, error) {
	body, err := json.Marshal(query)
	if err != nil {
		return nil, fmt.Errorf("experience: encoding search: %w", err)
	}
	status, decoded, err := osJSON(ctx, b.client, http.MethodPost, b.baseURL+"/"+b.admittedIndex+"/_search", body)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("experience: search returned status %d: %v", status, decoded)
	}
	var out []Experience
	for _, hit := range osSearchHits(decoded) {
		var exp Experience
		if err := osDecodeSource(hit, &exp); err != nil {
			return nil, err
		}
		out = append(out, exp)
	}
	return out, nil
}

// SoftExpire implements Backend: partial-update expired_at (never deletes).
func (b *OpenSearchBackend) SoftExpire(ctx context.Context, _, fingerprint string, at time.Time) error {
	body, _ := json.Marshal(map[string]any{"doc": map[string]any{"expired_at": at.UTC()}})
	url := fmt.Sprintf("%s/%s/_update/%s?refresh=true", b.baseURL, b.admittedIndex, fingerprint)
	return osDo(ctx, b.client, http.MethodPost, url, body)
}

// BumpRetrieval implements the tier's optional retrievalBumper via a script update.
func (b *OpenSearchBackend) BumpRetrieval(ctx context.Context, _, fingerprint string) error {
	body, _ := json.Marshal(map[string]any{
		"script": map[string]any{"source": "ctx._source.retrieval_count += 1"},
	})
	url := fmt.Sprintf("%s/%s/_update/%s", b.baseURL, b.admittedIndex, fingerprint)
	return osDo(ctx, b.client, http.MethodPost, url, body)
}

// quarantineDoc is the persisted quarantine shape (top-level tenant/fingerprint
// for querying; the experience blob is stored but not indexed).
type quarantineDoc struct {
	TenantID    string     `json:"tenant_id"`
	Fingerprint string     `json:"fingerprint"`
	Verdict     Verdict    `json:"verdict"`
	Reason      string     `json:"reason"`
	At          time.Time  `json:"at"`
	Experience  Experience `json:"experience"`
}

// PutQuarantine implements Backend: upsert into the quarantine index.
func (b *OpenSearchBackend) PutQuarantine(ctx context.Context, q Quarantined) error {
	doc := quarantineDoc{
		TenantID: q.Experience.TenantID, Fingerprint: q.Experience.Fingerprint,
		Verdict: q.Verdict, Reason: q.Reason, At: q.At, Experience: q.Experience,
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("experience: encoding quarantine doc: %w", err)
	}
	url := fmt.Sprintf("%s/%s/_doc/%s?refresh=true", b.baseURL, b.quarantineIdx, q.Experience.Fingerprint)
	return osDo(ctx, b.client, http.MethodPut, url, body)
}

// ListQuarantine implements Backend, tenant-scoped.
func (b *OpenSearchBackend) ListQuarantine(ctx context.Context, tenantID string) ([]Quarantined, error) {
	q := map[string]any{
		"size":  1000,
		"query": map[string]any{"bool": map[string]any{"filter": []any{map[string]any{"term": map[string]any{"tenant_id": tenantID}}}}},
	}
	body, _ := json.Marshal(q)
	status, decoded, err := osJSON(ctx, b.client, http.MethodPost, b.baseURL+"/"+b.quarantineIdx+"/_search", body)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("experience: list quarantine status %d: %v", status, decoded)
	}
	var out []Quarantined
	for _, hit := range osSearchHits(decoded) {
		var d quarantineDoc
		if err := osDecodeSource(hit, &d); err != nil {
			return nil, err
		}
		out = append(out, Quarantined{Experience: d.Experience, Verdict: d.Verdict, Reason: d.Reason, At: d.At})
	}
	return out, nil
}

// GetQuarantine implements Backend by (tenant, fingerprint) key.
func (b *OpenSearchBackend) GetQuarantine(ctx context.Context, tenantID, fingerprint string) (Quarantined, bool, error) {
	status, decoded, err := osJSON(ctx, b.client, http.MethodGet, fmt.Sprintf("%s/%s/_doc/%s", b.baseURL, b.quarantineIdx, fingerprint), nil)
	if err != nil {
		return Quarantined{}, false, err
	}
	if status == http.StatusNotFound {
		return Quarantined{}, false, nil
	}
	if status != http.StatusOK {
		return Quarantined{}, false, fmt.Errorf("experience: get quarantine %s status %d", fingerprint, status)
	}
	var d quarantineDoc
	if err := osDecodeSource(decoded, &d); err != nil {
		return Quarantined{}, false, err
	}
	if d.TenantID != tenantID {
		return Quarantined{}, false, nil
	}
	return Quarantined{Experience: d.Experience, Verdict: d.Verdict, Reason: d.Reason, At: d.At}, true, nil
}

// DeleteQuarantine implements Backend; a 404 is not an error (idempotent).
func (b *OpenSearchBackend) DeleteQuarantine(ctx context.Context, _, fingerprint string) error {
	url := fmt.Sprintf("%s/%s/_doc/%s?refresh=true", b.baseURL, b.quarantineIdx, fingerprint)
	status, _, err := osJSON(ctx, b.client, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusNotFound {
		return fmt.Errorf("experience: delete quarantine %s status %d", fingerprint, status)
	}
	return nil
}

// --- small OpenSearch HTTP helpers (self-contained; internal/store's are
// unexported, so experience carries its own thin copies) ---

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
		return fmt.Errorf("experience: response carries no _source")
	}
	raw, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, dst)
}

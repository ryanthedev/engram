package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/knowledge"
)

// --- test doubles -----------------------------------------------------

// knowledgeReqCapture records one HTTP request a fake knowledge cluster saw.
type knowledgeReqCapture struct {
	path string
	raw  string
}

// newFakeKnowledgeServer returns an httptest server that records every
// request and answers via respond. White-box (package retrieval) so
// knowledge_test.go can reach buildQuery directly for the DW-5.1 regression
// guard; this fake-cluster helper is redeclared locally (rather than shared
// with opensearch_test.go's package retrieval_test) since the two test files
// live in different Go packages.
func newFakeKnowledgeServer(t *testing.T, respond func(path string) (int, any)) (*httptest.Server, func() []knowledgeReqCapture) {
	t.Helper()
	var mu sync.Mutex
	var captured []knowledgeReqCapture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		r.Body.Close()
		mu.Lock()
		captured = append(captured, knowledgeReqCapture{path: r.URL.Path, raw: string(raw)})
		mu.Unlock()
		status, body := respond(r.URL.Path)
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []knowledgeReqCapture {
		mu.Lock()
		defer mu.Unlock()
		return append([]knowledgeReqCapture(nil), captured...)
	}
}

func knowledgeHitsBody(total int64, hits ...map[string]any) map[string]any {
	if hits == nil {
		hits = []map[string]any{}
	}
	return map[string]any{"hits": map[string]any{"total": map[string]any{"value": total}, "hits": hits}}
}

func knowledgeHit(id string, score float64, source map[string]any) map[string]any {
	return map[string]any{"_id": id, "_score": score, "_source": source}
}

// fakeKnowledgeRegistry is an in-memory knowledge.CollectionRegistry backing
// Collections tests (mirrors internal/knowledge/seed_test.go's fakeRegistry).
type fakeKnowledgeRegistry struct {
	specs   map[string]knowledge.CollectionSpec
	listErr error
	getErr  error
}

func newFakeKnowledgeRegistry(specs ...knowledge.CollectionSpec) *fakeKnowledgeRegistry {
	r := &fakeKnowledgeRegistry{specs: map[string]knowledge.CollectionSpec{}}
	for _, s := range specs {
		r.specs[s.Name] = s
	}
	return r
}

func (f *fakeKnowledgeRegistry) Get(_ context.Context, name string) (knowledge.CollectionSpec, error) {
	if f.getErr != nil {
		return knowledge.CollectionSpec{}, f.getErr
	}
	spec, ok := f.specs[name]
	if !ok {
		return knowledge.CollectionSpec{}, fmt.Errorf("fake: %w", knowledge.ErrNotFound)
	}
	return spec, nil
}

func (f *fakeKnowledgeRegistry) Create(context.Context, knowledge.CollectionSpec) error { return nil }
func (f *fakeKnowledgeRegistry) Update(context.Context, knowledge.CollectionSpec) error { return nil }

func (f *fakeKnowledgeRegistry) List(context.Context) ([]knowledge.CollectionSummary, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]knowledge.CollectionSummary, 0, len(f.specs))
	for _, s := range f.specs {
		out = append(out, knowledge.CollectionSummary{Name: s.Name, Access: s.Access})
	}
	return out, nil
}

func (f *fakeKnowledgeRegistry) Provision(context.Context, string) error { return nil }

var _ knowledge.CollectionRegistry = (*fakeKnowledgeRegistry)(nil)

// arxivSpec is a representative collection spec used across the test file:
// non-default TextField (proving the retriever never hardcodes "text"),
// one filterable+sortable date field, one filterable-only keyword field.
func arxivSpec(index string) knowledge.CollectionSpec {
	return knowledge.CollectionSpec{
		Name:      "arxiv",
		Index:     index,
		TextField: "abstract",
		Mappings: map[string]knowledge.FieldSpec{
			"categories": {Type: "keyword", Filterable: true},
			"published":  {Type: "date", Filterable: true, Sortable: true},
			"internal":   {Type: "keyword"}, // declared but neither filterable nor sortable
		},
	}
}

// --- DW-5.1: BM25 search + the memory-path regression guard -----------

// TestKnowledgeSearchReturnsRankedHits covers DW-5.1's clean case: a BM25
// query over the collection's configured (non-default) TextField returns
// hits sourced with the collection name.
func TestKnowledgeSearchReturnsRankedHits(t *testing.T) {
	srv, captured := newFakeKnowledgeServer(t, func(string) (int, any) {
		return http.StatusOK, knowledgeHitsBody(2,
			knowledgeHit("2401.1", 1.9, map[string]any{"abstract": "chain of thought"}),
			knowledgeHit("2401.2", 1.1, map[string]any{"abstract": "code generation"}),
		)
	})
	r := NewKnowledgeRetriever(srv.Client(), srv.URL, newFakeKnowledgeRegistry())
	hits, err := r.Search(context.Background(), arxivSpec("knowledge-arxiv"), "chain-of-thought", nil, nil, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 || hits[0].ID != "2401.1" || hits[0].Source != "arxiv" {
		t.Fatalf("got %+v, want 2 hits sourced \"arxiv\", first id 2401.1", hits)
	}
	reqs := captured()
	if len(reqs) != 1 || reqs[0].path != "/knowledge-arxiv/_search" {
		t.Fatalf("captured requests = %+v, want one POST to /knowledge-arxiv/_search", reqs)
	}
	if !strings.Contains(reqs[0].raw, `"abstract":"chain-of-thought"`) {
		t.Errorf("request body missing BM25 match over the configured text field \"abstract\": %s", reqs[0].raw)
	}
	if strings.Contains(reqs[0].raw, `"knn"`) || strings.Contains(reqs[0].raw, `"hybrid"`) {
		t.Errorf("knowledge search must be BM25-only, no knn/hybrid: %s", reqs[0].raw)
	}
}

// TestBuildQueryMemoryPathByteIdenticalWhenSortNil is the DW-5.1 regression
// guard for the Assumption Verification: buildQuery gained an additive sort
// parameter (and a text=="" match_all fallback, unreachable from memory) for
// the knowledge retriever. Every existing memory caller passes sort=nil and
// MUST see byte-for-byte unchanged output. The "want" bodies below were
// captured from the UNMODIFIED buildQuery (pre-Phase-5) for the same inputs,
// spanning every branch memory traffic can hit — this is a golden-byte
// comparison, not a re-derivation of the implementation under test.
func TestBuildQueryMemoryPathByteIdenticalWhenSortNil(t *testing.T) {
	vec := []float32{0.1, 0.2, 0.3}
	filters := []any{map[string]any{"term": map[string]any{"tenant_id": "t1"}}}

	cases := []struct {
		name    string
		mode    SearchMode
		vec     []float32
		filters []any
		want    string
	}{
		{
			name: "bm25 only, no filters",
			mode: ModeBM25Only,
			want: `{"_source":{"excludes":["text_embedding","fact_embedding"]},"query":{"match":{"text":"hello"}},"size":5}`,
		},
		{
			name:    "bm25 only, with filters",
			mode:    ModeBM25Only,
			filters: filters,
			want:    `{"_source":{"excludes":["text_embedding","fact_embedding"]},"query":{"bool":{"filter":[{"term":{"tenant_id":"t1"}}],"must":[{"match":{"text":"hello"}}]}},"size":5}`,
		},
		{
			name: "knn only",
			mode: ModeKNNOnly,
			vec:  vec,
			want: `{"_source":{"excludes":["text_embedding","fact_embedding"]},"query":{"knn":{"text_embedding":{"k":5,"vector":[0.1,0.2,0.3]}}},"size":5}`,
		},
		{
			name: "hybrid with vector",
			mode: ModeHybrid,
			vec:  vec,
			want: `{"_source":{"excludes":["text_embedding","fact_embedding"]},"query":{"hybrid":{"queries":[{"match":{"text":"hello"}},{"knn":{"text_embedding":{"k":5,"vector":[0.1,0.2,0.3]}}}]}},"size":5}`,
		},
		{
			name: "hybrid, no vector falls back to bm25",
			mode: ModeHybrid,
			want: `{"_source":{"excludes":["text_embedding","fact_embedding"]},"query":{"match":{"text":"hello"}},"size":5}`,
		},
		{
			name:    "hybrid with vector and filters",
			mode:    ModeHybrid,
			vec:     vec,
			filters: filters,
			want:    `{"_source":{"excludes":["text_embedding","fact_embedding"]},"query":{"hybrid":{"queries":[{"bool":{"filter":[{"term":{"tenant_id":"t1"}}],"must":[{"match":{"text":"hello"}}]}},{"knn":{"text_embedding":{"filter":{"bool":{"filter":[{"term":{"tenant_id":"t1"}}]}},"k":5,"vector":[0.1,0.2,0.3]}}}]}},"size":5}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := buildQuery(queryOpts{
				mode: tc.mode, textField: "text", vectorField: "text_embedding",
				text: "hello", vec: tc.vec, k: 5, filters: tc.filters,
			})
			if string(got) != tc.want {
				t.Errorf("buildQuery(sort=nil) body =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

// TestKnowledgeSearchEmptyQueryIsFilterOnly covers the "empty query with
// only filters" edge case: query="" still selects documents via match_all +
// the filter clause, rather than matching nothing.
func TestKnowledgeSearchEmptyQueryIsFilterOnly(t *testing.T) {
	srv, captured := newFakeKnowledgeServer(t, func(string) (int, any) {
		return http.StatusOK, knowledgeHitsBody(1, knowledgeHit("2401.1", 1.0, nil))
	})
	r := NewKnowledgeRetriever(srv.Client(), srv.URL, newFakeKnowledgeRegistry())
	hits, err := r.Search(context.Background(), arxivSpec("knowledge-arxiv"), "",
		[]Predicate{{Field: "categories", Op: "term", Value: "cs.CL"}}, nil, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(hits))
	}
	raw := captured()[0].raw
	if !strings.Contains(raw, `"match_all"`) {
		t.Errorf("empty-query filter-only search should use match_all: %s", raw)
	}
	if strings.Contains(raw, `"match":{"abstract":""}`) {
		t.Errorf("empty-query search must not send an empty match clause: %s", raw)
	}
}

// --- DW-5.2: term/range/prefix filters + unknown-field errors ---------

// TestKnowledgeSearchFilterClauseShapes is DW-5.2's table-driven core: each
// op builds the right OpenSearch clause shape.
func TestKnowledgeSearchFilterClauseShapes(t *testing.T) {
	cases := []struct {
		name    string
		pred    Predicate
		want    string
		wantErr bool
	}{
		{name: "term", pred: Predicate{Field: "categories", Op: "term", Value: "cs.CL"}, want: `"term":{"categories":"cs.CL"}`},
		{name: "prefix", pred: Predicate{Field: "categories", Op: "prefix", Value: "cs."}, want: `"prefix":{"categories":"cs."}`},
		{name: "range both bounds", pred: Predicate{Field: "published", Op: "range", Value: map[string]any{"gte": "2024-01-01", "lte": "2024-12-31"}}, want: `"range":{"published":{"gte":"2024-01-01","lte":"2024-12-31"}}`},
		{name: "range gte only", pred: Predicate{Field: "published", Op: "range", Value: map[string]any{"gte": "2024-01-01"}}, want: `"range":{"published":{"gte":"2024-01-01"}}`},
		{name: "range malformed value", pred: Predicate{Field: "published", Op: "range", Value: "not-a-map"}, wantErr: true},
		{name: "range no bounds", pred: Predicate{Field: "published", Op: "range", Value: map[string]any{}}, wantErr: true},
		{name: "unsupported op", pred: Predicate{Field: "categories", Op: "fuzzy", Value: "x"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, captured := newFakeKnowledgeServer(t, func(string) (int, any) {
				return http.StatusOK, knowledgeHitsBody(0)
			})
			r := NewKnowledgeRetriever(srv.Client(), srv.URL, newFakeKnowledgeRegistry())
			_, err := r.Search(context.Background(), arxivSpec("knowledge-arxiv"), "x", []Predicate{tc.pred}, nil, 10)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			raw := captured()[0].raw
			if !strings.Contains(raw, tc.want) {
				t.Errorf("request body missing %s: %s", tc.want, raw)
			}
		})
	}
}

// TestKnowledgeSearchUnknownFilterFieldNamesValidFields covers DW-5.2's
// dirty case: an unknown or unfilterable field errors, naming the valid
// filterable fields so an LLM caller can self-correct.
func TestKnowledgeSearchUnknownFilterFieldNamesValidFields(t *testing.T) {
	r := NewKnowledgeRetriever(http.DefaultClient, "http://unused.invalid", newFakeKnowledgeRegistry())
	spec := arxivSpec("knowledge-arxiv")

	cases := []struct {
		name  string
		field string
	}{
		{name: "field not in mappings", field: "doi"},
		{name: "declared but not filterable", field: "internal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.Search(context.Background(), spec, "x", []Predicate{{Field: tc.field, Op: "term", Value: "v"}}, nil, 10)
			if err == nil {
				t.Fatal("want error, got nil")
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.field) {
				t.Errorf("error %q does not name the offending field %q", msg, tc.field)
			}
			for _, valid := range []string{"categories", "published"} {
				if !strings.Contains(msg, valid) {
					t.Errorf("error %q does not name valid filterable field %q", msg, valid)
				}
			}
		})
	}
}

// --- DW-5.3: sort ordering + non-sortable-field errors -----------------

// TestKnowledgeSearchSortAppliesSortClause covers DW-5.3's clean case: a
// sort by a registered sortable field is sent as an OpenSearch sort clause.
func TestKnowledgeSearchSortAppliesSortClause(t *testing.T) {
	srv, captured := newFakeKnowledgeServer(t, func(string) (int, any) {
		return http.StatusOK, knowledgeHitsBody(0)
	})
	r := NewKnowledgeRetriever(srv.Client(), srv.URL, newFakeKnowledgeRegistry())
	_, err := r.Search(context.Background(), arxivSpec("knowledge-arxiv"), "x", nil,
		[]SortKey{{Field: "published", Order: "desc"}}, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	raw := captured()[0].raw
	if !strings.Contains(raw, `"sort":[{"published":{"order":"desc"}}]`) {
		t.Errorf("request body missing sort clause: %s", raw)
	}
}

// TestKnowledgeSearchNonSortableFieldErrors covers DW-5.3's dirty case: a
// declared-but-not-sortable field, an undeclared field, and an invalid order
// value all error with a self-correcting message.
func TestKnowledgeSearchNonSortableFieldErrors(t *testing.T) {
	r := NewKnowledgeRetriever(http.DefaultClient, "http://unused.invalid", newFakeKnowledgeRegistry())
	spec := arxivSpec("knowledge-arxiv")

	cases := []struct {
		name string
		key  SortKey
	}{
		{name: "declared but not sortable", key: SortKey{Field: "categories", Order: "asc"}},
		{name: "undeclared field", key: SortKey{Field: "doi", Order: "asc"}},
		{name: "invalid order", key: SortKey{Field: "published", Order: "sideways"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.Search(context.Background(), spec, "x", nil, []SortKey{tc.key}, 10)
			if err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
}

// TestKnowledgeSearchKBounds covers the k=0/1/max boundary: the request's
// "size" reflects clampK's contract (0 -> DefaultK, in-range passes through,
// over MaxK is capped).
func TestKnowledgeSearchKBounds(t *testing.T) {
	cases := []struct {
		k        int
		wantSize int
	}{
		{k: 0, wantSize: DefaultK},
		{k: 1, wantSize: 1},
		{k: MaxK + 50, wantSize: MaxK},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("k=%d", tc.k), func(t *testing.T) {
			srv, captured := newFakeKnowledgeServer(t, func(string) (int, any) {
				return http.StatusOK, knowledgeHitsBody(0)
			})
			r := NewKnowledgeRetriever(srv.Client(), srv.URL, newFakeKnowledgeRegistry())
			if _, err := r.Search(context.Background(), arxivSpec("knowledge-arxiv"), "x", nil, nil, tc.k); err != nil {
				t.Fatalf("Search: %v", err)
			}
			want := fmt.Sprintf(`"size":%d`, tc.wantSize)
			if raw := captured()[0].raw; !strings.Contains(raw, want) {
				t.Errorf("k=%d: request body missing %s: %s", tc.k, want, raw)
			}
		})
	}
}

// --- DW-5.4: Collections staleness -------------------------------------

// TestCollectionsReportsCountAndStaleness covers DW-5.4's clean case: count
// and the newest harvested_at/doc-date are read from the aggregation
// response for a populated collection.
func TestCollectionsReportsCountAndStaleness(t *testing.T) {
	harvested := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	published := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	srv, captured := newFakeKnowledgeServer(t, func(string) (int, any) {
		return http.StatusOK, map[string]any{
			"hits": map[string]any{"total": map[string]any{"value": 42}},
			"aggregations": map[string]any{
				"newest_harvested_at": map[string]any{"value": float64(harvested.UnixMilli())},
				"newest_doc_date_0":   map[string]any{"value": float64(published.UnixMilli())},
			},
		}
	})
	reg := newFakeKnowledgeRegistry(arxivSpec("knowledge-arxiv"))
	r := NewKnowledgeRetriever(srv.Client(), srv.URL, reg)

	metas, err := r.Collections(context.Background())
	if err != nil {
		t.Fatalf("Collections: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("got %d collections, want 1", len(metas))
	}
	m := metas[0]
	if m.Name != "arxiv" || m.Count != 42 {
		t.Fatalf("got %+v, want name=arxiv count=42", m)
	}
	if m.NewestHarvestedAt == nil || !m.NewestHarvestedAt.Equal(harvested) {
		t.Errorf("NewestHarvestedAt = %v, want %v", m.NewestHarvestedAt, harvested)
	}
	if m.NewestDocDate == nil || !m.NewestDocDate.Equal(published) {
		t.Errorf("NewestDocDate = %v, want %v", m.NewestDocDate, published)
	}

	raw := captured()[0].raw
	if !strings.Contains(raw, `"size":0`) || !strings.Contains(raw, `"track_total_hits":true`) {
		t.Errorf("aggregation request missing size:0/track_total_hits:true: %s", raw)
	}
	if !strings.Contains(raw, `"harvested_at"`) || !strings.Contains(raw, `"published"`) {
		t.Errorf("aggregation request missing max aggs over harvested_at/published: %s", raw)
	}
}

// TestCollectionsEmptyCollectionHasNilStaleness covers DW-5.4's boundary
// case: an existing-but-empty collection reports count=0 and nil timestamps,
// not an error.
func TestCollectionsEmptyCollectionHasNilStaleness(t *testing.T) {
	srv, _ := newFakeKnowledgeServer(t, func(string) (int, any) {
		return http.StatusOK, map[string]any{
			"hits":         map[string]any{"total": map[string]any{"value": 0}},
			"aggregations": map[string]any{"newest_harvested_at": map[string]any{"value": nil}},
		}
	})
	reg := newFakeKnowledgeRegistry(knowledge.CollectionSpec{Name: "empty", Index: "knowledge-empty", TextField: "text"})
	r := NewKnowledgeRetriever(srv.Client(), srv.URL, reg)

	metas, err := r.Collections(context.Background())
	if err != nil {
		t.Fatalf("Collections: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("got %d collections, want 1", len(metas))
	}
	m := metas[0]
	if m.Count != 0 || m.NewestHarvestedAt != nil || m.NewestDocDate != nil {
		t.Errorf("empty collection = %+v, want count=0 and nil timestamps", m)
	}
}

// TestCollectionsUnprovisionedIndexReadsAsZero covers the edge case where a
// collection is registered but its live index isn't provisioned yet
// (Phase 3's documented Create-partial-failure repair scenario): the house
// index-not-found-as-empty rule applies, not an error.
func TestCollectionsUnprovisionedIndexReadsAsZero(t *testing.T) {
	srv, _ := newFakeKnowledgeServer(t, func(string) (int, any) {
		return http.StatusNotFound, map[string]any{"error": map[string]any{"type": "index_not_found_exception"}}
	})
	reg := newFakeKnowledgeRegistry(knowledge.CollectionSpec{Name: "fresh", Index: "knowledge-fresh", TextField: "text"})
	r := NewKnowledgeRetriever(srv.Client(), srv.URL, reg)

	metas, err := r.Collections(context.Background())
	if err != nil {
		t.Fatalf("Collections: %v", err)
	}
	if len(metas) != 1 || metas[0].Name != "fresh" || metas[0].Count != 0 {
		t.Fatalf("got %+v, want one zero-stats \"fresh\" entry", metas)
	}
}

// TestKnowledgeSearchUnprovisionedIndexReturnsNoHits covers the Search-side
// mirror of the same 404-as-empty rule.
func TestKnowledgeSearchUnprovisionedIndexReturnsNoHits(t *testing.T) {
	srv, _ := newFakeKnowledgeServer(t, func(string) (int, any) {
		return http.StatusNotFound, map[string]any{"error": map[string]any{"type": "index_not_found_exception"}}
	})
	r := NewKnowledgeRetriever(srv.Client(), srv.URL, newFakeKnowledgeRegistry())
	hits, err := r.Search(context.Background(), arxivSpec("knowledge-fresh"), "x", nil, nil, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("got %d hits, want 0", len(hits))
	}
}

// TestCollectionsPropagatesRegistryErrors covers a dirty registry-listing
// failure: Collections surfaces it rather than silently returning an empty
// list.
func TestCollectionsPropagatesRegistryErrors(t *testing.T) {
	reg := newFakeKnowledgeRegistry()
	reg.listErr = errors.New("boom")
	r := NewKnowledgeRetriever(http.DefaultClient, "http://unused.invalid", reg)
	if _, err := r.Collections(context.Background()); err == nil {
		t.Fatal("want error, got nil")
	}
}

// TestValidateKnowledgeIndexRejectsPathTraversal covers the Phase-3-style
// security barricade: a crafted Index value must never reach an HTTP path.
func TestValidateKnowledgeIndexRejectsPathTraversal(t *testing.T) {
	cases := []string{"", "../secret", "knowledge-arxiv/../../x", "has spaces", "UPPER"}
	for _, idx := range cases {
		if err := validateKnowledgeIndex(idx); err == nil {
			t.Errorf("validateKnowledgeIndex(%q) = nil, want error", idx)
		}
	}
	if err := validateKnowledgeIndex("knowledge-arxiv-v2"); err != nil {
		t.Errorf("validateKnowledgeIndex(valid) = %v, want nil", err)
	}
}

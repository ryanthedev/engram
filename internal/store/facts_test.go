package store_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/store"
)

// scanFactsServer is a hermetic httptest stand-in for OpenSearch's
// _search endpoint: it answers one canned page per call (in order) and
// captures every request body, so ScanLiveFacts' query shape and
// pagination behavior can be asserted without a live cluster (mirrors
// robustness_test.go's errorServer pattern).
type scanFactsServer struct {
	pages [][]map[string]any
	calls []map[string]any
}

func newScanFactsServer(pages ...[]map[string]any) *scanFactsServer {
	return &scanFactsServer{pages: pages}
}

func (s *scanFactsServer) start(t *testing.T) *store.OpenSearchStore {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		s.calls = append(s.calls, body)
		var hits []map[string]any
		if idx := len(s.calls) - 1; idx < len(s.pages) {
			hits = s.pages[idx]
		}
		anyHits := make([]any, len(hits))
		for i, h := range hits {
			anyHits[i] = h
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"hits": map[string]any{"hits": anyHits}})
	}))
	t.Cleanup(srv.Close)
	return store.NewOpenSearchStore(srv.Client(), srv.URL, store.WithSemanticIndex("engram-semantic-scratch"))
}

// factHit builds one raw _search hit for f, matching the shape
// versionedFromGet decodes (see facts.go).
func factHit(id string, f memory.SemanticFact) map[string]any {
	raw, _ := json.Marshal(f)
	var src map[string]any
	_ = json.Unmarshal(raw, &src)
	return map[string]any{"_id": id, "_source": src, "_seq_no": float64(1), "_primary_term": float64(1)}
}

func scanFact(subject string, createdAt time.Time, contentKey string) memory.SemanticFact {
	return memory.SemanticFact{
		Subject: subject, Predicate: "p", Object: "o", TenantID: "t1",
		ContentKey: contentKey, ValidAt: createdAt, CreatedAt: createdAt,
	}
}

// --- query shape (regression-shaped: the filter/sort clauses ScanLiveFacts
// must always emit) --------------------------------------------------------

func TestScanLiveFacts_QueryShapeOnFirstPage(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	srv := newScanFactsServer([]map[string]any{factHit("f1", scanFact("A", t0, "ck-a"))})
	s := srv.start(t)

	facts, next, err := s.ScanLiveFacts(context.Background(), "t1", store.FactCursor{}, 10)
	if err != nil {
		t.Fatalf("ScanLiveFacts: %v", err)
	}
	if len(facts) != 1 || facts[0].Fact.Subject != "A" {
		t.Fatalf("facts = %+v, want one fact for A", facts)
	}
	if next != (store.FactCursor{}) {
		t.Fatalf("next cursor = %+v, want zero (short page, exhausted)", next)
	}

	if len(srv.calls) != 1 {
		t.Fatalf("server received %d requests, want 1", len(srv.calls))
	}
	body := srv.calls[0]
	if _, hasSearchAfter := body["search_after"]; hasSearchAfter {
		t.Errorf("first-page request carries search_after, want none (zero cursor)")
	}

	sortClauses, _ := body["sort"].([]any)
	if len(sortClauses) != 2 {
		t.Fatalf("sort clauses = %v, want 2 (created_at, content_key)", sortClauses)
	}
	if _, ok := sortClauses[0].(map[string]any)["created_at"]; !ok {
		t.Errorf("sort[0] = %v, want created_at", sortClauses[0])
	}
	if _, ok := sortClauses[1].(map[string]any)["content_key"]; !ok {
		t.Errorf("sort[1] = %v, want content_key", sortClauses[1])
	}

	query, _ := body["query"].(map[string]any)
	boolQ, _ := query["bool"].(map[string]any)
	filters, _ := boolQ["filter"].([]any)
	if len(filters) != 3 {
		t.Fatalf("filter clauses = %v, want 3 (invalid_at unset, expired_at unset, tenant_id term)", filters)
	}
	tenantTerm, _ := filters[2].(map[string]any)["term"].(map[string]any)
	if tenantTerm["tenant_id"] != "t1" {
		t.Errorf("tenant filter = %v, want tenant_id=t1", filters[2])
	}
}

// --- pagination -------------------------------------------------------

// TestScanLiveFacts_FullPageResumesWithSearchAfter drives two pages with
// size=2: a full page (exactly size items) must return a non-zero next
// cursor built from its LAST item's (created_at, content_key), and the
// FOLLOWING call must carry that cursor as search_after — the round-trip
// contract FactScanner (and graph.Rebuild) depends on. The second page is
// short (1 < size), so it must report exhaustion (zero next cursor) —
// mirrors the "full page ⇒ assume more, verify on the next call" rule
// graph.ScanEntities/ScanEdges already use in this codebase.
func TestScanLiveFacts_FullPageResumesWithSearchAfter(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Second)
	t2 := t0.Add(2 * time.Second)
	page1 := []map[string]any{factHit("f1", scanFact("A", t0, "ck-a")), factHit("f2", scanFact("B", t1, "ck-b"))}
	page2 := []map[string]any{factHit("f3", scanFact("C", t2, "ck-c"))}
	srv := newScanFactsServer(page1, page2)
	s := srv.start(t)
	ctx := context.Background()

	facts1, next1, err := s.ScanLiveFacts(ctx, "t1", store.FactCursor{}, 2)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(facts1) != 2 {
		t.Fatalf("page 1 facts = %d, want 2", len(facts1))
	}
	wantCursor := store.FactCursor{CreatedAt: t1, ContentKey: "ck-b"} // the PAGE's last item, not overall last
	if next1 != wantCursor {
		t.Fatalf("next cursor after a full page = %+v, want %+v", next1, wantCursor)
	}

	facts2, next2, err := s.ScanLiveFacts(ctx, "t1", next1, 2)
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(facts2) != 1 || facts2[0].Fact.Subject != "C" {
		t.Fatalf("page 2 facts = %+v, want one fact for C", facts2)
	}
	if next2 != (store.FactCursor{}) {
		t.Fatalf("next cursor after a short page = %+v, want zero (exhausted)", next2)
	}

	if len(srv.calls) != 2 {
		t.Fatalf("server received %d requests, want 2", len(srv.calls))
	}
	searchAfter, ok := srv.calls[1]["search_after"].([]any)
	if !ok || len(searchAfter) != 2 {
		t.Fatalf("page-2 request search_after = %v, want [millis, content_key]", srv.calls[1]["search_after"])
	}
	if got, want := int64(searchAfter[0].(float64)), t1.UnixMilli(); got != want {
		t.Errorf("search_after[0] = %d, want %d (page 1's last item, t1, in millis)", got, want)
	}
	if searchAfter[1] != "ck-b" {
		t.Errorf("search_after[1] = %v, want ck-b", searchAfter[1])
	}
}

// --- missing index / error propagation (mirrors robustness_test.go's
// established contract for every other read path in this file) ----------

func TestScanLiveFacts_MissingIndexReturnsEmptyNotError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"type": "index_not_found_exception", "reason": "no such index"},
		})
	}))
	t.Cleanup(srv.Close)
	s := store.NewOpenSearchStore(srv.Client(), srv.URL, store.WithSemanticIndex("engram-semantic-missing"))

	facts, next, err := s.ScanLiveFacts(context.Background(), "t1", store.FactCursor{}, 10)
	if err != nil {
		t.Fatalf("ScanLiveFacts against a missing index = error %v, want nil", err)
	}
	if len(facts) != 0 || next != (store.FactCursor{}) {
		t.Fatalf("ScanLiveFacts against a missing index = (%v, %v), want (nil, zero)", facts, next)
	}
}

func TestScanLiveFacts_NonIndexNotFoundErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "search_phase_execution_exception"}})
	}))
	t.Cleanup(srv.Close)
	s := store.NewOpenSearchStore(srv.Client(), srv.URL, store.WithSemanticIndex("engram-semantic-scratch"))

	if _, _, err := s.ScanLiveFacts(context.Background(), "t1", store.FactCursor{}, 10); err == nil {
		t.Fatal("ScanLiveFacts on a 500 = nil error, want an error")
	}
}

// TestScanLiveFacts_DefaultSizeWhenNonPositive: size<=0 falls back to
// DefaultScanBatchSize rather than sending an unbounded/zero-size query.
func TestScanLiveFacts_DefaultSizeWhenNonPositive(t *testing.T) {
	srv := newScanFactsServer(nil)
	s := srv.start(t)

	if _, _, err := s.ScanLiveFacts(context.Background(), "t1", store.FactCursor{}, 0); err != nil {
		t.Fatalf("ScanLiveFacts: %v", err)
	}
	size, _ := srv.calls[0]["size"].(float64)
	if int(size) != store.DefaultScanBatchSize {
		t.Errorf("size = %v, want %d (DefaultScanBatchSize)", srv.calls[0]["size"], store.DefaultScanBatchSize)
	}
}

package store_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/ryanthedev/engram/internal/store"
)

// prefixCluster answers the semantic _search with a canned, already-sorted page
// and records each request body it saw.
func prefixCluster(t *testing.T, status int, page []string) (*store.OpenSearchStore, *[]map[string]any) {
	t.Helper()
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		bodies = append(bodies, b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusNotFound {
			_, _ = io.WriteString(w, `{"error":{"type":"index_not_found_exception"},"status":404}`)
			return
		}
		hits := make([]map[string]any, 0, len(page))
		for _, id := range page {
			hits = append(hits, map[string]any{"_id": id})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"hits": map[string]any{"hits": hits}})
	}))
	t.Cleanup(srv.Close)
	return store.NewOpenSearchStore(srv.Client(), srv.URL, store.WithSemanticIndex("sem")), &bodies
}

// TestResolveFactIDPrefixSeeksAndStopsAtTheRun pins the seek: the query sorts
// on _id ascending, seeks with search_after:[prefix], carries the caller's
// filters as bool filter clauses, fetches no _source, and the result is only
// the LEADING run of ids carrying the prefix — the first id past it ends the
// match, since nothing later in a sorted page can carry the prefix.
func TestResolveFactIDPrefixSeeksAndStopsAtTheRun(t *testing.T) {
	s, bodies := prefixCluster(t, http.StatusOK, []string{"abcd00001", "abcd00002", "abce00000", "abcd00003"})
	filters := []any{map[string]any{"term": map[string]any{"tenant_id": "t1"}}}

	ids, err := s.ResolveFactIDPrefix(context.Background(), "abcd0000", filters, 10)
	if err != nil {
		t.Fatalf("ResolveFactIDPrefix: %v", err)
	}
	if want := []string{"abcd00001", "abcd00002"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("ids = %v, want the leading run %v", ids, want)
	}
	if len(*bodies) != 1 {
		t.Fatalf("requests = %d, want 1", len(*bodies))
	}
	b := (*bodies)[0]
	if !reflect.DeepEqual(b["sort"], []any{map[string]any{"_id": "asc"}}) {
		t.Errorf("sort = %v, want _id asc", b["sort"])
	}
	if !reflect.DeepEqual(b["search_after"], []any{"abcd0000"}) {
		t.Errorf("search_after = %v, want [prefix]", b["search_after"])
	}
	if b["_source"] != false || b["size"] != float64(10) {
		t.Errorf("_source=%v size=%v, want false and the limit", b["_source"], b["size"])
	}
	wantQuery := map[string]any{"bool": map[string]any{"filter": []any{map[string]any{"term": map[string]any{"tenant_id": "t1"}}}}}
	if !reflect.DeepEqual(b["query"], wantQuery) {
		t.Errorf("query = %v, want the filters as bool filter clauses %v", b["query"], wantQuery)
	}
}

// TestResolveFactIDPrefixRefusesUnscoped: no filters means no authorization,
// so the resolution is refused before any request (fail-closed) — as are an
// empty prefix and a non-positive limit.
func TestResolveFactIDPrefixRefusesUnscoped(t *testing.T) {
	s, bodies := prefixCluster(t, http.StatusOK, nil)
	filters := []any{map[string]any{"term": map[string]any{"tenant_id": "t1"}}}
	for name, call := range map[string]func() error{
		"nil filters":   func() error { _, err := s.ResolveFactIDPrefix(context.Background(), "abcd0000", nil, 10); return err },
		"empty filters": func() error { _, err := s.ResolveFactIDPrefix(context.Background(), "abcd0000", []any{}, 10); return err },
		"empty prefix":  func() error { _, err := s.ResolveFactIDPrefix(context.Background(), "", filters, 10); return err },
		"zero limit":    func() error { _, err := s.ResolveFactIDPrefix(context.Background(), "abcd0000", filters, 0); return err },
	} {
		if call() == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if len(*bodies) != 0 {
		t.Errorf("%d request(s) sent for refused resolutions, want 0", len(*bodies))
	}
}

// TestResolveFactIDPrefixMissingIndexIsNoMatch: before the first fact is ever
// written the semantic index does not exist — that is "no ids", not an error.
func TestResolveFactIDPrefixMissingIndexIsNoMatch(t *testing.T) {
	s, _ := prefixCluster(t, http.StatusNotFound, nil)
	ids, err := s.ResolveFactIDPrefix(context.Background(), "abcd0000",
		[]any{map[string]any{"term": map[string]any{"tenant_id": "t1"}}}, 10)
	if err != nil || ids != nil {
		t.Errorf("ids=%v err=%v, want nil, nil", ids, err)
	}
}

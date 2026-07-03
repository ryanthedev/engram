package retrieval_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/retrieval"
)

// reqCapture records one HTTP request the fake cluster observed.
type reqCapture struct {
	path  string
	query url.Values
	raw   string
}

// respondFunc answers one request for the given index, returning the
// canned status and JSON-serializable body.
type respondFunc func(index string) (status int, body any)

// newFakeSearchServer returns an httptest server that records every request
// and answers via respond, keyed by the index name parsed from the request
// path ({index}/_search).
func newFakeSearchServer(t *testing.T, respond respondFunc) (*httptest.Server, func() []reqCapture) {
	t.Helper()
	var mu sync.Mutex
	var captured []reqCapture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		r.Body.Close()
		mu.Lock()
		captured = append(captured, reqCapture{path: r.URL.Path, query: r.URL.Query(), raw: string(raw)})
		mu.Unlock()

		idx := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/_search")
		status, body := respond(idx)
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []reqCapture {
		mu.Lock()
		defer mu.Unlock()
		return append([]reqCapture(nil), captured...)
	}
}

func hitsBody(hits ...map[string]any) map[string]any {
	if hits == nil {
		hits = []map[string]any{}
	}
	return map[string]any{"hits": map[string]any{"hits": hits}}
}

func hit(id string, score float64) map[string]any {
	return map[string]any{"_id": id, "_score": score, "_source": map[string]any{"k": "v"}}
}

// failingHandler fails the test if it is ever hit — used to prove an empty
// query short-circuits without any HTTP call.
func failingHandler(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("unexpected HTTP call for an empty query")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestMultiRetrieverEmptyQueryShortCircuits covers the empty-query dirty
// test (DW-1.6): no HTTP call is made and the result is empty, not an error.
func TestMultiRetrieverEmptyQueryShortCircuits(t *testing.T) {
	srv := failingHandler(t)
	r := retrieval.NewOpenSearchRetriever(srv.Client(), srv.URL, embed.NewFakeEmbedder(4, nil))
	hits, err := r.Search(context.Background(), retrieval.Query{Text: ""}, retrieval.Filter{})
	if err != nil {
		t.Fatalf("empty query returned error: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("empty query returned %d hits, want 0", len(hits))
	}
}

// TestMultiRetrieverZeroResults covers the zero-matches dirty test (DW-1.6):
// a well-formed query against an empty index returns an empty slice, not an
// error.
func TestMultiRetrieverZeroResults(t *testing.T) {
	srv, _ := newFakeSearchServer(t, func(string) (int, any) { return http.StatusOK, hitsBody() })
	r := retrieval.NewOpenSearchRetriever(srv.Client(), srv.URL, embed.NewFakeEmbedder(4, nil))
	hits, err := r.Search(context.Background(), retrieval.Query{Text: "nothing matches"}, retrieval.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("got %d hits, want 0", len(hits))
	}
}

// TestTierRetrieverQueryShapeTableDriven is DW-1.6's table-driven core: for
// each {mode, filter} combination, the right query-shape markers appear (or
// don't) in the request body, and the RRF search_pipeline param is attached
// only for hybrid mode.
func TestTierRetrieverQueryShapeTableDriven(t *testing.T) {
	cases := []struct {
		name         string
		mode         retrieval.SearchMode
		filter       retrieval.Filter
		wantContains []string
		wantAbsent   []string
		wantPipeline bool
	}{
		{
			name:         "hybrid no filter",
			mode:         retrieval.ModeHybrid,
			wantContains: []string{`"hybrid"`, `"knn"`, `"match"`},
			wantPipeline: true,
		},
		{
			name:         "bm25 only",
			mode:         retrieval.ModeBM25Only,
			wantContains: []string{`"match"`},
			wantAbsent:   []string{`"hybrid"`, `"knn"`},
		},
		{
			name:         "knn only",
			mode:         retrieval.ModeKNNOnly,
			wantContains: []string{`"knn"`},
			wantAbsent:   []string{`"hybrid"`, `"match"`},
		},
		{
			name:         "hybrid with tenant filter",
			mode:         retrieval.ModeHybrid,
			filter:       retrieval.Filter{TenantID: "t1"},
			wantContains: []string{`"hybrid"`, `"tenant_id":"t1"`, `"filter"`},
			wantPipeline: true,
		},
		{
			name:         "hybrid with valid-only",
			mode:         retrieval.ModeHybrid,
			filter:       retrieval.Filter{ValidOnly: true},
			wantContains: []string{`"invalid_at"`, `"expired_at"`}, // semantic tier only
			wantPipeline: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, captured := newFakeSearchServer(t, func(idx string) (int, any) {
				return http.StatusOK, hitsBody(hit(idx+"-1", 1.0))
			})
			r := retrieval.NewOpenSearchRetriever(srv.Client(), srv.URL, embed.NewFakeEmbedder(4, nil),
				retrieval.WithIndices("ep-idx", "sem-idx"), retrieval.WithMode(tc.mode))
			if _, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, tc.filter); err != nil {
				t.Fatalf("Search: %v", err)
			}
			reqs := captured()
			if len(reqs) == 0 {
				t.Fatal("no requests captured")
			}
			for _, c := range reqs {
				for _, want := range tc.wantContains {
					// The validity clause only applies to the semantic tier
					// (episodic has no valid_at/invalid_at/expired_at).
					if (want == `"invalid_at"` || want == `"expired_at"`) && !strings.Contains(c.path, "sem-idx") {
						continue
					}
					if !strings.Contains(c.raw, want) {
						t.Errorf("[%s] request to %s missing %s: %s", tc.name, c.path, want, c.raw)
					}
				}
				for _, notWant := range tc.wantAbsent {
					if strings.Contains(c.raw, notWant) {
						t.Errorf("[%s] request to %s unexpectedly contains %s: %s", tc.name, c.path, notWant, c.raw)
					}
				}
				if hasPipeline := c.query.Get("search_pipeline") != ""; hasPipeline != tc.wantPipeline {
					t.Errorf("[%s] request to %s pipeline=%v, want %v", tc.name, c.path, hasPipeline, tc.wantPipeline)
				}
			}
		})
	}
}

// TestTierRetrieverHybridQueryShape covers DW-1.2: a normal hybrid search
// issues a query with the "hybrid" wrapper carrying both a match clause and
// a knn clause, through the RRF search_pipeline.
func TestTierRetrieverHybridQueryShape(t *testing.T) {
	srv, captured := newFakeSearchServer(t, func(idx string) (int, any) {
		return http.StatusOK, hitsBody(hit(idx+"-1", 0.9))
	})
	r := retrieval.NewOpenSearchRetriever(srv.Client(), srv.URL, embed.NewFakeEmbedder(4, nil),
		retrieval.WithIndices("ep-idx", "sem-idx"))
	hits, err := r.Search(context.Background(), retrieval.Query{Text: "orders-svc leak", K: 5}, retrieval.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2 (one per tier)", len(hits))
	}

	for _, c := range captured() {
		if !strings.Contains(c.raw, `"hybrid"`) || !strings.Contains(c.raw, `"knn"`) || !strings.Contains(c.raw, `"match"`) {
			t.Errorf("request to %s missing hybrid/knn/match: %s", c.path, c.raw)
		}
		if c.query.Get("search_pipeline") == "" {
			t.Errorf("request to %s missing search_pipeline param", c.path)
		}
	}
}

// TestTierRetrieverEmbeddingTimeoutDegradesToBM25 covers the fallback dirty
// test (DW-1.6): an embedder that blows the embed budget must not fail the
// search — it degrades to a plain BM25 query (no hybrid/knn wrapper, no RRF
// pipeline param) and still returns results.
func TestTierRetrieverEmbeddingTimeoutDegradesToBM25(t *testing.T) {
	srv, captured := newFakeSearchServer(t, func(idx string) (int, any) {
		return http.StatusOK, hitsBody(hit(idx+"-1", 1.0))
	})
	var logged strings.Builder
	logger := slog.New(slog.NewTextHandler(&logged, nil))

	slow := slowEmbedder{delay: 200 * time.Millisecond}
	r := retrieval.NewOpenSearchRetriever(srv.Client(), srv.URL, slow,
		retrieval.WithIndices("ep-idx", "sem-idx"),
		retrieval.WithEmbedTimeout(5*time.Millisecond),
		retrieval.WithLogger(logger))

	start := time.Now()
	hits, err := r.Search(context.Background(), retrieval.Query{Text: "orders-svc leak"}, retrieval.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Errorf("Search took %v; embed timeout should have bounded it well under the slow embedder's 200ms delay", elapsed)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2 (BM25 fallback on both tiers)", len(hits))
	}
	for _, c := range captured() {
		if strings.Contains(c.raw, `"hybrid"`) || strings.Contains(c.raw, `"knn"`) {
			t.Errorf("degraded request to %s still carries hybrid/knn: %s", c.path, c.raw)
		}
		if !strings.Contains(c.raw, `"match"`) {
			t.Errorf("degraded request to %s missing the BM25 match clause: %s", c.path, c.raw)
		}
		if c.query.Get("search_pipeline") != "" {
			t.Errorf("degraded request to %s should not use the RRF pipeline", c.path)
		}
	}
	if !strings.Contains(logged.String(), "degraded") {
		t.Errorf("degradation was not logged/flagged: %s", logged.String())
	}
}

// TestTierRetrieverKNNOnlyDegradesToEmpty covers the eval harness's
// kNN-only measurement mode under an unavailable embedder: no results, not
// an error.
func TestTierRetrieverKNNOnlyDegradesToEmpty(t *testing.T) {
	srv, _ := newFakeSearchServer(t, func(string) (int, any) {
		t.Error("kNN-only mode with no vector must not call the cluster")
		return http.StatusOK, hitsBody()
	})
	r := retrieval.NewOpenSearchRetriever(srv.Client(), srv.URL, erroringEmbedder{},
		retrieval.WithMode(retrieval.ModeKNNOnly))
	hits, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, retrieval.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("got %d hits, want 0", len(hits))
	}
}

// TestTierRetrieverAppliesTenantAndUserFilters covers DW-1.4: TenantID and
// UserID scope filters are applied inside the query (term clauses), not
// post-filtered.
func TestTierRetrieverAppliesTenantAndUserFilters(t *testing.T) {
	srv, captured := newFakeSearchServer(t, func(string) (int, any) { return http.StatusOK, hitsBody() })
	r := retrieval.NewOpenSearchRetriever(srv.Client(), srv.URL, embed.NewFakeEmbedder(4, nil),
		retrieval.WithIndices("ep-idx", "sem-idx"))
	_, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, retrieval.Filter{TenantID: "t1", UserID: "agent-9"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, c := range captured() {
		if !strings.Contains(c.raw, `"tenant_id":"t1"`) {
			t.Errorf("request to %s missing tenant_id filter: %s", c.path, c.raw)
		}
		// Filter.UserID maps onto the stored owner_agent_id field (no
		// separate user_id field on the record types).
		if !strings.Contains(c.raw, `"owner_agent_id":"agent-9"`) {
			t.Errorf("request to %s missing owner_agent_id filter: %s", c.path, c.raw)
		}
	}
}

// TestTierRetrieverValidOnlyAppliesOnlyToSemanticTier covers DW-1.4: the
// bi-temporal current-state filter is meaningful only where valid_at /
// invalid_at / expired_at exist (semantic); episodic has no such fields, so
// ValidOnly must be a no-op there.
func TestTierRetrieverValidOnlyAppliesOnlyToSemanticTier(t *testing.T) {
	srv, captured := newFakeSearchServer(t, func(string) (int, any) { return http.StatusOK, hitsBody() })
	r := retrieval.NewOpenSearchRetriever(srv.Client(), srv.URL, embed.NewFakeEmbedder(4, nil),
		retrieval.WithIndices("ep-idx", "sem-idx"))
	_, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, retrieval.Filter{ValidOnly: true})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, c := range captured() {
		hasValidity := strings.Contains(c.raw, `"invalid_at"`)
		wantValidity := strings.Contains(c.path, "sem-idx")
		if hasValidity != wantValidity {
			t.Errorf("request to %s: validity clause present=%v, want %v", c.path, hasValidity, wantValidity)
		}
	}
}

// TestMultiRetrieverMergesSortsAndTruncates covers DW-1.2: hits from both
// tiers are merged into one list, sorted by score descending, and truncated
// to K.
func TestMultiRetrieverMergesSortsAndTruncates(t *testing.T) {
	srv, _ := newFakeSearchServer(t, func(idx string) (int, any) {
		switch idx {
		case "ep-idx":
			return http.StatusOK, hitsBody(hit("e1", 0.5), hit("e2", 0.2))
		default:
			return http.StatusOK, hitsBody(hit("s1", 0.9), hit("s2", 0.1))
		}
	})
	r := retrieval.NewOpenSearchRetriever(srv.Client(), srv.URL, embed.NewFakeEmbedder(4, nil),
		retrieval.WithIndices("ep-idx", "sem-idx"))
	hits, err := r.Search(context.Background(), retrieval.Query{Text: "x", K: 2}, retrieval.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2 (truncated to K)", len(hits))
	}
	if hits[0].ID != "s1" || hits[1].ID != "e1" {
		t.Errorf("merged order = [%s(%v), %s(%v)], want [s1(0.9), e1(0.5)]", hits[0].ID, hits[0].Score, hits[1].ID, hits[1].Score)
	}
}

// TestMultiRetrieverOneTierFailingStillReturnsOtherTiersHits proves a single
// tier outage degrades rather than failing the whole search.
func TestMultiRetrieverOneTierFailingStillReturnsOtherTiersHits(t *testing.T) {
	srv, _ := newFakeSearchServer(t, func(idx string) (int, any) {
		if idx == "ep-idx" {
			return http.StatusInternalServerError, map[string]any{"error": "boom"}
		}
		return http.StatusOK, hitsBody(hit("s1", 1.0))
	})
	r := retrieval.NewOpenSearchRetriever(srv.Client(), srv.URL, embed.NewFakeEmbedder(4, nil),
		retrieval.WithIndices("ep-idx", "sem-idx"))
	hits, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, retrieval.Filter{})
	if err != nil {
		t.Fatalf("Search should degrade, not fail: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "s1" {
		t.Errorf("got %+v, want the surviving tier's hit", hits)
	}
}

// TestMultiRetrieverAllTiersFailingReturnsError proves total failure is not
// silently swallowed.
func TestMultiRetrieverAllTiersFailingReturnsError(t *testing.T) {
	srv, _ := newFakeSearchServer(t, func(string) (int, any) {
		return http.StatusInternalServerError, map[string]any{"error": "boom"}
	})
	r := retrieval.NewOpenSearchRetriever(srv.Client(), srv.URL, embed.NewFakeEmbedder(4, nil))
	_, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, retrieval.Filter{})
	if err == nil {
		t.Fatal("all tiers failing should return an error")
	}
}

// slowEmbedder blocks until ctx is done or delay elapses, whichever first —
// used to simulate an embedding service that blows the query-embed budget.
type slowEmbedder struct{ delay time.Duration }

func (s slowEmbedder) Embed(ctx context.Context, _ []string) ([][]float32, error) {
	select {
	case <-time.After(s.delay):
		return [][]float32{{1, 2, 3, 4}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s slowEmbedder) Info() embed.ModelInfo {
	return embed.ModelInfo{Model: "slow", Revision: "v1", Dim: 4}
}

// erroringEmbedder always fails — simulates an embedding service that is
// simply down (as opposed to slow).
type erroringEmbedder struct{}

func (erroringEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("embedding service unavailable")
}

func (erroringEmbedder) Info() embed.ModelInfo {
	return embed.ModelInfo{Model: "erroring", Revision: "v1", Dim: 4}
}

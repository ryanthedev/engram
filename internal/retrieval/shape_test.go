package retrieval_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/retrieval"
)

// TestDW_1_1_BuildQueryExcludesEmbeddings: every tier query body carries a
// _source exclusion for both embedding fields, so vectors never leave the
// cluster.
func TestDW_1_1_BuildQueryExcludesEmbeddings(t *testing.T) {
	srv, captured := newFakeSearchServer(t, func(idx string) (int, any) {
		return 200, hitsBody(hit(idx+"-1", 1.0))
	})
	r := retrieval.NewOpenSearchRetriever(srv.Client(), srv.URL, embed.NewFakeEmbedder(4, nil),
		retrieval.WithIndices("ep-idx", "sem-idx"))
	if _, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, retrieval.Filter{}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	reqs := captured()
	if len(reqs) != 2 {
		t.Fatalf("captured %d requests, want 2 (one per tier)", len(reqs))
	}
	for _, c := range reqs {
		var body struct {
			Source struct {
				Excludes []string `json:"excludes"`
			} `json:"_source"`
		}
		if err := json.Unmarshal([]byte(c.raw), &body); err != nil {
			t.Fatalf("request to %s is not JSON: %v", c.path, err)
		}
		want := []string{"text_embedding", "fact_embedding"}
		if !reflect.DeepEqual(body.Source.Excludes, want) {
			t.Errorf("request to %s _source.excludes = %v, want %v", c.path, body.Source.Excludes, want)
		}
	}
}

// TestDW_1_3_QuerySizeClampedInRequestBody: the per-tier query size sent to
// OpenSearch is q.K clamped into [1, MaxK] — below (k<=0 -> DefaultK), at, and
// above (k>MaxK -> MaxK) the bounds.
func TestDW_1_3_QuerySizeClampedInRequestBody(t *testing.T) {
	cases := []struct {
		name     string
		k        int
		wantSize float64
	}{
		{"negative k falls back to DefaultK", -1, retrieval.DefaultK},
		{"zero k falls back to DefaultK", 0, retrieval.DefaultK},
		{"in-range k passes through", 42, 42},
		{"k at MaxK stays", retrieval.MaxK, retrieval.MaxK},
		{"k over MaxK is capped", retrieval.MaxK + 150, retrieval.MaxK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, captured := newFakeSearchServer(t, func(string) (int, any) { return 200, hitsBody() })
			r := retrieval.NewOpenSearchRetriever(srv.Client(), srv.URL, embed.NewFakeEmbedder(4, nil),
				retrieval.WithIndices("ep-idx", "sem-idx"))
			if _, err := r.Search(context.Background(), retrieval.Query{Text: "x", K: tc.k}, retrieval.Filter{}); err != nil {
				t.Fatalf("Search: %v", err)
			}
			for _, c := range captured() {
				var body map[string]any
				if err := json.Unmarshal([]byte(c.raw), &body); err != nil {
					t.Fatalf("request to %s is not JSON: %v", c.path, err)
				}
				if size, _ := body["size"].(float64); size != tc.wantSize {
					t.Errorf("request to %s size = %v, want %v", c.path, body["size"], tc.wantSize)
				}
			}
		})
	}
}

// TestDW_1_2_SearchReturnsProjectedFields is the end-to-end shaping check:
// raw _source documents carrying embeddings and ACL provenance come back from
// Search reduced to the tier's allowlist, and (DW-1.4) a hit whose response
// carried no _score still has a populated Score.
func TestDW_1_2_SearchReturnsProjectedFields(t *testing.T) {
	srv, _ := newFakeSearchServer(t, func(idx string) (int, any) {
		if idx == "ep-idx" {
			return 200, hitsBody(map[string]any{
				"_id": "e1", // no _score: fallback must populate it (DW-1.4)
				"_source": map[string]any{
					"text": "raw event", "kind": "observation", "event_id": "ev-1",
					"text_embedding": []float64{0.1, 0.2},
					"tenant_id":      "t1", "scope": "org", "owner_agent_id": "a1", "team_id": "tx",
				},
			})
		}
		return 200, hitsBody(map[string]any{
			"_id": "s1", "_score": 0.8,
			"_source": map[string]any{
				"statement": "A p B", "subject": "A", "predicate": "p", "object": "B",
				"fact_embedding": []float64{0.3},
				"tenant_id":      "t1", "scope": "org", "owner_agent_id": "a1", "team_id": "tx",
			},
		})
	})
	r := retrieval.NewOpenSearchRetriever(srv.Client(), srv.URL, embed.NewFakeEmbedder(4, nil),
		retrieval.WithIndices("ep-idx", "sem-idx"))
	hits, err := r.Search(context.Background(), retrieval.Query{Text: "x"}, retrieval.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	for _, h := range hits {
		for _, k := range []string{"text_embedding", "fact_embedding", "tenant_id", "team_id", "scope", "owner_agent_id"} {
			if _, ok := h.Fields[k]; ok {
				t.Errorf("hit %s leaked field %q: %v", h.ID, k, h.Fields)
			}
		}
		if h.Score == 0 {
			t.Errorf("hit %s has zero Score", h.ID)
		}
	}
	byID := map[string]retrieval.Hit{hits[0].ID: hits[0], hits[1].ID: hits[1]}
	if byID["e1"].Fields["text"] != "raw event" || byID["e1"].Fields["kind"] != "observation" {
		t.Errorf("episodic hit lost allowlisted fields: %v", byID["e1"].Fields)
	}
	if byID["s1"].Fields["statement"] != "A p B" || byID["s1"].Fields["object"] != "B" {
		t.Errorf("semantic hit lost allowlisted fields: %v", byID["s1"].Fields)
	}
}

// TestDW_1_2_SearchToleratesMissingSource (dirty): a hit with no _source at
// all (nil Fields) flows through projection without panicking and is still
// returned.
func TestDW_1_2_SearchToleratesMissingSource(t *testing.T) {
	srv, _ := newFakeSearchServer(t, func(string) (int, any) {
		return 200, hitsBody(map[string]any{"_id": "bare", "_score": 0.5})
	})
	r := retrieval.NewOpenSearchRetriever(srv.Client(), srv.URL, embed.NewFakeEmbedder(4, nil),
		retrieval.WithIndices("ep-idx", "sem-idx"))
	hits, err := r.Search(context.Background(), retrieval.Query{Text: "x", K: 1}, retrieval.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "bare" {
		t.Fatalf("got %+v, want the bare hit", hits)
	}
}

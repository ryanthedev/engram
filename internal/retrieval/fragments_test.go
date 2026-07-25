package retrieval

// Phase-2 unit tests for fragment extraction and the by-id drill-down:
// buildQuery's highlight clause + body suppression (DW-2.1/2.2), its
// memory-path inertness (DW-2.5), parseHits' highlight read, the
// KnowledgeRetriever full-body escape (DW-2.3), and GetDocument (DW-2.4's
// retrieval leg).

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ryanthedev/engram/internal/knowledge"
)

// TestBuildQueryDW_2_2_HighlightDefaultsEmptyTags: a highlight-enabled query
// emits the sizing knobs plus pre/post tags of exactly [""] when the
// collection declares none — OpenSearch's markers-off escape from its <em>
// default — and suppresses the text field from _source.
func TestBuildQueryDW_2_2_HighlightDefaultsEmptyTags(t *testing.T) {
	body, usePipeline := buildQuery(queryOpts{
		mode: ModeBM25Only, textField: "abstract", text: "q", k: 5,
		fragmentSize: 240, numberOfFragments: 3,
	})
	if usePipeline {
		t.Error("BM25-only must not use the RRF pipeline")
	}
	raw := string(body)
	for _, want := range []string{
		`"highlight":{"fields":{"abstract":{`,
		`"fragment_size":240`,
		`"number_of_fragments":3`,
		`"pre_tags":[""]`,
		`"post_tags":[""]`,
		`"excludes":["text_embedding","fact_embedding","abstract"]`,
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("query missing %s: %s", want, raw)
		}
	}
}

// TestBuildQueryDW_2_2_CustomTags: collection-declared marker tags land in
// the highlight clause verbatim — never a hardcoded pair.
func TestBuildQueryDW_2_2_CustomTags(t *testing.T) {
	body, _ := buildQuery(queryOpts{
		mode: ModeBM25Only, textField: "text", text: "q", k: 5,
		fragmentSize: 100, numberOfFragments: 1,
		highlightPreTag: "«", highlightPostTag: "»",
	})
	raw := string(body)
	if !strings.Contains(raw, `"pre_tags":["«"]`) || !strings.Contains(raw, `"post_tags":["»"]`) {
		t.Errorf("query missing custom tags: %s", raw)
	}
}

// TestBuildQueryDW_2_5_ZeroValueOmitsHighlight: the memory path (zero-value
// highlight opts) emits NO highlight clause and the unchanged embedding-only
// excludes — the structural half of the memory-path inertness guarantee
// (the golden matrix in buildquery_golden_test.go pins the exact bytes).
func TestBuildQueryDW_2_5_ZeroValueOmitsHighlight(t *testing.T) {
	body, _ := buildQuery(queryOpts{mode: ModeBM25Only, textField: "text", text: "q", k: 5})
	raw := string(body)
	if strings.Contains(raw, "highlight") {
		t.Errorf("zero-value opts must not emit a highlight clause: %s", raw)
	}
	if !strings.Contains(raw, `"excludes":["text_embedding","fact_embedding"]`) {
		t.Errorf("zero-value opts must keep the embedding-only excludes: %s", raw)
	}
}

// TestParseHitsReadsHighlightFragments: a hit's highlight section flattens
// into Hit.Fragments in response order.
func TestParseHitsReadsHighlightFragments(t *testing.T) {
	decoded := map[string]any{"hits": map[string]any{"hits": []any{
		map[string]any{
			"_id": "d1", "_score": 2.0,
			"_source":   map[string]any{"title": "T"},
			"highlight": map[string]any{"text": []any{"frag one", "frag two"}},
		},
	}}}
	hits := parseHits(decoded, "docs")
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(hits))
	}
	if len(hits[0].Fragments) != 2 || hits[0].Fragments[0] != "frag one" || hits[0].Fragments[1] != "frag two" {
		t.Errorf("Fragments = %v, want [frag one, frag two]", hits[0].Fragments)
	}
}

// TestParseHitsDW_2_5_NoHighlightKeyInert: a memory-shaped response (no
// highlight key — memory queries never request one) yields nil Fragments,
// proving the shared parseHits change is inert on the memory path.
func TestParseHitsDW_2_5_NoHighlightKeyInert(t *testing.T) {
	decoded := map[string]any{"hits": map[string]any{"hits": []any{
		map[string]any{"_id": "m1", "_score": 1.0, "_source": map[string]any{"text": "memory"}},
	}}}
	hits := parseHits(decoded, "episodic")
	if hits[0].Fragments != nil {
		t.Errorf("Fragments = %v, want nil for a response with no highlight key", hits[0].Fragments)
	}
}

// TestKnowledgeSearchDW_2_1_DefaultRequestsFragments: the default search
// asks OpenSearch for highlighting sized by the collection's FragmentSizing
// fallback (240/3 when unset) and excludes from _source the embeddings, the
// text field (fragments replace it), and the harvest provenance envelope —
// bookkeeping no search caller reads back off a hit. Order is significant:
// the unconditional embedding excludes come first, then the text field, then
// the sorted provenance set (store.SearchProvenanceExcludes).
func TestKnowledgeSearchDW_2_1_DefaultRequestsFragments(t *testing.T) {
	srv, captured := newFakeKnowledgeServer(t, func(string) (int, any) {
		return http.StatusOK, knowledgeHitsBody(0)
	})
	r := NewKnowledgeRetriever(srv.Client(), srv.URL, newFakeKnowledgeRegistry())
	if _, _, err := r.Search(context.Background(), arxivSpec("knowledge-arxiv"), "q", nil, nil, 10, 0, false); err != nil {
		t.Fatalf("Search: %v", err)
	}
	raw := captured()[0].raw
	for _, want := range []string{
		`"fragment_size":240`, `"number_of_fragments":3`,
		`"excludes":["text_embedding","fact_embedding","abstract",` +
			`"collection","harvest_id","harvested_at","source","source_version"]`,
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("default search request missing %s: %s", want, raw)
		}
	}
}

// TestKnowledgeSearchPerCollectionSizingAndTags: declared sizing and marker
// tags override the global fallback verbatim.
func TestKnowledgeSearchPerCollectionSizingAndTags(t *testing.T) {
	srv, captured := newFakeKnowledgeServer(t, func(string) (int, any) {
		return http.StatusOK, knowledgeHitsBody(0)
	})
	spec := arxivSpec("knowledge-arxiv")
	spec.FragmentSize, spec.NumberOfFragments = 100, 1
	spec.HighlightPreTag, spec.HighlightPostTag = "[[", "]]"
	r := NewKnowledgeRetriever(srv.Client(), srv.URL, newFakeKnowledgeRegistry())
	if _, _, err := r.Search(context.Background(), spec, "q", nil, nil, 10, 0, false); err != nil {
		t.Fatalf("Search: %v", err)
	}
	raw := captured()[0].raw
	for _, want := range []string{
		`"fragment_size":100`, `"number_of_fragments":1`,
		`"pre_tags":["[["]`, `"post_tags":["]]"]`,
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("request missing %s: %s", want, raw)
		}
	}
}

// TestKnowledgeSearchDW_2_3_FullBodySkipsHighlight: fullBody=true restores
// the pre-fragment request byte-for-byte — no highlight clause, text field
// kept in _source — which IS today's whole-body behavior.
//
// The excludes assertion below is load-bearing beyond the text field: it also
// pins that a full-body search carries NO harvest-provenance excludes. The
// vault exporter (internal/cli/vaultknowledge.go) drains every collection
// through this path and reads title/body/memory_ref back out of fields_json,
// and its decoder degrades to an empty doc rather than erroring — so leaking
// the default search's excludes into this path would rewrite the user's vault
// with empty notes and exit 0. Silent. Keep this assertion exact.
func TestKnowledgeSearchDW_2_3_FullBodySkipsHighlight(t *testing.T) {
	srv, captured := newFakeKnowledgeServer(t, func(string) (int, any) {
		return http.StatusOK, knowledgeHitsBody(0)
	})
	r := NewKnowledgeRetriever(srv.Client(), srv.URL, newFakeKnowledgeRegistry())
	if _, _, err := r.Search(context.Background(), arxivSpec("knowledge-arxiv"), "q", nil, nil, 10, 0, true); err != nil {
		t.Fatalf("Search: %v", err)
	}
	raw := captured()[0].raw
	if strings.Contains(raw, "highlight") {
		t.Errorf("full-body search must not request highlighting: %s", raw)
	}
	if !strings.Contains(raw, `"excludes":["text_embedding","fact_embedding"]`) {
		t.Errorf("full-body search must keep the text field in _source: %s", raw)
	}
	// The full-body request must equal the pre-fragment query PLUS
	// track_total_hits (Phase 3: Search always requests the exact total,
	// full_body or not — fetchKnowledgeDocs' paging depends on it even on
	// its full_body=true calls).
	want, _ := buildQuery(queryOpts{mode: ModeBM25Only, textField: "abstract", text: "q", k: 10, trackTotalHits: true})
	if raw != string(want) {
		t.Errorf("full-body request diverged from the pre-fragment query:\n got %s\nwant %s", raw, want)
	}
}

// --- GetDocument (DW-2.4's retrieval leg) ------------------------------

func getDocSpec(index string) knowledge.CollectionSpec {
	s := arxivSpec(index)
	return s
}

// TestGetDocumentReturnsFullSource: a found doc returns its whole _source.
func TestGetDocumentReturnsFullSource(t *testing.T) {
	srv, captured := newFakeKnowledgeServer(t, func(string) (int, any) {
		return http.StatusOK, map[string]any{
			"_id": "d1", "found": true,
			"_source": map[string]any{"title": "T", "abstract": "full body", "collection": "arxiv"},
		}
	})
	r := NewKnowledgeRetriever(srv.Client(), srv.URL, newFakeKnowledgeRegistry())
	doc, ok, err := r.GetDocument(context.Background(), getDocSpec("knowledge-arxiv"), "d1")
	if err != nil || !ok {
		t.Fatalf("GetDocument = ok=%v err=%v, want found", ok, err)
	}
	if doc["abstract"] != "full body" || doc["title"] != "T" {
		t.Errorf("doc = %v, want the full stored _source", doc)
	}
	if reqs := captured(); len(reqs) != 1 || reqs[0].path != "/knowledge-arxiv/_doc/d1" {
		t.Fatalf("captured = %+v, want one GET /knowledge-arxiv/_doc/d1", reqs)
	}
}

// TestGetDocumentEscapesID: a harvester-chosen id with path-hostile
// characters is escaped into a single path segment, never interpolated raw.
func TestGetDocumentEscapesID(t *testing.T) {
	srv, captured := newFakeKnowledgeServer(t, func(string) (int, any) {
		return http.StatusOK, map[string]any{"found": true, "_source": map[string]any{"a": "b"}}
	})
	r := NewKnowledgeRetriever(srv.Client(), srv.URL, newFakeKnowledgeRegistry())
	if _, _, err := r.GetDocument(context.Background(), getDocSpec("knowledge-arxiv"), "docs/read me.md#s1"); err != nil {
		t.Fatalf("GetDocument: %v", err)
	}
	got := captured()[0].path
	// httptest reports the decoded path; the load-bearing property is that
	// the id stayed ONE segment under _doc/ rather than splitting the route.
	if !strings.HasPrefix(got, "/knowledge-arxiv/_doc/") || strings.Count(got, "/") != 3+strings.Count("docs/read me.md#s1", "/") {
		t.Errorf("path = %q, want the id escaped under /_doc/", got)
	}
}

// TestGetDocumentMissesReadAsAbsent: a 404 — missing doc or a
// registered-but-unprovisioned index — is ok=false, never an error.
func TestGetDocumentMissesReadAsAbsent(t *testing.T) {
	srv, _ := newFakeKnowledgeServer(t, func(string) (int, any) {
		return http.StatusNotFound, map[string]any{"found": false}
	})
	r := NewKnowledgeRetriever(srv.Client(), srv.URL, newFakeKnowledgeRegistry())
	doc, ok, err := r.GetDocument(context.Background(), getDocSpec("knowledge-arxiv"), "ghost")
	if err != nil || ok || doc != nil {
		t.Errorf("GetDocument(miss) = %v ok=%v err=%v, want nil/false/nil", doc, ok, err)
	}
}

// TestGetDocumentRejectsBadIndex: the index barricade holds on the by-id
// path exactly as it does on Search (path-traversal hardening).
func TestGetDocumentRejectsBadIndex(t *testing.T) {
	r := NewKnowledgeRetriever(http.DefaultClient, "http://unused.invalid", newFakeKnowledgeRegistry())
	if _, _, err := r.GetDocument(context.Background(), getDocSpec("../episodic-events"), "d1"); err == nil {
		t.Error("want an index validation error, got nil")
	}
}

// TestGetDocumentEmptyIDIsAbsent: an empty id addresses nothing — an opaque
// miss, not a malformed cluster request.
func TestGetDocumentEmptyIDIsAbsent(t *testing.T) {
	r := NewKnowledgeRetriever(http.DefaultClient, "http://unused.invalid", newFakeKnowledgeRegistry())
	doc, ok, err := r.GetDocument(context.Background(), getDocSpec("knowledge-arxiv"), "")
	if err != nil || ok || doc != nil {
		t.Errorf("GetDocument(\"\") = %v ok=%v err=%v, want nil/false/nil", doc, ok, err)
	}
}

// TestGetDocumentUnexpectedStatusErrors: a 5xx is infrastructure, not
// absence — it must surface, never masquerade as not-found.
func TestGetDocumentUnexpectedStatusErrors(t *testing.T) {
	srv, _ := newFakeKnowledgeServer(t, func(string) (int, any) {
		return http.StatusInternalServerError, map[string]any{"error": "boom"}
	})
	r := NewKnowledgeRetriever(srv.Client(), srv.URL, newFakeKnowledgeRegistry())
	if _, _, err := r.GetDocument(context.Background(), getDocSpec("knowledge-arxiv"), "d1"); err == nil {
		t.Error("want an error for status 500, got nil")
	}
}

// TestGetDocumentMissingSourceErrors: a 200 with no _source object is a
// malformed cluster answer — loud, not a silent empty doc.
func TestGetDocumentMissingSourceErrors(t *testing.T) {
	srv, _ := newFakeKnowledgeServer(t, func(string) (int, any) {
		return http.StatusOK, map[string]any{"found": true}
	})
	r := NewKnowledgeRetriever(srv.Client(), srv.URL, newFakeKnowledgeRegistry())
	if _, _, err := r.GetDocument(context.Background(), getDocSpec("knowledge-arxiv"), "d1"); err == nil {
		t.Error("want an error for a 200 without _source, got nil")
	}
}

// TestKnowledgeHitFragmentsSurviveJSON: retrieval.Hit's Fragments ride the
// decode/re-encode of a full search response (sanity on the seam Phase 3
// consumes).
func TestKnowledgeHitFragmentsSurviveJSON(t *testing.T) {
	payload := knowledgeHitsBody(1, map[string]any{
		"_id": "d1", "_score": 1.5,
		"_source":   map[string]any{"title": "T"},
		"highlight": map[string]any{"abstract": []any{"a fragment"}},
	})
	b, _ := json.Marshal(payload)
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	hits := parseHits(decoded, "arxiv")
	if len(hits) != 1 || len(hits[0].Fragments) != 1 || hits[0].Fragments[0] != "a fragment" {
		t.Errorf("hits = %+v, want one hit carrying [a fragment]", hits)
	}
}

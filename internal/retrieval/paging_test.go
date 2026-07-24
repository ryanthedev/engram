package retrieval

// Phase-3 unit tests for offset/exact-total paging: buildQuery's from/
// track_total_hits wiring (both zero-value-off, mirroring the Phase-2
// highlight seam's memory-path inertness), KnowledgeRetriever.Search
// threading offset onto the request and total off the response, the
// offset-beyond-total dirty case (empty hits, exact total, not an error),
// and the max_result_window clamp (a self-correcting Go error, never a raw
// OpenSearch request).

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestBuildQueryDW_3_1_OffsetEmitsFromKey: a positive offset adds "from" to
// the request; the zero value (first page) emits no "from" key at all —
// exactly the DW-1.3 golden-matrix contract memory callers rely on, now
// extended to the offset seam this phase wires live.
func TestBuildQueryDW_3_1_OffsetEmitsFromKey(t *testing.T) {
	withOffset, _ := buildQuery(queryOpts{mode: ModeBM25Only, textField: "text", text: "q", k: 5, offset: 20})
	if !strings.Contains(string(withOffset), `"from":20`) {
		t.Errorf("offset=20 query missing \"from\":20: %s", withOffset)
	}
	zero, _ := buildQuery(queryOpts{mode: ModeBM25Only, textField: "text", text: "q", k: 5, offset: 0})
	if strings.Contains(string(zero), `"from"`) {
		t.Errorf("offset=0 (first page) must emit no \"from\" key: %s", zero)
	}
}

// TestBuildQueryDW_3_1_TrackTotalHitsGatedByFlag: track_total_hits appears
// only when trackTotalHits is set — memory callers never set it (their zero
// value keeps the golden-matrix bytes untouched).
func TestBuildQueryDW_3_1_TrackTotalHitsGatedByFlag(t *testing.T) {
	on, _ := buildQuery(queryOpts{mode: ModeBM25Only, textField: "text", text: "q", k: 5, trackTotalHits: true})
	if !strings.Contains(string(on), `"track_total_hits":true`) {
		t.Errorf("trackTotalHits=true query missing track_total_hits:true: %s", on)
	}
	off, _ := buildQuery(queryOpts{mode: ModeBM25Only, textField: "text", text: "q", k: 5})
	if strings.Contains(string(off), "track_total_hits") {
		t.Errorf("zero-value opts must not emit track_total_hits: %s", off)
	}
}

// TestKnowledgeSearchDW_3_1_OffsetThreadedToRequestAndTotalReturned proves
// Search puts a non-zero offset on the wire as "from" and hands the
// response's exact total straight back to the caller unmodified.
func TestKnowledgeSearchDW_3_1_OffsetThreadedToRequestAndTotalReturned(t *testing.T) {
	srv, captured := newFakeKnowledgeServer(t, func(string) (int, any) {
		return http.StatusOK, knowledgeHitsBody(137, knowledgeHit("2401.9", 1.0, map[string]any{"abstract": "x"}))
	})
	r := NewKnowledgeRetriever(srv.Client(), srv.URL, newFakeKnowledgeRegistry())
	hits, total, err := r.Search(context.Background(), arxivSpec("knowledge-arxiv"), "x", nil, nil, 10, 50, false)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(hits))
	}
	if total != 137 {
		t.Errorf("total = %d, want the exact response total 137", total)
	}
	raw := captured()[0].raw
	if !strings.Contains(raw, `"from":50`) {
		t.Errorf("request missing \"from\":50: %s", raw)
	}
	if !strings.Contains(raw, `"track_total_hits":true`) {
		t.Errorf("request missing track_total_hits:true: %s", raw)
	}
}

// TestKnowledgeSearchDW_3_1_OffsetBeyondTotalIsEmptyNotError is the dirty
// case: an offset past the real match count returns zero hits and the
// still-exact total, never an error — exactly what a caller draining a
// collection to completion sees on its last page.
func TestKnowledgeSearchDW_3_1_OffsetBeyondTotalIsEmptyNotError(t *testing.T) {
	srv, _ := newFakeKnowledgeServer(t, func(string) (int, any) {
		return http.StatusOK, knowledgeHitsBody(3) // no hits: offset(9000) is past the 3 real matches
	})
	r := NewKnowledgeRetriever(srv.Client(), srv.URL, newFakeKnowledgeRegistry())
	hits, total, err := r.Search(context.Background(), arxivSpec("knowledge-arxiv"), "x", nil, nil, 10, 9000, false)
	if err != nil {
		t.Fatalf("Search(offset past total): %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("got %d hits, want 0", len(hits))
	}
	if total != 3 {
		t.Errorf("total = %d, want the exact match count 3 even though this page is empty", total)
	}
}

// TestKnowledgeSearchDW_3_1_NegativeOffsetClampsToZero proves clampOffset's
// silent-normalize posture (mirroring clampK): a negative offset — external
// input that can never legitimately mean "skip -N hits" — is treated as the
// first page rather than erroring or reaching the wire as-is.
func TestKnowledgeSearchDW_3_1_NegativeOffsetClampsToZero(t *testing.T) {
	srv, captured := newFakeKnowledgeServer(t, func(string) (int, any) {
		return http.StatusOK, knowledgeHitsBody(0)
	})
	r := NewKnowledgeRetriever(srv.Client(), srv.URL, newFakeKnowledgeRegistry())
	if _, _, err := r.Search(context.Background(), arxivSpec("knowledge-arxiv"), "x", nil, nil, 10, -5, false); err != nil {
		t.Fatalf("Search(negative offset): %v", err)
	}
	if raw := captured()[0].raw; strings.Contains(raw, `"from"`) {
		t.Errorf("negative offset must clamp to 0 (no \"from\" key), got: %s", raw)
	}
}

// TestKnowledgeSearchDW_3_2_OffsetPlusKExceedsMaxResultWindow proves the
// clamp fires BEFORE any HTTP call (a search_phase_execution_exception would
// otherwise surface as a raw, caller-unfriendly OpenSearch 500) and the
// error names both the offending values and the cap so an LLM caller can fix
// its own call.
func TestKnowledgeSearchDW_3_2_OffsetPlusKExceedsMaxResultWindow(t *testing.T) {
	// The handler must never actually be invoked (asserted below via
	// captured()) — it stands in for what would otherwise be OpenSearch's own
	// search_phase_execution_exception on an over-window from/size request.
	srv, captured := newFakeKnowledgeServer(t, func(string) (int, any) {
		return http.StatusInternalServerError, map[string]any{"error": "unreachable: clamp should fail before this"}
	})
	r := NewKnowledgeRetriever(srv.Client(), srv.URL, newFakeKnowledgeRegistry())
	_, _, err := r.Search(context.Background(), arxivSpec("knowledge-arxiv"), "x", nil, nil, 50, MaxResultWindow, false)
	if err == nil {
		t.Fatal("want a self-correcting error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{fmt.Sprintf("%d", MaxResultWindow), "offset", "k"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
	if len(captured()) != 0 {
		t.Errorf("clamp must fail BEFORE any HTTP call, got %d requests", len(captured()))
	}
}

// TestKnowledgeSearchDW_3_2_OffsetPlusKAtExactWindowSucceeds is the boundary:
// offset+k landing EXACTLY on MaxResultWindow is allowed (the clamp rejects
// only what exceeds it).
func TestKnowledgeSearchDW_3_2_OffsetPlusKAtExactWindowSucceeds(t *testing.T) {
	srv, captured := newFakeKnowledgeServer(t, func(string) (int, any) {
		return http.StatusOK, knowledgeHitsBody(0)
	})
	r := NewKnowledgeRetriever(srv.Client(), srv.URL, newFakeKnowledgeRegistry())
	_, _, err := r.Search(context.Background(), arxivSpec("knowledge-arxiv"), "x", nil, nil, 50, MaxResultWindow-50, false)
	if err != nil {
		t.Fatalf("Search(offset+k == MaxResultWindow): %v", err)
	}
	if len(captured()) != 1 {
		t.Errorf("want exactly one request at the boundary, got %d", len(captured()))
	}
}

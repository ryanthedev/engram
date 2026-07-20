package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/store"
)

// scanEpisodicServer is the episodic mirror of facts_test.go's
// scanFactsServer: a hermetic httptest stand-in for the episodic index's
// _search endpoint answering one canned page per call and capturing every
// request body, so ScanEpisodic's query shape, byte budgeting, and cursor
// round-trip can be asserted without a live cluster.
type scanEpisodicServer struct {
	pages [][]map[string]any
	calls []map[string]any
}

func (s *scanEpisodicServer) start(t *testing.T) *store.OpenSearchStore {
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
	return store.NewOpenSearchStore(srv.Client(), srv.URL, store.WithEpisodicIndex("engram-episodic-scratch"))
}

// episodicHit builds one raw _search hit for rec, matching the shape
// decodeSource expects.
func episodicHit(id string, rec memory.Episodic) map[string]any {
	raw, _ := json.Marshal(rec)
	var src map[string]any
	_ = json.Unmarshal(raw, &src)
	return map[string]any{"_id": id, "_source": src}
}

// scanEpisodicRec builds a processed episodic record for tenant t1 whose
// Text is textBytes long.
func scanEpisodicRec(eventID string, createdAt time.Time, textBytes int) memory.Episodic {
	processed := createdAt.Add(time.Minute)
	return memory.Episodic{
		EventID: eventID, TenantID: "t1", TeamID: "teamX", Scope: "team",
		OwnerAgentID: "a1", Kind: "conversation", Text: strings.Repeat("x", textBytes),
		OccurredAt: createdAt, CreatedAt: createdAt, ProcessedAt: &processed,
	}
}

// drainScanEpisodic walks ScanEpisodic to exhaustion (bounded — a cursor
// that never empties is itself a failure) and returns every page.
func drainScanEpisodic(t *testing.T, s *store.OpenSearchStore) [][]memory.Episodic {
	t.Helper()
	var pages [][]memory.Episodic
	after := ""
	for i := 0; i < 20; i++ {
		recs, next, err := s.ScanEpisodic(context.Background(), "t1", after)
		if err != nil {
			t.Fatalf("ScanEpisodic page %d: %v", i, err)
		}
		pages = append(pages, recs)
		if next == "" {
			return pages
		}
		after = next
	}
	t.Fatal("ScanEpisodic never exhausted within 20 pages")
	return nil
}

// TestDW_1_4_ScanEpisodicQueryShape pins the clauses that keep the export
// wire clean: tenant term + processed_at-exists filters, a must_not on
// dead_lettered (unprocessed and dead-lettered docs are absent by query
// construction), the deterministic (created_at, event_id) sort, the default
// batch size, and no search_after on the first page.
func TestDW_1_4_ScanEpisodicQueryShape(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	srv := &scanEpisodicServer{pages: [][]map[string]any{{episodicHit("d1", scanEpisodicRec("ev-1", t0, 5))}}}
	s := srv.start(t)

	recs, next, err := s.ScanEpisodic(context.Background(), "t1", "")
	if err != nil {
		t.Fatalf("ScanEpisodic: %v", err)
	}
	if len(recs) != 1 || recs[0].EventID != "ev-1" {
		t.Fatalf("recs = %+v, want one record for ev-1", recs)
	}
	if next != "" {
		t.Fatalf("next = %q, want empty (short page, exhausted)", next)
	}

	body := srv.calls[0]
	if _, has := body["search_after"]; has {
		t.Error("first-page request carries search_after, want none")
	}
	if size, _ := body["size"].(float64); int(size) != store.DefaultScanBatchSize {
		t.Errorf("size = %v, want %d", body["size"], store.DefaultScanBatchSize)
	}
	sortClauses, _ := body["sort"].([]any)
	if len(sortClauses) != 2 {
		t.Fatalf("sort = %v, want 2 clauses (created_at, event_id)", sortClauses)
	}
	if _, ok := sortClauses[0].(map[string]any)["created_at"]; !ok {
		t.Errorf("sort[0] = %v, want created_at", sortClauses[0])
	}
	if _, ok := sortClauses[1].(map[string]any)["event_id"]; !ok {
		t.Errorf("sort[1] = %v, want event_id", sortClauses[1])
	}
	boolQ, _ := body["query"].(map[string]any)["bool"].(map[string]any)
	filters, _ := boolQ["filter"].([]any)
	if len(filters) != 2 {
		t.Fatalf("filter clauses = %v, want 2 (tenant term, processed_at exists)", filters)
	}
	tenantTerm, _ := filters[0].(map[string]any)["term"].(map[string]any)
	if tenantTerm["tenant_id"] != "t1" {
		t.Errorf("filter[0] = %v, want tenant_id=t1 term", filters[0])
	}
	exists, _ := filters[1].(map[string]any)["exists"].(map[string]any)
	if exists["field"] != "processed_at" {
		t.Errorf("filter[1] = %v, want processed_at exists (unprocessed docs excluded)", filters[1])
	}
	mustNot, _ := boolQ["must_not"].([]any)
	if len(mustNot) != 1 {
		t.Fatalf("must_not = %v, want 1 clause (dead_lettered)", mustNot)
	}
	dl, _ := mustNot[0].(map[string]any)["term"].(map[string]any)
	if dl["dead_lettered"] != true {
		t.Errorf("must_not[0] = %v, want dead_lettered=true term (dead-lettered docs excluded)", mustNot[0])
	}
}

// TestScanEpisodic_FullPageResumesWithSearchAfter drives a full count-bound
// page: exactly DefaultScanBatchSize small records return a non-empty token
// built from the LAST record's (created_at millis, event_id), and the next
// call carries those values as search_after — the round-trip contract the
// Export handler's wire cursor depends on.
func TestScanEpisodic_FullPageResumesWithSearchAfter(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	page1 := make([]map[string]any, store.DefaultScanBatchSize)
	var lastAt time.Time
	for i := range page1 {
		lastAt = t0.Add(time.Duration(i) * time.Second)
		page1[i] = episodicHit("d", scanEpisodicRec("ev-"+strings.Repeat("z", 1)+string(rune('a'+i%26)), lastAt, 3))
	}
	last := scanEpisodicRec("ev-last", lastAt, 3)
	page1[len(page1)-1] = episodicHit("d-last", last)
	page2 := []map[string]any{episodicHit("d-next", scanEpisodicRec("ev-next", lastAt.Add(time.Second), 3))}
	srv := &scanEpisodicServer{pages: [][]map[string]any{page1, page2}}
	s := srv.start(t)
	ctx := context.Background()

	recs1, next1, err := s.ScanEpisodic(ctx, "t1", "")
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(recs1) != store.DefaultScanBatchSize || next1 == "" {
		t.Fatalf("page 1: %d recs, next %q — want a full page with a continuation token", len(recs1), next1)
	}

	recs2, next2, err := s.ScanEpisodic(ctx, "t1", next1)
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(recs2) != 1 || recs2[0].EventID != "ev-next" || next2 != "" {
		t.Fatalf("page 2 = %d recs (next %q), want the single ev-next record then exhaustion", len(recs2), next2)
	}

	searchAfter, ok := srv.calls[1]["search_after"].([]any)
	if !ok || len(searchAfter) != 2 {
		t.Fatalf("page-2 search_after = %v, want [millis, event_id]", srv.calls[1]["search_after"])
	}
	if got, want := int64(searchAfter[0].(float64)), last.CreatedAt.UnixMilli(); got != want {
		t.Errorf("search_after[0] = %d, want %d (page 1's last created_at in millis)", got, want)
	}
	if searchAfter[1] != "ev-last" {
		t.Errorf("search_after[1] = %v, want ev-last (page 1's last event_id)", searchAfter[1])
	}
}

// TestDW_1_5_ScanEpisodicByteBudgetSplitsPages: a synthetic oversized-Text
// set (each record under the budget, the set far over it) produces multiple
// pages, none exceeding EpisodicPageByteBudget, every record surfacing
// exactly once — and the resume token points at the CUT position, not the
// end of the fetched batch, so truncation never skips records.
func TestDW_1_5_ScanEpisodicByteBudgetSplitsPages(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	big := store.EpisodicPageByteBudget/2 - 1024 // two fit a page, three do not
	r1 := scanEpisodicRec("ev-1", t0, big)
	r2 := scanEpisodicRec("ev-2", t0.Add(time.Second), big)
	r3 := scanEpisodicRec("ev-3", t0.Add(2*time.Second), big)
	srv := &scanEpisodicServer{pages: [][]map[string]any{
		{episodicHit("d1", r1), episodicHit("d2", r2), episodicHit("d3", r3)},
		{episodicHit("d3", r3)}, // what a real search_after past ev-2 would return
	}}
	s := srv.start(t)

	pages := drainScanEpisodic(t, s)
	if len(pages) != 2 {
		t.Fatalf("pages = %d, want 2 (three big records must not fit one byte-budgeted page)", len(pages))
	}
	var seen []string
	for i, page := range pages {
		bytes := 0
		for _, r := range page {
			bytes += len(r.Text)
			seen = append(seen, r.EventID)
		}
		if bytes > store.EpisodicPageByteBudget {
			t.Errorf("page %d carries %d text bytes, exceeding the %d budget", i, bytes, store.EpisodicPageByteBudget)
		}
	}
	if want := []string{"ev-1", "ev-2", "ev-3"}; strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Errorf("records seen = %v, want %v exactly once each in order", seen, want)
	}
	// The truncated page's resume token must carry the LAST INCLUDED record's
	// sort key (ev-2) — not ev-3's, which would silently skip it.
	searchAfter, ok := srv.calls[1]["search_after"].([]any)
	if !ok || len(searchAfter) != 2 || searchAfter[1] != "ev-2" {
		t.Fatalf("page-2 search_after = %v, want [.., ev-2] (resume at the cut)", srv.calls[1]["search_after"])
	}
}

// TestDW_1_5_ScanEpisodicSingleOversizedRecordStillProgresses: a record
// whose Text alone exceeds the budget is returned alone (an oversized page
// is the documented degenerate case) and the cursor still advances past it —
// the export can never wedge on one huge event.
func TestDW_1_5_ScanEpisodicSingleOversizedRecordStillProgresses(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	huge := scanEpisodicRec("ev-huge", t0, store.EpisodicPageByteBudget+4096)
	small := scanEpisodicRec("ev-small", t0.Add(time.Second), 10)
	srv := &scanEpisodicServer{pages: [][]map[string]any{
		{episodicHit("d1", huge), episodicHit("d2", small)},
		{episodicHit("d2", small)},
	}}
	s := srv.start(t)

	pages := drainScanEpisodic(t, s)
	if len(pages) != 2 || len(pages[0]) != 1 || pages[0][0].EventID != "ev-huge" {
		t.Fatalf("pages = %v, want the huge record alone on page 1", pages)
	}
	if len(pages[1]) != 1 || pages[1][0].EventID != "ev-small" {
		t.Fatalf("page 2 = %+v, want the small record (scan progressed past the oversized one)", pages[1])
	}
}

// TestScanEpisodic_BadCursorRejected: an undecodable resume token is
// rejected with the ErrBadCursor sentinel BEFORE any query is issued — a
// forged token can only be refused, never silently restart or reposition.
func TestScanEpisodic_BadCursorRejected(t *testing.T) {
	srv := &scanEpisodicServer{}
	s := srv.start(t)

	_, _, err := s.ScanEpisodic(context.Background(), "t1", "%%%not-json%%%")
	if !errors.Is(err, store.ErrBadCursor) {
		t.Fatalf("err = %v, want wrapping store.ErrBadCursor", err)
	}
	if len(srv.calls) != 0 {
		t.Errorf("a bad cursor still issued %d queries, want 0", len(srv.calls))
	}
}

// TestScanEpisodic_MissingIndexReturnsEmptyNotError mirrors the established
// contract of every read path in facts.go: a not-yet-created episodic index
// is an empty tier, not an error.
func TestScanEpisodic_MissingIndexReturnsEmptyNotError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"type": "index_not_found_exception", "reason": "no such index"},
		})
	}))
	t.Cleanup(srv.Close)
	s := store.NewOpenSearchStore(srv.Client(), srv.URL, store.WithEpisodicIndex("engram-episodic-missing"))

	recs, next, err := s.ScanEpisodic(context.Background(), "t1", "")
	if err != nil || len(recs) != 0 || next != "" {
		t.Fatalf("missing index = (%v, %q, %v), want (empty, \"\", nil)", recs, next, err)
	}
}

// TestScanEpisodic_NonIndexNotFoundErrorPropagates: any other backend
// failure surfaces as an error (the caller fails the export closed).
func TestScanEpisodic_NonIndexNotFoundErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "search_phase_execution_exception"}})
	}))
	t.Cleanup(srv.Close)
	s := store.NewOpenSearchStore(srv.Client(), srv.URL, store.WithEpisodicIndex("engram-episodic-scratch"))

	if _, _, err := s.ScanEpisodic(context.Background(), "t1", ""); err == nil {
		t.Fatal("ScanEpisodic on a 500 = nil error, want an error")
	}
}

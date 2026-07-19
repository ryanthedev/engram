package graph

import (
	"context"
	"errors"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/memory"
)

// --- fakes ------------------------------------------------------------

// fakeDropper is a spy IndexDropper: it records that it ran (and when,
// relative to a shared event log) and can be made to fail.
type fakeDropper struct {
	log   *[]string
	err   error
	calls int
}

func (d *fakeDropper) DropAndRecreate(context.Context) error {
	d.calls++
	if d.log != nil {
		*d.log = append(*d.log, "drop")
	}
	return d.err
}

// fakePagedScanner is a FactScanner over a fixed, pre-paginated fixture:
// pages[0] is returned first (with a next cursor pointing at pages[1] if
// there is one), and so on. errOnPage, if set, fails that page's call
// instead of returning it (0-indexed) — used to test mid-scan failure.
type fakePagedScanner struct {
	log       *[]string
	pages     [][]memory.SemanticFact
	errOnPage int // -1 = never
	calls     int
}

func newFakeScanner(log *[]string, facts ...memory.SemanticFact) *fakePagedScanner {
	return &fakePagedScanner{log: log, pages: [][]memory.SemanticFact{facts}, errOnPage: -1}
}

func (s *fakePagedScanner) ScanLiveFacts(_ context.Context, _ string, cursor FactCursor) ([]memory.SemanticFact, FactCursor, error) {
	page := int(cursor.CreatedAtUnixMilli) // fixture encodes the page index directly in the cursor
	if s.calls == 0 {
		if s.log != nil {
			*s.log = append(*s.log, "scan")
		}
	}
	s.calls++
	if page == s.errOnPage {
		return nil, FactCursor{}, errors.New("fake scanner: simulated failure")
	}
	if page >= len(s.pages) {
		return nil, FactCursor{}, nil
	}
	facts := s.pages[page]
	if page+1 < len(s.pages) {
		return facts, FactCursor{CreatedAtUnixMilli: int64(page + 1)}, nil
	}
	return facts, FactCursor{}, nil
}

// --- fixtures -----------------------------------------------------------

// liveFact is fact() with a deterministic SourceID and ContentKey set — the
// shape a real live semantic fact carries (ScanLiveFacts always returns
// facts with SourceIDs populated).
func liveFact(subject, predicate, object, statement, sourceID string) memory.SemanticFact {
	f := fact(subject, predicate, object, statement)
	f.SourceIDs = []string{sourceID}
	f.ContentKey = subject + "|" + predicate + "|" + object
	f.CreatedAt = time.Now().UTC()
	return f
}

// --- DW-3.1: drop-then-replay orchestration ------------------------------

func TestRebuild_DropsBeforeScanning(t *testing.T) {
	ctx := context.Background()
	stage, _, _ := newNameKeyedTestStage(t)
	var log []string
	dropper := &fakeDropper{log: &log}
	scanner := newFakeScanner(&log, liveFact("A", "owns", "B", "A owns B", "ev-1"))

	if _, err := Rebuild(ctx, dropper, scanner, stage, "t1", nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if dropper.calls != 1 {
		t.Fatalf("dropper called %d times, want 1", dropper.calls)
	}
	if len(log) < 2 || log[0] != "drop" || log[1] != "scan" {
		t.Fatalf("call order = %v, want [drop scan ...]", log)
	}
}

func TestRebuild_ReplaysEveryScannedFactAcrossPages(t *testing.T) {
	ctx := context.Background()
	stage, store, backend := newNameKeyedTestStage(t)
	scanner := &fakePagedScanner{errOnPage: -1, pages: [][]memory.SemanticFact{
		{liveFact("A", "owns", "B", "A owns B", "ev-1")},
		{liveFact("C", "owns", "D", "C owns D", "ev-2")},
	}}

	report, err := Rebuild(ctx, &fakeDropper{}, scanner, stage, "t1", nil)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if report.FactsReplayed != 2 {
		t.Fatalf("FactsReplayed = %d, want 2", report.FactsReplayed)
	}
	count, err := store.CountEntities(ctx, "t1")
	if err != nil {
		t.Fatalf("CountEntities: %v", err)
	}
	if count != 4 {
		t.Fatalf("entity count = %d, want 4 (A,B,C,D)", count)
	}
	aCands, _ := backend.CandidateEntities(ctx, "t1", "A")
	edges, _ := backend.Neighbors(ctx, "t1", aCands[0].ID)
	if len(edges) != 1 || edges[0].Predicate != "owns" {
		t.Fatalf("A's edges = %v, want exactly one 'owns' edge", edges)
	}
}

// --- DW-3.2: a superseded fact never gets an edge ------------------------

// TestRebuild_SupersededFactNeverGetsAnEdge mirrors the real ScanLiveFacts
// contract: only LIVE facts are ever returned by a scanner, so a
// superseded predecessor is never in the fixture at all — a rebuild has no
// way to know it ever existed, which is exactly the point. This asserts
// the graph left behind carries an edge for the live successor and NO
// edge under the predecessor's triple.
func TestRebuild_SupersededFactNeverGetsAnEdge(t *testing.T) {
	ctx := context.Background()
	stage, _, backend := newNameKeyedTestStage(t)

	// Only the CURRENT truth is ever handed to Rebuild — "service-a owns
	// billing-db" (the superseded predecessor) is deliberately absent, as a
	// real ScanLiveFacts call would never return it.
	scanner := newFakeScanner(nil, liveFact("service-a", "owns", "billing-db-v2", "service-a owns billing-db-v2", "ev-2"))

	if _, err := Rebuild(ctx, &fakeDropper{}, scanner, stage, "t1", nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	cands, err := backend.CandidateEntities(ctx, "t1", "service-a")
	if err != nil || len(cands) != 1 {
		t.Fatalf("expected exactly one service-a entity: %v (err=%v)", cands, err)
	}
	edges, err := backend.Neighbors(ctx, "t1", cands[0].ID)
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("edges from service-a = %v, want exactly 1 (the live successor)", edges)
	}
	if !edges[0].Live() {
		t.Fatalf("the one edge that exists must be live: %+v", edges[0])
	}
	for _, e := range edges {
		if e.ToEntityID == "" {
			continue
		}
		to, ok, _ := backend.GetEntity(ctx, "t1", e.ToEntityID)
		if ok && to.Name == "billing-db" {
			t.Fatalf("found an edge to the superseded object %q — it should never have been written", to.Name)
		}
	}
}

// --- DW-3.5: idempotent across two independent runs ----------------------

// TestRebuild_IdempotentAcrossTwoRuns drives Rebuild twice against two
// INDEPENDENT, initially-empty backends fed the identical fact fixture in
// the identical order — each backend stands in for what a real drop +
// recreate leaves behind (empty graph indices) before that run's replay.
// Deterministic entity/edge fingerprinting (graph.go's newEntityID /
// edgeFingerprint) plus deterministic dedup (RuleJudge + FakeEmbedder +
// WithNameKeyedDedup) means two independent runs over the same input must
// converge to the same graph.
func TestRebuild_IdempotentAcrossTwoRuns(t *testing.T) {
	ctx := context.Background()
	facts := []memory.SemanticFact{
		liveFact("A", "owns", "B", "A owns B", "ev-1"),
		liveFact("B", "located_in", "C", "B located_in C", "ev-2"),
	}

	run := func() (*MemBackend, RebuildReport) {
		stage, _, backend := newNameKeyedTestStage(t)
		scanner := newFakeScanner(nil, facts...)
		report, err := Rebuild(ctx, &fakeDropper{}, scanner, stage, "t1", nil)
		if err != nil {
			t.Fatalf("Rebuild: %v", err)
		}
		return backend, report
	}

	b1, r1 := run()
	b2, r2 := run()

	if r1.FactsReplayed != r2.FactsReplayed {
		t.Fatalf("FactsReplayed differ: run1=%d run2=%d", r1.FactsReplayed, r2.FactsReplayed)
	}
	if got, want := edgeSignatures(ctx, t, b1), edgeSignatures(ctx, t, b2); !slices.Equal(got, want) {
		t.Fatalf("edge sets differ across two independent runs:\n run1=%v\n run2=%v", got, want)
	}
}

// edgeSignatures returns a sorted, backend-identity-independent summary of
// every live edge: (subjectName, predicate, objectName). Comparing THIS
// rather than raw entity/edge IDs is deliberate — entity IDs are salted by
// first-seen SourceID, and both runs use the same SourceIDs here, but
// asserting on names+predicate is the more meaningful "same graph" check
// and stays valid even if that salting ever changes.
func edgeSignatures(ctx context.Context, t *testing.T, b *MemBackend) []string {
	t.Helper()
	var sigs []string
	b.mu.Lock()
	edges := make([]Edge, 0, len(b.edges))
	for _, e := range b.edges {
		edges = append(edges, e)
	}
	b.mu.Unlock()
	for _, e := range edges {
		from, _, err := b.GetEntity(ctx, e.TenantID, e.FromEntityID)
		if err != nil {
			t.Fatalf("GetEntity(from): %v", err)
		}
		to, _, err := b.GetEntity(ctx, e.TenantID, e.ToEntityID)
		if err != nil {
			t.Fatalf("GetEntity(to): %v", err)
		}
		sigs = append(sigs, from.Name+" -"+e.Predicate+"-> "+to.Name)
	}
	sort.Strings(sigs)
	return sigs
}

// --- dirty paths ----------------------------------------------------------

func TestRebuild_DropperErrorPropagatesAndSkipsScan(t *testing.T) {
	ctx := context.Background()
	stage, _, _ := newNameKeyedTestStage(t)
	scanner := newFakeScanner(nil, liveFact("A", "owns", "B", "A owns B", "ev-1"))
	dropper := &fakeDropper{err: errors.New("boom")}

	_, err := Rebuild(ctx, dropper, scanner, stage, "t1", nil)
	if err == nil {
		t.Fatal("Rebuild with a failing dropper = nil error, want an error")
	}
	if scanner.calls != 0 {
		t.Fatalf("scanner called %d times after a dropper failure, want 0", scanner.calls)
	}
}

func TestRebuild_ScannerErrorPropagatesWithPartialProgress(t *testing.T) {
	ctx := context.Background()
	stage, _, _ := newNameKeyedTestStage(t)
	scanner := &fakePagedScanner{errOnPage: 1, pages: [][]memory.SemanticFact{
		{liveFact("A", "owns", "B", "A owns B", "ev-1")},
		{liveFact("C", "owns", "D", "C owns D", "ev-2")},
	}}

	report, err := Rebuild(ctx, &fakeDropper{}, scanner, stage, "t1", nil)
	if err == nil {
		t.Fatal("Rebuild with a failing page-2 scan = nil error, want an error")
	}
	if report.FactsReplayed != 1 {
		t.Fatalf("FactsReplayed on partial failure = %d, want 1 (page 1 landed before the error)", report.FactsReplayed)
	}
}

func TestRebuild_RequiresTenantID(t *testing.T) {
	ctx := context.Background()
	stage, _, _ := newNameKeyedTestStage(t)
	dropper := &fakeDropper{}
	scanner := newFakeScanner(nil)

	if _, err := Rebuild(ctx, dropper, scanner, stage, "", nil); err == nil {
		t.Fatal("Rebuild with an empty tenant id = nil error, want an error")
	}
	if dropper.calls != 0 {
		t.Fatalf("dropper called %d times for an invalid tenant id, want 0 (fail before any side effect)", dropper.calls)
	}
}

// --- edge cases -------------------------------------------------------

func TestRebuild_EmptyStoreNoFacts(t *testing.T) {
	ctx := context.Background()
	stage, store, _ := newNameKeyedTestStage(t)
	dropper := &fakeDropper{}
	scanner := newFakeScanner(nil) // zero facts

	report, err := Rebuild(ctx, dropper, scanner, stage, "t1", nil)
	if err != nil {
		t.Fatalf("Rebuild against an empty store: %v", err)
	}
	if report.FactsReplayed != 0 {
		t.Fatalf("FactsReplayed = %d, want 0", report.FactsReplayed)
	}
	if dropper.calls != 1 {
		t.Fatalf("dropper called %d times, want 1 (drop still runs even with nothing to replay)", dropper.calls)
	}
	count, _ := store.CountEntities(ctx, "t1")
	if count != 0 {
		t.Fatalf("entity count = %d, want 0", count)
	}
}

// TestRebuild_TenantWithOnlyRetractions: every live fact is a retraction
// (empty Object, per ingest.ParseExtraction's convention) — Stage.Process
// still derives the subject mention but writes no edge, and Rebuild must
// not treat "no edges written" as a failure.
func TestRebuild_TenantWithOnlyRetractions(t *testing.T) {
	ctx := context.Background()
	stage, store, backend := newNameKeyedTestStage(t)
	scanner := newFakeScanner(nil, liveFact("service-a", "owns", "", "service-a owns: retracted", "ev-1"))

	report, err := Rebuild(ctx, &fakeDropper{}, scanner, stage, "t1", nil)
	if err != nil {
		t.Fatalf("Rebuild against all-retraction facts: %v", err)
	}
	if report.FactsReplayed != 1 {
		t.Fatalf("FactsReplayed = %d, want 1", report.FactsReplayed)
	}
	count, _ := store.CountEntities(ctx, "t1")
	if count != 1 {
		t.Fatalf("entity count = %d, want 1 (subject only)", count)
	}
	cands, _ := backend.CandidateEntities(ctx, "t1", "service-a")
	edges, _ := backend.Neighbors(ctx, "t1", cands[0].ID)
	if len(edges) != 0 {
		t.Fatalf("edges = %v, want none for a retraction-only tenant", edges)
	}
}

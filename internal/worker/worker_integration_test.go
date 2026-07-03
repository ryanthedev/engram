//go:build integration

package worker_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/ingest"
	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/store"
	"github.com/ryanthedev/engram/internal/testutil"
	"github.com/ryanthedev/engram/internal/worker"
)

// liveWorker wires a Worker + Sweeper over the real pinned cluster with
// scratch indices unique to name.
func liveWorker(t *testing.T, name string) (*worker.Worker, *worker.Sweeper, *store.OpenSearchStore, string, string, string) {
	t.Helper()
	base := testutil.OpenSearchURL()
	if _, err := store.Apply(context.Background(), testutil.HTTPClient, base); err != nil {
		t.Fatalf("applying cluster contract: %v", err)
	}
	epIdx := testutil.ScratchIndexName("episodic", name)
	semIdx := testutil.ScratchIndexName("semantic", name)
	ledIdx := testutil.ScratchIndexName("ledger", name)
	for _, idx := range []string{epIdx, semIdx, ledIdx} {
		testutil.DeleteIndex(t, base, idx)
		t.Cleanup(func(idx string) func() { return func() { testutil.DeleteIndex(t, base, idx) } }(idx))
		testutil.CreateScratchIndex(t, base, idx)
	}
	s := store.NewOpenSearchStore(testutil.HTTPClient, base,
		store.WithEpisodicIndex(epIdx), store.WithSemanticIndex(semIdx),
		store.WithLedgerIndex(ledIdx), store.WithLedgerLease(300*time.Millisecond))
	w := worker.New(s, &ingest.RuleExtractor{}, ingest.RuleReconciler{}, nil,
		worker.Config{ExtractorVersion: "v1", ClaimLease: time.Second}, quietLogger)
	sw := &worker.Sweeper{Store: s, Worker: w, Logger: quietLogger}
	return w, sw, s, base, epIdx, semIdx
}

// appendAndRefresh appends ev and refreshes the episodic index — the tests
// below drive ProcessEvent directly, but production reaches events through
// search (ClaimBatch / FindByEventID), which implies search visibility;
// refreshing keeps the direct-drive honest to that precondition.
func appendAndRefresh(t *testing.T, s *store.OpenSearchStore, base, epIdx string, evs ...memory.Episodic) {
	t.Helper()
	for _, ev := range evs {
		if _, err := s.Append(context.Background(), ev); err != nil {
			t.Fatalf("Append %s: %v", ev.EventID, err)
		}
	}
	testutil.RefreshIndex(t, base, epIdx)
}

// liveHeads returns the live facts for subject+predicate from the real
// index (refresh first).
func liveHeads(t *testing.T, s *store.OpenSearchStore, base, semIdx, tenant, subject, predicate string) []store.VersionedFact {
	t.Helper()
	testutil.RefreshIndex(t, base, semIdx)
	probe := memory.SemanticFact{
		TenantID: tenant, Subject: subject, Predicate: predicate,
		ContentKey: memory.ContentKey(tenant, subject, predicate, "__probe__"),
	}
	cands, err := s.Candidates(context.Background(), probe, 50)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	var live []store.VersionedFact
	for _, c := range cands {
		if c.Fact.Subject == subject && c.Fact.Predicate == predicate && c.Fact.InvalidAt == nil && c.Fact.ExpiredAt == nil {
			live = append(live, c)
		}
	}
	return live
}

// TestDW_2_2_LiveExtractReconcileUpdate closes the loop on the real
// cluster: extract → top-k candidate retrieval → UPDATE writes the new fact
// and closes the prior bi-temporally (DW-2.2 + DW-2.3 live).
func TestDW_2_2_LiveExtractReconcileUpdate(t *testing.T) {
	w, _, s, base, epIdx, semIdx := liveWorker(t, t.Name())
	ctx := context.Background()

	evA := event("ev-live-a", factLine("svc", "owner", "bob", tv0))
	appendAndRefresh(t, s, base, epIdx, evA)
	if err := w.ProcessEvent(ctx, evA); err != nil {
		t.Fatalf("ProcessEvent A: %v", err)
	}
	heads := liveHeads(t, s, base, semIdx, "t1", "svc", "owner")
	if len(heads) != 1 || heads[0].Fact.Object != "bob" {
		t.Fatalf("seed heads = %+v, want one bob", heads)
	}
	predID := heads[0].ID

	evB := event("ev-live-b", factLine("svc", "owner", "ana", tv1))
	appendAndRefresh(t, s, base, epIdx, evB)
	if err := w.ProcessEvent(ctx, evB); err != nil {
		t.Fatalf("ProcessEvent B: %v", err)
	}

	heads = liveHeads(t, s, base, semIdx, "t1", "svc", "owner")
	if len(heads) != 1 || heads[0].Fact.Object != "ana" || heads[0].Fact.Supersedes != predID {
		t.Fatalf("heads after update = %+v, want one ana superseding %s", heads, predID)
	}
	pred, ok, err := s.GetFact(ctx, predID)
	if err != nil || !ok {
		t.Fatalf("GetFact(pred): ok=%v err=%v", ok, err)
	}
	if pred.Fact.InvalidAt == nil || !pred.Fact.InvalidAt.Equal(tv1) || pred.Fact.InvalidatedTxAt == nil {
		t.Fatalf("predecessor = %+v, want invalid_at=%v with invalidated_tx_at", pred.Fact, tv1)
	}
}

// TestDW_2_5_LiveParallelWorkersConverge is the live concurrency test:
// parallel workers race duplicate ADDs and divergent UPDATEs against the
// real cluster (real 409s, real guarded closes, real refresh lag), then the
// repair sweep converges everything to a single live head with no duplicate
// live content keys.
func TestDW_2_5_LiveParallelWorkersConverge(t *testing.T) {
	w, sw, s, base, epIdx, semIdx := liveWorker(t, t.Name())
	ctx := context.Background()

	seed := event("ev-live-seed", factLine("svc", "owner", "bob", tv0))
	appendAndRefresh(t, s, base, epIdx, seed)
	if err := w.ProcessEvent(ctx, seed); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	testutil.RefreshIndex(t, base, semIdx)

	var events []memory.Episodic
	for i := 0; i < 3; i++ { // duplicate ADDs: identical fact ⇒ identical doc id
		events = append(events, event(fmt.Sprintf("ev-live-dup-%d", i), factLine("db", "tier", "gold", tv1)))
	}
	for i := 0; i < 3; i++ { // divergent UPDATEs of the seeded head
		events = append(events, event(fmt.Sprintf("ev-live-upd-%d", i), factLine("svc", "owner", fmt.Sprintf("eng-%d", i), tv1.Add(time.Duration(i)*time.Hour))))
	}
	appendAndRefresh(t, s, base, epIdx, events...)

	var wg sync.WaitGroup
	workers := make([]*worker.Worker, len(events))
	for i := range events {
		workers[i] = worker.New(s, &ingest.RuleExtractor{}, ingest.RuleReconciler{}, nil,
			worker.Config{ExtractorVersion: "v1"}, quietLogger)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for attempt := 0; attempt < 5; attempt++ {
				if err := workers[i].ProcessEvent(ctx, events[i]); err == nil {
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
			t.Errorf("event %s never settled", events[i].EventID)
		}(i)
	}
	wg.Wait()

	// Converge: the sweep needs refreshed scans; a few passes bound the
	// documented transient window.
	for i := 0; i < 3; i++ {
		testutil.RefreshIndex(t, base, semIdx)
		if _, err := sw.Sweep(ctx); err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
	}
	testutil.RefreshIndex(t, base, semIdx)

	if heads := liveHeads(t, s, base, semIdx, "t1", "svc", "owner"); len(heads) != 1 {
		t.Fatalf("svc/owner live heads after sweep = %d (%+v), want exactly 1", len(heads), heads)
	}
	if heads := liveHeads(t, s, base, semIdx, "t1", "db", "tier"); len(heads) != 1 {
		t.Fatalf("db/tier live heads = %d (%+v), want exactly 1 (duplicate ADDs deduped)", len(heads), heads)
	}
	keys, err := s.DuplicateLiveContentKeys(ctx, 10)
	if err != nil {
		t.Fatalf("DuplicateLiveContentKeys: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("duplicate live content keys after sweep = %v, want none", keys)
	}
	// Note on the 409 path: whether this race produces real 409s depends on
	// sub-millisecond interleaving (the realtime candidate pre-read usually
	// converts the loser to a NOOP before it ever creates), so no count is
	// asserted here. The op_type=create 409 → re-reconcile path is proven
	// deterministically by TestDW_2_5_DuplicateAddConflictReReconciles
	// (worker level, injected race) and live against this cluster by
	// TestOpenSearchStoreLiveAppendCreateUpdate (store level).
}

// TestDW_2_8_LiveSweepCompletesCrashedClose reproduces the crash-between-
// create-and-close state directly on the real cluster (successor indexed
// with its durable supersedes link, predecessor still open) and proves one
// sweep pass completes the close — exactly one live head.
func TestDW_2_8_LiveSweepCompletesCrashedClose(t *testing.T) {
	_, sw, s, base, _, semIdx := liveWorker(t, t.Name())
	ctx := context.Background()

	pred := memory.SemanticFact{
		Subject: "svc", Predicate: "owner", Object: "bob", Statement: "svc owner bob",
		TenantID: "t1", ContentKey: memory.ContentKey("t1", "svc", "owner", "bob"),
		ValidAt: tv0, CreatedAt: time.Now().UTC(),
	}
	predID := memory.FactDocID(pred.ContentKey, pred.ValidAt)
	succ := memory.SemanticFact{
		Subject: "svc", Predicate: "owner", Object: "ana", Statement: "svc owner ana",
		TenantID: "t1", ContentKey: memory.ContentKey("t1", "svc", "owner", "ana"),
		Supersedes: predID, ValidAt: tv1, CreatedAt: time.Now().UTC(),
	}
	succID := memory.FactDocID(succ.ContentKey, succ.ValidAt)
	if err := s.Create(ctx, predID, pred); err != nil {
		t.Fatalf("Create pred: %v", err)
	}
	if err := s.Create(ctx, succID, succ); err != nil {
		t.Fatalf("Create succ: %v", err)
	}
	testutil.RefreshIndex(t, base, semIdx)

	rep, err := sw.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rep.ClosesCompleted != 1 {
		t.Fatalf("sweep report = %+v, want exactly one completed close", rep)
	}
	closed, ok, err := s.GetFact(ctx, predID)
	if err != nil || !ok {
		t.Fatalf("GetFact(pred): ok=%v err=%v", ok, err)
	}
	if closed.Fact.InvalidAt == nil || !closed.Fact.InvalidAt.Equal(tv1) || closed.Fact.InvalidatedTxAt == nil {
		t.Fatalf("predecessor after sweep = %+v, want closed at %v with audit stamp", closed.Fact, tv1)
	}
	if heads := liveHeads(t, s, base, semIdx, "t1", "svc", "owner"); len(heads) != 1 || heads[0].ID != succID {
		t.Fatalf("live heads after sweep = %+v, want only the successor", heads)
	}
}

// TestDW_2_8_LiveLateArrivalMultiHopChain is the live-cluster regression for
// the review's Issue 1: on a chain of two closed versions plus a live head,
// a late arrival must be bounded by its true valid-time successor (a CLOSED
// record the live-only candidate read cannot see) and must trim its
// containing closed neighbor — pairwise-disjoint intervals across the whole
// chain, live head untouched.
func TestDW_2_8_LiveLateArrivalMultiHopChain(t *testing.T) {
	w, _, s, base, epIdx, semIdx := liveWorker(t, t.Name())
	ctx := context.Background()
	tvMid := tv0.Add(12 * time.Hour)

	chain := []memory.Episodic{
		event("ev-hop-1", factLine("svc", "owner", "bob", tv0)),
		event("ev-hop-2", factLine("svc", "owner", "ana", tv1)),
		event("ev-hop-3", factLine("svc", "owner", "cid", tv2)),
	}
	for _, ev := range chain {
		appendAndRefresh(t, s, base, epIdx, ev)
		if err := w.ProcessEvent(ctx, ev); err != nil {
			t.Fatalf("ProcessEvent %s: %v", ev.EventID, err)
		}
		testutil.RefreshIndex(t, base, semIdx)
	}

	late := event("ev-hop-late", factLine("svc", "owner", "eve", tvMid))
	appendAndRefresh(t, s, base, epIdx, late)
	if err := w.ProcessEvent(ctx, late); err != nil {
		t.Fatalf("ProcessEvent late arrival: %v", err)
	}
	testutil.RefreshIndex(t, base, semIdx)

	// Fetch every version of the chain by content-addressed doc id.
	docID := func(object string, validAt time.Time) string {
		return memory.FactDocID(memory.ContentKey("t1", "svc", "owner", object), validAt)
	}
	versions := map[string]memory.SemanticFact{}
	for object, at := range map[string]time.Time{"bob": tv0, "ana": tv1, "cid": tv2, "eve": tvMid} {
		vf, ok, err := s.GetFact(ctx, docID(object, at))
		if err != nil || !ok {
			t.Fatalf("GetFact(%s): ok=%v err=%v", object, ok, err)
		}
		versions[object] = vf.Fact
	}

	if eve := versions["eve"]; eve.InvalidAt == nil || !eve.InvalidAt.Equal(tv1) || eve.Supersedes != "" {
		t.Fatalf("eve = [%v, %v) supersedes=%q, want [%v, %v) with no supersedes link", eve.ValidAt, eve.InvalidAt, eve.Supersedes, tvMid, tv1)
	}
	if bob := versions["bob"]; bob.InvalidAt == nil || !bob.InvalidAt.Equal(tvMid) || bob.InvalidatedTxAt == nil {
		t.Fatalf("bob = [%v, %v), want trimmed to [%v, %v) with an audit stamp", bob.ValidAt, bob.InvalidAt, tv0, tvMid)
	}
	if ana := versions["ana"]; ana.InvalidAt == nil || !ana.InvalidAt.Equal(tv2) {
		t.Fatalf("ana = [%v, %v), must stay [%v, %v)", ana.ValidAt, ana.InvalidAt, tv1, tv2)
	}
	if cid := versions["cid"]; cid.InvalidAt != nil {
		t.Fatal("live head cid must stay untouched")
	}

	// Pairwise disjointness across the whole chain (open head = +inf).
	type span struct {
		name       string
		start, end time.Time
		open       bool
	}
	var spans []span
	for name, fc := range versions {
		sp := span{name: name, start: fc.ValidAt, open: fc.InvalidAt == nil}
		if !sp.open {
			sp.end = *fc.InvalidAt
			if sp.end.Before(sp.start) {
				t.Errorf("inverted interval on %s: [%v, %v)", name, sp.start, sp.end)
			}
		}
		spans = append(spans, sp)
	}
	for i := 0; i < len(spans); i++ {
		for j := i + 1; j < len(spans); j++ {
			a, b := spans[i], spans[j]
			aBeforeB := !a.open && !a.end.After(b.start)
			bBeforeA := !b.open && !b.end.After(a.start)
			if !aBeforeB && !bBeforeA {
				t.Errorf("overlapping intervals: %s [%v, %v open=%v) and %s [%v, %v open=%v)",
					a.name, a.start, a.end, a.open, b.name, b.start, b.end, b.open)
			}
		}
	}

	if heads := liveHeads(t, s, base, semIdx, "t1", "svc", "owner"); len(heads) != 1 || heads[0].Fact.Object != "cid" {
		t.Fatalf("live heads = %+v, want only cid", heads)
	}
}

// TestDW_2_8_LiveNeighborAwareCloseBound proves the Issue-2 fix on the real
// cluster: the crash-window + late-arrival state (predecessor ana live, its
// successor cid live via supersedes, and a historical record eve already
// bounded inside the window), then one sweep pass — the completing close
// must land at eve's valid_at (neighbor-aware min), not cid's, leaving the
// whole chain pairwise disjoint.
func TestDW_2_8_LiveNeighborAwareCloseBound(t *testing.T) {
	_, sw, s, base, _, semIdx := liveWorker(t, t.Name())
	ctx := context.Background()
	tvMid := tv1.Add(6 * time.Hour)

	mk := func(object string, validAt time.Time, supersedes string, invalidAt *time.Time) string {
		fact := memory.SemanticFact{
			Subject: "svc", Predicate: "owner", Object: object, Statement: "svc owner " + object,
			TenantID: "t1", ContentKey: memory.ContentKey("t1", "svc", "owner", object),
			Supersedes: supersedes, ValidAt: validAt, InvalidAt: invalidAt, CreatedAt: time.Now().UTC(),
		}
		id := memory.FactDocID(fact.ContentKey, fact.ValidAt)
		if err := s.Create(ctx, id, fact); err != nil {
			t.Fatalf("Create %s: %v", object, err)
		}
		return id
	}
	anaID := mk("ana", tv1, "", nil)               // predecessor, still open (crashed close)
	mk("cid", tv2, anaID, nil)                     // successor, live, durable supersedes link
	inv := tv2
	mk("eve", tvMid, "", &inv)                     // late arrival already bounded inside the window
	testutil.RefreshIndex(t, base, semIdx)

	rep, err := sw.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rep.ClosesCompleted != 1 {
		t.Fatalf("sweep report = %+v, want exactly one completed close", rep)
	}
	ana, ok, err := s.GetFact(ctx, anaID)
	if err != nil || !ok {
		t.Fatalf("GetFact(ana): ok=%v err=%v", ok, err)
	}
	if ana.Fact.InvalidAt == nil || !ana.Fact.InvalidAt.Equal(tvMid) {
		t.Fatalf("ana interval = [%v, %v), want the neighbor-aware [%v, %v) — not cid's %v", ana.Fact.ValidAt, ana.Fact.InvalidAt, tv1, tvMid, tv2)
	}
	if heads := liveHeads(t, s, base, semIdx, "t1", "svc", "owner"); len(heads) != 1 || heads[0].Fact.Object != "cid" {
		t.Fatalf("live heads after sweep = %+v, want only cid", heads)
	}
}

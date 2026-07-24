//go:build integration

package server_test

// End-to-end integration for MemoryPurge through the REAL stack —
// engramclient -> gRPC with the auth interceptor -> server barricade ->
// OpenSearchStore -> live cluster. It is the proof that the migration safety
// net actually works on real data, covering:
//
//   - role denial (a token without memory-admin cannot purge);
//   - cross-tenant isolation (a purge cannot reach another tenant's rows even
//     when both tenants share an event id);
//   - dry run returns real counts and mutates nothing;
//   - REPLAY DUPLICATES — two episodic docs sharing one event_id — are BOTH
//     removed, which is the specific hazard this feature exists for;
//   - a tombstoned fact is absent from Search but still reachable via Audit;
//   - the ledger row is gone, so a re-ingest re-extracts cleanly.
//
// Scratch indices named after the test keep parallel runs on the shared dev
// cluster from colliding; everything created is cleaned up.

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/authgrpc"
	"github.com/ryanthedev/engram/internal/engramclient"
	"github.com/ryanthedev/engram/internal/mcp"
	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/retrieval"
	"github.com/ryanthedev/engram/internal/server"
	"github.com/ryanthedev/engram/internal/store"
	"github.com/ryanthedev/engram/internal/testutil"
)

// purgeStack is the live wired stack plus one client per role and direct
// access to the store and index names for out-of-band assertions.
type purgeStack struct {
	st                     *store.OpenSearchStore
	base                   string
	epIdx, semIdx, ledIdx  string
	admin, reader, tenantB *engramclient.Client
}

// startPurgeStack wires a real OpenSearchStore + retriever behind an
// authenticated gRPC server over scratch indices, and dials three clients: a
// memory-admin in tenant A, a role-less caller in tenant A, and a
// memory-admin in tenant B (the cross-tenant probe).
//
// The retriever runs in BM25-only mode: purge has nothing to do with vectors,
// and requiring a live embedder would couple this test to a service it does
// not exercise.
func startPurgeStack(t *testing.T) *purgeStack {
	t.Helper()
	base := testutil.OpenSearchURL()
	if _, err := store.Apply(context.Background(), testutil.HTTPClient, base); err != nil {
		t.Fatalf("applying cluster contract: %v", err)
	}
	epIdx := testutil.ScratchIndexName("episodic", t.Name())
	semIdx := testutil.ScratchIndexName("semantic", t.Name())
	ledIdx := testutil.ScratchIndexName("ledger", t.Name())
	for _, idx := range []string{epIdx, semIdx, ledIdx} {
		testutil.DeleteIndex(t, base, idx)
		t.Cleanup(func(idx string) func() { return func() { testutil.DeleteIndex(t, base, idx) } }(idx))
		testutil.CreateScratchIndex(t, base, idx)
	}
	st := store.NewOpenSearchStore(testutil.HTTPClient, base,
		store.WithEpisodicIndex(epIdx), store.WithSemanticIndex(semIdx), store.WithLedgerIndex(ledIdx))
	retriever := retrieval.NewOpenSearchRetriever(testutil.HTTPClient, base, nil,
		retrieval.WithIndices(epIdx, semIdx), retrieval.WithMode(retrieval.ModeBM25Only))

	svc := server.New(st, retriever)
	svc.Auditor = st
	svc.Purger = st

	identity := func(tenant string, roles ...string) auth.Identity {
		return auth.Identity{TenantID: tenant, UserID: "u", AgentID: "a", Roles: roles}
	}
	verifier := tokenVerifier{
		"admin-a-tok":  identity("t-purge-a", server.RoleMemoryAdmin),
		"reader-a-tok": identity("t-purge-a"),
		"admin-b-tok":  identity("t-purge-b", server.RoleMemoryAdmin),
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcServer := grpc.NewServer(grpc.ChainUnaryInterceptor(
		authgrpc.UnaryServerInterceptor(verifier, slog.Default()),
	))
	engrampb.RegisterEngramServer(grpcServer, svc)
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)

	dial := func(token string) *engramclient.Client {
		client, err := engramclient.Dial(lis.Addr().String(), token)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = client.Close() })
		return client
	}
	return &purgeStack{
		st: st, base: base, epIdx: epIdx, semIdx: semIdx, ledIdx: ledIdx,
		admin: dial("admin-a-tok"), reader: dial("reader-a-tok"), tenantB: dial("admin-b-tok"),
	}
}

// seedPurgeEvent appends n episodic docs under the same event id (n>1 is the
// replay duplicate a retried Ingest produces), claims a completed ledger row,
// and writes one live semantic fact sourced from the event. It returns the
// fact's doc id so a test can audit it after the tombstone lands.
func seedPurgeEvent(t *testing.T, s *store.OpenSearchStore, tenantID, eventID, subject string, n int) string {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if _, err := s.Append(ctx, memory.Episodic{
			EventID: eventID, TenantID: tenantID, Scope: "private", Text: subject + " note " + eventID,
			OccurredAt: time.Now().UTC(), CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("appending %s copy %d: %v", eventID, i, err)
		}
	}
	key := memory.LedgerKey{TenantID: tenantID, EventID: eventID, ExtractorVersion: "it-v1"}
	if _, err := s.ClaimLedger(ctx, key); err != nil {
		t.Fatalf("claiming ledger for %s: %v", eventID, err)
	}
	if err := s.UpdateLedger(ctx, key, store.LedgerState{Phase: store.LedgerComplete}); err != nil {
		t.Fatalf("completing ledger for %s: %v", eventID, err)
	}

	validAt := time.Now().UTC().Truncate(time.Second)
	ck := memory.ContentKey(tenantID, subject, "prefers", "dark-mode")
	factID := memory.FactDocID(ck, validAt)
	if err := s.Create(ctx, factID, memory.SemanticFact{
		Subject: subject, Predicate: "prefers", Object: "dark-mode",
		Statement: subject + " prefers dark-mode", ContentKey: ck, ExtractorVersion: "it-v1",
		TenantID: tenantID, Scope: "private", SourceIDs: []string{eventID},
		ValidAt: validAt, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seeding fact for %s: %v", eventID, err)
	}
	return factID
}

// refreshAll makes every seeded write visible to search/by-query.
func (s *purgeStack) refreshAll(t *testing.T) {
	t.Helper()
	for _, idx := range []string{s.epIdx, s.semIdx, s.ledIdx} {
		testutil.RefreshIndex(t, s.base, idx)
	}
}

// TestPurgeEndToEndRemovesReplaysAndTombstonesFacts is the main lifecycle:
// dry run first (nothing changes), then the confirmed purge, then the
// verification that search lost the fact while audit kept it and the ledger
// row is gone.
func TestPurgeEndToEndRemovesReplaysAndTombstonesFacts(t *testing.T) {
	ctx := context.Background()
	stack := startPurgeStack(t)

	// Two appends of ONE event id: the replay duplicate event_id does not
	// deduplicate. Plus a neighbour event that must survive untouched.
	purgedFact := seedPurgeEvent(t, stack.st, "t-purge-a", "ev-bad", "alice", 2)
	keptFact := seedPurgeEvent(t, stack.st, "t-purge-a", "ev-good", "bob", 1)
	stack.refreshAll(t)

	// Both facts are live and searchable up front.
	if n := searchHitCount(t, stack.admin, "dark-mode"); n != 2 {
		t.Fatalf("pre-purge search hits = %d, want 2", n)
	}

	// --- dry run: real counts, zero mutation ---------------------------
	dry, err := stack.admin.MemoryPurge(ctx, []string{"ev-bad"}, true)
	if err != nil {
		t.Fatalf("MemoryPurge(dry run): %v", err)
	}
	if !dry.DryRun {
		t.Errorf("dry-run response DryRun = false, want true")
	}
	if dry.Episodic != 2 || dry.Ledger != 1 || dry.Semantic != 1 {
		t.Errorf("dry-run counts = %+v, want episodic=2 ledger=1 semantic=1", dry)
	}
	stack.refreshAll(t)
	if n := searchHitCount(t, stack.admin, "dark-mode"); n != 2 {
		t.Errorf("post-dry-run search hits = %d, want 2 (a dry run must mutate nothing)", n)
	}

	// --- confirmed purge -----------------------------------------------
	got, err := stack.admin.MemoryPurge(ctx, []string{"ev-bad"}, false)
	if err != nil {
		t.Fatalf("MemoryPurge(confirm): %v", err)
	}
	if got.DryRun {
		t.Errorf("confirmed response DryRun = true, want false")
	}
	if got.Episodic != 2 || got.Ledger != 1 || got.Semantic != 1 {
		t.Errorf("purge counts = %+v, want the dry run's episodic=2 ledger=1 semantic=1", got)
	}
	stack.refreshAll(t)

	// BOTH replay duplicates are gone; the neighbour event is intact.
	remaining, err := stack.st.FindByEventID(ctx, "ev-bad")
	if err != nil {
		t.Fatalf("FindByEventID(ev-bad): %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("episodic docs for ev-bad after purge = %d, want 0 (both replay duplicates must go)", len(remaining))
	}
	if kept, err := stack.st.FindByEventID(ctx, "ev-good"); err != nil || len(kept) != 1 {
		t.Errorf("neighbour event docs = %d (err=%v), want 1", len(kept), err)
	}

	// The tombstoned fact is absent from Search...
	if n := searchHitCount(t, stack.admin, "dark-mode"); n != 1 {
		t.Errorf("post-purge search hits = %d, want 1 (only bob's fact stays live)", n)
	}
	// ...but still reachable through Audit, with its expired_at recorded —
	// that asymmetry is the whole point of the soft delete.
	audit, err := stack.admin.Audit(ctx, purgedFact)
	if err != nil {
		t.Fatalf("Audit(purged fact): %v", err)
	}
	var sawTombstone bool
	for _, v := range audit.Versions {
		if v.ID == purgedFact && v.ExpiredAt != nil {
			sawTombstone = true
		}
	}
	if !sawTombstone {
		t.Errorf("audit of the purged fact shows no expired_at version: %+v", audit.Versions)
	}
	if _, err := stack.admin.Audit(ctx, keptFact); err != nil {
		t.Errorf("Audit(neighbour fact): %v", err)
	}

	// The ledger row is gone, so a re-ingest re-extracts instead of
	// short-circuiting on a LedgerComplete entry (D13).
	entry, err := stack.st.ClaimLedger(ctx, memory.LedgerKey{TenantID: "t-purge-a", EventID: "ev-bad", ExtractorVersion: "it-v1"})
	if err != nil {
		t.Fatalf("re-claiming the purged ledger key: %v", err)
	}
	if !entry.Claimed {
		t.Errorf("re-claim did not win: the ledger row survived, so a corrected re-ingest would silently no-op")
	}
}

// TestPurgeDeniesWithoutRoleAndAcrossTenants: the two authorization
// guarantees, on the live stack. A role-less token in the SAME tenant is
// denied outright; a memory-admin in ANOTHER tenant is allowed to call but
// reaches nothing, because the tenant comes from its own token and the
// request has no tenant field to lie in.
func TestPurgeDeniesWithoutRoleAndAcrossTenants(t *testing.T) {
	ctx := context.Background()
	stack := startPurgeStack(t)
	seedPurgeEvent(t, stack.st, "t-purge-a", "ev-shared", "alice", 1)
	stack.refreshAll(t)

	// Role denial: no memory-admin, no purge — not even a dry run.
	_, err := stack.reader.MemoryPurge(ctx, []string{"ev-shared"}, true)
	wantCode(t, err, codes.PermissionDenied, "purge without the memory-admin role")
	_, err = stack.reader.MemoryPurge(ctx, []string{"ev-shared"}, false)
	wantCode(t, err, codes.PermissionDenied, "confirmed purge without the memory-admin role")

	// Cross-tenant: tenant B holds memory-admin and names tenant A's exact
	// event id. The call succeeds and removes NOTHING.
	got, err := stack.tenantB.MemoryPurge(ctx, []string{"ev-shared"}, false)
	if err != nil {
		t.Fatalf("cross-tenant MemoryPurge: %v", err)
	}
	if got.Episodic != 0 || got.Ledger != 0 || got.Semantic != 0 {
		t.Fatalf("cross-tenant purge counts = %+v, want all zero", got)
	}
	stack.refreshAll(t)
	if docs, err := stack.st.FindByEventID(ctx, "ev-shared"); err != nil || len(docs) != 1 {
		t.Errorf("tenant A's event docs after a cross-tenant purge = %d (err=%v), want 1", len(docs), err)
	}

	// Argument validation is enforced for an authorized caller too.
	_, err = stack.admin.MemoryPurge(ctx, nil, true)
	wantCode(t, err, codes.InvalidArgument, "purge with no event ids")
	_, err = stack.admin.MemoryPurge(ctx, []string{""}, true)
	wantCode(t, err, codes.InvalidArgument, "purge with a blank event id")

	// Purging an id that was never ingested is success-with-zero, not
	// NOT_FOUND: batches must stay idempotently re-runnable.
	got, err = stack.admin.MemoryPurge(ctx, []string{"ev-never-existed"}, false)
	if err != nil {
		t.Fatalf("purging an unknown event: %v", err)
	}
	if got.Episodic != 0 || got.Ledger != 0 || got.Semantic != 0 {
		t.Errorf("unknown-event purge counts = %+v, want all zero", got)
	}
}

// searchHitCount runs one authenticated search and reports how many hits came
// back — the reader's-eye check that a tombstone really removed a fact from
// the live read path.
func searchHitCount(t *testing.T, c *engramclient.Client, query string) int {
	t.Helper()
	res, err := c.Search(context.Background(), query, 20, mcp.SearchFilter{})
	if err != nil {
		t.Fatalf("Search(%q): %v", query, err)
	}
	return len(res.Hits)
}

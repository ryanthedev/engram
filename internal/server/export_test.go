package server_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/acl"
	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/graph"
	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/server"
	"github.com/ryanthedev/engram/internal/store"
)

// aclFunc is a per-record ReadAuthorizer fake (audit_test's fakeReadAuthz is
// all-or-nothing; export tests need record-granular decisions and errors).
type aclFunc func(auth.Identity, acl.Record) (bool, error)

func (f aclFunc) CanRead(_ context.Context, id auth.Identity, r acl.Record) (bool, error) {
	return f(id, r)
}

// errExporter is a server.Exporter fake whose scans can be forced to error,
// exercising the handler's scan-error fail-closed paths that MemBackend (which
// never errors) cannot reach. A nil *Fn delegates to the embedded backend, so
// one tier can fail while the other behaves normally.
type errExporter struct {
	backend     *graph.MemBackend
	scanEntErr  error
	scanEdgeErr error
}

func (e errExporter) ScanEntities(ctx context.Context, tenantID string, c graph.Cursor) ([]graph.Entity, graph.Cursor, error) {
	if e.scanEntErr != nil {
		return nil, graph.Cursor{}, e.scanEntErr
	}
	return e.backend.ScanEntities(ctx, tenantID, c)
}

func (e errExporter) ScanEdges(ctx context.Context, tenantID string, c graph.Cursor) ([]graph.Edge, graph.Cursor, error) {
	if e.scanEdgeErr != nil {
		return nil, graph.Cursor{}, e.scanEdgeErr
	}
	return e.backend.ScanEdges(ctx, tenantID, c)
}

var _ server.Exporter = errExporter{}

// exportEntity seeds one live entity; ids are zero-padded so lexicographic
// scan order matches insertion order and multi-page walks are deterministic.
func exportEntity(tenant string, n int) graph.Entity {
	return graph.Entity{
		ID: fmt.Sprintf("%s-e%05d", tenant, n), TenantID: tenant,
		Scope: acl.ScopeTeam, TeamID: "teamX", OwnerAgentID: "a1",
		Name: fmt.Sprintf("Entity %d", n), MentionCount: 1,
		ValidAt: time.Unix(1000, 0).UTC(), CreatedAt: time.Unix(1000, 0).UTC(),
	}
}

func exportEdge(tenant string, n int) graph.Edge {
	return graph.Edge{
		ID: fmt.Sprintf("%s-g%05d", tenant, n), TenantID: tenant,
		Scope: acl.ScopeTeam, TeamID: "teamX", OwnerAgentID: "a1",
		FromEntityID: exportEntity(tenant, 0).ID, ToEntityID: exportEntity(tenant, 1).ID,
		Predicate: "knows", Statement: fmt.Sprintf("stmt %d", n),
		ValidAt: time.Unix(1000, 0).UTC(), CreatedAt: time.Unix(1000, 0).UTC(),
	}
}

// seedGraph puts nEntities+nEdges live records for tenant into b.
func seedGraph(t *testing.T, b *graph.MemBackend, tenant string, nEntities, nEdges int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < nEntities; i++ {
		if err := b.PutEntity(ctx, exportEntity(tenant, i)); err != nil {
			t.Fatalf("seed entity: %v", err)
		}
	}
	for i := 0; i < nEdges; i++ {
		if err := b.PutEdge(ctx, exportEdge(tenant, i)); err != nil {
			t.Fatalf("seed edge: %v", err)
		}
	}
}

// fakeEpisodicExporter is a slice-backed server.EpisodicExporter whose
// resume token is the next slice index; scanErr forces the scan-error path
// and a non-integer token reports the store's bad-cursor sentinel, exactly
// as *store.OpenSearchStore does for a forged token.
type fakeEpisodicExporter struct {
	recs     []memory.Episodic
	pageSize int
	scanErr  error
}

func (f fakeEpisodicExporter) ScanEpisodic(_ context.Context, _ string, after string) ([]memory.Episodic, string, error) {
	if f.scanErr != nil {
		return nil, "", f.scanErr
	}
	start := 0
	if after != "" {
		n, err := strconv.Atoi(after)
		if err != nil {
			return nil, "", fmt.Errorf("fake: %w", store.ErrBadCursor)
		}
		start = n
	}
	if start >= len(f.recs) {
		return nil, "", nil
	}
	end := min(start+f.pageSize, len(f.recs))
	next := ""
	if end < len(f.recs) {
		next = strconv.Itoa(end)
	}
	return f.recs[start:end], next, nil
}

var _ server.EpisodicExporter = fakeEpisodicExporter{}

// exportEpisodicRec seeds one processed episodic record for the fake.
func exportEpisodicRec(tenant string, n int) memory.Episodic {
	return memory.Episodic{
		EventID: fmt.Sprintf("%s-ev%05d", tenant, n), TenantID: tenant,
		Scope: acl.ScopeTeam, TeamID: "teamX", OwnerAgentID: "a1",
		Kind: "conversation", Text: fmt.Sprintf("prose %d", n),
		SourceIDs:  []string{"src-1"},
		OccurredAt: time.Unix(500, 0).UTC(), CreatedAt: time.Unix(1000, 0).UTC(),
	}
}

// walkExportAll pages Export to exhaustion (bounded iterations — a cursor
// that never empties is itself a failure) and returns every record seen plus
// the number of pages.
func walkExportAll(ctx context.Context, t *testing.T, svc *server.Server) (episodics []*engrampb.ExportEpisodic, entities []*engrampb.ExportEntity, edges []*engrampb.ExportEdge, pages int) {
	t.Helper()
	cursor := ""
	for i := 0; i < 20; i++ {
		resp, err := svc.Export(ctx, &engrampb.ExportRequest{Cursor: cursor})
		if err != nil {
			t.Fatalf("Export page %d: %v", i, err)
		}
		episodics = append(episodics, resp.GetEpisodics()...)
		entities = append(entities, resp.GetEntities()...)
		edges = append(edges, resp.GetEdges()...)
		pages++
		cursor = resp.GetNextCursor()
		if cursor == "" {
			return episodics, entities, edges, pages
		}
	}
	t.Fatal("Export never returned an empty next_cursor within 20 pages")
	return nil, nil, nil, 0
}

// walkExport is walkExportAll for the graph-only tests that predate the
// episodic stage.
func walkExport(ctx context.Context, t *testing.T, svc *server.Server) (entities []*engrampb.ExportEntity, edges []*engrampb.ExportEdge, pages int) {
	t.Helper()
	_, entities, edges, pages = walkExportAll(ctx, t, svc)
	return entities, edges, pages
}

// TestDW_2_2_ExportPagesToExhaustion: 501 live entities (one past the 500
// scan batch) + 3 edges page out completely — every record exactly once,
// next_cursor advancing between pages and empty on the final one, each page
// bounded by the scan batch size per tier.
func TestDW_2_2_ExportPagesToExhaustion(t *testing.T) {
	b := graph.NewMemBackend()
	seedGraph(t, b, "t1", 501, 3)
	// Live filtering must survive the handler: one expired entity and one
	// invalidated edge never appear (DW-2.2 says LIVE records).
	expired := exportEntity("t1", 90000)
	now := time.Unix(2000, 0).UTC()
	expired.ExpiredAt = &now
	if err := b.PutEntity(context.Background(), expired); err != nil {
		t.Fatal(err)
	}
	invalidated := exportEdge("t1", 90000)
	invalidated.InvalidAt = &now
	if err := b.PutEdge(context.Background(), invalidated); err != nil {
		t.Fatal(err)
	}

	svc := &server.Server{Exporter: b, ACL: aclFunc(func(auth.Identity, acl.Record) (bool, error) { return true, nil })}
	entities, edges, pages := walkExport(authedCtx("t1", "u1", "a1"), t, svc)

	if len(entities) != 501 || len(edges) != 3 {
		t.Fatalf("walked %d entities / %d edges, want 501 / 3", len(entities), len(edges))
	}
	if pages < 2 {
		t.Errorf("pages = %d, want >= 2 (501 entities must not fit one page)", pages)
	}
	seen := map[string]bool{}
	for _, e := range entities {
		if seen[e.GetId()] {
			t.Fatalf("entity %s returned twice", e.GetId())
		}
		seen[e.GetId()] = true
		if e.GetId() == expired.ID {
			t.Fatal("expired entity leaked into the export")
		}
	}
	for _, e := range edges {
		if e.GetId() == invalidated.ID {
			t.Fatal("invalidated edge leaked into the export")
		}
	}
}

// TestDW_2_2_ExportEmptyTenantOneTerminalPage: an empty tenant gets exactly
// one empty page with an empty (terminal) next_cursor — no error, no second
// round trip.
func TestDW_2_2_ExportEmptyTenantOneTerminalPage(t *testing.T) {
	svc := &server.Server{Exporter: graph.NewMemBackend()}
	resp, err := svc.Export(authedCtx("t1", "u1", "a1"), &engrampb.ExportRequest{})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(resp.GetEntities()) != 0 || len(resp.GetEdges()) != 0 || resp.GetNextCursor() != "" {
		t.Fatalf("empty tenant: got %d entities, %d edges, cursor %q — want one empty terminal page",
			len(resp.GetEntities()), len(resp.GetEdges()), resp.GetNextCursor())
	}
}

// TestDW_2_2_ExportPageExactlyOnBoundContinues: exactly 500 entities (one
// full page) — the full page reports a continuation cursor, and the next
// call cleanly finds the tier exhausted and chains into edges.
func TestDW_2_2_ExportPageExactlyOnBoundContinues(t *testing.T) {
	b := graph.NewMemBackend()
	seedGraph(t, b, "t1", 500, 2)
	svc := &server.Server{Exporter: b}

	page1, err := svc.Export(authedCtx("t1", "u1", "a1"), &engrampb.ExportRequest{})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(page1.GetEntities()) != 500 || page1.GetNextCursor() == "" {
		t.Fatalf("page 1: %d entities, cursor %q — want 500 + continuation", len(page1.GetEntities()), page1.GetNextCursor())
	}
	if len(page1.GetEdges()) != 0 {
		t.Fatalf("page 1 carried %d edges; a full entity page must not chain", len(page1.GetEdges()))
	}

	page2, err := svc.Export(authedCtx("t1", "u1", "a1"), &engrampb.ExportRequest{Cursor: page1.GetNextCursor()})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page2.GetEntities()) != 0 || len(page2.GetEdges()) != 2 || page2.GetNextCursor() != "" {
		t.Fatalf("page 2: %d entities, %d edges, cursor %q — want 0 entities, 2 edges, terminal",
			len(page2.GetEntities()), len(page2.GetEdges()), page2.GetNextCursor())
	}
}

// TestDW_2_3_ExportTenantIsolation: with tenants A and B both populated, an
// identity bound to A never receives a B record on any page — the tenant is
// pinned from the verified identity (the request has no tenancy field).
func TestDW_2_3_ExportTenantIsolation(t *testing.T) {
	b := graph.NewMemBackend()
	seedGraph(t, b, "tenant-a", 501, 3)
	seedGraph(t, b, "tenant-b", 501, 3)
	svc := &server.Server{Exporter: b}

	entities, edges, _ := walkExport(authedCtx("tenant-a", "u1", "a1"), t, svc)
	if len(entities) != 501 || len(edges) != 3 {
		t.Fatalf("tenant A walked %d entities / %d edges, want its own 501 / 3", len(entities), len(edges))
	}
	for _, e := range entities {
		if strings.HasPrefix(e.GetId(), "tenant-b") {
			t.Fatalf("tenant B entity %s leaked to tenant A", e.GetId())
		}
	}
	for _, e := range edges {
		if strings.HasPrefix(e.GetId(), "tenant-b") {
			t.Fatalf("tenant B edge %s leaked to tenant A", e.GetId())
		}
	}
}

// TestDW_2_3_ExportNoIdentityRejected: a context the barricade never touched
// (no verified identity) fails closed with Unauthenticated — Export has no
// request-field tenant fallback by design.
func TestDW_2_3_ExportNoIdentityRejected(t *testing.T) {
	svc := &server.Server{Exporter: graph.NewMemBackend()}
	_, err := svc.Export(context.Background(), &engrampb.ExportRequest{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", status.Code(err))
	}
}

// TestDW_2_4_ExportACLDeniedRecordsOmitted: a record the ACL denies is
// silently skipped — the page continues, the call succeeds, and nothing
// distinguishes "denied" from "absent" (no oracle).
func TestDW_2_4_ExportACLDeniedRecordsOmitted(t *testing.T) {
	b := graph.NewMemBackend()
	seedGraph(t, b, "t1", 5, 2)
	deniedEntity, deniedEdge := exportEntity("t1", 2).ID, exportEdge("t1", 1).ID
	// acl.Record carries no record id, so mark the to-be-denied records with
	// a sentinel owner and deny on that.
	marked := exportEntity("t1", 2)
	marked.OwnerAgentID = "deny-me"
	if err := b.PutEntity(context.Background(), marked); err != nil {
		t.Fatal(err)
	}
	markedEdge := exportEdge("t1", 1)
	markedEdge.OwnerAgentID = "deny-me"
	if err := b.PutEdge(context.Background(), markedEdge); err != nil {
		t.Fatal(err)
	}
	svc := &server.Server{Exporter: b, ACL: aclFunc(func(_ auth.Identity, r acl.Record) (bool, error) {
		return r.OwnerAgentID != "deny-me", nil
	})}

	entities, edges, _ := walkExport(authedCtx("t1", "u1", "a1"), t, svc)
	if len(entities) != 4 || len(edges) != 1 {
		t.Fatalf("got %d entities / %d edges, want 4 / 1 after ACL omission", len(entities), len(edges))
	}
	for _, e := range entities {
		if e.GetId() == deniedEntity {
			t.Fatal("ACL-denied entity leaked into the export")
		}
	}
	for _, e := range edges {
		if e.GetId() == deniedEdge {
			t.Fatal("ACL-denied edge leaked into the export")
		}
	}
}

// TestDW_2_4_ExportACLErrorFailsClosed: an ACL evaluation error aborts the
// whole call with Internal — uncertainty never degrades to disclosure.
func TestDW_2_4_ExportACLErrorFailsClosed(t *testing.T) {
	b := graph.NewMemBackend()
	seedGraph(t, b, "t1", 3, 0)
	svc := &server.Server{Exporter: b, ACL: aclFunc(func(auth.Identity, acl.Record) (bool, error) {
		return false, fmt.Errorf("acl backend unreachable")
	})}
	resp, err := svc.Export(authedCtx("t1", "u1", "a1"), &engrampb.ExportRequest{})
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %v, want Internal", status.Code(err))
	}
	if resp != nil {
		t.Fatal("a failed-closed call must return no partial page")
	}
}

// TestDW_2_4_ExportNilExporterUnimplemented: an unwired Exporter seam is
// Unimplemented, mirroring the Audit seam contract.
func TestDW_2_4_ExportNilExporterUnimplemented(t *testing.T) {
	svc := &server.Server{}
	_, err := svc.Export(authedCtx("t1", "u1", "a1"), &engrampb.ExportRequest{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("code = %v, want Unimplemented", status.Code(err))
	}
}

// TestExportGarbageCursorInvalidArgument: the cursor is the one
// client-controlled input; anything undecodable is rejected opaquely with
// InvalidArgument before any scan runs.
func TestExportGarbageCursorInvalidArgument(t *testing.T) {
	svc := &server.Server{Exporter: graph.NewMemBackend()}
	for _, cursor := range []string{
		"%%%not-base64%%%",
		"bm90LWpzb24",                         // base64("not-json")
		"eyJzIjoiYm9ndXMtc3RhZ2UiLCJhIjoiIn0", // base64({"s":"bogus-stage","a":""})
	} {
		_, err := svc.Export(authedCtx("t1", "u1", "a1"), &engrampb.ExportRequest{Cursor: cursor})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("cursor %q: code = %v, want InvalidArgument", cursor, status.Code(err))
		}
		if msg := status.Convert(err).Message(); msg != "invalid cursor" {
			t.Errorf("cursor %q: message %q, want the opaque \"invalid cursor\" (no decode detail leak)", cursor, msg)
		}
	}
}

// TestExportStaleCursorStaysSafe: a structurally valid cursor whose position
// no longer exists (records gone) exhausts cleanly inside the caller's own
// tenant — no error, no foreign data.
func TestExportStaleCursorStaysSafe(t *testing.T) {
	full := graph.NewMemBackend()
	seedGraph(t, full, "t1", 501, 0)
	svc := &server.Server{Exporter: full}
	page1, err := svc.Export(authedCtx("t1", "u1", "a1"), &engrampb.ExportRequest{})
	if err != nil || page1.GetNextCursor() == "" {
		t.Fatalf("page 1: err=%v cursor=%q", err, page1.GetNextCursor())
	}

	// Same cursor replayed against an emptied backend, and against a
	// different tenant's identity: both stay safe.
	svc.Exporter = graph.NewMemBackend()
	resp, err := svc.Export(authedCtx("t1", "u1", "a1"), &engrampb.ExportRequest{Cursor: page1.GetNextCursor()})
	if err != nil {
		t.Fatalf("stale cursor on emptied backend: %v", err)
	}
	if len(resp.GetEntities()) != 0 || resp.GetNextCursor() != "" {
		t.Fatalf("stale cursor: got %d entities, cursor %q — want clean exhaustion", len(resp.GetEntities()), resp.GetNextCursor())
	}

	other := graph.NewMemBackend()
	seedGraph(t, other, "tenant-b", 3, 0)
	svc.Exporter = other
	resp, err = svc.Export(authedCtx("t1", "u1", "a1"), &engrampb.ExportRequest{Cursor: page1.GetNextCursor()})
	if err != nil {
		t.Fatalf("stale cursor under other-tenant data: %v", err)
	}
	if len(resp.GetEntities()) != 0 {
		t.Fatalf("a replayed cursor surfaced %d foreign-tenant entities", len(resp.GetEntities()))
	}
}

// TestExportRecordFieldMapping: entity and edge wire records carry the vault
// renderer's fields verbatim — and never the internal embedding/name-key.
func TestExportRecordFieldMapping(t *testing.T) {
	b := graph.NewMemBackend()
	when := time.Unix(1234, 0).UTC()
	ent := graph.Entity{
		ID: "ent-1", NameKey: "internal-key", TenantID: "t1", TeamID: "teamX",
		Scope: acl.ScopeTeam, OwnerAgentID: "a1", Name: "Acme", Aliases: []string{"ACME Corp"},
		Embedding: []float32{0.1, 0.2}, SourceIDs: []string{"ev-1", "ev-2"},
		MentionCount: 7, ValidAt: when, CreatedAt: when,
	}
	edge := graph.Edge{
		ID: "edge-1", TenantID: "t1", TeamID: "teamX", Scope: acl.ScopeTeam, OwnerAgentID: "a1",
		FromEntityID: "ent-1", ToEntityID: "ent-2", Predicate: "supplies",
		Statement: "Acme supplies widgets", SourceIDs: []string{"ev-3"},
		ValidAt: when, CreatedAt: when,
	}
	ctx := context.Background()
	if err := b.PutEntity(ctx, ent); err != nil {
		t.Fatal(err)
	}
	if err := b.PutEdge(ctx, edge); err != nil {
		t.Fatal(err)
	}

	svc := &server.Server{Exporter: b}
	resp, err := svc.Export(authedCtx("t1", "u1", "a1"), &engrampb.ExportRequest{})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(resp.GetEntities()) != 1 || len(resp.GetEdges()) != 1 {
		t.Fatalf("got %d entities / %d edges, want 1 / 1", len(resp.GetEntities()), len(resp.GetEdges()))
	}
	e := resp.GetEntities()[0]
	if e.GetId() != "ent-1" || e.GetName() != "Acme" || e.GetMentionCount() != 7 ||
		len(e.GetAliases()) != 1 || e.GetAliases()[0] != "ACME Corp" ||
		len(e.GetSourceIds()) != 2 || e.GetScope() != acl.ScopeTeam ||
		e.GetTeamId() != "teamX" || e.GetOwnerAgentId() != "a1" ||
		!e.GetValidAt().AsTime().Equal(when) || !e.GetCreatedAt().AsTime().Equal(when) {
		t.Errorf("entity mapping = %+v, fields did not round-trip", e)
	}
	g := resp.GetEdges()[0]
	if g.GetId() != "edge-1" || g.GetFromEntityId() != "ent-1" || g.GetToEntityId() != "ent-2" ||
		g.GetPredicate() != "supplies" || g.GetStatement() != "Acme supplies widgets" ||
		len(g.GetSourceIds()) != 1 || g.GetScope() != acl.ScopeTeam ||
		!g.GetValidAt().AsTime().Equal(when) {
		t.Errorf("edge mapping = %+v, fields did not round-trip", g)
	}
}

// TestDW_2_2_ExportEdgesPageToExhaustion covers the edge-tier continuation
// branch (export.go's edge next_cursor path): with zero entities, the first
// call exhausts the entity tier and chains into edges; 501 edges (one past
// the 500 scan batch) then force multi-page edge paging. Every edge must
// surface exactly once, the cursor advancing between pages and empty on the
// last — the edge-tier mirror of TestDW_2_2_ExportPagesToExhaustion.
func TestDW_2_2_ExportEdgesPageToExhaustion(t *testing.T) {
	b := graph.NewMemBackend()
	seedGraph(t, b, "t1", 0, 501)
	// A live filter must still apply on the edge tier through the handler.
	invalidated := exportEdge("t1", 90000)
	now := time.Unix(2000, 0).UTC()
	invalidated.InvalidAt = &now
	if err := b.PutEdge(context.Background(), invalidated); err != nil {
		t.Fatal(err)
	}

	svc := &server.Server{Exporter: b}
	entities, edges, pages := walkExport(authedCtx("t1", "u1", "a1"), t, svc)

	if len(entities) != 0 {
		t.Fatalf("walked %d entities, want 0 (none seeded)", len(entities))
	}
	if len(edges) != 501 {
		t.Fatalf("walked %d edges, want 501", len(edges))
	}
	if pages < 2 {
		t.Errorf("pages = %d, want >= 2 (501 edges must not fit one page)", pages)
	}
	seen := map[string]bool{}
	for _, e := range edges {
		if seen[e.GetId()] {
			t.Fatalf("edge %s returned twice", e.GetId())
		}
		seen[e.GetId()] = true
		if e.GetId() == invalidated.ID {
			t.Fatal("invalidated edge leaked into the export")
		}
	}
}

// TestDW_2_4_ExportEntityScanErrorFailsClosed: a store failure on the entity
// scan aborts the whole call with Internal — no partial page (export.go's
// entity-scan error path).
func TestDW_2_4_ExportEntityScanErrorFailsClosed(t *testing.T) {
	svc := &server.Server{Exporter: errExporter{backend: graph.NewMemBackend(), scanEntErr: fmt.Errorf("entity index unreachable")}}
	resp, err := svc.Export(authedCtx("t1", "u1", "a1"), &engrampb.ExportRequest{})
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %v, want Internal", status.Code(err))
	}
	if resp != nil {
		t.Fatal("a failed entity scan must return no partial page")
	}
}

// TestDW_2_4_ExportEdgeScanErrorFailsClosed: a store failure on the edge scan
// (reached after the entity tier exhausts cleanly) aborts with Internal and
// no partial page (export.go's edge-scan error path).
func TestDW_2_4_ExportEdgeScanErrorFailsClosed(t *testing.T) {
	// Empty entity tier so the handler chains straight into the edge scan.
	svc := &server.Server{Exporter: errExporter{backend: graph.NewMemBackend(), scanEdgeErr: fmt.Errorf("edge index unreachable")}}
	resp, err := svc.Export(authedCtx("t1", "u1", "a1"), &engrampb.ExportRequest{})
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %v, want Internal", status.Code(err))
	}
	if resp != nil {
		t.Fatal("a failed edge scan must return no partial page")
	}
}

// TestDW_2_4_ExportEdgeACLErrorFailsClosed: an ACL evaluation error on an
// edge aborts the whole call with Internal and no partial page — the
// edge-tier mirror of TestDW_2_4_ExportACLErrorFailsClosed.
func TestDW_2_4_ExportEdgeACLErrorFailsClosed(t *testing.T) {
	b := graph.NewMemBackend()
	seedGraph(t, b, "t1", 0, 3) // edges only, so the ACL error is hit on the edge tier
	svc := &server.Server{Exporter: b, ACL: aclFunc(func(auth.Identity, acl.Record) (bool, error) {
		return false, fmt.Errorf("acl backend unreachable")
	})}
	resp, err := svc.Export(authedCtx("t1", "u1", "a1"), &engrampb.ExportRequest{})
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %v, want Internal", status.Code(err))
	}
	if resp != nil {
		t.Fatal("a failed-closed edge ACL check must return no partial page")
	}
}

// TestDW_1_2_ExportDrainsEpisodicThenChains: the wire cursor drains the
// episodic tier first (its own string sub-cursor advancing page by page),
// then chains into entities and edges when the tier exhausts mid-call —
// every record of all three tiers exactly once, and the final episodic page
// (a short one) sharing its response with the whole graph.
func TestDW_1_2_ExportDrainsEpisodicThenChains(t *testing.T) {
	b := graph.NewMemBackend()
	seedGraph(t, b, "t1", 3, 2)
	recs := make([]memory.Episodic, 5)
	for i := range recs {
		recs[i] = exportEpisodicRec("t1", i)
	}
	svc := &server.Server{
		Exporter:         b,
		EpisodicExporter: fakeEpisodicExporter{recs: recs, pageSize: 2},
	}

	// Page-by-page staging: the first response is episodic-only (its tier is
	// not exhausted, so nothing chains yet).
	first, err := svc.Export(authedCtx("t1", "u1", "a1"), &engrampb.ExportRequest{})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first.GetEpisodics()) != 2 || len(first.GetEntities()) != 0 || len(first.GetEdges()) != 0 || first.GetNextCursor() == "" {
		t.Fatalf("first page = %d episodics / %d entities / %d edges (cursor %q), want 2/0/0 with continuation",
			len(first.GetEpisodics()), len(first.GetEntities()), len(first.GetEdges()), first.GetNextCursor())
	}

	episodics, entities, edges, pages := walkExportAll(authedCtx("t1", "u1", "a1"), t, svc)
	if len(episodics) != 5 || len(entities) != 3 || len(edges) != 2 {
		t.Fatalf("walked %d episodics / %d entities / %d edges, want 5 / 3 / 2", len(episodics), len(entities), len(edges))
	}
	if pages != 3 {
		t.Errorf("pages = %d, want 3 (2+2 episodic pages, then the short page chaining into the whole graph)", pages)
	}
	seen := map[string]bool{}
	for i, ep := range episodics {
		if seen[ep.GetEventId()] {
			t.Fatalf("episodic %s returned twice", ep.GetEventId())
		}
		seen[ep.GetEventId()] = true
		if want := exportEpisodicRec("t1", i).EventID; ep.GetEventId() != want {
			t.Fatalf("episodic[%d] = %s, want %s (scan order preserved)", i, ep.GetEventId(), want)
		}
	}
}

// TestDW_1_2_EmptyEpisodicTierChainsSameResponse: an empty (but wired)
// episodic tier chains straight into the entity stage within the SAME
// response — an empty tenant costs one round trip, not three.
func TestDW_1_2_EmptyEpisodicTierChainsSameResponse(t *testing.T) {
	b := graph.NewMemBackend()
	seedGraph(t, b, "t1", 2, 1)
	svc := &server.Server{Exporter: b, EpisodicExporter: fakeEpisodicExporter{pageSize: 2}}

	resp, err := svc.Export(authedCtx("t1", "u1", "a1"), &engrampb.ExportRequest{})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(resp.GetEpisodics()) != 0 || len(resp.GetEntities()) != 2 || len(resp.GetEdges()) != 1 || resp.GetNextCursor() != "" {
		t.Fatalf("got %d episodics / %d entities / %d edges (cursor %q), want 0/2/1 in one terminal response",
			len(resp.GetEpisodics()), len(resp.GetEntities()), len(resp.GetEdges()), resp.GetNextCursor())
	}
}

// TestDW_1_2_BadEpisodicSubCursorInvalidArgument: a wire cursor whose stage
// is valid but whose episodic sub-cursor the store cannot decode (forged or
// corrupted) is rejected with the same opaque InvalidArgument as any other
// bad cursor — never Internal, never a silent restart.
func TestDW_1_2_BadEpisodicSubCursorInvalidArgument(t *testing.T) {
	svc := &server.Server{
		Exporter:         graph.NewMemBackend(),
		EpisodicExporter: fakeEpisodicExporter{recs: []memory.Episodic{exportEpisodicRec("t1", 0)}, pageSize: 1},
	}
	forged := base64.RawURLEncoding.EncodeToString([]byte(`{"s":"episodic","e":"%%%forged%%%"}`))
	_, err := svc.Export(authedCtx("t1", "u1", "a1"), &engrampb.ExportRequest{Cursor: forged})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
	if msg := status.Convert(err).Message(); msg != "invalid cursor" {
		t.Errorf("message = %q, want the opaque \"invalid cursor\"", msg)
	}
}

// TestDW_1_3_EpisodicACLDeniedOmitted: an episodic record the ACL denies is
// silently skipped — the page continues, the call succeeds, and nothing
// distinguishes "denied" from "absent".
func TestDW_1_3_EpisodicACLDeniedOmitted(t *testing.T) {
	recs := []memory.Episodic{exportEpisodicRec("t1", 0), exportEpisodicRec("t1", 1), exportEpisodicRec("t1", 2)}
	recs[1].OwnerAgentID = "deny-me"
	svc := &server.Server{
		Exporter:         graph.NewMemBackend(),
		EpisodicExporter: fakeEpisodicExporter{recs: recs, pageSize: 10},
		ACL: aclFunc(func(_ auth.Identity, r acl.Record) (bool, error) {
			return r.OwnerAgentID != "deny-me", nil
		}),
	}
	episodics, _, _, _ := walkExportAll(authedCtx("t1", "u1", "a1"), t, svc)
	if len(episodics) != 2 {
		t.Fatalf("got %d episodics, want 2 after ACL omission", len(episodics))
	}
	for _, ep := range episodics {
		if ep.GetEventId() == recs[1].EventID {
			t.Fatal("ACL-denied episodic leaked into the export")
		}
	}
}

// TestDW_1_3_EpisodicACLErrorFailsClosed: an ACL evaluation error on an
// episodic record aborts the whole call with Internal and no partial page —
// uncertainty never degrades to disclosure.
func TestDW_1_3_EpisodicACLErrorFailsClosed(t *testing.T) {
	svc := &server.Server{
		Exporter:         graph.NewMemBackend(),
		EpisodicExporter: fakeEpisodicExporter{recs: []memory.Episodic{exportEpisodicRec("t1", 0)}, pageSize: 10},
		ACL: aclFunc(func(auth.Identity, acl.Record) (bool, error) {
			return false, fmt.Errorf("acl backend unreachable")
		}),
	}
	resp, err := svc.Export(authedCtx("t1", "u1", "a1"), &engrampb.ExportRequest{})
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %v, want Internal", status.Code(err))
	}
	if resp != nil {
		t.Fatal("a failed-closed episodic ACL check must return no partial page")
	}
}

// TestDW_1_3_ExportNoIdentityRejectedWithEpisodic: with the episodic seam
// wired, a context the barricade never touched still fails closed with
// Unauthenticated before any tier is scanned.
func TestDW_1_3_ExportNoIdentityRejectedWithEpisodic(t *testing.T) {
	svc := &server.Server{
		Exporter:         graph.NewMemBackend(),
		EpisodicExporter: fakeEpisodicExporter{recs: []memory.Episodic{exportEpisodicRec("t1", 0)}, pageSize: 10},
	}
	_, err := svc.Export(context.Background(), &engrampb.ExportRequest{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", status.Code(err))
	}
}

// TestExportEpisodicScanErrorFailsClosed: a store failure on the episodic
// scan (not a cursor problem) aborts the whole call with Internal — no
// partial page, mirroring the entity/edge scan-error contract.
func TestExportEpisodicScanErrorFailsClosed(t *testing.T) {
	svc := &server.Server{
		Exporter:         graph.NewMemBackend(),
		EpisodicExporter: fakeEpisodicExporter{scanErr: fmt.Errorf("episodic index unreachable")},
	}
	resp, err := svc.Export(authedCtx("t1", "u1", "a1"), &engrampb.ExportRequest{})
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %v, want Internal", status.Code(err))
	}
	if resp != nil {
		t.Fatal("a failed episodic scan must return no partial page")
	}
}

// TestDW_1_1_ExportEpisodicFieldMapping: the episodic wire record carries the
// plan's pinned field set verbatim — event_id, kind, text, occurred_at,
// source_ids, scope, team_id, owner_agent_id — and nothing internal (the
// proto message has no embedding or outbox fields to leak).
func TestDW_1_1_ExportEpisodicFieldMapping(t *testing.T) {
	when := time.Unix(4321, 0).UTC()
	processed := when.Add(time.Hour)
	rec := memory.Episodic{
		EventID: "ev-map", TenantID: "t1", TeamID: "teamX", Scope: acl.ScopeTeam,
		OwnerAgentID: "a1", Kind: "tool_result", Text: "the full fat body",
		SourceIDs: []string{"s1", "s2"}, TextEmbedding: []float32{0.1},
		OccurredAt: when, CreatedAt: when.Add(time.Minute), ProcessedAt: &processed,
	}
	svc := &server.Server{
		Exporter:         graph.NewMemBackend(),
		EpisodicExporter: fakeEpisodicExporter{recs: []memory.Episodic{rec}, pageSize: 10},
	}
	resp, err := svc.Export(authedCtx("t1", "u1", "a1"), &engrampb.ExportRequest{})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(resp.GetEpisodics()) != 1 {
		t.Fatalf("got %d episodics, want 1", len(resp.GetEpisodics()))
	}
	ep := resp.GetEpisodics()[0]
	if ep.GetEventId() != "ev-map" || ep.GetKind() != "tool_result" || ep.GetText() != "the full fat body" ||
		!ep.GetOccurredAt().AsTime().Equal(when) || len(ep.GetSourceIds()) != 2 ||
		ep.GetScope() != acl.ScopeTeam || ep.GetTeamId() != "teamX" || ep.GetOwnerAgentId() != "a1" {
		t.Errorf("episodic mapping = %+v, fields did not round-trip", ep)
	}
}

//go:build integration

package server_test

// Phase-6 end-to-end integration: the six knowledge operations through the
// REAL stack — engramclient (the mcp.Backend) -> gRPC with the auth
// interceptor -> server barricade -> live registry/store/retriever ->
// OpenSearch. Covers the full lifecycle (create -> ingest -> search with
// filter+sort -> collections count/staleness -> mark-and-sweep delete) plus
// the auth-denial paths (DW-6.2/DW-6.3) and the self-correcting malformed
// filter (DW-6.4). Uses a scratch meta-index and unique collection names so
// parallel runs on the shared cluster cannot collide; everything created is
// cleaned up.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/authgrpc"
	"github.com/ryanthedev/engram/internal/engramclient"
	"github.com/ryanthedev/engram/internal/mcp"
	"github.com/ryanthedev/engram/internal/retrieval"
	"github.com/ryanthedev/engram/internal/server"
	"github.com/ryanthedev/engram/internal/store"
	"github.com/ryanthedev/engram/internal/testutil"
)

// tokenVerifier is a stub authgrpc.Verifier mapping fixed test tokens to
// role-carrying identities. Only token VERIFICATION is faked — role
// enforcement, the barricade, and every seam behind it are real.
type tokenVerifier map[string]auth.Identity

func (v tokenVerifier) Verify(_ context.Context, raw string) (auth.Identity, error) {
	id, ok := v[raw]
	if !ok {
		return auth.Identity{}, errors.New("unknown test token")
	}
	return id, nil
}

// knowledgeStack is the live wired stack plus per-role Backends.
type knowledgeStack struct {
	base                              string
	addr                              string
	admin, harvester, curator, reader mcp.Backend
}

// startKnowledgeStack wires the real registry/store/retriever over the live
// cluster behind an authenticated gRPC server, and dials one engramclient per
// role.
func startKnowledgeStack(t *testing.T) *knowledgeStack {
	t.Helper()
	base := testutil.OpenSearchURL()
	if _, err := store.Apply(context.Background(), testutil.HTTPClient, base); err != nil {
		t.Fatalf("applying cluster contract: %v", err)
	}
	metaIndex := fmt.Sprintf("knowledge-collections-it6%d", time.Now().UnixNano())
	t.Cleanup(func() { testutil.DeleteIndex(t, base, metaIndex) })

	registry := store.NewCollectionRegistry(testutil.HTTPClient, base, store.WithRegistryMetaIndex(metaIndex))
	svc := &server.Server{
		Registry:        registry,
		KnowledgeWriter: store.NewKnowledgeStore(testutil.HTTPClient, base),
		KnowledgeReader: retrieval.NewKnowledgeRetriever(testutil.HTTPClient, base, registry),
	}

	identity := func(roles ...string) auth.Identity {
		return auth.Identity{TenantID: "t-it6", UserID: "u", AgentID: "a", Roles: roles}
	}
	verifier := tokenVerifier{
		"admin-tok":     identity(server.RoleKnowledgeAdmin),
		"harvester-tok": identity(server.RoleHarvester),
		"curator-tok":   identity("curator"),
		"reader-tok":    identity(),
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

	dial := func(token string) mcp.Backend {
		client, err := engramclient.Dial(lis.Addr().String(), token)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = client.Close() })
		return client
	}
	return &knowledgeStack{
		base:      base,
		addr:      lis.Addr().String(),
		admin:     dial("admin-tok"),
		harvester: dial("harvester-tok"),
		curator:   dial("curator-tok"),
		reader:    dial("reader-tok"),
	}
}

// scratchCollection returns a unique legal collection name and registers
// cleanup of its physical indices (the alias dies with its index).
func scratchCollection(t *testing.T, base, prefix string) string {
	t.Helper()
	name := fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
	t.Cleanup(func() {
		for v := 1; v <= 2; v++ {
			testutil.DeleteIndex(t, base, fmt.Sprintf("knowledge-%s-v%d", name, v))
		}
	})
	return name
}

// wantCode asserts err carries a gRPC status code.
func wantCode(t *testing.T, err error, want codes.Code, op string) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Errorf("%s = %v (code %v), want %v", op, err, got, want)
	}
}

// TestDW_6_1_KnowledgeEndToEnd drives the full lifecycle through the Backend
// against the live cluster: create_collection -> ingest (with the
// collection's non-default text field) -> search with a filter + sort ->
// collections (count + staleness) -> mark-and-sweep delete.
func TestDW_6_1_KnowledgeEndToEnd(t *testing.T) {
	ctx := context.Background()
	stack := startKnowledgeStack(t)
	pub := scratchCollection(t, stack.base, "it6pub")

	// Create (admin): public collection, BM25 over "abstract", generic fields.
	err := stack.admin.CreateCollection(ctx, mcp.CollectionSpec{
		Name:      pub,
		TextField: "abstract",
		Mappings: map[string]mcp.FieldSpec{
			"category":  {Type: "keyword", Filterable: true},
			"published": {Type: "date", Filterable: true, Sortable: true},
		},
		Public: true,
	})
	if err != nil {
		t.Fatalf("CreateCollection(%s): %v", pub, err)
	}
	// A duplicate create is AlreadyExists (registry ErrConflict mapping).
	err = stack.admin.CreateCollection(ctx, mcp.CollectionSpec{Name: pub, TextField: "abstract", Public: true})
	wantCode(t, err, codes.AlreadyExists, "duplicate CreateCollection")

	// Ingest (harvester): three docs under harvest h1.
	docs := []mcp.KnowledgeDoc{
		{ID: "d1", Title: "One", Text: "neural networks for retrieval", Fields: map[string]any{"category": "ai", "published": "2026-03-01"}},
		{ID: "d2", Title: "Two", Text: "neural architecture search", Fields: map[string]any{"category": "ai", "published": "2026-06-01"}},
		{ID: "d3", Title: "Three", Text: "databases and neural indexes", Fields: map[string]any{"category": "db", "published": "2026-01-01"}},
	}
	indexed, err := stack.harvester.KnowledgeIngest(ctx, pub, "it-feed", "h1", docs)
	if err != nil {
		t.Fatalf("KnowledgeIngest: %v", err)
	}
	if indexed != 3 {
		t.Fatalf("indexed = %d, want 3", indexed)
	}
	testutil.RefreshIndex(t, stack.base, "knowledge-"+pub)

	// Search (any authenticated caller): query + term filter + sort desc.
	hits, total, err := stack.reader.KnowledgeSearch(ctx, pub, "neural",
		[]mcp.Predicate{{Field: "category", Op: "term", Value: "ai"}},
		[]mcp.SortKey{{Field: "published", Order: "desc"}}, 10, 0, false)
	if err != nil {
		t.Fatalf("KnowledgeSearch: %v", err)
	}
	if len(hits) != 2 || hits[0].ID != "d2" || hits[1].ID != "d1" {
		t.Fatalf("filtered+sorted hits = %+v, want [d2 d1]", hits)
	}
	if total != 2 {
		t.Errorf("total = %d, want the exact match count 2", total)
	}
	if hits[0].Collection != pub {
		t.Errorf("hit Collection = %q, want collection name %q", hits[0].Collection, pub)
	}
	// Range filter over the date field.
	hits, _, err = stack.reader.KnowledgeSearch(ctx, pub, "",
		[]mcp.Predicate{{Field: "published", Op: "range", Value: map[string]any{"gte": "2026-02-01"}}}, nil, 10, 0, false)
	if err != nil {
		t.Fatalf("KnowledgeSearch(range): %v", err)
	}
	if len(hits) != 2 {
		t.Errorf("range hits = %+v, want d1+d2", hits)
	}
	// Malformed filter is self-correcting (DW-6.4 at the live edge).
	_, _, err = stack.reader.KnowledgeSearch(ctx, pub, "neural",
		[]mcp.Predicate{{Field: "nope", Op: "term", Value: "x"}}, nil, 10, 0, false)
	wantCode(t, err, codes.InvalidArgument, "unknown filter field")
	if !strings.Contains(err.Error(), "category, published") {
		t.Errorf("unknown-field error %q does not name the valid fields", err)
	}
	// Unknown collection names itself.
	_, _, err = stack.reader.KnowledgeSearch(ctx, "it6ghost", "q", nil, nil, 5, 0, false)
	wantCode(t, err, codes.InvalidArgument, "unknown collection")
	if err != nil && !strings.Contains(err.Error(), "it6ghost") {
		t.Errorf("unknown-collection error %q does not name it", err)
	}

	// --- DW-3.1: offset paging + exact total ----------------------------
	page1, total1, err := stack.reader.KnowledgeSearch(ctx, pub, "neural", nil,
		[]mcp.SortKey{{Field: "published", Order: "desc"}}, 2, 0, false)
	if err != nil {
		t.Fatalf("KnowledgeSearch(offset=0): %v", err)
	}
	if len(page1) != 2 || total1 != 3 {
		t.Fatalf("page1 = %+v total=%d, want 2 hits and total=3", page1, total1)
	}
	page2, total2, err := stack.reader.KnowledgeSearch(ctx, pub, "neural", nil,
		[]mcp.SortKey{{Field: "published", Order: "desc"}}, 2, 2, false)
	if err != nil {
		t.Fatalf("KnowledgeSearch(offset=2): %v", err)
	}
	if len(page2) != 1 || total2 != 3 {
		t.Fatalf("page2 = %+v total=%d, want 1 hit and total=3", page2, total2)
	}
	if page1[0].ID == page2[0].ID {
		t.Errorf("page1[0]=%s and page2[0]=%s must differ (paging did not advance)", page1[0].ID, page2[0].ID)
	}
	// DW-3.1 dirty case: an offset past the total is empty, not an error, and
	// the total stays exact.
	pastEnd, totalPastEnd, err := stack.reader.KnowledgeSearch(ctx, pub, "neural", nil, nil, 10, 1000, false)
	if err != nil {
		t.Fatalf("KnowledgeSearch(offset past total): %v", err)
	}
	if len(pastEnd) != 0 || totalPastEnd != 3 {
		t.Errorf("offset past total = %+v total=%d, want 0 hits and total=3", pastEnd, totalPastEnd)
	}

	// Collections (DW-6.4): count + staleness through the Backend.
	infos, err := stack.reader.KnowledgeCollections(ctx)
	if err != nil {
		t.Fatalf("KnowledgeCollections: %v", err)
	}
	var found *mcp.CollectionInfo
	for i := range infos {
		if infos[i].Name == pub {
			found = &infos[i]
		}
	}
	if found == nil {
		t.Fatalf("collection %s missing from listing: %+v", pub, infos)
	}
	if found.Count != 3 {
		t.Errorf("count = %d, want 3", found.Count)
	}
	if found.NewestHarvestedAt == nil || time.Since(*found.NewestHarvestedAt) > 5*time.Minute {
		t.Errorf("NewestHarvestedAt = %v, want a recent server-stamped time", found.NewestHarvestedAt)
	}
	if found.NewestDocDate == nil || !found.NewestDocDate.Equal(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("NewestDocDate = %v, want 2026-06-01", found.NewestDocDate)
	}
	if !found.Mappings["published"].Sortable {
		t.Errorf("listing lost the mappings: %+v", found.Mappings)
	}

	// Mark-and-sweep (harvester): h2 re-touches only d1; the sweep removes
	// the two rows the latest run did not touch.
	if _, err := stack.harvester.KnowledgeIngest(ctx, pub, "it-feed", "h2", docs[:1]); err != nil {
		t.Fatalf("KnowledgeIngest(h2): %v", err)
	}
	testutil.RefreshIndex(t, stack.base, "knowledge-"+pub)
	deleted, err := stack.harvester.KnowledgeDelete(ctx, pub, "it-feed", "h2")
	if err != nil {
		t.Fatalf("KnowledgeDelete: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2 (d2+d3 from h1)", deleted)
	}
	testutil.RefreshIndex(t, stack.base, "knowledge-"+pub)
	hits, _, err = stack.reader.KnowledgeSearch(ctx, pub, "neural", nil, nil, 10, 0, false)
	if err != nil {
		t.Fatalf("KnowledgeSearch(after sweep): %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "d1" {
		t.Errorf("post-sweep hits = %+v, want only d1", hits)
	}

	// UpdateCollection (admin): add a field live; it becomes filterable.
	err = stack.admin.UpdateCollection(ctx, mcp.CollectionSpec{
		Name:      pub,
		TextField: "abstract",
		Mappings: map[string]mcp.FieldSpec{
			"category":  {Type: "keyword", Filterable: true},
			"published": {Type: "date", Filterable: true, Sortable: true},
			"lang":      {Type: "keyword", Filterable: true},
		},
		Public: true,
	})
	if err != nil {
		t.Fatalf("UpdateCollection: %v", err)
	}
	if _, _, err := stack.reader.KnowledgeSearch(ctx, pub, "",
		[]mcp.Predicate{{Field: "lang", Op: "term", Value: "en"}}, nil, 5, 0, false); err != nil {
		t.Errorf("filter on live-added field: %v", err)
	}
}

// TestDW_6_2_6_3_AuthDenialsEndToEnd drives the denial paths through the
// live stack: a role-gated read without the role, every write without the
// write role, harvester attempting collection management, and a caller with
// no valid token at all.
func TestDW_6_2_6_3_AuthDenialsEndToEnd(t *testing.T) {
	ctx := context.Background()
	stack := startKnowledgeStack(t)
	gated := scratchCollection(t, stack.base, "it6sec")

	err := stack.admin.CreateCollection(ctx, mcp.CollectionSpec{
		Name:      gated,
		TextField: "text",
		Mappings:  map[string]mcp.FieldSpec{"kind": {Type: "keyword", Filterable: true}},
		Roles:     []string{"curator"},
	})
	if err != nil {
		t.Fatalf("CreateCollection(%s): %v", gated, err)
	}

	// DW-6.2: gated read without the role -> PermissionDenied; with it -> OK.
	_, _, err = stack.reader.KnowledgeSearch(ctx, gated, "anything", nil, nil, 5, 0, false)
	wantCode(t, err, codes.PermissionDenied, "gated read without role")
	if _, _, err := stack.curator.KnowledgeSearch(ctx, gated, "anything", nil, nil, 5, 0, false); err != nil {
		t.Errorf("gated read with role: %v", err)
	}
	// The gated collection is invisible to a reader's listing, visible to a curator.
	infos, err := stack.reader.KnowledgeCollections(ctx)
	if err != nil {
		t.Fatalf("KnowledgeCollections(reader): %v", err)
	}
	for _, info := range infos {
		if info.Name == gated {
			t.Errorf("gated collection leaked into an unauthorized listing")
		}
	}
	infos, err = stack.curator.KnowledgeCollections(ctx)
	if err != nil {
		t.Fatalf("KnowledgeCollections(curator): %v", err)
	}
	seen := false
	for _, info := range infos {
		seen = seen || info.Name == gated
	}
	if !seen {
		t.Errorf("curator listing misses the gated collection: %+v", infos)
	}

	// DW-6.3: every write without the harvester/admin role -> PermissionDenied.
	doc := []mcp.KnowledgeDoc{{ID: "d1", Text: "t"}}
	_, err = stack.reader.KnowledgeIngest(ctx, gated, "s", "h1", doc)
	wantCode(t, err, codes.PermissionDenied, "ingest without role")
	_, err = stack.curator.KnowledgeIngest(ctx, gated, "s", "h1", doc)
	wantCode(t, err, codes.PermissionDenied, "ingest with only a read role")
	_, err = stack.reader.KnowledgeDelete(ctx, gated, "s", "h1")
	wantCode(t, err, codes.PermissionDenied, "delete without role")
	wantCode(t, stack.reader.CreateCollection(ctx, mcp.CollectionSpec{Name: "it6x", TextField: "text"}),
		codes.PermissionDenied, "create without role")
	// Harvester may ingest but NOT manage collections (admin only).
	wantCode(t, stack.harvester.CreateCollection(ctx, mcp.CollectionSpec{Name: "it6y", TextField: "text"}),
		codes.PermissionDenied, "create as harvester")
	wantCode(t, stack.harvester.UpdateCollection(ctx, mcp.CollectionSpec{Name: gated, TextField: "text"}),
		codes.PermissionDenied, "update as harvester")

	// Writes are role-gated, not collection-gated: the harvester may ingest
	// even into a read-gated collection.
	if _, err := stack.harvester.KnowledgeIngest(ctx, gated, "s", "h1", doc); err != nil {
		t.Fatalf("harvester ingest into gated collection: %v", err)
	}

	// No valid token at all: the transport barricade rejects before any
	// handler runs — Unauthenticated, not PermissionDenied.
	anon, err := engramclient.Dial(stack.addr, "no-such-token")
	if err != nil {
		t.Fatalf("dial with unknown token: %v", err)
	}
	defer anon.Close()
	_, err = anon.KnowledgeCollections(ctx)
	wantCode(t, err, codes.Unauthenticated, "call with unknown token")
}

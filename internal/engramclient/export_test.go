package engramclient_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/authgrpc"
	"github.com/ryanthedev/engram/internal/engramclient"
	"github.com/ryanthedev/engram/internal/graph"
	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/server"
)

// pagingEpisodicExporter is a slice-backed server.EpisodicExporter whose
// resume token is the next slice index — the client only ever round-trips
// the wire cursor, so the token format is invisible to it.
type pagingEpisodicExporter struct {
	recs     []memory.Episodic
	pageSize int
}

func (f pagingEpisodicExporter) ScanEpisodic(_ context.Context, _ string, after string) ([]memory.Episodic, string, error) {
	start := 0
	if after != "" {
		n, err := strconv.Atoi(after)
		if err != nil {
			return nil, "", fmt.Errorf("bad token %q", after)
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

// startRichExportServer runs an engram gRPC server on a loopback port behind
// the production auth interceptor, with BOTH export seams wired: nEpisodics
// episodic records (paged pageSize at a time) and one graph entity for
// tenant t1. Returns the dial address.
func startRichExportServer(t *testing.T, nEpisodics, pageSize int) string {
	t.Helper()
	now := time.Unix(1000, 0).UTC()
	recs := make([]memory.Episodic, nEpisodics)
	for i := range recs {
		recs[i] = memory.Episodic{
			EventID: fmt.Sprintf("ev%05d", i), TenantID: "t1", Scope: "private",
			OwnerAgentID: "a1", Kind: "conversation", Text: fmt.Sprintf("prose %d", i),
			SourceIDs: []string{"src-1"}, OccurredAt: now, CreatedAt: now,
		}
	}
	backend := graph.NewMemBackend()
	if err := backend.PutEntity(context.Background(), graph.Entity{
		ID: "e00001", TenantID: "t1", Scope: "private", OwnerAgentID: "a1",
		Name: "Entity", MentionCount: 1, ValidAt: now, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	grpcServer := grpc.NewServer(grpc.ChainUnaryInterceptor(
		authgrpc.UnaryServerInterceptor(fakeVerifier{token: "good-token", id: auth.Identity{TenantID: "t1", UserID: "u1", AgentID: "a1"}}, quiet),
	))
	engrampb.RegisterEngramServer(grpcServer, &server.Server{
		Exporter:         backend,
		EpisodicExporter: pagingEpisodicExporter{recs: recs, pageSize: pageSize},
	})
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)
	return lis.Addr().String()
}

// TestDW_1_4_ClientExportPageEpisodicsAcrossPages: engramclient.ExportPage
// exposes the episodic stage's records as plain structs across multiple
// pages of a real authenticated connection — every record exactly once, in
// scan order, with the event fields (EventID, Kind, Text, OccurredAt,
// SourceIDs) intact — before the graph tiers arrive on later pages.
func TestDW_1_4_ClientExportPageEpisodicsAcrossPages(t *testing.T) {
	addr := startRichExportServer(t, 5, 2)
	c, err := engramclient.Dial(addr, "good-token")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	var episodics []engramclient.ExportEpisodic
	var entities int
	cursor := ""
	pagesWithEpisodics := 0
	for i := 0; i < 20; i++ {
		page, err := c.ExportPage(context.Background(), cursor)
		if err != nil {
			t.Fatalf("ExportPage %d: %v", i, err)
		}
		if len(page.Episodics) > 0 {
			pagesWithEpisodics++
		}
		episodics = append(episodics, page.Episodics...)
		entities += len(page.Entities)
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if cursor != "" {
		t.Fatal("export never exhausted within 20 pages")
	}

	if len(episodics) != 5 {
		t.Fatalf("drained %d episodics, want 5", len(episodics))
	}
	if pagesWithEpisodics < 3 {
		t.Errorf("episodics arrived on %d pages, want >= 3 (5 records at 2 per page)", pagesWithEpisodics)
	}
	if entities != 1 {
		t.Errorf("drained %d entities, want the seeded 1 (graph tiers still on the wire)", entities)
	}
	want := time.Unix(1000, 0).UTC()
	for i, ep := range episodics {
		if ep.EventID != fmt.Sprintf("ev%05d", i) {
			t.Fatalf("episodic[%d].EventID = %s, want ev%05d (scan order, exactly once)", i, ep.EventID, i)
		}
		if ep.Kind != "conversation" || ep.Text != fmt.Sprintf("prose %d", i) {
			t.Errorf("episodic[%d] content = %+v, fields did not survive the adapter", i, ep)
		}
		if ep.OccurredAt == nil || !ep.OccurredAt.Equal(want) {
			t.Errorf("episodic[%d].OccurredAt = %v, want %v", i, ep.OccurredAt, want)
		}
		if len(ep.SourceIDs) != 1 || ep.SourceIDs[0] != "src-1" {
			t.Errorf("episodic[%d].SourceIDs = %v, want [src-1]", i, ep.SourceIDs)
		}
	}
}

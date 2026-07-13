package engramclient_test

// Phase-5 translation tests for the memory Search path: mcp.SearchFilter travels
// FLAT on the wire, field for field. The adapter translates types, never
// semantics — no filter is invented, dropped, or reinterpreted here.

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/engramclient"
	"github.com/ryanthedev/engram/internal/mcp"
)

// searchCapture records the last SearchRequest and answers with one hit.
type searchCapture struct {
	engrampb.UnimplementedEngramServer
	req *engrampb.SearchRequest
}

func (s *searchCapture) Search(_ context.Context, req *engrampb.SearchRequest) (*engrampb.SearchResponse, error) {
	s.req = req
	return &engrampb.SearchResponse{Hits: []*engrampb.Hit{
		{Id: "sem-1", Score: 0.5, Source: "semantic", FieldsJson: `{"statement":"x"}`},
	}}, nil
}

func dialSearch(t *testing.T, srv *searchCapture) *engramclient.Client {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcServer := grpc.NewServer()
	engrampb.RegisterEngramServer(grpcServer, srv)
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)

	client, err := engramclient.Dial(lis.Addr().String(), "test-token")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestDW_5_1_SearchFilterTravelsFlatOnTheWire: every SearchFilter field maps to
// its own named SearchRequest field. The generic predicate form never appears on
// the wire — it is retrieval's internal vocabulary, compiled server-side.
func TestDW_5_1_SearchFilterTravelsFlatOnTheWire(t *testing.T) {
	srv := &searchCapture{}
	c := dialSearch(t, srv)

	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	hits, err := c.Search(context.Background(), "orders-svc leak", 7, mcp.SearchFilter{
		Kind: "conversation", Subject: "orders-svc", Predicate: "owned_by", Object: "team-a",
		ExtractorVersion:  "v3",
		Since:             since,
		Until:             until,
		IncludeSuperseded: true,
		Sources:           []string{"semantic"},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "sem-1" || hits[0].Fields != `{"statement":"x"}` {
		t.Errorf("hits = %+v", hits)
	}

	req := srv.req
	if req.GetQuery() != "orders-svc leak" || req.GetK() != 7 {
		t.Errorf("envelope = %v", req)
	}
	if req.GetKind() != "conversation" || req.GetSubject() != "orders-svc" ||
		req.GetPredicate() != "owned_by" || req.GetObject() != "team-a" || req.GetExtractorVersion() != "v3" {
		t.Errorf("flat filter fields = %v", req)
	}
	if !req.GetSince().AsTime().Equal(since) || !req.GetUntil().AsTime().Equal(until) {
		t.Errorf("time bounds = %v / %v, want %v / %v", req.GetSince().AsTime(), req.GetUntil().AsTime(), since, until)
	}
	if !req.GetIncludeSuperseded() {
		t.Error("include_superseded was dropped in translation")
	}
	if got := req.GetSources(); len(got) != 1 || got[0] != "semantic" {
		t.Errorf("sources = %v, want [semantic]", got)
	}
}

// TestDW_5_6_ZeroFilterSendsNoFilterFields: the unfiltered call — a zero
// SearchFilter — must put NOTHING on the wire but query and k. In particular
// include_superseded stays false, which is what makes the server derive the
// ValidOnly:true this client used to hardcode: today's behavior, unchanged.
func TestDW_5_6_ZeroFilterSendsNoFilterFields(t *testing.T) {
	srv := &searchCapture{}
	c := dialSearch(t, srv)

	if _, err := c.Search(context.Background(), "q", 5, mcp.SearchFilter{}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	req := srv.req
	if req.GetKind() != "" || req.GetSubject() != "" || req.GetPredicate() != "" ||
		req.GetObject() != "" || req.GetExtractorVersion() != "" {
		t.Errorf("a zero filter put term fields on the wire: %v", req)
	}
	if req.GetSince() != nil || req.GetUntil() != nil {
		t.Errorf("a zero filter put time bounds on the wire: since=%v until=%v", req.GetSince(), req.GetUntil())
	}
	if req.GetIncludeSuperseded() {
		t.Error("include_superseded must default to false — the server derives ValidOnly from it")
	}
	if req.GetSources() != nil {
		t.Errorf("sources = %v, want nil (nil means every source; an empty list is an error)", req.GetSources())
	}
}

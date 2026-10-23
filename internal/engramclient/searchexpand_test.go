package engramclient_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/ryanthedev/engram/internal/mcp"
)

// TestSearchExpandMirrorsSearch: SearchExpand(expand=false) puts the SAME
// request on the wire as Search for a fully populated filter, and expand=true
// differs from it in the expand field alone. This is what lets SearchExpand
// duplicate Search's mapping without the two drifting apart on a filter.
func TestSearchExpandMirrorsSearch(t *testing.T) {
	srv := &searchCapture{}
	c := dialSearch(t, srv)
	f := mcp.SearchFilter{
		Kind: "conversation", Subject: "s", Predicate: "p", Object: "o", ExtractorVersion: "v3",
		Since:             time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Until:             time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		IncludeSuperseded: true,
		Sources:           []string{"semantic"},
	}
	ctx := context.Background()

	if _, err := c.Search(ctx, "q", 7, f); err != nil {
		t.Fatalf("Search: %v", err)
	}
	viaSearch := srv.req
	if _, err := c.SearchExpand(ctx, "q", 7, f, false); err != nil {
		t.Fatalf("SearchExpand(false): %v", err)
	}
	if !proto.Equal(srv.req, viaSearch) {
		t.Errorf("SearchExpand(false) sent %v, want Search's %v", srv.req, viaSearch)
	}

	res, err := c.SearchExpand(ctx, "q", 7, f, true)
	if err != nil {
		t.Fatalf("SearchExpand(true): %v", err)
	}
	if !srv.req.GetExpand() {
		t.Error("SearchExpand(true) did not set expand on the wire")
	}
	srv.req.Expand = false
	if !proto.Equal(srv.req, viaSearch) {
		t.Errorf("SearchExpand(true) differs from Search beyond expand: %v vs %v", srv.req, viaSearch)
	}
	if len(res.Hits) != 1 || res.Hits[0].ID != "sem-1" {
		t.Errorf("hits = %+v, want the server's one hit", res.Hits)
	}
}

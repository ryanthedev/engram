package engramclient

import (
	"context"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/mcp"
)

// SearchExpand is Search with graph expansion under the caller's control
// (SearchRequest.expand): expand=true asks the server to run the graph
// post-hook and return its expansions in SearchResult.Expanded; false is
// exactly Search, which leaves expand at its proto default and so gets none.
//
// It is a separate method, not a Search parameter, because Search's signature
// is pinned by the mcp.Backend interface this client satisfies, and the flag
// has no home in mcp.SearchFilter yet. It exists for the CLI's --expand. When
// SearchFilter grows an Expand field, Search should map it and this method
// should go: the mapping below deliberately mirrors Search's field for field,
// so the two cannot diverge on any filter in the meantime.
func (c *Client) SearchExpand(ctx context.Context, query string, k int, f mcp.SearchFilter, expand bool) (mcp.SearchResult, error) {
	req := &engrampb.SearchRequest{
		Query:             query,
		K:                 int32(k),
		Kind:              f.Kind,
		Subject:           f.Subject,
		Predicate:         f.Predicate,
		Object:            f.Object,
		ExtractorVersion:  f.ExtractorVersion,
		IncludeSuperseded: f.IncludeSuperseded,
		Sources:           f.Sources,
		Expand:            expand,
	}
	if !f.Since.IsZero() {
		req.Since = timestamppb.New(f.Since)
	}
	if !f.Until.IsZero() {
		req.Until = timestamppb.New(f.Until)
	}
	resp, err := c.api.Search(ctx, req)
	if err != nil {
		return mcp.SearchResult{}, err
	}
	return mcp.SearchResult{
		Hits:     toMCPHits(resp.GetHits()),
		Expanded: toMCPHits(resp.GetExpanded()),
	}, nil
}

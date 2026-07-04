// Package engramclient is the shared gRPC client the MCP server and the CLI
// use to reach engramd. It attaches the bearer token to every call and adapts
// the proto surface into the small Backend the MCP tools consume, so both
// surfaces speak to the service the same way.
package engramclient

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/authgrpc"
	"github.com/ryanthedev/engram/internal/mcp"
)

// Client is a connected Engram gRPC client bound to one bearer token.
type Client struct {
	conn *grpc.ClientConn
	api  engrampb.EngramClient
}

var _ mcp.Backend = (*Client)(nil)

// Dial connects to engramd at addr, attaching token to every call. The local
// stack is plaintext; production terminates TLS at the ingress. token may be
// empty for calls to exempt methods, but every real RPC needs one.
func Dial(addr, token string) (*Client, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(authgrpc.BearerCreds{Token: token}),
	)
	if err != nil {
		return nil, fmt.Errorf("engramclient: dialing %s: %w", addr, err)
	}
	return &Client{conn: conn, api: engrampb.NewEngramClient(conn)}, nil
}

// Close releases the connection.
func (c *Client) Close() error { return c.conn.Close() }

// Ingest appends one episodic event and returns its storage id.
func (c *Client) Ingest(ctx context.Context, eventID, text, source string) (string, error) {
	req := &engrampb.IngestRequest{EventId: eventID, Text: text, Kind: "mcp"}
	if source != "" {
		req.SourceIds = []string{source}
	}
	resp, err := c.api.Ingest(ctx, req)
	if err != nil {
		return "", err
	}
	return resp.GetId(), nil
}

// Search runs one hybrid query and returns fused hits.
func (c *Client) Search(ctx context.Context, query string, k int) ([]mcp.Hit, error) {
	resp, err := c.api.Search(ctx, &engrampb.SearchRequest{Query: query, K: int32(k), ValidOnly: true})
	if err != nil {
		return nil, err
	}
	hits := make([]mcp.Hit, 0, len(resp.GetHits()))
	for _, h := range resp.GetHits() {
		hits = append(hits, mcp.Hit{ID: h.GetId(), Score: h.GetScore(), Source: h.GetSource(), Fields: h.GetFieldsJson()})
	}
	return hits, nil
}

// Status reports server health, the caller's identity, and tier counts.
func (c *Client) Status(ctx context.Context) (mcp.Status, error) {
	resp, err := c.api.Status(ctx, &engrampb.StatusRequest{})
	if err != nil {
		return mcp.Status{}, err
	}
	return mcp.Status{
		Healthy:           resp.GetHealthy(),
		TenantID:          resp.GetTenantId(),
		UserID:            resp.GetUserId(),
		AgentID:           resp.GetAgentId(),
		EpisodicCount:     resp.GetEpisodicCount(),
		SemanticCount:     resp.GetSemanticCount(),
		OpenSearchVersion: resp.GetOpensearchVersion(),
	}, nil
}

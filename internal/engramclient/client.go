// Package engramclient is the shared gRPC client the MCP server and the CLI
// use to reach engramd. It attaches the bearer token to every call and adapts
// the proto surface into the small Backend the MCP tools consume, so both
// surfaces speak to the service the same way.
package engramclient

import (
	"context"
	"fmt"
	"time"

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
	return c.IngestScoped(ctx, eventID, text, source, "", "")
}

// IngestScoped appends one episodic event at an explicit scope (private/team/
// org) and team, subject to the server's write-time ACL guard. An empty scope
// defaults to private server-side. This is the CLI's scoped-write surface
// (Phase 4); the MCP tool uses the default-scope Ingest.
func (c *Client) IngestScoped(ctx context.Context, eventID, text, source, scope, team string) (string, error) {
	req := &engrampb.IngestRequest{EventId: eventID, Text: text, Kind: "mcp", Scope: scope, TeamId: team}
	if source != "" {
		req.SourceIds = []string{source}
	}
	resp, err := c.api.Ingest(ctx, req)
	if err != nil {
		return "", err
	}
	return resp.GetId(), nil
}

// FactVersion is one version in an audited fact's history.
type FactVersion struct {
	ID         string     `json:"id"`
	Subject    string     `json:"subject"`
	Predicate  string     `json:"predicate"`
	Object     string     `json:"object"`
	Statement  string     `json:"statement"`
	Supersedes string     `json:"supersedes,omitempty"`
	ValidAt    time.Time  `json:"valid_at"`
	InvalidAt  *time.Time `json:"invalid_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiredAt  *time.Time `json:"expired_at,omitempty"`
}

// AuditResult is a fact's provenance plus its full version history.
type AuditResult struct {
	TenantID         string        `json:"tenant_id"`
	TeamID           string        `json:"team_id"`
	Scope            string        `json:"scope"`
	OwnerAgentID     string        `json:"owner_agent_id"`
	SourceIDs        []string      `json:"source_ids"`
	ExtractorVersion string        `json:"extractor_version"`
	Versions         []FactVersion `json:"versions"`
}

// Audit fetches a fact's provenance and full bi-temporal history (DW-4.6).
func (c *Client) Audit(ctx context.Context, id string) (AuditResult, error) {
	resp, err := c.api.Audit(ctx, &engrampb.AuditRequest{Id: id})
	if err != nil {
		return AuditResult{}, err
	}
	p := resp.GetProvenance()
	out := AuditResult{
		TenantID:         p.GetTenantId(),
		TeamID:           p.GetTeamId(),
		Scope:            p.GetScope(),
		OwnerAgentID:     p.GetOwnerAgentId(),
		SourceIDs:        p.GetSourceIds(),
		ExtractorVersion: p.GetExtractorVersion(),
	}
	for _, v := range resp.GetVersions() {
		fv := FactVersion{
			ID: v.GetId(), Subject: v.GetSubject(), Predicate: v.GetPredicate(),
			Object: v.GetObject(), Statement: v.GetStatement(), Supersedes: v.GetSupersedes(),
			ValidAt: v.GetValidAt().AsTime(), CreatedAt: v.GetCreatedAt().AsTime(),
		}
		if v.GetInvalidAt() != nil {
			t := v.GetInvalidAt().AsTime()
			fv.InvalidAt = &t
		}
		if v.GetExpiredAt() != nil {
			t := v.GetExpiredAt().AsTime()
			fv.ExpiredAt = &t
		}
		out.Versions = append(out.Versions, fv)
	}
	return out, nil
}

// Export fetches one bounded page of the caller's tenant-scoped graph
// (entities then edges) for the vault exporter. cursor is the opaque token
// from the previous page's next_cursor; empty starts from the beginning, and
// an empty returned next_cursor means the export is complete. Tenant and ACL
// scoping are enforced server-side from the bearer token's identity.
func (c *Client) Export(ctx context.Context, cursor string) (*engrampb.ExportResponse, error) {
	return c.api.Export(ctx, &engrampb.ExportRequest{Cursor: cursor})
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

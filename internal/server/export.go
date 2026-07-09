package server

// This file is the Export RPC (Obsidian vault exporter, Phase 2): one
// bounded, tenant-scoped, ACL-filtered page of the caller's live graph per
// call, resumed by an opaque wire cursor. Unary by design so it inherits the
// existing auth barricade — see the plan's rejected-streaming rationale.

import (
	"context"
	"encoding/base64"
	"encoding/json"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/acl"
	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/authgrpc"
	"github.com/ryanthedev/engram/internal/graph"
)

// Exporter is the full-graph read seam the Export RPC pages over
// (consumer-defined, mirroring StatusProbe/Auditor; *graph.Store satisfies
// it — and *graph.MemBackend in tests). The cursor contract is Phase 1's:
// zero Cursor in = start, zero Cursor out = tier exhausted; results are
// live- and tenant-filtered inside the implementation.
type Exporter interface {
	ScanEntities(ctx context.Context, tenantID string, cursor graph.Cursor) ([]graph.Entity, graph.Cursor, error)
	ScanEdges(ctx context.Context, tenantID string, cursor graph.Cursor) ([]graph.Edge, graph.Cursor, error)
}

// Export wire-cursor stages: the entity tier is drained first, then the edge
// tier — the graph scan advances the two tiers independently, so the wire
// cursor records which tier the export is in plus that tier's sub-cursor.
const (
	stageEntities = "entities"
	stageEdges    = "edges"
)

// exportCursor is the decoded wire cursor: the stage plus the Phase 1 graph
// sub-cursor for that stage (round-tripped via graph.Cursor's TextMarshaler).
// It deliberately carries NO tenancy — the tenant is re-pinned from the
// verified identity on every call, so a stale or fabricated cursor can only
// reposition inside the caller's own tenant, never widen it.
type exportCursor struct {
	Stage string       `json:"s"`
	After graph.Cursor `json:"a"`
}

// encodeExportCursor renders c as the opaque wire token (base64url of JSON).
func encodeExportCursor(c exportCursor) string {
	b, _ := json.Marshal(c) // marshaling string fields cannot fail
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeExportCursor is the input-validation half of the barricade for the
// ONE client-controlled Export input: empty means start; anything else must
// decode to a known stage or the whole request is rejected upstream as
// InvalidArgument (opaquely — the error detail never reaches the client).
func decodeExportCursor(token string) (exportCursor, error) {
	if token == "" {
		return exportCursor{Stage: stageEntities}, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return exportCursor{}, err
	}
	var c exportCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return exportCursor{}, err
	}
	if c.Stage != stageEntities && c.Stage != stageEdges {
		return exportCursor{}, status.Error(codes.InvalidArgument, "unknown export stage")
	}
	return c, nil
}

// Export implements engrampb.EngramServer: one bounded page of the caller's
// live graph — entities first, then edges — plus the continuation cursor
// (empty = exhausted). Security posture (this is the tenant boundary):
//   - tenant is pinned from the verified Identity, never from the request
//     (there is no request tenancy field at all); a missing identity fails
//     closed with Unauthenticated even for in-process callers that bypass
//     the interceptor (defense in depth on a security-critical path);
//   - every record passes s.ACL.CanRead (nil ACL skips the scope check per
//     the ReadAuthorizer contract; production always wires it); a denied
//     record is omitted, an ACL error aborts the call — never partial trust;
//   - the page bound is inherited from the Phase 1 scan batch size: at most
//     one entity page plus one edge page per response (< 2×500 records, and
//     a full entity page never chains into edges), far under the 4 MB cap.
//
// When the entity tier exhausts mid-call the handler chains straight into
// the first edge page, so an empty tenant gets exactly one empty page with
// a terminal (empty) cursor rather than a pointless extra round trip.
func (s *Server) Export(ctx context.Context, req *engrampb.ExportRequest) (*engrampb.ExportResponse, error) {
	if s.Exporter == nil {
		return nil, status.Error(codes.Unimplemented, "export is not configured")
	}
	id, ok := authgrpc.IdentityFrom(ctx)
	if !ok || id.TenantID == "" {
		return nil, status.Error(codes.Unauthenticated, "unauthenticated")
	}
	cur, err := decodeExportCursor(req.GetCursor())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid cursor")
	}

	resp := &engrampb.ExportResponse{}
	if cur.Stage == stageEntities {
		entities, next, err := s.Exporter.ScanEntities(ctx, id.TenantID, cur.After)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "export entities: %v", err)
		}
		for _, e := range entities {
			allowed, err := s.canExport(ctx, id, acl.Record{
				TenantID: e.TenantID, TeamID: e.TeamID, Scope: e.Scope, OwnerAgentID: e.OwnerAgentID,
			})
			if err != nil {
				return nil, status.Errorf(codes.Internal, "export authorization: %v", err)
			}
			if allowed {
				resp.Entities = append(resp.Entities, exportEntityProto(e))
			}
		}
		if next != (graph.Cursor{}) {
			resp.NextCursor = encodeExportCursor(exportCursor{Stage: stageEntities, After: next})
			return resp, nil
		}
		cur = exportCursor{Stage: stageEdges} // entity tier exhausted: chain into edges
	}

	edges, next, err := s.Exporter.ScanEdges(ctx, id.TenantID, cur.After)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "export edges: %v", err)
	}
	for _, e := range edges {
		allowed, err := s.canExport(ctx, id, acl.Record{
			TenantID: e.TenantID, TeamID: e.TeamID, Scope: e.Scope, OwnerAgentID: e.OwnerAgentID,
		})
		if err != nil {
			return nil, status.Errorf(codes.Internal, "export authorization: %v", err)
		}
		if allowed {
			resp.Edges = append(resp.Edges, exportEdgeProto(e))
		}
	}
	if next != (graph.Cursor{}) {
		resp.NextCursor = encodeExportCursor(exportCursor{Stage: stageEdges, After: next})
	}
	return resp, nil
}

// canExport is the per-record scope check: the same ReadAuthorizer contract
// the audit path uses (nil ACL skips it; an error is the caller's cue to
// fail the whole call closed rather than guess).
func (s *Server) canExport(ctx context.Context, id auth.Identity, rec acl.Record) (bool, error) {
	if s.ACL == nil {
		return true, nil
	}
	return s.ACL.CanRead(ctx, id, rec)
}

// exportEntityProto maps a graph entity to its export wire form. Embedding
// and NameKey are internal (similarity signal / lookup key) and never leave
// the server; tenant_id is implied by the caller's identity.
func exportEntityProto(e graph.Entity) *engrampb.ExportEntity {
	return &engrampb.ExportEntity{
		Id:           e.ID,
		Name:         e.Name,
		Aliases:      e.Aliases,
		MentionCount: int64(e.MentionCount),
		SourceIds:    e.SourceIDs,
		Scope:        e.Scope,
		TeamId:       e.TeamID,
		OwnerAgentId: e.OwnerAgentID,
		ValidAt:      timestamppb.New(e.ValidAt),
		CreatedAt:    timestamppb.New(e.CreatedAt),
	}
}

// exportEdgeProto maps a graph edge to its export wire form.
func exportEdgeProto(e graph.Edge) *engrampb.ExportEdge {
	return &engrampb.ExportEdge{
		Id:           e.ID,
		FromEntityId: e.FromEntityID,
		ToEntityId:   e.ToEntityID,
		Predicate:    e.Predicate,
		Statement:    e.Statement,
		SourceIds:    e.SourceIDs,
		Scope:        e.Scope,
		TeamId:       e.TeamID,
		OwnerAgentId: e.OwnerAgentID,
		ValidAt:      timestamppb.New(e.ValidAt),
		CreatedAt:    timestamppb.New(e.CreatedAt),
	}
}

package server

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/acl"
	"github.com/ryanthedev/engram/internal/authgrpc"
	"github.com/ryanthedev/engram/internal/memory"
)

// EpisodicReader fetches one full episodic record by doc id for the Read RPC
// (consumer-defined seam; *store.OpenSearchStore satisfies it). ok=false
// means no such doc. The returned record still carries its ACL fields — the
// handler authorizes against them before projecting them away.
type EpisodicReader interface {
	GetEpisodic(ctx context.Context, id string) (memory.Episodic, bool, error)
}

// Read source values — the dispatch contract shared with search Hit.Source.
const (
	sourceEpisodic = "episodic"
	sourceSemantic = "semantic"
	sourceGraph    = "graph"
)

// Read implements engrampb.EngramServer: it returns ONE record's full content
// by (id, source) — the drill-down behind memory_read. It is a by-id trust
// boundary and runs the same fail-closed authorization as Search/Audit: the
// record is FETCHED whole (ACL fields included), the caller is AUTHORIZED
// against those fields (tenant pin + CanRead), and only then is the record
// PROJECTED into the response with the ACL fields dropped. Every denial —
// unknown id, cross-tenant record, id/source mismatch, ACL denial — returns
// the same opaque NOT_FOUND, so absence and denial are indistinguishable and
// no existence leaks. Dispatch on source touches exactly ONE index path;
// a miss never probes the other index.
func (s *Server) Read(ctx context.Context, req *engrampb.ReadRequest) (*engrampb.ReadResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	switch req.GetSource() {
	case sourceEpisodic:
		return s.readEpisodic(ctx, req.GetId())
	case sourceSemantic:
		return s.readSemantic(ctx, req.GetId())
	case sourceGraph:
		// Per the drill-down research: a graph hit's statement already IS the
		// whole memory — there is nothing more to read.
		return nil, status.Error(codes.Unimplemented, "graph records have no drill-down; the search hit already carries the full statement")
	default:
		return nil, status.Error(codes.InvalidArgument, `source must be "episodic" or "semantic"`)
	}
}

// errReadNotFound is the single opaque denial for every Read miss: unknown
// id, cross-tenant record, or an ACL denial — never distinguishable.
var errReadNotFound = status.Error(codes.NotFound, "record not found")

// readEpisodic serves Read for the episodic tier: fetch the full record,
// authorize the caller against its ACL fields, then project the content-only
// response. The ordering is the load-bearing security property — the ACL
// fields must exist at authorization time and be gone from the response.
func (s *Server) readEpisodic(ctx context.Context, docID string) (*engrampb.ReadResponse, error) {
	if s.Episodic == nil {
		return nil, status.Error(codes.Unimplemented, "read is not configured")
	}
	id, _ := authgrpc.IdentityFrom(ctx)

	// FETCH: the whole stored record, ACL fields included.
	rec, ok, err := s.Episodic.GetEpisodic(ctx, docID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read: %v", err)
	}
	if !ok {
		return nil, errReadNotFound
	}
	// AUTHORIZE: tenancy boundary first — a token never reads another
	// tenant's record — then the same scope policy search and Audit enforce.
	if rec.TenantID != id.TenantID {
		return nil, errReadNotFound
	}
	if s.ACL != nil {
		allowed, err := s.ACL.CanRead(ctx, id, acl.Record{
			TenantID: rec.TenantID, TeamID: rec.TeamID,
			Scope: rec.Scope, OwnerAgentID: rec.OwnerAgentID,
		})
		if err != nil {
			return nil, status.Errorf(codes.Internal, "read authorization: %v", err)
		}
		if !allowed {
			return nil, errReadNotFound
		}
	}
	// PROJECT: content fields only — no tenant_id/team_id/scope/
	// owner_agent_id, no embedding, no outbox state.
	return &engrampb.ReadResponse{
		Source: sourceEpisodic,
		Episodic: &engrampb.EpisodicRecord{
			Id:         docID,
			EventId:    rec.EventID,
			Kind:       rec.Kind,
			Text:       rec.Text,
			SourceIds:  rec.SourceIDs,
			OccurredAt: timestamppb.New(rec.OccurredAt),
			CreatedAt:  timestamppb.New(rec.CreatedAt),
		},
	}, nil
}

// readSemantic serves Read for the semantic tier by delegating to Audit —
// the one fail-closed by-id fact read (fetch, tenant pin, CanRead) — so the
// two paths can never diverge. The response adds the TARGET version
// explicitly: a superseded id returns that immutable version with its
// possibly-closed validity interval, which is expected, not an error.
func (s *Server) readSemantic(ctx context.Context, docID string) (*engrampb.ReadResponse, error) {
	audit, err := s.Audit(ctx, &engrampb.AuditRequest{Id: docID})
	if err != nil {
		// Audit is already fail-closed; normalize its NOT_FOUND wording to
		// Read's single denial so the two source branches stay byte-identical
		// (one opaque message for every miss, whatever the tier).
		if status.Code(err) == codes.NotFound {
			return nil, errReadNotFound
		}
		return nil, err // Internal / Unimplemented pass through
	}
	resp := &engrampb.ReadResponse{
		Source:     sourceSemantic,
		Provenance: audit.GetProvenance(),
		Versions:   audit.GetVersions(),
	}
	for _, v := range audit.GetVersions() {
		if v.GetId() == docID {
			resp.Fact = v
			break
		}
	}
	return resp, nil
}

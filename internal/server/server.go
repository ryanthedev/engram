// Package server implements the Engram gRPC service (api/proto/engram.proto)
// over the Store and Retriever seams: Ingest is the sync episodic append,
// Search is the hybrid read path. It depends on the seam interfaces, not any
// concrete OpenSearch type, so it is agnostic to what backs them.
package server

import (
	"context"
	"encoding/json"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/authgrpc"
	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/retrieval"
	"github.com/ryanthedev/engram/internal/store"
)

// StatusProbe reports coarse tier counts and the cluster version for the
// Status RPC (consumer-defined seam; *store.OpenSearchStore satisfies it).
type StatusProbe interface {
	Counts(ctx context.Context, tenantID string) (episodic, semantic int64, version string, err error)
}

// Server implements engrampb.EngramServer over a Store (write path) and a
// Retriever (read path). The append is the enqueue (D12): Phase 1 writes
// only episodic; Phase 2 adds the async extraction/reconciliation worker
// behind the same Store. Past the auth barricade every handler resolves the
// caller's Identity from the context (authgrpc.IdentityFrom) and derives
// tenancy/provenance from it — client-supplied tenancy fields are advisory
// and cannot widen the token's binding.
type Server struct {
	engrampb.UnimplementedEngramServer
	Store     store.Store
	Retriever retrieval.Retriever
	// Probe backs the Status RPC; nil disables count reporting.
	Probe StatusProbe
}

// New returns a Server wired to s (write path) and r (read path).
func New(s store.Store, r retrieval.Retriever) *Server {
	return &Server{Store: s, Retriever: r}
}

// Ingest implements engrampb.EngramServer: it durably appends one episodic
// event and returns its storage id. event_id is required (D13); an empty
// event_id is rejected with INVALID_ARGUMENT since proto3 cannot express a
// required field.
func (s *Server) Ingest(ctx context.Context, req *engrampb.IngestRequest) (*engrampb.IngestResponse, error) {
	if req.GetEventId() == "" {
		return nil, status.Error(codes.InvalidArgument, "event_id is required")
	}

	occurredAt := time.Now().UTC()
	if req.GetOccurredAt() != nil {
		occurredAt = req.GetOccurredAt().AsTime()
	}

	// Tenancy/provenance are authoritative from the verified Identity when the
	// barricade injected one (production); client-supplied fields are the
	// fallback only for in-process callers/tests that bypass the interceptor.
	tenantID, ownerAgentID := req.GetTenantId(), req.GetOwnerAgentId()
	if id, ok := authgrpc.IdentityFrom(ctx); ok {
		tenantID, ownerAgentID = id.TenantID, id.AgentID
	}

	rec := memory.Episodic{
		EventID:      req.GetEventId(),
		TenantID:     tenantID,
		TeamID:       req.GetTeamId(),
		Scope:        req.GetScope(),
		OwnerAgentID: ownerAgentID,
		SourceIDs:    req.GetSourceIds(),
		Kind:         req.GetKind(),
		Text:         req.GetText(),
		OccurredAt:   occurredAt,
		CreatedAt:    time.Now().UTC(),
	}
	id, err := s.Store.Append(ctx, rec)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "appending episodic event: %v", err)
	}
	return &engrampb.IngestResponse{Id: id}, nil
}

// Search implements engrampb.EngramServer: one hybrid query (BM25 + kNN ->
// RRF, D1) scoped by tenancy and validity filters, returning a single fused,
// ranked hit list.
func (s *Server) Search(ctx context.Context, req *engrampb.SearchRequest) (*engrampb.SearchResponse, error) {
	q := retrieval.Query{Text: req.GetQuery(), K: int(req.GetK())}
	f := retrieval.Filter{
		TenantID:  req.GetTenantId(),
		UserID:    req.GetUserId(),
		ValidOnly: req.GetValidOnly(),
	}
	// The verified Identity fixes the tenancy boundary (the barricade). User-
	// and agent-level scoping is Phase 4 (ACL); until then a token sees its
	// whole tenant, so only TenantID is overridden here — driving the
	// retriever's owner_agent_id filter from the token's user would wrongly
	// hide facts written by any other agent.
	if id, ok := authgrpc.IdentityFrom(ctx); ok {
		f.TenantID = id.TenantID
	}
	hits, err := s.Retriever.Search(ctx, q, f)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "search: %v", err)
	}

	out := make([]*engrampb.Hit, len(hits))
	for i, h := range hits {
		fieldsJSON, err := json.Marshal(h.Fields)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "encoding hit %s fields: %v", h.ID, err)
		}
		out[i] = &engrampb.Hit{
			Id:         h.ID,
			Score:      h.Score,
			Source:     h.Source,
			FieldsJson: string(fieldsJSON),
		}
	}
	return &engrampb.SearchResponse{Hits: out}, nil
}

// Status implements engrampb.EngramServer: it reports liveness, the caller's
// resolved identity, and coarse tier counts for their tenant. Counts are
// best-effort — a probe error yields healthy=false rather than failing the
// call, so status stays useful during a degraded backend.
func (s *Server) Status(ctx context.Context, _ *engrampb.StatusRequest) (*engrampb.StatusResponse, error) {
	id, _ := authgrpc.IdentityFrom(ctx)
	resp := &engrampb.StatusResponse{
		TenantId: id.TenantID,
		UserId:   id.UserID,
		AgentId:  id.AgentID,
	}
	if s.Probe != nil {
		ep, sem, version, err := s.Probe.Counts(ctx, id.TenantID)
		if err != nil {
			return resp, nil // degraded: identity still reported, healthy stays false
		}
		resp.Healthy = true
		resp.EpisodicCount = ep
		resp.SemanticCount = sem
		resp.OpensearchVersion = version
	}
	return resp, nil
}

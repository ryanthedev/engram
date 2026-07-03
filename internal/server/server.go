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
	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/retrieval"
	"github.com/ryanthedev/engram/internal/store"
)

// Server implements engrampb.EngramServer over a Store (write path) and a
// Retriever (read path). The append is the enqueue (D12): Phase 1 writes
// only episodic; Phase 2 adds the async extraction/reconciliation worker
// behind the same Store.
type Server struct {
	engrampb.UnimplementedEngramServer
	Store     store.Store
	Retriever retrieval.Retriever
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

	rec := memory.Episodic{
		EventID:      req.GetEventId(),
		TenantID:     req.GetTenantId(),
		TeamID:       req.GetTeamId(),
		Scope:        req.GetScope(),
		OwnerAgentID: req.GetOwnerAgentId(),
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

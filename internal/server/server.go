// Package server implements the Engram gRPC service (api/proto/engram.proto)
// over the Store and Retriever seams: Ingest is the sync episodic append,
// Search is the hybrid read path. It depends on the seam interfaces, not any
// concrete OpenSearch type, so it is agnostic to what backs them.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/acl"
	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/authgrpc"
	"github.com/ryanthedev/engram/internal/knowledge"
	"github.com/ryanthedev/engram/internal/knowledgeauth"
	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/retrieval"
	"github.com/ryanthedev/engram/internal/store"
)

// StatusProbe reports coarse tier counts and the cluster version for the
// Status RPC (consumer-defined seam; *store.OpenSearchStore satisfies it).
type StatusProbe interface {
	Counts(ctx context.Context, tenantID string) (episodic, semantic int64, version string, err error)
}

// Auditor fetches a fact's provenance and full bi-temporal history for the
// Audit RPC (consumer-defined seam; *store.OpenSearchStore satisfies it).
// ok=false means no such fact.
type Auditor interface {
	AuditFact(ctx context.Context, id string) (target store.VersionedFact, versions []store.VersionedFact, ok bool, err error)
}

// ReadAuthorizer decides whether an identity may read a record — the audit
// path's scope check (consumer-defined seam; *acl.Filter satisfies it). When
// nil, the server skips the ACL check (tenant scoping still applies).
type ReadAuthorizer interface {
	CanRead(ctx context.Context, id auth.Identity, r acl.Record) (bool, error)
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
	// Auditor backs the Audit RPC; nil disables auditing (UNIMPLEMENTED).
	Auditor Auditor
	// ACL authorizes audit and export reads (Phase 4); nil skips the scope
	// check.
	ACL ReadAuthorizer
	// Exporter backs the Export RPC (the Phase 1 graph scan); nil disables
	// exporting (UNIMPLEMENTED). See export.go.
	Exporter Exporter

	// Knowledge platform seams (Phase 6, knowledge.go). A nil Registry (or
	// nil Writer/Reader for the operations needing them) disables the six
	// knowledge RPCs (UNIMPLEMENTED). KnowledgeAuth's zero value is ready.
	Registry        knowledge.CollectionRegistry
	KnowledgeWriter KnowledgeWriter
	KnowledgeReader KnowledgeReader
	KnowledgeAuth   knowledgeauth.Authorizer

	// Episodic backs the Read RPC's episodic branch; nil disables episodic
	// reads (UNIMPLEMENTED). See read.go.
	Episodic EpisodicReader
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
		// A scope-authorization denial is the caller's fault, not the server's:
		// surface it as PermissionDenied with an opaque message (no policy
		// detail leaked), everything else as Internal.
		if errors.Is(err, acl.ErrScopeDenied) || errors.Is(err, acl.ErrUnknownScope) {
			return nil, status.Error(codes.PermissionDenied, "not authorized to write this scope")
		}
		return nil, status.Errorf(codes.Internal, "appending episodic event: %v", err)
	}
	return &engrampb.IngestResponse{Id: id}, nil
}

// Search implements engrampb.EngramServer: one hybrid query (BM25 + kNN ->
// RRF, D1) scoped by tenancy and validity filters, returning a single fused,
// ranked hit list.
func (s *Server) Search(ctx context.Context, req *engrampb.SearchRequest) (*engrampb.SearchResponse, error) {
	// The flat filter fields are compiled into retrieval's predicate form here,
	// at the barricade — a malformed filter is rejected before any tier runs.
	preds, err := compileSearchFilter(req)
	if err != nil {
		return nil, err
	}
	q := retrieval.Query{Text: req.GetQuery(), K: int(req.GetK())}
	f := retrieval.Filter{
		TenantID: req.GetTenantId(),
		UserID:   req.GetUserId(),
		// Current-state-only unless the caller explicitly asked for history.
		// This is the sole producer of ValidOnly, and its default reproduces the
		// hardcoded true that engramclient used to send.
		ValidOnly:  !req.GetIncludeSuperseded(),
		Predicates: preds,
		Sources:    req.GetSources(),
	}
	// The verified Identity fixes the tenancy boundary AND drives ACL scope
	// enforcement (Phase 4): the retriever compiles f.Identity into the filter
	// applied inside every query, so a token sees only the scopes it reaches.
	// TenantID is still pinned for the non-ACL path (eval/tests).
	if id, ok := authgrpc.IdentityFrom(ctx); ok {
		f.TenantID = id.TenantID
		f.Identity = id
	}
	hits, err := s.Retriever.Search(ctx, q, f)
	if err != nil {
		// A filter the retriever rejects (an unknown source, an unfilterable
		// source named alongside filters) is the caller's mistake, not a server
		// fault: report it as InvalidArgument with the retriever's own
		// vocabulary-naming message. Everything else is Internal.
		if errors.Is(err, retrieval.ErrInvalidFilter) {
			return nil, status.Errorf(codes.InvalidArgument, "%v", err)
		}
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

// Audit implements engrampb.EngramServer: it returns a fact's provenance and
// full bi-temporal version history (DW-4.6). It is fail-closed and leaks no
// existence: an unknown id, a cross-tenant fact, or a fact the caller may not
// read all return NOT_FOUND — never the fact and never a distinguishable
// error. The ACL read check reuses the same scope policy search enforces.
func (s *Server) Audit(ctx context.Context, req *engrampb.AuditRequest) (*engrampb.AuditResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	if s.Auditor == nil {
		return nil, status.Error(codes.Unimplemented, "audit is not configured")
	}
	id, _ := authgrpc.IdentityFrom(ctx)
	target, versions, ok, err := s.Auditor.AuditFact(ctx, req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "audit: %v", err)
	}
	notFound := status.Error(codes.NotFound, "fact not found")
	if !ok {
		return nil, notFound
	}
	// Tenancy boundary: a token never audits another tenant's fact.
	if target.Fact.TenantID != id.TenantID {
		return nil, notFound
	}
	// Scope check: the caller must be authorized to read the target fact.
	if s.ACL != nil {
		rec := acl.Record{
			TenantID: target.Fact.TenantID, TeamID: target.Fact.TeamID,
			Scope: target.Fact.Scope, OwnerAgentID: target.Fact.OwnerAgentID,
		}
		allowed, err := s.ACL.CanRead(ctx, id, rec)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "audit authorization: %v", err)
		}
		if !allowed {
			return nil, notFound
		}
	}
	resp := &engrampb.AuditResponse{
		Provenance: &engrampb.Provenance{
			TenantId:         target.Fact.TenantID,
			TeamId:           target.Fact.TeamID,
			Scope:            target.Fact.Scope,
			OwnerAgentId:     target.Fact.OwnerAgentID,
			SourceIds:        target.Fact.SourceIDs,
			ExtractorVersion: target.Fact.ExtractorVersion,
			CreatedAt:        timestamppb.New(target.Fact.CreatedAt),
		},
	}
	for _, v := range versions {
		resp.Versions = append(resp.Versions, factVersionProto(v))
	}
	return resp, nil
}

// factVersionProto maps a stored fact version to its proto form, carrying the
// open/closed bi-temporal interval bounds (nil timestamps stay nil).
func factVersionProto(v store.VersionedFact) *engrampb.FactVersion {
	f := v.Fact
	fv := &engrampb.FactVersion{
		Id:         v.ID,
		Subject:    f.Subject,
		Predicate:  f.Predicate,
		Object:     f.Object,
		Statement:  f.Statement,
		ContentKey: f.ContentKey,
		Supersedes: f.Supersedes,
		ValidAt:    timestamppb.New(f.ValidAt),
		CreatedAt:  timestamppb.New(f.CreatedAt),
	}
	if f.InvalidAt != nil {
		fv.InvalidAt = timestamppb.New(*f.InvalidAt)
	}
	if f.ExpiredAt != nil {
		fv.ExpiredAt = timestamppb.New(*f.ExpiredAt)
	}
	return fv
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

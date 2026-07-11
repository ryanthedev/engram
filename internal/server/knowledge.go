// This file holds the knowledge-platform gRPC handlers (Phase 6): the
// request barricade for the six knowledge operations. Every handler resolves
// the verified Identity from
// the context, authorizes it via knowledgeauth (writes first, so an
// unauthorized caller learns nothing about its arguments), validates ALL
// request input at this edge — collection existence, filter/sort shape
// against the collection's declared mappings, argument presence — and only
// then calls the inner registry/store/retriever seams, which may assume
// validated input (cc-defensive barricade; the retriever's own re-validation
// is deliberate defense-in-depth, not duplication). Domain sentinels map to
// gRPC codes here: ErrForbidden -> PermissionDenied (opaque), ErrConflict ->
// AlreadyExists, ErrNotFound -> NotFound / InvalidArgument-naming-it,
// validation -> InvalidArgument with self-correcting valid-field lists
// (usability, not a security boundary — the plan pins this trade explicitly).

package server

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/authgrpc"
	"github.com/ryanthedev/engram/internal/knowledge"
	"github.com/ryanthedev/engram/internal/retrieval"
)

// Knowledge write roles (DW-6.3). Ingest and delete are harvest-run
// operations: the harvester role suffices, and admin subsumes it. Collection
// create/update change schema and access policy: admin only.
const (
	// RoleKnowledgeAdmin may perform every knowledge write, including
	// collection create/update.
	RoleKnowledgeAdmin = "admin"
	// RoleHarvester may ingest documents and run the mark-and-sweep delete.
	RoleHarvester = "harvester"
)

// KnowledgeWriter is the document write path (consumer-defined seam;
// *store.KnowledgeStore satisfies it).
type KnowledgeWriter interface {
	BulkIndex(ctx context.Context, index, textField string, docs []knowledge.Document, harvestID string) (indexed int, err error)
	DeleteByQuery(ctx context.Context, index, collection, source, currentHarvestID string) (deleted int, err error)
}

// KnowledgeReader is the BM25 read path (consumer-defined seam;
// *retrieval.KnowledgeRetriever satisfies it).
type KnowledgeReader interface {
	Search(ctx context.Context, spec knowledge.CollectionSpec, query string, filters []retrieval.Predicate, sort []retrieval.SortKey, k int) ([]retrieval.Hit, error)
	Collections(ctx context.Context) ([]retrieval.CollectionMeta, error)
}

// errKnowledgeUnconfigured mirrors the Auditor precedent: a Server without
// knowledge wiring answers the knowledge RPCs with UNIMPLEMENTED (the same
// code the Phase-1 UnimplementedEngramServer embedding produced, so the
// Phase-1 contract tests hold unchanged).
var errKnowledgeUnconfigured = status.Error(codes.Unimplemented, "knowledge platform is not configured")

// knowledgeConfigured reports whether the knowledge seams a handler needs are
// wired. The registry is required by every operation.
func (s *Server) knowledgeConfigured() bool { return s.Registry != nil }

// resolveCollection validates and resolves a request's collection name via
// the registry. An unknown collection is INVALID_ARGUMENT naming it — the
// plan accepts existence naming here for LLM self-correction.
func (s *Server) resolveCollection(ctx context.Context, name string) (knowledge.CollectionSpec, error) {
	if name == "" {
		return knowledge.CollectionSpec{}, status.Error(codes.InvalidArgument, "collection is required")
	}
	spec, err := s.Registry.Get(ctx, name)
	if err != nil {
		if errors.Is(err, knowledge.ErrNotFound) {
			return knowledge.CollectionSpec{}, status.Errorf(codes.InvalidArgument, "unknown collection %q", name)
		}
		return knowledge.CollectionSpec{}, status.Errorf(codes.Internal, "resolving collection %q: %v", name, err)
	}
	return spec, nil
}

// authorizeKnowledgeWrite denies unless the verified caller holds one of
// roles. The identity comes exclusively from the auth barricade's context —
// never from request fields — and an absent identity fails closed inside
// knowledgeauth. The denial is opaque (no role oracle).
func (s *Server) authorizeKnowledgeWrite(ctx context.Context, roles ...string) error {
	id, _ := authgrpc.IdentityFrom(ctx)
	for _, role := range roles {
		if s.KnowledgeAuth.AuthorizeWrite(id, role) == nil {
			return nil
		}
	}
	return status.Error(codes.PermissionDenied, "not authorized for this knowledge operation")
}

// KnowledgeIngest implements engrampb.EngramServer: bulk-upsert one harvest
// batch (DW-6.1). Requires the harvester or admin role (DW-6.3).
func (s *Server) KnowledgeIngest(ctx context.Context, req *engrampb.KnowledgeIngestRequest) (*engrampb.KnowledgeIngestResponse, error) {
	if !s.knowledgeConfigured() || s.KnowledgeWriter == nil {
		return nil, errKnowledgeUnconfigured
	}
	if err := s.authorizeKnowledgeWrite(ctx, RoleHarvester, RoleKnowledgeAdmin); err != nil {
		return nil, err
	}
	if req.GetSource() == "" || req.GetHarvestId() == "" {
		return nil, status.Error(codes.InvalidArgument, "source and harvest_id are required")
	}
	spec, err := s.resolveCollection(ctx, req.GetCollection())
	if err != nil {
		return nil, err
	}
	docs := make([]knowledge.Document, len(req.GetDocs()))
	for i, d := range req.GetDocs() {
		if d.GetId() == "" {
			return nil, status.Errorf(codes.InvalidArgument, "doc at position %d has an empty id", i)
		}
		// Batch-level provenance is injected into every row's Fields here (the
		// Phase-4 BulkIndex contract): collection and source back the
		// mark-and-sweep delete's term filters.
		fields := d.GetFields().AsMap()
		if fields == nil {
			fields = make(map[string]any, 2)
		}
		fields["collection"] = spec.Name
		fields["source"] = req.GetSource()
		docs[i] = knowledge.Document{
			ID:            d.GetId(),
			Title:         d.GetTitle(),
			Text:          d.GetText(),
			SourceVersion: d.GetSourceVersion(),
			Fields:        fields,
		}
	}
	indexed, err := s.KnowledgeWriter.BulkIndex(ctx, spec.Index, spec.TextField, docs, req.GetHarvestId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "ingesting knowledge batch: %v", err)
	}
	return &engrampb.KnowledgeIngestResponse{Indexed: int32(indexed)}, nil
}

// KnowledgeSearch implements engrampb.EngramServer: one BM25 query over a
// collection with validated generic filters and sort (DW-6.1). Reads require
// the collection's access policy to admit the caller (DW-6.2).
func (s *Server) KnowledgeSearch(ctx context.Context, req *engrampb.KnowledgeSearchRequest) (*engrampb.KnowledgeSearchResponse, error) {
	if !s.knowledgeConfigured() || s.KnowledgeReader == nil {
		return nil, errKnowledgeUnconfigured
	}
	spec, err := s.resolveCollection(ctx, req.GetCollection())
	if err != nil {
		return nil, err
	}
	id, _ := authgrpc.IdentityFrom(ctx)
	if err := s.KnowledgeAuth.AuthorizeRead(id, spec.Access.Public, spec.Access.Roles); err != nil {
		return nil, status.Error(codes.PermissionDenied, "not authorized to read this collection")
	}
	filters, err := predicatesFromProto(spec, req.GetFilters())
	if err != nil {
		return nil, err
	}
	sortKeys, err := sortKeysFromProto(spec, req.GetSort())
	if err != nil {
		return nil, err
	}
	// k is bounded by the retriever's clamp to [1, MaxK]; negative/zero means
	// "server-chosen" by contract, so it passes through rather than erroring.
	hits, err := s.KnowledgeReader.Search(ctx, spec, req.GetQuery(), filters, sortKeys, int(req.GetK()))
	if err != nil {
		// The barricade already validated shape; a retriever failure here is
		// infrastructure, not caller input.
		return nil, status.Errorf(codes.Internal, "searching collection %q: %v", spec.Name, err)
	}
	out := make([]*engrampb.Hit, len(hits))
	for i, h := range hits {
		fieldsJSON, err := json.Marshal(h.Fields)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "encoding hit %s fields: %v", h.ID, err)
		}
		out[i] = &engrampb.Hit{Id: h.ID, Score: h.Score, Source: h.Source, FieldsJson: string(fieldsJSON)}
	}
	return &engrampb.KnowledgeSearchResponse{Hits: out}, nil
}

// KnowledgeCollections implements engrampb.EngramServer: it lists ONLY the
// collections the caller may read (unauthorized ones are silently absent —
// leak-free), each with its spec, document count, and staleness (DW-6.4).
func (s *Server) KnowledgeCollections(ctx context.Context, _ *engrampb.KnowledgeCollectionsRequest) (*engrampb.KnowledgeCollectionsResponse, error) {
	if !s.knowledgeConfigured() || s.KnowledgeReader == nil {
		return nil, errKnowledgeUnconfigured
	}
	metas, err := s.KnowledgeReader.Collections(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing knowledge collections: %v", err)
	}
	id, _ := authgrpc.IdentityFrom(ctx)
	resp := &engrampb.KnowledgeCollectionsResponse{}
	for _, meta := range metas {
		spec, err := s.Registry.Get(ctx, meta.Name)
		if err != nil {
			if errors.Is(err, knowledge.ErrNotFound) {
				continue // deleted between enumeration and Get; skip, don't fail the page
			}
			return nil, status.Errorf(codes.Internal, "resolving collection %q: %v", meta.Name, err)
		}
		if s.KnowledgeAuth.AuthorizeRead(id, spec.Access.Public, spec.Access.Roles) != nil {
			continue // not readable by this caller: invisible, not an error
		}
		info := &engrampb.CollectionInfo{Spec: collectionSpecProto(spec), Count: meta.Count}
		if meta.NewestHarvestedAt != nil {
			info.NewestHarvestedAt = timestamppb.New(*meta.NewestHarvestedAt)
		}
		if meta.NewestDocDate != nil {
			info.NewestDocDate = timestamppb.New(*meta.NewestDocDate)
		}
		resp.Collections = append(resp.Collections, info)
	}
	return resp, nil
}

// KnowledgeDelete implements engrampb.EngramServer: the mark-and-sweep
// delete (DW-6.1). Requires the harvester or admin role (DW-6.3).
func (s *Server) KnowledgeDelete(ctx context.Context, req *engrampb.KnowledgeDeleteRequest) (*engrampb.KnowledgeDeleteResponse, error) {
	if !s.knowledgeConfigured() || s.KnowledgeWriter == nil {
		return nil, errKnowledgeUnconfigured
	}
	if err := s.authorizeKnowledgeWrite(ctx, RoleHarvester, RoleKnowledgeAdmin); err != nil {
		return nil, err
	}
	if req.GetSource() == "" || req.GetCurrentHarvestId() == "" {
		return nil, status.Error(codes.InvalidArgument, "source and current_harvest_id are required")
	}
	spec, err := s.resolveCollection(ctx, req.GetCollection())
	if err != nil {
		return nil, err
	}
	deleted, err := s.KnowledgeWriter.DeleteByQuery(ctx, spec.Index, spec.Name, req.GetSource(), req.GetCurrentHarvestId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "sweeping collection %q: %v", spec.Name, err)
	}
	return &engrampb.KnowledgeDeleteResponse{Deleted: int64(deleted)}, nil
}

// CreateCollection implements engrampb.EngramServer: register + provision a
// new collection (DW-6.1). Admin role only (DW-6.3).
func (s *Server) CreateCollection(ctx context.Context, req *engrampb.CreateCollectionRequest) (*engrampb.CreateCollectionResponse, error) {
	if !s.knowledgeConfigured() {
		return nil, errKnowledgeUnconfigured
	}
	if err := s.authorizeKnowledgeWrite(ctx, RoleKnowledgeAdmin); err != nil {
		return nil, err
	}
	spec, err := collectionSpecFromProto(req.GetSpec())
	if err != nil {
		return nil, err
	}
	if err := s.Registry.Create(ctx, spec); err != nil {
		if errors.Is(err, knowledge.ErrConflict) {
			return nil, status.Errorf(codes.AlreadyExists, "collection %q already exists", spec.Name)
		}
		return nil, status.Errorf(codes.Internal, "creating collection %q: %v", spec.Name, err)
	}
	return &engrampb.CreateCollectionResponse{}, nil
}

// UpdateCollection implements engrampb.EngramServer: amend an existing
// collection's spec (DW-6.1). Admin role only (DW-6.3).
func (s *Server) UpdateCollection(ctx context.Context, req *engrampb.UpdateCollectionRequest) (*engrampb.UpdateCollectionResponse, error) {
	if !s.knowledgeConfigured() {
		return nil, errKnowledgeUnconfigured
	}
	if err := s.authorizeKnowledgeWrite(ctx, RoleKnowledgeAdmin); err != nil {
		return nil, err
	}
	spec, err := collectionSpecFromProto(req.GetSpec())
	if err != nil {
		return nil, err
	}
	if err := s.Registry.Update(ctx, spec); err != nil {
		switch {
		case errors.Is(err, knowledge.ErrNotFound):
			return nil, status.Errorf(codes.NotFound, "collection %q not found", spec.Name)
		case errors.Is(err, knowledge.ErrConflict):
			return nil, status.Errorf(codes.AlreadyExists, "collection %q was modified concurrently; re-read and retry", spec.Name)
		default:
			return nil, status.Errorf(codes.Internal, "updating collection %q: %v", spec.Name, err)
		}
	}
	return &engrampb.UpdateCollectionResponse{}, nil
}

// --- proto <-> domain translation + barricade validation helpers.

// predicatesFromProto validates each filter against spec's mappings (field
// declared AND filterable) and its op/value shape, translating to the
// retriever's form. Errors are INVALID_ARGUMENT and name the valid filterable
// fields / ops so an LLM caller can self-correct (DW-6.4).
func predicatesFromProto(spec knowledge.CollectionSpec, filters []*engrampb.Predicate) ([]retrieval.Predicate, error) {
	if len(filters) == 0 {
		return nil, nil
	}
	out := make([]retrieval.Predicate, 0, len(filters))
	for _, p := range filters {
		fs, ok := spec.Mappings[p.GetField()]
		if !ok || !fs.Filterable {
			return nil, status.Errorf(codes.InvalidArgument,
				"unknown or unfilterable field %q on collection %q; valid filterable fields: %s",
				p.GetField(), spec.Name, fieldList(spec, func(f knowledge.FieldSpec) bool { return f.Filterable }))
		}
		pred := retrieval.Predicate{Field: p.GetField()}
		switch p.GetOp() {
		case engrampb.PredicateOp_PREDICATE_OP_TERM, engrampb.PredicateOp_PREDICATE_OP_PREFIX:
			if pred.Op = "term"; p.GetOp() == engrampb.PredicateOp_PREDICATE_OP_PREFIX {
				pred.Op = "prefix"
			}
			scalar := p.GetScalar()
			if scalar == nil {
				return nil, status.Errorf(codes.InvalidArgument, "%s filter on field %q requires a scalar value", pred.Op, pred.Field)
			}
			pred.Value = scalar.AsInterface()
		case engrampb.PredicateOp_PREDICATE_OP_RANGE:
			pred.Op = "range"
			rng := p.GetRange()
			if rng == nil || (rng.GetGte() == nil && rng.GetLte() == nil) {
				return nil, status.Errorf(codes.InvalidArgument, "range filter on field %q must set at least one of gte/lte", pred.Field)
			}
			bounds := make(map[string]any, 2)
			if v := rng.GetGte(); v != nil {
				bounds["gte"] = v.AsInterface()
			}
			if v := rng.GetLte(); v != nil {
				bounds["lte"] = v.AsInterface()
			}
			pred.Value = bounds
		default:
			return nil, status.Errorf(codes.InvalidArgument, "invalid filter op on field %q; valid ops: term, range, prefix", pred.Field)
		}
		out = append(out, pred)
	}
	return out, nil
}

// sortKeysFromProto validates each sort key against spec's mappings (field
// declared AND sortable) and its order, translating to the retriever's form.
func sortKeysFromProto(spec knowledge.CollectionSpec, keys []*engrampb.SortKey) ([]retrieval.SortKey, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	out := make([]retrieval.SortKey, 0, len(keys))
	for _, k := range keys {
		fs, ok := spec.Mappings[k.GetField()]
		if !ok || !fs.Sortable {
			return nil, status.Errorf(codes.InvalidArgument,
				"unknown or unsortable field %q on collection %q; valid sortable fields: %s",
				k.GetField(), spec.Name, fieldList(spec, func(f knowledge.FieldSpec) bool { return f.Sortable }))
		}
		var order string
		switch k.GetOrder() {
		case engrampb.SortOrder_SORT_ORDER_ASC:
			order = "asc"
		case engrampb.SortOrder_SORT_ORDER_DESC:
			order = "desc"
		default:
			return nil, status.Errorf(codes.InvalidArgument, "invalid sort order on field %q; valid orders: asc, desc", k.GetField())
		}
		out = append(out, retrieval.SortKey{Field: k.GetField(), Order: order})
	}
	return out, nil
}

// fieldList renders the sorted names of spec's mappings fields matching keep,
// for self-correcting validation errors.
func fieldList(spec knowledge.CollectionSpec, keep func(knowledge.FieldSpec) bool) string {
	var names []string
	for name, f := range spec.Mappings {
		if keep(f) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "(none declared)"
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// collectionSpecFromProto validates and translates a create/update spec.
// Index is deliberately never populated: it is registry-assigned, and the
// proto carries no index field at all (physical storage stays hidden).
func collectionSpecFromProto(p *engrampb.CollectionSpec) (knowledge.CollectionSpec, error) {
	if p.GetName() == "" {
		return knowledge.CollectionSpec{}, status.Error(codes.InvalidArgument, "spec.name is required")
	}
	spec := knowledge.CollectionSpec{
		Name:      p.GetName(),
		TextField: p.GetTextField(),
		Access:    knowledge.AccessPolicy{Public: p.GetAccess().GetPublic(), Roles: p.GetAccess().GetRoles()},
	}
	if m := p.GetMappings(); len(m) > 0 {
		spec.Mappings = make(map[string]knowledge.FieldSpec, len(m))
		for name, f := range m {
			spec.Mappings[name] = knowledge.FieldSpec{Type: f.GetType(), Filterable: f.GetFilterable(), Sortable: f.GetSortable()}
		}
	}
	return spec, nil
}

// collectionSpecProto renders a domain spec for the wire, omitting Index.
func collectionSpecProto(spec knowledge.CollectionSpec) *engrampb.CollectionSpec {
	out := &engrampb.CollectionSpec{
		Name:      spec.Name,
		TextField: spec.TextField,
		Access:    &engrampb.AccessPolicy{Public: spec.Access.Public, Roles: spec.Access.Roles},
	}
	if len(spec.Mappings) > 0 {
		out.Mappings = make(map[string]*engrampb.FieldSpec, len(spec.Mappings))
		for name, f := range spec.Mappings {
			out.Mappings[name] = &engrampb.FieldSpec{Type: f.Type, Filterable: f.Filterable, Sortable: f.Sortable}
		}
	}
	return out
}

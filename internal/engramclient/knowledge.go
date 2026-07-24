// Knowledge Backend methods (knowledge platform Phase 6): the real gRPC
// adapter for the six knowledge operations, replacing the Phase-1 stubs. Each
// method is pure translation — mcp DTOs to/from engrampb messages — with
// client-side shape errors (unsupported op, non-map range value) returned
// before dialing so an MCP caller gets a cheap, descriptive failure; the
// server barricade remains the authority on everything semantic.

package engramclient

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/mcp"
)

// KnowledgeIngest bulk-upserts one harvest batch and returns how many docs
// were indexed.
func (c *Client) KnowledgeIngest(ctx context.Context, collection, source, harvestID string, docs []mcp.KnowledgeDoc) (int, error) {
	req := &engrampb.KnowledgeIngestRequest{
		Collection: collection,
		Source:     source,
		HarvestId:  harvestID,
		Docs:       make([]*engrampb.KnowledgeDocument, len(docs)),
	}
	for i, d := range docs {
		doc := &engrampb.KnowledgeDocument{
			Id:            d.ID,
			Title:         d.Title,
			Text:          d.Text,
			SourceVersion: d.SourceVersion,
		}
		if len(d.Fields) > 0 {
			fields, err := structpb.NewStruct(d.Fields)
			if err != nil {
				return 0, fmt.Errorf("engramclient: encoding fields of doc %q: %w", d.ID, err)
			}
			doc.Fields = fields
		}
		req.Docs[i] = doc
	}
	resp, err := c.api.KnowledgeIngest(ctx, req)
	if err != nil {
		return 0, err
	}
	return int(resp.GetIndexed()), nil
}

// KnowledgeSearch runs one BM25 query over a collection with generic field
// filters and sort, paged via offset (0 = first page) and returning the
// exact total match count (OpenSearch track_total_hits, never a capped
// estimate). By default each hit carries extracted fragments and a
// fields_json WITHOUT the document body (drill the whole document with
// Read(id, collection)); fullBody=true restores whole bodies inline and
// yields no fragments.
func (c *Client) KnowledgeSearch(ctx context.Context, collection, query string, filters []mcp.Predicate, sortKeys []mcp.SortKey, k, offset int, fullBody bool) ([]mcp.KnowledgeHit, int64, error) {
	req := &engrampb.KnowledgeSearchRequest{Collection: collection, Query: query, K: int32(k), Offset: int32(offset), FullBody: fullBody}
	for _, p := range filters {
		pred, err := predicateProto(p)
		if err != nil {
			return nil, 0, err
		}
		req.Filters = append(req.Filters, pred)
	}
	for _, sk := range sortKeys {
		key, err := sortKeyProto(sk)
		if err != nil {
			return nil, 0, err
		}
		req.Sort = append(req.Sort, key)
	}
	resp, err := c.api.KnowledgeSearch(ctx, req)
	if err != nil {
		return nil, 0, err
	}
	hits := make([]mcp.KnowledgeHit, 0, len(resp.GetHits()))
	for _, h := range resp.GetHits() {
		hits = append(hits, mcp.KnowledgeHit{
			ID: h.GetId(), Score: h.GetScore(), Collection: h.GetCollection(),
			Fields: h.GetFieldsJson(), Fragments: h.GetFragments(),
		})
	}
	return hits, resp.GetTotal(), nil
}

// KnowledgeCollections lists the collections the caller may read, with field
// mappings, document count, and staleness.
func (c *Client) KnowledgeCollections(ctx context.Context) ([]mcp.CollectionInfo, error) {
	resp, err := c.api.KnowledgeCollections(ctx, &engrampb.KnowledgeCollectionsRequest{})
	if err != nil {
		return nil, err
	}
	infos := make([]mcp.CollectionInfo, 0, len(resp.GetCollections()))
	for _, ci := range resp.GetCollections() {
		info := mcp.CollectionInfo{
			CollectionSpec: collectionSpecFromProto(ci.GetSpec()),
			Count:          ci.GetCount(),
		}
		info.NewestHarvestedAt = timeFromProto(ci.GetNewestHarvestedAt())
		info.NewestDocDate = timeFromProto(ci.GetNewestDocDate())
		infos = append(infos, info)
	}
	return infos, nil
}

// KnowledgeDelete runs the mark-and-sweep delete and reports the count.
func (c *Client) KnowledgeDelete(ctx context.Context, collection, source, currentHarvestID string) (int, error) {
	resp, err := c.api.KnowledgeDelete(ctx, &engrampb.KnowledgeDeleteRequest{
		Collection: collection, Source: source, CurrentHarvestId: currentHarvestID,
	})
	if err != nil {
		return 0, err
	}
	return int(resp.GetDeleted()), nil
}

// CreateCollection registers a new collection and provisions its backing
// index live.
func (c *Client) CreateCollection(ctx context.Context, spec mcp.CollectionSpec) error {
	_, err := c.api.CreateCollection(ctx, &engrampb.CreateCollectionRequest{Spec: collectionSpecProto(spec)})
	return err
}

// UpdateCollection amends an existing collection's spec.
func (c *Client) UpdateCollection(ctx context.Context, spec mcp.CollectionSpec) error {
	_, err := c.api.UpdateCollection(ctx, &engrampb.UpdateCollectionRequest{Spec: collectionSpecProto(spec)})
	return err
}

// IsAlreadyExists reports whether err is the AlreadyExists gRPC status
// CreateCollection returns when the named collection is already registered
// -- the caller-facing predicate for tolerating that conflict on a re-run
// without depending on gRPC directly. internal/cli (and any other business
// package) sits outside the transport boundary (internal/importlint);
// engramclient is the one edge allowed to know about gRPC status codes, so
// that classification belongs here rather than at each caller.
func IsAlreadyExists(err error) bool { return status.Code(err) == codes.AlreadyExists }

// IsPermissionDenied reports whether err is the PermissionDenied gRPC status
// a knowledge write or read returns when the caller's token lacks the
// required role (see internal/knowledgeauth).
func IsPermissionDenied(err error) bool { return status.Code(err) == codes.PermissionDenied }

// --- mcp DTO <-> proto translation helpers.

// predicateProto translates one generic filter. term/prefix carry a scalar;
// range requires a map value with at least one of gte/lte.
func predicateProto(p mcp.Predicate) (*engrampb.Predicate, error) {
	out := &engrampb.Predicate{Field: p.Field}
	switch p.Op {
	case "term", "prefix":
		if out.Op = engrampb.PredicateOp_PREDICATE_OP_TERM; p.Op == "prefix" {
			out.Op = engrampb.PredicateOp_PREDICATE_OP_PREFIX
		}
		scalar, err := structpb.NewValue(p.Value)
		if err != nil {
			return nil, fmt.Errorf("engramclient: encoding %s filter value on field %q: %w", p.Op, p.Field, err)
		}
		out.Value = &engrampb.Predicate_Scalar{Scalar: scalar}
	case "range":
		out.Op = engrampb.PredicateOp_PREDICATE_OP_RANGE
		bounds, ok := p.Value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("engramclient: range filter on field %q requires a value object with gte/lte bounds, got %T", p.Field, p.Value)
		}
		rng := &engrampb.Range{}
		for bound, set := range map[string]func(*structpb.Value){
			"gte": func(v *structpb.Value) { rng.Gte = v },
			"lte": func(v *structpb.Value) { rng.Lte = v },
		} {
			raw, present := bounds[bound]
			if !present {
				continue
			}
			v, err := structpb.NewValue(raw)
			if err != nil {
				return nil, fmt.Errorf("engramclient: encoding range %s on field %q: %w", bound, p.Field, err)
			}
			set(v)
		}
		if rng.Gte == nil && rng.Lte == nil {
			return nil, fmt.Errorf("engramclient: range filter on field %q must set at least one of gte/lte", p.Field)
		}
		out.Value = &engrampb.Predicate_Range{Range: rng}
	default:
		return nil, fmt.Errorf("engramclient: unsupported filter op %q on field %q; valid ops: term, range, prefix", p.Op, p.Field)
	}
	return out, nil
}

// sortKeyProto translates one sort key; order must be asc or desc.
func sortKeyProto(k mcp.SortKey) (*engrampb.SortKey, error) {
	out := &engrampb.SortKey{Field: k.Field}
	switch k.Order {
	case "asc":
		out.Order = engrampb.SortOrder_SORT_ORDER_ASC
	case "desc":
		out.Order = engrampb.SortOrder_SORT_ORDER_DESC
	default:
		return nil, fmt.Errorf("engramclient: invalid sort order %q on field %q; valid orders: asc, desc", k.Order, k.Field)
	}
	return out, nil
}

// collectionSpecProto flattens the mcp spec (Public/Roles) into the proto's
// AccessPolicy; there is no index field to carry (registry-internal).
func collectionSpecProto(spec mcp.CollectionSpec) *engrampb.CollectionSpec {
	out := &engrampb.CollectionSpec{
		Name:              spec.Name,
		TextField:         spec.TextField,
		Access:            &engrampb.AccessPolicy{Public: spec.Public, Roles: spec.Roles},
		FragmentSize:      int32(spec.FragmentSize),
		NumberOfFragments: int32(spec.NumberOfFragments),
		HighlightPreTag:   spec.HighlightPreTag,
		HighlightPostTag:  spec.HighlightPostTag,
	}
	if len(spec.Mappings) > 0 {
		out.Mappings = make(map[string]*engrampb.FieldSpec, len(spec.Mappings))
		for name, f := range spec.Mappings {
			out.Mappings[name] = &engrampb.FieldSpec{Type: f.Type, Filterable: f.Filterable, Sortable: f.Sortable}
		}
	}
	return out
}

// collectionSpecFromProto is collectionSpecProto's inverse.
func collectionSpecFromProto(p *engrampb.CollectionSpec) mcp.CollectionSpec {
	spec := mcp.CollectionSpec{
		Name:              p.GetName(),
		TextField:         p.GetTextField(),
		Public:            p.GetAccess().GetPublic(),
		Roles:             p.GetAccess().GetRoles(),
		FragmentSize:      int(p.GetFragmentSize()),
		NumberOfFragments: int(p.GetNumberOfFragments()),
		HighlightPreTag:   p.GetHighlightPreTag(),
		HighlightPostTag:  p.GetHighlightPostTag(),
	}
	if m := p.GetMappings(); len(m) > 0 {
		spec.Mappings = make(map[string]mcp.FieldSpec, len(m))
		for name, f := range m {
			spec.Mappings[name] = mcp.FieldSpec{Type: f.GetType(), Filterable: f.GetFilterable(), Sortable: f.GetSortable()}
		}
	}
	return spec
}

// timeFromProto converts an optional proto timestamp to *time.Time (nil for
// unset — an empty or undated collection).
func timeFromProto(ts *timestamppb.Timestamp) *time.Time {
	if ts == nil {
		return nil
	}
	t := ts.AsTime()
	return &t
}

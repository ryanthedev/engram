package server_test

// Phase-6 barricade tests for the six knowledge gRPC handlers: read
// authorization (DW-6.2), write authorization (DW-6.3), collections
// count/staleness + malformed-filter self-correction (DW-6.4), request
// translation into the inner seams, and domain-sentinel -> gRPC-code mapping.
// Seams are faked (consumer-defined interfaces); the live-cluster path is
// covered by knowledge_integration_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/authgrpc"
	"github.com/ryanthedev/engram/internal/knowledge"
	"github.com/ryanthedev/engram/internal/retrieval"
	"github.com/ryanthedev/engram/internal/server"
)

// fakeRegistry is a map-backed knowledge.CollectionRegistry.
type fakeRegistry struct {
	specs     map[string]knowledge.CollectionSpec
	createErr error
	updateErr error
	created   *knowledge.CollectionSpec
	updated   *knowledge.CollectionSpec
}

func (r *fakeRegistry) Get(_ context.Context, name string) (knowledge.CollectionSpec, error) {
	spec, ok := r.specs[name]
	if !ok {
		return knowledge.CollectionSpec{}, fmt.Errorf("fake registry: %q: %w", name, knowledge.ErrNotFound)
	}
	return spec, nil
}

func (r *fakeRegistry) Create(_ context.Context, spec knowledge.CollectionSpec) error {
	r.created = &spec
	return r.createErr
}

func (r *fakeRegistry) Update(_ context.Context, spec knowledge.CollectionSpec) error {
	r.updated = &spec
	return r.updateErr
}

func (r *fakeRegistry) List(context.Context) ([]knowledge.CollectionSummary, error) {
	var out []knowledge.CollectionSummary
	for name, spec := range r.specs {
		out = append(out, knowledge.CollectionSummary{Name: name, Access: spec.Access})
	}
	return out, nil
}

func (r *fakeRegistry) Provision(context.Context, string) error { return nil }

// fakeKnowledgeWriter records the store-facing write calls.
type fakeKnowledgeWriter struct {
	index, textField, harvestID        string
	docs                               []knowledge.Document
	delIndex, delCollection, delSource string
	delHarvestID                       string
	deleted                            int
	err                                error
}

func (w *fakeKnowledgeWriter) BulkIndex(_ context.Context, index, textField string, docs []knowledge.Document, harvestID string) (int, error) {
	w.index, w.textField, w.docs, w.harvestID = index, textField, docs, harvestID
	return len(docs), w.err
}

func (w *fakeKnowledgeWriter) DeleteByQuery(_ context.Context, index, collection, source, currentHarvestID string) (int, error) {
	w.delIndex, w.delCollection, w.delSource, w.delHarvestID = index, collection, source, currentHarvestID
	return w.deleted, w.err
}

// fakeKnowledgeReader records the retriever-facing read calls.
type fakeKnowledgeReader struct {
	spec    knowledge.CollectionSpec
	query   string
	filters []retrieval.Predicate
	sort    []retrieval.SortKey
	k       int
	hits    []retrieval.Hit
	metas   []retrieval.CollectionMeta
	err     error
}

func (r *fakeKnowledgeReader) Search(_ context.Context, spec knowledge.CollectionSpec, query string, filters []retrieval.Predicate, sortKeys []retrieval.SortKey, k int) ([]retrieval.Hit, error) {
	r.spec, r.query, r.filters, r.sort, r.k = spec, query, filters, sortKeys, k
	return r.hits, r.err
}

func (r *fakeKnowledgeReader) Collections(context.Context) ([]retrieval.CollectionMeta, error) {
	return r.metas, r.err
}

// papersSpec is the shared role-gated test collection.
func papersSpec(public bool, roles ...string) knowledge.CollectionSpec {
	return knowledge.CollectionSpec{
		Name:      "papers",
		Index:     "knowledge-papers",
		TextField: "abstract",
		Mappings: map[string]knowledge.FieldSpec{
			"year":     {Type: "integer", Filterable: true, Sortable: true},
			"category": {Type: "keyword", Filterable: true},
			"body":     {Type: "text"}, // declared but neither filterable nor sortable
		},
		Access: knowledge.AccessPolicy{Public: public, Roles: roles},
	}
}

// knowledgeServer builds a wired Server over fakes.
func knowledgeServer(reg *fakeRegistry, w *fakeKnowledgeWriter, r *fakeKnowledgeReader) *server.Server {
	return &server.Server{Registry: reg, KnowledgeWriter: w, KnowledgeReader: r}
}

// identityCtx returns a context carrying a verified identity with roles, the
// way the auth interceptor injects it.
func identityCtx(roles ...string) context.Context {
	return authgrpc.WithIdentity(context.Background(),
		auth.Identity{TenantID: "t1", UserID: "alice", AgentID: "claude", Roles: roles})
}

// TestDW_6_2_ReadAuthorization pins the read barricade: a role-gated
// collection denies a caller without the role, admits one holding it, and a
// public collection admits any authenticated caller.
func TestDW_6_2_ReadAuthorization(t *testing.T) {
	tests := []struct {
		name     string
		public   bool
		gate     []string
		ctx      context.Context
		wantCode codes.Code
	}{
		{"gated read without the role is denied", false, []string{"curator"}, identityCtx("reader"), codes.PermissionDenied},
		{"gated read with the role succeeds", false, []string{"curator"}, identityCtx("curator"), codes.OK},
		{"public read succeeds for any authenticated caller", true, nil, identityCtx(), codes.OK},
		{"unauthenticated caller is denied even when public", true, nil, context.Background(), codes.PermissionDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &fakeRegistry{specs: map[string]knowledge.CollectionSpec{"papers": papersSpec(tt.public, tt.gate...)}}
			s := knowledgeServer(reg, &fakeKnowledgeWriter{}, &fakeKnowledgeReader{})
			_, err := s.KnowledgeSearch(tt.ctx, &engrampb.KnowledgeSearchRequest{Collection: "papers", Query: "q"})
			if got := status.Code(err); got != tt.wantCode {
				t.Errorf("KnowledgeSearch = %v (code %v), want %v", err, got, tt.wantCode)
			}
		})
	}
}

// TestDW_6_3_WriteAuthorization pins the write barricade: ingest/delete need
// harvester or admin; create/update need admin — anything less is
// PermissionDenied, and the denial is checked BEFORE argument validation (an
// unauthorized caller learns nothing about its request).
func TestDW_6_3_WriteAuthorization(t *testing.T) {
	reg := &fakeRegistry{specs: map[string]knowledge.CollectionSpec{"papers": papersSpec(true)}}
	spec := &engrampb.CollectionSpec{Name: "papers"}
	calls := map[string]func(ctx context.Context, s *server.Server) error{
		"KnowledgeIngest": func(ctx context.Context, s *server.Server) error {
			_, err := s.KnowledgeIngest(ctx, &engrampb.KnowledgeIngestRequest{
				Collection: "papers", Source: "feed", HarvestId: "h1",
				Docs: []*engrampb.KnowledgeDocument{{Id: "d1", Text: "t"}},
			})
			return err
		},
		"KnowledgeDelete": func(ctx context.Context, s *server.Server) error {
			_, err := s.KnowledgeDelete(ctx, &engrampb.KnowledgeDeleteRequest{
				Collection: "papers", Source: "feed", CurrentHarvestId: "h1",
			})
			return err
		},
		"CreateCollection": func(ctx context.Context, s *server.Server) error {
			_, err := s.CreateCollection(ctx, &engrampb.CreateCollectionRequest{Spec: spec})
			return err
		},
		"UpdateCollection": func(ctx context.Context, s *server.Server) error {
			_, err := s.UpdateCollection(ctx, &engrampb.UpdateCollectionRequest{Spec: spec})
			return err
		},
	}

	tests := []struct {
		name  string
		roles []string
		want  map[string]codes.Code
	}{
		{"no roles: every write is denied", nil, map[string]codes.Code{
			"KnowledgeIngest": codes.PermissionDenied, "KnowledgeDelete": codes.PermissionDenied,
			"CreateCollection": codes.PermissionDenied, "UpdateCollection": codes.PermissionDenied,
		}},
		{"reader role: every write is denied", []string{"reader"}, map[string]codes.Code{
			"KnowledgeIngest": codes.PermissionDenied, "KnowledgeDelete": codes.PermissionDenied,
			"CreateCollection": codes.PermissionDenied, "UpdateCollection": codes.PermissionDenied,
		}},
		{"harvester may ingest/delete but not manage collections", []string{server.RoleHarvester}, map[string]codes.Code{
			"KnowledgeIngest": codes.OK, "KnowledgeDelete": codes.OK,
			"CreateCollection": codes.PermissionDenied, "UpdateCollection": codes.PermissionDenied,
		}},
		{"admin may do all four", []string{server.RoleKnowledgeAdmin}, map[string]codes.Code{
			"KnowledgeIngest": codes.OK, "KnowledgeDelete": codes.OK,
			"CreateCollection": codes.OK, "UpdateCollection": codes.OK,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for op, call := range calls {
				s := knowledgeServer(reg, &fakeKnowledgeWriter{}, &fakeKnowledgeReader{})
				err := call(identityCtx(tt.roles...), s)
				if got := status.Code(err); got != tt.want[op] {
					t.Errorf("%s with roles %v = %v (code %v), want %v", op, tt.roles, err, got, tt.want[op])
				}
			}
		})
	}
}

// TestDW_6_4_MalformedFilterNamesValidFields pins the self-correcting
// validation contract: a malformed filter or sort is INVALID_ARGUMENT and
// names the valid fields/ops/orders so an LLM caller can fix itself.
func TestDW_6_4_MalformedFilterNamesValidFields(t *testing.T) {
	scalar := func(v any) *engrampb.Predicate_Scalar {
		pv, _ := structpb.NewValue(v)
		return &engrampb.Predicate_Scalar{Scalar: pv}
	}
	tests := []struct {
		name    string
		filters []*engrampb.Predicate
		sort    []*engrampb.SortKey
		wantIn  string
	}{
		{"unknown filter field names the valid ones",
			[]*engrampb.Predicate{{Field: "yr", Op: engrampb.PredicateOp_PREDICATE_OP_TERM, Value: scalar(2026)}},
			nil, "valid filterable fields: category, year"},
		{"unfilterable declared field names the valid ones",
			[]*engrampb.Predicate{{Field: "body", Op: engrampb.PredicateOp_PREDICATE_OP_TERM, Value: scalar("x")}},
			nil, "valid filterable fields: category, year"},
		{"unspecified op names the valid ops",
			[]*engrampb.Predicate{{Field: "year", Value: scalar(2026)}},
			nil, "valid ops: term, range, prefix"},
		{"term without a scalar value",
			[]*engrampb.Predicate{{Field: "year", Op: engrampb.PredicateOp_PREDICATE_OP_TERM}},
			nil, "requires a scalar value"},
		{"range without bounds",
			[]*engrampb.Predicate{{Field: "year", Op: engrampb.PredicateOp_PREDICATE_OP_RANGE,
				Value: &engrampb.Predicate_Range{Range: &engrampb.Range{}}}},
			nil, "at least one of gte/lte"},
		{"unsortable sort field names the sortable ones",
			nil, []*engrampb.SortKey{{Field: "category", Order: engrampb.SortOrder_SORT_ORDER_ASC}},
			"valid sortable fields: year"},
		{"unspecified sort order names the valid orders",
			nil, []*engrampb.SortKey{{Field: "year"}},
			"valid orders: asc, desc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &fakeRegistry{specs: map[string]knowledge.CollectionSpec{"papers": papersSpec(true)}}
			s := knowledgeServer(reg, &fakeKnowledgeWriter{}, &fakeKnowledgeReader{})
			_, err := s.KnowledgeSearch(identityCtx(), &engrampb.KnowledgeSearchRequest{
				Collection: "papers", Query: "q", Filters: tt.filters, Sort: tt.sort,
			})
			if got := status.Code(err); got != codes.InvalidArgument {
				t.Fatalf("code = %v (%v), want InvalidArgument", got, err)
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("error %q does not contain %q", err, tt.wantIn)
			}
		})
	}
}

// TestUnknownCollectionIsInvalidArgumentNamingIt pins the plan's edge case
// for both read and write paths.
func TestUnknownCollectionIsInvalidArgumentNamingIt(t *testing.T) {
	reg := &fakeRegistry{specs: map[string]knowledge.CollectionSpec{}}
	s := knowledgeServer(reg, &fakeKnowledgeWriter{}, &fakeKnowledgeReader{})

	_, err := s.KnowledgeSearch(identityCtx(), &engrampb.KnowledgeSearchRequest{Collection: "ghost", Query: "q"})
	if got := status.Code(err); got != codes.InvalidArgument || !strings.Contains(err.Error(), `"ghost"`) {
		t.Errorf("search unknown collection = %v (code %v), want InvalidArgument naming it", err, got)
	}

	_, err = s.KnowledgeIngest(identityCtx(server.RoleHarvester), &engrampb.KnowledgeIngestRequest{
		Collection: "ghost", Source: "feed", HarvestId: "h1",
	})
	if got := status.Code(err); got != codes.InvalidArgument || !strings.Contains(err.Error(), `"ghost"`) {
		t.Errorf("ingest unknown collection = %v (code %v), want InvalidArgument naming it", err, got)
	}
}

// TestKnowledgeSearchTranslatesRequestAndHits proves the handler passes the
// resolved spec, validated filters/sort, and k to the retriever, and maps
// hits (with fields_json) back out.
func TestKnowledgeSearchTranslatesRequestAndHits(t *testing.T) {
	reg := &fakeRegistry{specs: map[string]knowledge.CollectionSpec{"papers": papersSpec(true)}}
	reader := &fakeKnowledgeReader{hits: []retrieval.Hit{
		{ID: "d1", Score: 3.5, Source: "papers", Fields: map[string]any{"title": "T", "year": 2026}},
	}}
	s := knowledgeServer(reg, &fakeKnowledgeWriter{}, reader)

	gte, _ := structpb.NewValue(2024)
	term, _ := structpb.NewValue("cs.AI")
	resp, err := s.KnowledgeSearch(identityCtx(), &engrampb.KnowledgeSearchRequest{
		Collection: "papers",
		Query:      "transformers",
		Filters: []*engrampb.Predicate{
			{Field: "category", Op: engrampb.PredicateOp_PREDICATE_OP_TERM, Value: &engrampb.Predicate_Scalar{Scalar: term}},
			{Field: "year", Op: engrampb.PredicateOp_PREDICATE_OP_RANGE, Value: &engrampb.Predicate_Range{Range: &engrampb.Range{Gte: gte}}},
		},
		Sort: []*engrampb.SortKey{{Field: "year", Order: engrampb.SortOrder_SORT_ORDER_DESC}},
		K:    7,
	})
	if err != nil {
		t.Fatalf("KnowledgeSearch: %v", err)
	}
	if reader.spec.Name != "papers" || reader.spec.TextField != "abstract" {
		t.Errorf("retriever got spec %+v", reader.spec)
	}
	if reader.query != "transformers" || reader.k != 7 {
		t.Errorf("retriever got query=%q k=%d", reader.query, reader.k)
	}
	if len(reader.filters) != 2 || reader.filters[0].Op != "term" || reader.filters[0].Value != "cs.AI" {
		t.Errorf("filters = %+v", reader.filters)
	}
	bounds, _ := reader.filters[1].Value.(map[string]any)
	if reader.filters[1].Op != "range" || bounds["gte"] != float64(2024) {
		t.Errorf("range filter = %+v", reader.filters[1])
	}
	if len(reader.sort) != 1 || reader.sort[0] != (retrieval.SortKey{Field: "year", Order: "desc"}) {
		t.Errorf("sort = %+v", reader.sort)
	}
	if len(resp.GetHits()) != 1 {
		t.Fatalf("hits = %v", resp.GetHits())
	}
	h := resp.GetHits()[0]
	if h.GetId() != "d1" || h.GetScore() != 3.5 || h.GetSource() != "papers" {
		t.Errorf("hit = %v", h)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(h.GetFieldsJson()), &fields); err != nil || fields["title"] != "T" {
		t.Errorf("fields_json = %q (%v)", h.GetFieldsJson(), err)
	}
}

// TestKnowledgeIngestInjectsProvenanceAndTextField proves the handler injects
// collection/source into every doc's Fields (the Phase-4 BulkIndex contract)
// and passes the collection's configured TextField and index.
func TestKnowledgeIngestInjectsProvenanceAndTextField(t *testing.T) {
	reg := &fakeRegistry{specs: map[string]knowledge.CollectionSpec{"papers": papersSpec(true)}}
	w := &fakeKnowledgeWriter{}
	s := knowledgeServer(reg, w, &fakeKnowledgeReader{})

	fields, _ := structpb.NewStruct(map[string]any{"year": 2026})
	resp, err := s.KnowledgeIngest(identityCtx(server.RoleHarvester), &engrampb.KnowledgeIngestRequest{
		Collection: "papers", Source: "arxiv_oai", HarvestId: "h9",
		Docs: []*engrampb.KnowledgeDocument{
			{Id: "d1", Title: "T", Text: "abstract body", SourceVersion: "v3", Fields: fields},
			{Id: "d2", Text: "no fields doc"},
		},
	})
	if err != nil {
		t.Fatalf("KnowledgeIngest: %v", err)
	}
	if resp.GetIndexed() != 2 {
		t.Errorf("indexed = %d, want 2", resp.GetIndexed())
	}
	if w.index != "knowledge-papers" || w.textField != "abstract" || w.harvestID != "h9" {
		t.Errorf("writer got index=%q textField=%q harvestID=%q", w.index, w.textField, w.harvestID)
	}
	for i, d := range w.docs {
		if d.Fields["collection"] != "papers" || d.Fields["source"] != "arxiv_oai" {
			t.Errorf("doc %d missing injected provenance: %+v", i, d.Fields)
		}
	}
	if w.docs[0].Fields["year"] != float64(2026) || w.docs[0].SourceVersion != "v3" {
		t.Errorf("doc 0 lost caller fields: %+v", w.docs[0])
	}

	// Barricade: a doc without an id is InvalidArgument, not a store call.
	_, err = s.KnowledgeIngest(identityCtx(server.RoleHarvester), &engrampb.KnowledgeIngestRequest{
		Collection: "papers", Source: "s", HarvestId: "h",
		Docs: []*engrampb.KnowledgeDocument{{Text: "no id"}},
	})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Errorf("empty doc id = %v (code %v), want InvalidArgument", err, got)
	}
}

// TestKnowledgeDeleteTranslates proves the sweep call carries the resolved
// index plus the request's source and current harvest id.
func TestKnowledgeDeleteTranslates(t *testing.T) {
	reg := &fakeRegistry{specs: map[string]knowledge.CollectionSpec{"papers": papersSpec(true)}}
	w := &fakeKnowledgeWriter{deleted: 3}
	s := knowledgeServer(reg, w, &fakeKnowledgeReader{})

	resp, err := s.KnowledgeDelete(identityCtx(server.RoleKnowledgeAdmin), &engrampb.KnowledgeDeleteRequest{
		Collection: "papers", Source: "arxiv_oai", CurrentHarvestId: "h10",
	})
	if err != nil {
		t.Fatalf("KnowledgeDelete: %v", err)
	}
	if resp.GetDeleted() != 3 {
		t.Errorf("deleted = %d, want 3", resp.GetDeleted())
	}
	if w.delIndex != "knowledge-papers" || w.delCollection != "papers" || w.delSource != "arxiv_oai" || w.delHarvestID != "h10" {
		t.Errorf("writer got %q %q %q %q", w.delIndex, w.delCollection, w.delSource, w.delHarvestID)
	}
}

// TestDW_6_4_CollectionsCountAndStaleness proves the listing zips registry
// specs with retriever metas — count + both staleness timestamps — and
// silently omits collections the caller may not read (leak-free).
func TestDW_6_4_CollectionsCountAndStaleness(t *testing.T) {
	harvested := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	docDate := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	reg := &fakeRegistry{specs: map[string]knowledge.CollectionSpec{
		"pub":    {Name: "pub", Index: "knowledge-pub", TextField: "text", Access: knowledge.AccessPolicy{Public: true}},
		"secret": {Name: "secret", Index: "knowledge-secret", TextField: "text", Access: knowledge.AccessPolicy{Roles: []string{"curator"}}},
	}}
	reader := &fakeKnowledgeReader{metas: []retrieval.CollectionMeta{
		{Name: "pub", Count: 42, NewestHarvestedAt: &harvested, NewestDocDate: &docDate},
		{Name: "secret", Count: 7},
	}}
	s := knowledgeServer(reg, &fakeKnowledgeWriter{}, reader)

	resp, err := s.KnowledgeCollections(identityCtx(), &engrampb.KnowledgeCollectionsRequest{})
	if err != nil {
		t.Fatalf("KnowledgeCollections: %v", err)
	}
	if len(resp.GetCollections()) != 1 {
		t.Fatalf("caller without the role sees %d collections, want 1 (public only): %v", len(resp.GetCollections()), resp)
	}
	info := resp.GetCollections()[0]
	if info.GetSpec().GetName() != "pub" || info.GetCount() != 42 {
		t.Errorf("info = %v", info)
	}
	if !info.GetNewestHarvestedAt().AsTime().Equal(harvested) {
		t.Errorf("newest_harvested_at = %v, want %v", info.GetNewestHarvestedAt().AsTime(), harvested)
	}
	if !info.GetNewestDocDate().AsTime().Equal(docDate) {
		t.Errorf("newest_doc_date = %v, want %v", info.GetNewestDocDate().AsTime(), docDate)
	}

	// A curator sees both.
	resp, err = s.KnowledgeCollections(identityCtx("curator"), &engrampb.KnowledgeCollectionsRequest{})
	if err != nil {
		t.Fatalf("KnowledgeCollections(curator): %v", err)
	}
	if len(resp.GetCollections()) != 2 {
		t.Errorf("curator sees %d collections, want 2", len(resp.GetCollections()))
	}
}

// TestCollectionLifecycleErrorMapping pins the sentinel -> code map for
// create/update: ErrConflict -> AlreadyExists, ErrNotFound -> NotFound.
func TestCollectionLifecycleErrorMapping(t *testing.T) {
	reg := &fakeRegistry{
		specs:     map[string]knowledge.CollectionSpec{},
		createErr: fmt.Errorf("wrapped: %w", knowledge.ErrConflict),
		updateErr: fmt.Errorf("wrapped: %w", knowledge.ErrNotFound),
	}
	s := knowledgeServer(reg, &fakeKnowledgeWriter{}, &fakeKnowledgeReader{})
	ctx := identityCtx(server.RoleKnowledgeAdmin)

	_, err := s.CreateCollection(ctx, &engrampb.CreateCollectionRequest{Spec: &engrampb.CollectionSpec{Name: "dup"}})
	if got := status.Code(err); got != codes.AlreadyExists {
		t.Errorf("create conflict = %v (code %v), want AlreadyExists", err, got)
	}
	_, err = s.UpdateCollection(ctx, &engrampb.UpdateCollectionRequest{Spec: &engrampb.CollectionSpec{Name: "ghost"}})
	if got := status.Code(err); got != codes.NotFound {
		t.Errorf("update missing = %v (code %v), want NotFound", err, got)
	}
	// Spec translation: access + mappings survive into the registry call.
	reg.createErr = nil
	_, err = s.CreateCollection(ctx, &engrampb.CreateCollectionRequest{Spec: &engrampb.CollectionSpec{
		Name: "papers", TextField: "abstract",
		Mappings: map[string]*engrampb.FieldSpec{"year": {Type: "integer", Filterable: true, Sortable: true}},
		Access:   &engrampb.AccessPolicy{Roles: []string{"curator"}},
	}})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	if reg.created.TextField != "abstract" || !reg.created.Mappings["year"].Sortable || reg.created.Access.Roles[0] != "curator" {
		t.Errorf("created spec = %+v", reg.created)
	}
	if reg.created.Index != "" {
		t.Errorf("Index must be registry-assigned, got %q from the wire", reg.created.Index)
	}
	// Missing spec name is InvalidArgument.
	_, err = s.CreateCollection(ctx, &engrampb.CreateCollectionRequest{})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Errorf("empty spec = %v (code %v), want InvalidArgument", err, got)
	}
}

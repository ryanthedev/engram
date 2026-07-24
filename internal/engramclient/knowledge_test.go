package engramclient_test

// Phase-6 translation tests for the knowledge Backend methods: each method
// now performs a real gRPC call (replacing the Phase-1 stubs), so these spin
// an in-process fake EngramServer on a loopback listener and assert the
// mcp DTO <-> engrampb translation in both directions — values through the
// scalar/range oneof, spec flattening, timestamps, and hits.

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/engramclient"
	"github.com/ryanthedev/engram/internal/mcp"
)

// captureServer records the last knowledge request and returns canned
// responses.
type captureServer struct {
	engrampb.UnimplementedEngramServer
	ingestReq  *engrampb.KnowledgeIngestRequest
	searchReq  *engrampb.KnowledgeSearchRequest
	deleteReq  *engrampb.KnowledgeDeleteRequest
	createReq  *engrampb.CreateCollectionRequest
	updateReq  *engrampb.UpdateCollectionRequest
	searchResp *engrampb.KnowledgeSearchResponse
	listResp   *engrampb.KnowledgeCollectionsResponse
}

func (s *captureServer) KnowledgeIngest(_ context.Context, req *engrampb.KnowledgeIngestRequest) (*engrampb.KnowledgeIngestResponse, error) {
	s.ingestReq = req
	return &engrampb.KnowledgeIngestResponse{Indexed: int32(len(req.GetDocs()))}, nil
}

func (s *captureServer) KnowledgeSearch(_ context.Context, req *engrampb.KnowledgeSearchRequest) (*engrampb.KnowledgeSearchResponse, error) {
	s.searchReq = req
	return s.searchResp, nil
}

func (s *captureServer) KnowledgeCollections(context.Context, *engrampb.KnowledgeCollectionsRequest) (*engrampb.KnowledgeCollectionsResponse, error) {
	return s.listResp, nil
}

func (s *captureServer) KnowledgeDelete(_ context.Context, req *engrampb.KnowledgeDeleteRequest) (*engrampb.KnowledgeDeleteResponse, error) {
	s.deleteReq = req
	return &engrampb.KnowledgeDeleteResponse{Deleted: 5}, nil
}

func (s *captureServer) CreateCollection(_ context.Context, req *engrampb.CreateCollectionRequest) (*engrampb.CreateCollectionResponse, error) {
	s.createReq = req
	return &engrampb.CreateCollectionResponse{}, nil
}

func (s *captureServer) UpdateCollection(_ context.Context, req *engrampb.UpdateCollectionRequest) (*engrampb.UpdateCollectionResponse, error) {
	s.updateReq = req
	return &engrampb.UpdateCollectionResponse{}, nil
}

// dialCapture serves srv on a loopback listener and returns a connected
// Client.
func dialCapture(t *testing.T, srv *captureServer) *engramclient.Client {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcServer := grpc.NewServer()
	engrampb.RegisterEngramServer(grpcServer, srv)
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)

	client, err := engramclient.Dial(lis.Addr().String(), "test-token")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestKnowledgeIngestTranslation(t *testing.T) {
	srv := &captureServer{}
	c := dialCapture(t, srv)

	indexed, err := c.KnowledgeIngest(context.Background(), "papers", "feed", "h1", []mcp.KnowledgeDoc{
		{ID: "d1", Title: "T", Text: "body", SourceVersion: "v2", Fields: map[string]any{"year": 2026.0}},
		{ID: "d2", Text: "plain"},
	})
	if err != nil {
		t.Fatalf("KnowledgeIngest: %v", err)
	}
	if indexed != 2 {
		t.Errorf("indexed = %d, want 2", indexed)
	}
	req := srv.ingestReq
	if req.GetCollection() != "papers" || req.GetSource() != "feed" || req.GetHarvestId() != "h1" {
		t.Errorf("request envelope = %v", req)
	}
	d := req.GetDocs()[0]
	if d.GetId() != "d1" || d.GetTitle() != "T" || d.GetText() != "body" || d.GetSourceVersion() != "v2" {
		t.Errorf("doc 0 = %v", d)
	}
	if got := d.GetFields().AsMap()["year"]; got != 2026.0 {
		t.Errorf("fields.year = %v, want 2026", got)
	}
	if req.GetDocs()[1].GetFields() != nil {
		t.Errorf("doc without fields must marshal a nil struct, got %v", req.GetDocs()[1].GetFields())
	}
}

func TestKnowledgeSearchTranslation(t *testing.T) {
	srv := &captureServer{searchResp: &engrampb.KnowledgeSearchResponse{Hits: []*engrampb.KnowledgeHit{
		{Id: "d1", Score: 4.2, Collection: "papers", FieldsJson: `{"title":"T"}`},
	}, Total: 1}}
	c := dialCapture(t, srv)

	hits, err := c.KnowledgeSearch(context.Background(), "papers", "transformers",
		[]mcp.Predicate{
			{Field: "category", Op: "term", Value: "cs.AI"},
			{Field: "published", Op: "range", Value: map[string]any{"gte": "2026-01-01", "lte": "2026-07-10"}},
			{Field: "title", Op: "prefix", Value: "atten"},
		},
		[]mcp.SortKey{{Field: "published", Order: "desc"}}, 9)
	if err != nil {
		t.Fatalf("KnowledgeSearch: %v", err)
	}
	if len(hits) != 1 || hits[0] != (mcp.Hit{ID: "d1", Score: 4.2, Source: "papers", Fields: `{"title":"T"}`}) {
		t.Errorf("hits = %+v", hits)
	}
	req := srv.searchReq
	if req.GetCollection() != "papers" || req.GetQuery() != "transformers" || req.GetK() != 9 {
		t.Errorf("request envelope = %v", req)
	}
	f := req.GetFilters()
	if f[0].GetOp() != engrampb.PredicateOp_PREDICATE_OP_TERM || f[0].GetScalar().GetStringValue() != "cs.AI" {
		t.Errorf("term filter = %v", f[0])
	}
	if f[1].GetOp() != engrampb.PredicateOp_PREDICATE_OP_RANGE ||
		f[1].GetRange().GetGte().GetStringValue() != "2026-01-01" ||
		f[1].GetRange().GetLte().GetStringValue() != "2026-07-10" {
		t.Errorf("range filter = %v", f[1])
	}
	if f[2].GetOp() != engrampb.PredicateOp_PREDICATE_OP_PREFIX || f[2].GetScalar().GetStringValue() != "atten" {
		t.Errorf("prefix filter = %v", f[2])
	}
	if sk := req.GetSort()[0]; sk.GetField() != "published" || sk.GetOrder() != engrampb.SortOrder_SORT_ORDER_DESC {
		t.Errorf("sort = %v", sk)
	}
}

// TestKnowledgeSearchClientSideShapeErrors pins the cheap pre-wire failures:
// an unsupported op, a non-map range value, an empty range, and a bad sort
// order never reach the server and return descriptive errors.
func TestKnowledgeSearchClientSideShapeErrors(t *testing.T) {
	srv := &captureServer{}
	c := dialCapture(t, srv)
	tests := []struct {
		name string
		pred mcp.Predicate
		want string
	}{
		{"unsupported op", mcp.Predicate{Field: "year", Op: "match", Value: 1}, "valid ops: term, range, prefix"},
		{"range with scalar value", mcp.Predicate{Field: "year", Op: "range", Value: 2026}, "gte/lte"},
		{"range with no bounds", mcp.Predicate{Field: "year", Op: "range", Value: map[string]any{}}, "at least one of gte/lte"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.KnowledgeSearch(context.Background(), "papers", "q", []mcp.Predicate{tt.pred}, nil, 1)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
			if srv.searchReq != nil {
				t.Errorf("malformed predicate must not reach the wire: %v", srv.searchReq)
			}
		})
	}
	if _, err := c.KnowledgeSearch(context.Background(), "papers", "q", nil,
		[]mcp.SortKey{{Field: "year", Order: "up"}}, 1); err == nil || !strings.Contains(err.Error(), "valid orders: asc, desc") {
		t.Errorf("bad sort order error = %v", err)
	}
}

func TestKnowledgeCollectionsTranslation(t *testing.T) {
	harvested := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	srv := &captureServer{listResp: &engrampb.KnowledgeCollectionsResponse{Collections: []*engrampb.CollectionInfo{{
		Spec: &engrampb.CollectionSpec{
			Name: "papers", TextField: "abstract",
			Mappings: map[string]*engrampb.FieldSpec{"year": {Type: "integer", Filterable: true, Sortable: true}},
			Access:   &engrampb.AccessPolicy{Public: false, Roles: []string{"curator"}},
		},
		Count:             321,
		NewestHarvestedAt: timestamppb.New(harvested),
		// NewestDocDate deliberately unset: must come back nil, not zero-time.
	}}}}
	c := dialCapture(t, srv)

	infos, err := c.KnowledgeCollections(context.Background())
	if err != nil {
		t.Fatalf("KnowledgeCollections: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("infos = %+v", infos)
	}
	info := infos[0]
	if info.Name != "papers" || info.TextField != "abstract" || info.Count != 321 {
		t.Errorf("info = %+v", info)
	}
	if !info.Mappings["year"].Sortable || info.Roles[0] != "curator" || info.Public {
		t.Errorf("spec detail lost: %+v", info)
	}
	if info.NewestHarvestedAt == nil || !info.NewestHarvestedAt.Equal(harvested) {
		t.Errorf("NewestHarvestedAt = %v, want %v", info.NewestHarvestedAt, harvested)
	}
	if info.NewestDocDate != nil {
		t.Errorf("unset NewestDocDate must stay nil, got %v", info.NewestDocDate)
	}
}

func TestKnowledgeDeleteAndCollectionLifecycleTranslation(t *testing.T) {
	srv := &captureServer{}
	c := dialCapture(t, srv)

	deleted, err := c.KnowledgeDelete(context.Background(), "papers", "feed", "h3")
	if err != nil || deleted != 5 {
		t.Errorf("KnowledgeDelete = %d, %v; want 5, nil", deleted, err)
	}
	if srv.deleteReq.GetCurrentHarvestId() != "h3" || srv.deleteReq.GetSource() != "feed" {
		t.Errorf("delete request = %v", srv.deleteReq)
	}

	spec := mcp.CollectionSpec{
		Name: "papers", TextField: "abstract",
		Mappings: map[string]mcp.FieldSpec{"year": {Type: "integer", Filterable: true}},
		Public:   false, Roles: []string{"curator"},
	}
	if err := c.CreateCollection(context.Background(), spec); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	got := srv.createReq.GetSpec()
	if got.GetName() != "papers" || got.GetTextField() != "abstract" ||
		!got.GetMappings()["year"].GetFilterable() || got.GetAccess().GetRoles()[0] != "curator" {
		t.Errorf("create spec = %v", got)
	}
	spec.Public = true
	if err := c.UpdateCollection(context.Background(), spec); err != nil {
		t.Fatalf("UpdateCollection: %v", err)
	}
	if !srv.updateReq.GetSpec().GetAccess().GetPublic() {
		t.Errorf("update spec = %v", srv.updateReq.GetSpec())
	}
}

// TestKnowledgeScalarValueRoundTrip drives non-string scalars through
// structpb to guard numeric/bool handling in predicateProto, plus the
// unencodable-scalar client-side failure.
func TestKnowledgeScalarValueRoundTrip(t *testing.T) {
	srv := &captureServer{}
	c := dialCapture(t, srv)
	_, err := c.KnowledgeSearch(context.Background(), "papers", "", []mcp.Predicate{
		{Field: "year", Op: "term", Value: 2026.0},
		{Field: "flagged", Op: "term", Value: true},
	}, nil, 1)
	if err != nil {
		t.Fatalf("KnowledgeSearch: %v", err)
	}
	f := srv.searchReq.GetFilters()
	if f[0].GetScalar().GetNumberValue() != 2026.0 {
		t.Errorf("number scalar = %v", f[0].GetScalar())
	}
	if f[1].GetScalar().GetBoolValue() != true {
		t.Errorf("bool scalar = %v", f[1].GetScalar())
	}
	// An unencodable scalar (e.g. a channel) fails client-side, pre-wire.
	if _, err := c.KnowledgeSearch(context.Background(), "papers", "", []mcp.Predicate{
		{Field: "year", Op: "term", Value: make(chan int)},
	}, nil, 1); err == nil {
		t.Error("unencodable scalar must error client-side")
	}
}

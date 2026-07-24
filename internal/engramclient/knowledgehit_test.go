package engramclient_test

// Phase-1 (fragments-and-paging plan) proto-foundation tests: the
// KnowledgeHit message shape (DW-1.1) and the CollectionSpec sizing fields'
// lossless mcp <-> proto round-trip through collectionSpecProto /
// collectionSpecFromProto (DW-1.2). The translation functions are unexported,
// so the round-trip drives them through the public client surface over the
// in-process gRPC stub: CreateCollection encodes, KnowledgeCollections
// decodes.

import (
	"context"
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/mcp"
)

// TestDW_1_1_KnowledgeHitProtoRoundTrip pins the new wire shapes: a
// KnowledgeHit (id/score/collection/fields_json/fragments) inside a
// KnowledgeSearchResponse carrying total, and a KnowledgeSearchRequest
// carrying offset + full_body — all surviving a proto marshal/unmarshal
// round-trip without loss.
func TestDW_1_1_KnowledgeHitProtoRoundTrip(t *testing.T) {
	req := &engrampb.KnowledgeSearchRequest{
		Collection: "docs",
		Query:      "fragment extraction",
		K:          16,
		Offset:     32,
		FullBody:   true,
	}
	resp := &engrampb.KnowledgeSearchResponse{
		Hits: []*engrampb.KnowledgeHit{{
			Id:         "docs/readme.md",
			Score:      7.25,
			Collection: "docs",
			FieldsJson: `{"title":"README"}`,
			Fragments:  []string{"first extracted fragment", "second fragment"},
		}},
		Total: 1234,
	}

	reqBytes, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	gotReq := &engrampb.KnowledgeSearchRequest{}
	if err := proto.Unmarshal(reqBytes, gotReq); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if !proto.Equal(req, gotReq) {
		t.Errorf("request round-trip = %v, want %v", gotReq, req)
	}
	if gotReq.GetOffset() != 32 || !gotReq.GetFullBody() {
		t.Errorf("offset/full_body lost: offset=%d full_body=%v", gotReq.GetOffset(), gotReq.GetFullBody())
	}

	respBytes, err := proto.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	gotResp := &engrampb.KnowledgeSearchResponse{}
	if err := proto.Unmarshal(respBytes, gotResp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !proto.Equal(resp, gotResp) {
		t.Errorf("response round-trip = %v, want %v", gotResp, resp)
	}
	h := gotResp.GetHits()[0]
	if h.GetCollection() != "docs" || len(h.GetFragments()) != 2 || gotResp.GetTotal() != 1234 {
		t.Errorf("KnowledgeHit fields lost: %v total=%d", h, gotResp.GetTotal())
	}
}

// TestDW_1_2_CollectionSpecSizingRoundTrip drives an mcp.CollectionSpec with
// every sizing/tag field set through collectionSpecProto (CreateCollection's
// encode) and back through collectionSpecFromProto (KnowledgeCollections'
// decode), asserting nothing is lost or defaulted in translation — fallback
// application belongs to consumption (knowledge.CollectionSpec.FragmentSizing),
// never to the wire.
func TestDW_1_2_CollectionSpecSizingRoundTrip(t *testing.T) {
	in := mcp.CollectionSpec{
		Name:              "docs",
		TextField:         "text",
		Mappings:          map[string]mcp.FieldSpec{"title": {Type: "keyword", Filterable: true}},
		Public:            true,
		FragmentSize:      512,
		NumberOfFragments: 5,
		HighlightPreTag:   "<em>",
		HighlightPostTag:  "</em>",
	}

	srv := &captureServer{}
	c := dialCapture(t, srv)
	if err := c.CreateCollection(context.Background(), in); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	wire := srv.createReq.GetSpec()
	if wire.GetFragmentSize() != 512 || wire.GetNumberOfFragments() != 5 ||
		wire.GetHighlightPreTag() != "<em>" || wire.GetHighlightPostTag() != "</em>" {
		t.Fatalf("sizing fields lost on encode: %v", wire)
	}

	// Echo the captured wire spec back through the list path to exercise the
	// decode direction of the same translation pair.
	srv.listResp = &engrampb.KnowledgeCollectionsResponse{
		Collections: []*engrampb.CollectionInfo{{Spec: wire}},
	}
	infos, err := c.KnowledgeCollections(context.Background())
	if err != nil {
		t.Fatalf("KnowledgeCollections: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("collections = %v", infos)
	}
	if got := infos[0].CollectionSpec; !reflect.DeepEqual(got, in) {
		t.Errorf("round-tripped spec = %+v, want %+v", got, in)
	}
}

// TestDW_1_2_CollectionSpecZeroSizingStaysZeroOnWire pins the unset case: a
// spec with NO sizing set round-trips as zero (proto default), NOT as the
// 240/3 fallback — an eager fallback at translation would make "unset"
// indistinguishable from "explicitly 240/3" and lock every collection to the
// defaults forever.
func TestDW_1_2_CollectionSpecZeroSizingStaysZeroOnWire(t *testing.T) {
	in := mcp.CollectionSpec{Name: "docs", TextField: "text", Public: true}

	srv := &captureServer{}
	c := dialCapture(t, srv)
	if err := c.CreateCollection(context.Background(), in); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	wire := srv.createReq.GetSpec()
	if wire.GetFragmentSize() != 0 || wire.GetNumberOfFragments() != 0 ||
		wire.GetHighlightPreTag() != "" || wire.GetHighlightPostTag() != "" {
		t.Errorf("unset sizing must stay zero on the wire, got %v", wire)
	}

	srv.listResp = &engrampb.KnowledgeCollectionsResponse{
		Collections: []*engrampb.CollectionInfo{{Spec: wire}},
	}
	infos, err := c.KnowledgeCollections(context.Background())
	if err != nil {
		t.Fatalf("KnowledgeCollections: %v", err)
	}
	if got := infos[0].CollectionSpec; !reflect.DeepEqual(got, in) {
		t.Errorf("zero-sizing spec round-trip = %+v, want %+v", got, in)
	}
}

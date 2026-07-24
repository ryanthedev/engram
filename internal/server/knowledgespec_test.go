package server_test

// Phase-1 (fragments-and-paging plan) server-side DW-1.2 test: the
// knowledge.CollectionSpec sizing/tag fields round-trip through the SERVER's
// collectionSpecFromProto (CreateCollection decode) and collectionSpecProto
// (KnowledgeCollections encode) without loss. This is the second of the two
// translation pairs — the client pair is covered in
// internal/engramclient/knowledgehit_test.go.

import (
	"testing"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/knowledge"
	"github.com/ryanthedev/engram/internal/retrieval"
	"github.com/ryanthedev/engram/internal/server"
)

func TestDW_1_2_ServerCollectionSpecSizingRoundTrip(t *testing.T) {
	wire := &engrampb.CollectionSpec{
		Name:              "docs",
		TextField:         "text",
		Access:            &engrampb.AccessPolicy{Public: true},
		Mappings:          map[string]*engrampb.FieldSpec{"title": {Type: "keyword", Filterable: true}},
		FragmentSize:      512,
		NumberOfFragments: 5,
		HighlightPreTag:   "<em>",
		HighlightPostTag:  "</em>",
	}

	// Decode direction: CreateCollection -> collectionSpecFromProto -> registry.
	reg := &fakeRegistry{specs: map[string]knowledge.CollectionSpec{}}
	s := knowledgeServer(reg, &fakeKnowledgeWriter{}, &fakeKnowledgeReader{})
	if _, err := s.CreateCollection(identityCtx(server.RoleKnowledgeAdmin), &engrampb.CreateCollectionRequest{Spec: wire}); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	domain := *reg.created
	if domain.FragmentSize != 512 || domain.NumberOfFragments != 5 ||
		domain.HighlightPreTag != "<em>" || domain.HighlightPostTag != "</em>" {
		t.Fatalf("sizing fields lost on decode: %+v", domain)
	}

	// Encode direction: the created domain spec listed back out through
	// KnowledgeCollections -> collectionSpecProto.
	reg.specs["docs"] = domain
	reader := &fakeKnowledgeReader{metas: []retrieval.CollectionMeta{{Name: "docs", Count: 2}}}
	s = knowledgeServer(reg, &fakeKnowledgeWriter{}, reader)
	resp, err := s.KnowledgeCollections(identityCtx(), &engrampb.KnowledgeCollectionsRequest{})
	if err != nil {
		t.Fatalf("KnowledgeCollections: %v", err)
	}
	if len(resp.GetCollections()) != 1 {
		t.Fatalf("collections = %v", resp.GetCollections())
	}
	got := resp.GetCollections()[0].GetSpec()
	if got.GetFragmentSize() != 512 || got.GetNumberOfFragments() != 5 ||
		got.GetHighlightPreTag() != "<em>" || got.GetHighlightPostTag() != "</em>" {
		t.Errorf("sizing fields lost on encode: %v", got)
	}

	// Unset stays unset on the wire (fallback belongs to FragmentSizing at
	// consumption, never to translation): the fallback and the wire value
	// must remain distinguishable.
	zero := knowledge.CollectionSpec{Name: "bare", Access: knowledge.AccessPolicy{Public: true}}
	reg.specs["bare"] = zero
	reader.metas = []retrieval.CollectionMeta{{Name: "bare"}}
	resp, err = s.KnowledgeCollections(identityCtx(), &engrampb.KnowledgeCollectionsRequest{})
	if err != nil {
		t.Fatalf("KnowledgeCollections (bare): %v", err)
	}
	bare := resp.GetCollections()[0].GetSpec()
	if bare.GetFragmentSize() != 0 || bare.GetNumberOfFragments() != 0 ||
		bare.GetHighlightPreTag() != "" || bare.GetHighlightPostTag() != "" {
		t.Errorf("unset sizing must encode as zero, got %v", bare)
	}
	size, count := zero.FragmentSizing()
	if size != knowledge.DefaultFragmentSize || count != knowledge.DefaultNumberOfFragments {
		t.Errorf("consumption fallback = (%d, %d), want (%d, %d)", size, count,
			knowledge.DefaultFragmentSize, knowledge.DefaultNumberOfFragments)
	}
}

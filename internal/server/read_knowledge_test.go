package server_test

// Phase-2 tests for the Read RPC's knowledge branch (DW-2.4): a readable
// registered collection returns the full stored document; an unreadable one
// fails closed with the SAME opaque not-found as a missing doc; a source that
// is neither a memory tier nor a registered collection gets a self-correcting
// message; and the barricade authorizes BEFORE it ever fetches. Also pins the
// KnowledgeSearch handler's fragment mapping and full_body threading (DW-2.1/
// 2.3's handler leg).

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/knowledge"
	"github.com/ryanthedev/engram/internal/retrieval"
	"github.com/ryanthedev/engram/internal/server"
)

// storedDoc is the canned full document the fake reader serves.
var storedDoc = map[string]any{
	"title": "Deep Dive", "text": "the whole 5,499-char body",
	"collection": "papers", "source": "gh", "harvest_id": "h1",
}

func readKnowledgeServer(spec knowledge.CollectionSpec, r *fakeKnowledgeReader) *server.Server {
	return knowledgeServer(&fakeRegistry{specs: map[string]knowledge.CollectionSpec{spec.Name: spec}}, nil, r)
}

// TestReadKnowledge_DW_2_4_ReturnsFullDocument: a readable collection source
// returns the whole stored document as fields_json.
func TestReadKnowledge_DW_2_4_ReturnsFullDocument(t *testing.T) {
	r := &fakeKnowledgeReader{doc: storedDoc, docOK: true}
	svc := readKnowledgeServer(papersSpec(true), r)

	resp, err := svc.Read(identityCtx(), &engrampb.ReadRequest{Id: "d1", Source: "papers"})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if resp.GetSource() != "papers" {
		t.Errorf("source = %q, want papers", resp.GetSource())
	}
	if r.docID != "d1" {
		t.Errorf("reader fetched %q, want d1", r.docID)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(resp.GetFieldsJson()), &got); err != nil {
		t.Fatalf("fields_json does not decode: %v", err)
	}
	if !reflect.DeepEqual(got, storedDoc) {
		t.Errorf("fields_json = %v, want the full stored document %v", got, storedDoc)
	}
	if resp.GetEpisodic() != nil || resp.GetFact() != nil {
		t.Error("knowledge read must not populate memory branches")
	}
}

// TestReadKnowledge_DW_2_4_UnreadableFailsClosedOpaque: a collection the
// caller may not read yields the SAME NotFound as a missing document — no
// distinguishable denial, no fetch ever issued (authorize BEFORE fetch).
func TestReadKnowledge_DW_2_4_UnreadableFailsClosedOpaque(t *testing.T) {
	r := &fakeKnowledgeReader{doc: storedDoc, docOK: true} // the doc EXISTS
	svc := readKnowledgeServer(papersSpec(false, "curator"), r)

	_, denyErr := svc.Read(identityCtx(), &engrampb.ReadRequest{Id: "d1", Source: "papers"})
	if status.Code(denyErr) != codes.NotFound {
		t.Fatalf("unreadable collection: code = %v, want NotFound", status.Code(denyErr))
	}
	if r.docID != "" {
		t.Errorf("document was fetched (%q) before authorization — barricade ordering broken", r.docID)
	}

	// Same wording as a genuine miss on a readable collection: opaque.
	r2 := &fakeKnowledgeReader{docOK: false}
	svc2 := readKnowledgeServer(papersSpec(true), r2)
	_, missErr := svc2.Read(identityCtx(), &engrampb.ReadRequest{Id: "ghost", Source: "papers"})
	if status.Code(missErr) != codes.NotFound {
		t.Fatalf("missing doc: code = %v, want NotFound", status.Code(missErr))
	}
	if denyErr.Error() != missErr.Error() {
		t.Errorf("denial (%q) and absence (%q) are distinguishable: existence leak", denyErr, missErr)
	}
}

// TestReadKnowledge_DW_2_4_UnknownSourceSelfCorrecting: a source that is
// neither a memory tier nor a registered collection names the valid
// vocabulary so an LLM caller can fix its own call.
func TestReadKnowledge_DW_2_4_UnknownSourceSelfCorrecting(t *testing.T) {
	svc := readKnowledgeServer(papersSpec(true), &fakeKnowledgeReader{})
	_, err := svc.Read(identityCtx(), &engrampb.ReadRequest{Id: "d1", Source: "nonesuch"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
	for _, want := range []string{`"nonesuch"`, "episodic", "semantic", "knowledge_collections"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

// TestReadKnowledge_UnconfiguredKeepsMemoryVocabulary: without a knowledge
// platform wired, an unknown source gets the pre-drill-down memory-only
// message (there IS no collection vocabulary to offer).
func TestReadKnowledge_UnconfiguredKeepsMemoryVocabulary(t *testing.T) {
	svc := &server.Server{}
	_, err := svc.Read(identityCtx(), &engrampb.ReadRequest{Id: "d1", Source: "papers"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
	if !strings.Contains(err.Error(), `source must be "episodic" or "semantic"`) {
		t.Errorf("error = %q, want the memory-only vocabulary", err)
	}
}

// TestReadKnowledge_ReaderErrorIsInternal: an infrastructure failure on the
// fetch surfaces as Internal — never disguised as (opaque) absence, and
// never leaking into the InvalidArgument vocabulary.
func TestReadKnowledge_ReaderErrorIsInternal(t *testing.T) {
	r := &fakeKnowledgeReader{docErr: errors.New("cluster on fire")}
	svc := readKnowledgeServer(papersSpec(true), r)
	_, err := svc.Read(identityCtx(), &engrampb.ReadRequest{Id: "d1", Source: "papers"})
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %v, want Internal", status.Code(err))
	}
}

// TestReadMemoryTiersUntouchedByKnowledgeWiring (DW-2.5): with a knowledge
// registry wired, the memory-tier dispatch is unchanged — an episodic read
// with no Episodic reader still answers Unimplemented, and graph still
// short-circuits.
func TestReadMemoryTiersUntouchedByKnowledgeWiring(t *testing.T) {
	svc := readKnowledgeServer(papersSpec(true), &fakeKnowledgeReader{})
	if _, err := svc.Read(identityCtx(), &engrampb.ReadRequest{Id: "x", Source: "episodic"}); status.Code(err) != codes.Unimplemented {
		t.Errorf("episodic without reader: code = %v, want Unimplemented", status.Code(err))
	}
	if _, err := svc.Read(identityCtx(), &engrampb.ReadRequest{Id: "x", Source: "graph"}); status.Code(err) != codes.Unimplemented {
		t.Errorf("graph: code = %v, want Unimplemented", status.Code(err))
	}
	if _, err := svc.Read(identityCtx(), &engrampb.ReadRequest{Source: "episodic"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty id: code = %v, want InvalidArgument", status.Code(err))
	}
}

// TestKnowledgeSearchHandlerMapsFragmentsAndFullBody: the handler threads
// req.full_body to the reader and maps Hit.Fragments onto the wire.
func TestKnowledgeSearchHandlerMapsFragmentsAndFullBody(t *testing.T) {
	r := &fakeKnowledgeReader{hits: []retrieval.Hit{{
		ID: "d1", Score: 3.0, Source: "papers",
		Fields:    map[string]any{"title": "T"},
		Fragments: []string{"frag a", "frag b"},
	}}}
	svc := readKnowledgeServer(papersSpec(true), r)

	resp, err := svc.KnowledgeSearch(identityCtx(), &engrampb.KnowledgeSearchRequest{
		Collection: "papers", Query: "q", FullBody: true,
	})
	if err != nil {
		t.Fatalf("KnowledgeSearch: %v", err)
	}
	if !r.fullBody {
		t.Error("full_body was not threaded to the reader")
	}
	h := resp.GetHits()[0]
	if !reflect.DeepEqual(h.GetFragments(), []string{"frag a", "frag b"}) {
		t.Errorf("fragments = %v, want [frag a, frag b]", h.GetFragments())
	}
	if h.GetCollection() != "papers" {
		t.Errorf("collection = %q, want papers", h.GetCollection())
	}
}

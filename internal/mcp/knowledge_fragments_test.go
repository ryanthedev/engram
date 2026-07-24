package mcp

// Phase-2 tool-surface tests: knowledge_search forwards full_body and emits
// {collection, fragments} hits (DW-2.1/2.2/2.3's tool leg), and memory_read
// forwards a collection source to the Backend instead of rejecting it at the
// MCP edge (DW-2.4's tool leg).

import (
	"context"
	"reflect"
	"testing"
)

// readRecorder overrides the fake backend's Read to capture the forwarded
// (id, source) pair and serve a canned knowledge document.
type readRecorder struct {
	knowledgeFake
	lastRead struct{ id, source string }
	result   ReadResult
}

func (b *readRecorder) Read(_ context.Context, id, source string) (ReadResult, error) {
	b.lastRead.id, b.lastRead.source = id, source
	return b.result, b.err
}

// TestKnowledgeSearchToolForwardsFullBody: the full_body argument reaches
// the Backend, and its absence stays false.
func TestKnowledgeSearchToolForwardsFullBody(t *testing.T) {
	b := &knowledgeFake{}
	c := startServer(t, b)
	c.call("initialize", nil)

	callTool(t, c, ToolKnowledgeSearch, map[string]any{"collection": "papers", "query": "q", "full_body": true})
	if !b.lastSearch.fullBody {
		t.Error("full_body=true did not reach the backend")
	}
	callTool(t, c, ToolKnowledgeSearch, map[string]any{"collection": "papers", "query": "q"})
	if b.lastSearch.fullBody {
		t.Error("omitted full_body must default to false")
	}
}

// TestKnowledgeSearchToolEmitsCollectionAndFragments: the tool result's hits
// carry the pinned {id, score, collection, fields_json, fragments[]} shape —
// a real collection key (never a repurposed "source") and a real fragments
// array.
func TestKnowledgeSearchToolEmitsCollectionAndFragments(t *testing.T) {
	b := &knowledgeFake{searchHits: []KnowledgeHit{{
		ID: "d1", Score: 2.0, Collection: "papers",
		Fields:    `{"title":"T"}`,
		Fragments: []string{"frag one", "frag two"},
	}}}
	c := startServer(t, b)
	c.call("initialize", nil)

	result := callTool(t, c, ToolKnowledgeSearch, map[string]any{"collection": "papers", "query": "q"})
	hit := structured(t, result)["hits"].([]any)[0].(map[string]any)
	if hit["collection"] != "papers" {
		t.Errorf(`hit["collection"] = %v, want "papers"`, hit["collection"])
	}
	if _, hasSource := hit["source"]; hasSource {
		t.Error("knowledge hit must not carry a source key")
	}
	if !reflect.DeepEqual(hit["fragments"], []any{"frag one", "frag two"}) {
		t.Errorf("fragments = %v, want [frag one, frag two]", hit["fragments"])
	}
	if hit["fields_json"] != `{"title":"T"}` {
		t.Errorf("fields_json = %v", hit["fields_json"])
	}
}

// TestMemoryReadForwardsCollectionSource (DW-2.4): a source that is not a
// memory tier passes through to the Backend as-is — the server barricade,
// not this edge, decides whether it is a collection.
func TestMemoryReadForwardsCollectionSource(t *testing.T) {
	b := &readRecorder{result: ReadResult{
		ID: "d1", Source: "papers",
		Fields: map[string]any{"title": "T", "text": "whole body"},
	}}
	c := startServer(t, b)
	c.call("initialize", nil)

	result := callTool(t, c, ToolRead, map[string]any{"id": "d1", "source": "papers"})
	if result["isError"] != false {
		t.Fatalf("isError = %v: %v", result["isError"], result)
	}
	if b.lastRead.id != "d1" || b.lastRead.source != "papers" {
		t.Errorf("backend got (%q, %q), want (d1, papers)", b.lastRead.id, b.lastRead.source)
	}
	fields := structured(t, result)["fields"].(map[string]any)
	if fields["text"] != "whole body" {
		t.Errorf("fields = %v, want the full document", fields)
	}
}

// TestMemoryReadGraphStillShortCircuits (DW-2.5): relaxing the source gate
// must not unshort-circuit graph.
func TestMemoryReadGraphStillShortCircuits(t *testing.T) {
	b := &readRecorder{}
	c := startServer(t, b)
	c.call("initialize", nil)

	result := callTool(t, c, ToolRead, map[string]any{"id": "g1", "source": "graph"})
	if result["isError"] != true {
		t.Fatalf("graph read must stay a tool error: %v", result)
	}
	if b.lastRead.id != "" {
		t.Error("graph must short-circuit before the backend")
	}
}

package mcp

// Phase-6 tests for the six knowledge MCP tools: every tool dispatches
// through the Backend seam (DW-6.1), knowledge_search budget-packs with an
// overflow_path spill on overflow (DW-6.1), knowledge_collections surfaces
// count + staleness (DW-6.4), argument validation fails as tool errors, and
// the memory tool surface is behaviorally untouched (DW-6.5).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// knowledgeFake records every knowledge call and returns canned results; the
// memory methods reuse fakeBackend behavior via embedding.
type knowledgeFake struct {
	fakeBackend

	lastIngest struct {
		collection, source, harvestID string
		docs                          []KnowledgeDoc
	}
	lastSearch struct {
		collection, query string
		filters           []Predicate
		sort              []SortKey
		k                 int
		offset            int
		fullBody          bool
	}
	lastDelete struct {
		collection, source, currentHarvestID string
	}
	lastReadID, lastReadSource string
	lastSpec                   CollectionSpec

	searchHits  []KnowledgeHit
	searchTotal int64
	collections []CollectionInfo
	err         error
}

func (b *knowledgeFake) KnowledgeIngest(_ context.Context, collection, source, harvestID string, docs []KnowledgeDoc) (int, error) {
	b.lastIngest.collection, b.lastIngest.source, b.lastIngest.harvestID, b.lastIngest.docs = collection, source, harvestID, docs
	return len(docs), b.err
}

func (b *knowledgeFake) KnowledgeSearch(_ context.Context, collection, query string, filters []Predicate, sort []SortKey, k, offset int, fullBody bool) ([]KnowledgeHit, int64, error) {
	b.lastSearch.collection, b.lastSearch.query, b.lastSearch.filters, b.lastSearch.sort, b.lastSearch.k = collection, query, filters, sort, k
	b.lastSearch.offset, b.lastSearch.fullBody = offset, fullBody
	return b.searchHits, b.searchTotal, b.err
}

func (b *knowledgeFake) KnowledgeCollections(context.Context) ([]CollectionInfo, error) {
	return b.collections, b.err
}

func (b *knowledgeFake) KnowledgeDelete(_ context.Context, collection, source, currentHarvestID string) (int, error) {
	b.lastDelete.collection, b.lastDelete.source, b.lastDelete.currentHarvestID = collection, source, currentHarvestID
	return 7, b.err
}

func (b *knowledgeFake) CreateCollection(_ context.Context, spec CollectionSpec) error {
	b.lastSpec = spec
	return b.err
}

func (b *knowledgeFake) UpdateCollection(_ context.Context, spec CollectionSpec) error {
	b.lastSpec = spec
	return b.err
}

var _ Backend = (*knowledgeFake)(nil)

// callTool drives one tools/call and returns the decoded result object.
func callTool(t *testing.T, c *refClient, name string, args map[string]any) map[string]any {
	t.Helper()
	resp := c.call("tools/call", map[string]any{"name": name, "arguments": args})
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/call %s: no result in %v", name, resp)
	}
	return result
}

// searchLines parses a search tool's TEXT-ONLY response back into the field
// names these tests assert on. memory_search and knowledge_search emit no
// structuredContent — the compact-line block is the whole response — so a
// test reading a structured copy would assert on something no caller sees.
func searchLines(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("search result has no content block: %v", result)
	}
	block, _ := content[0].(map[string]any)
	text, _ := block["text"].(string)
	return parseCompactLines(t, text)
}

// structured returns a tool result's structuredContent payload.
func structured(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	sc, ok := result["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("result has no structuredContent object: %v", result)
	}
	return sc
}

// TestDW_6_1_KnowledgeToolsDispatchThroughBackend proves each of the six
// tools reaches its Backend method with translated arguments and surfaces the
// backend's result.
func TestDW_6_1_KnowledgeToolsDispatchThroughBackend(t *testing.T) {
	b := &knowledgeFake{}
	c := startServer(t, b)
	c.call("initialize", nil)

	t.Run("knowledge_ingest", func(t *testing.T) {
		result := callTool(t, c, ToolKnowledgeIngest, map[string]any{
			"collection": "papers", "source": "feed", "harvest_id": "h1",
			"docs": []any{map[string]any{"id": "d1", "text": "body", "fields": map[string]any{"year": 2026}}},
		})
		if result["isError"] != false {
			t.Fatalf("isError = %v: %v", result["isError"], result)
		}
		if got := structured(t, result)["indexed"]; got != float64(1) {
			t.Errorf("indexed = %v, want 1", got)
		}
		if b.lastIngest.collection != "papers" || b.lastIngest.source != "feed" || b.lastIngest.harvestID != "h1" {
			t.Errorf("backend got %+v", b.lastIngest)
		}
		if len(b.lastIngest.docs) != 1 || b.lastIngest.docs[0].ID != "d1" || b.lastIngest.docs[0].Fields["year"] != float64(2026) {
			t.Errorf("docs translated wrong: %+v", b.lastIngest.docs)
		}
	})

	t.Run("knowledge_search", func(t *testing.T) {
		b.searchHits = []KnowledgeHit{{ID: "d1", Score: 2.5, Collection: "papers", Fields: `{"title":"x"}`}}
		result := callTool(t, c, ToolKnowledgeSearch, map[string]any{
			"collection": "papers", "query": "transformers",
			"filters": []any{map[string]any{"field": "year", "op": "range", "value": map[string]any{"gte": 2024}}},
			"sort":    []any{map[string]any{"field": "year", "order": "desc"}},
			"k":       5,
		})
		if result["isError"] != false {
			t.Fatalf("isError = %v: %v", result["isError"], result)
		}
		hits := searchLines(t, result)["hits"].([]any)
		// Self-addressing id: the collection travels with the row, so a
		// caller hands it straight back to memory_read.
		if len(hits) != 1 || !strings.HasPrefix(fmt.Sprint(hits[0]), "papers:d1\t") {
			t.Errorf("hits = %v", hits)
		}
		if b.lastSearch.collection != "papers" || b.lastSearch.query != "transformers" || b.lastSearch.k != 5 {
			t.Errorf("backend got %+v", b.lastSearch)
		}
		if len(b.lastSearch.filters) != 1 || b.lastSearch.filters[0].Op != "range" {
			t.Errorf("filters = %+v", b.lastSearch.filters)
		}
		if len(b.lastSearch.sort) != 1 || b.lastSearch.sort[0].Order != "desc" {
			t.Errorf("sort = %+v", b.lastSearch.sort)
		}
	})

	t.Run("knowledge_search defaults k when unset", func(t *testing.T) {
		callTool(t, c, ToolKnowledgeSearch, map[string]any{"collection": "papers"})
		if b.lastSearch.k != defaultRequestK {
			t.Errorf("k = %d, want defaultRequestK %d", b.lastSearch.k, defaultRequestK)
		}
	})

	t.Run("knowledge_delete", func(t *testing.T) {
		result := callTool(t, c, ToolKnowledgeDelete, map[string]any{
			"collection": "papers", "source": "feed", "current_harvest_id": "h2",
		})
		if got := structured(t, result)["deleted"]; got != float64(7) {
			t.Errorf("deleted = %v, want 7", got)
		}
		if b.lastDelete.currentHarvestID != "h2" {
			t.Errorf("backend got %+v", b.lastDelete)
		}
	})

	t.Run("knowledge_create_collection", func(t *testing.T) {
		result := callTool(t, c, ToolCreateCollection, map[string]any{
			"name": "papers", "text_field": "abstract",
			"mappings": map[string]any{"year": map[string]any{"type": "integer", "filterable": true, "sortable": true}},
			"roles":    []any{"curator"},
		})
		if result["isError"] != false {
			t.Fatalf("isError = %v: %v", result["isError"], result)
		}
		if b.lastSpec.Name != "papers" || b.lastSpec.TextField != "abstract" ||
			!b.lastSpec.Mappings["year"].Sortable || b.lastSpec.Roles[0] != "curator" {
			t.Errorf("spec translated wrong: %+v", b.lastSpec)
		}
	})

	t.Run("knowledge_update_collection", func(t *testing.T) {
		result := callTool(t, c, ToolUpdateCollection, map[string]any{"name": "papers", "public": true})
		if result["isError"] != false {
			t.Fatalf("isError = %v: %v", result["isError"], result)
		}
		if b.lastSpec.Name != "papers" || !b.lastSpec.Public {
			t.Errorf("spec translated wrong: %+v", b.lastSpec)
		}
	})
}

// TestDW_3_1_KnowledgeSearchToolThreadsOffsetAndTotal proves the
// knowledge_search tool accepts an offset argument, threads it to the
// Backend unchanged, and surfaces the Backend's exact total in the response
// envelope — the MCP-tool leg of DW-3.1's offset/total contract.
func TestDW_3_1_KnowledgeSearchToolThreadsOffsetAndTotal(t *testing.T) {
	b := &knowledgeFake{
		searchHits:  []KnowledgeHit{{ID: "d1", Score: 1, Collection: "papers"}},
		searchTotal: 250,
	}
	c := startServer(t, b)
	c.call("initialize", nil)

	result := callTool(t, c, ToolKnowledgeSearch, map[string]any{"collection": "papers", "offset": 100})
	if result["isError"] != false {
		t.Fatalf("isError = %v: %v", result["isError"], result)
	}
	if b.lastSearch.offset != 100 {
		t.Errorf("backend got offset %d, want 100", b.lastSearch.offset)
	}
	sc := searchLines(t, result)
	if got := sc["total"]; got != float64(250) {
		t.Errorf("total = %v, want 250", got)
	}
}

// TestKnowledgeSearchToolNegativeOffsetIsToolError pins the tool-boundary
// barricade: a negative offset is external-input nonsense ("skip -5 hits")
// and must be rejected before it ever reaches the Backend.
func TestKnowledgeSearchToolNegativeOffsetIsToolError(t *testing.T) {
	b := &knowledgeFake{}
	c := startServer(t, b)
	c.call("initialize", nil)

	result := callTool(t, c, ToolKnowledgeSearch, map[string]any{"collection": "papers", "offset": -1})
	if result["isError"] != true {
		t.Fatalf("isError = %v, want true: %v", result["isError"], result)
	}
	if b.lastSearch.collection != "" {
		t.Errorf("negative offset must not reach the backend, got a call: %+v", b.lastSearch)
	}
}

// TestDW_6_4_KnowledgeCollectionsToolSurfacesStaleness proves the tool
// reports each collection's count and staleness timestamps.
func TestDW_6_4_KnowledgeCollectionsToolSurfacesStaleness(t *testing.T) {
	harvested := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	docDate := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	b := &knowledgeFake{collections: []CollectionInfo{{
		CollectionSpec:    CollectionSpec{Name: "papers", TextField: "abstract", Public: true},
		Count:             1234,
		NewestHarvestedAt: &harvested,
		NewestDocDate:     &docDate,
	}}}
	c := startServer(t, b)
	c.call("initialize", nil)

	result := callTool(t, c, ToolKnowledgeCollections, map[string]any{})
	cols := structured(t, result)["collections"].([]any)
	if len(cols) != 1 {
		t.Fatalf("collections = %v", cols)
	}
	col := cols[0].(map[string]any)
	if col["name"] != "papers" || col["count"] != float64(1234) {
		t.Errorf("count/name wrong: %v", col)
	}
	if _, ok := col["newest_harvested_at"]; !ok {
		t.Errorf("staleness newest_harvested_at missing: %v", col)
	}
	if _, ok := col["newest_doc_date"]; !ok {
		t.Errorf("staleness newest_doc_date missing: %v", col)
	}
}

// TestDW_6_1_KnowledgeSearchBudgetPackAndSpill proves an oversized knowledge
// result set is budget-packed and the FULL set spilled to overflow_path —
// the same packer/spiller memory_search uses, with no facet fields.
func TestDW_6_1_KnowledgeSearchBudgetPackAndSpill(t *testing.T) {
	t.Setenv(spillDirEnv, t.TempDir())
	t.Setenv(searchBudgetBytesEnv, "600")

	hits := make([]KnowledgeHit, 20)
	for i := range hits {
		hits[i] = KnowledgeHit{ID: fmt.Sprintf("d%02d", i), Score: float64(20 - i), Collection: "papers",
			Fields: `{"title":"` + strings.Repeat("x", 80) + `"}`}
	}
	b := &knowledgeFake{searchHits: hits}
	c := startServer(t, b)
	c.call("initialize", nil)

	result := callTool(t, c, ToolKnowledgeSearch, map[string]any{"collection": "papers", "query": "q"})
	sc := searchLines(t, result)
	packed := sc["hits"].([]any)
	if len(packed) == 0 || len(packed) >= len(hits) {
		t.Fatalf("expected a shrunken non-empty page, got %d of %d hits", len(packed), len(hits))
	}
	if sc["omitted"] == nil || sc["omitted"].(float64) != float64(len(hits)-len(packed)) {
		t.Errorf("omitted = %v, want %d", sc["omitted"], len(hits)-len(packed))
	}
	// Knowledge search passes nil facet fields: no omitted_facets, hint still present.
	if _, ok := sc["omitted_facets"]; ok {
		t.Errorf("knowledge_search must not report memory-shaped facets: %v", sc["omitted_facets"])
	}
	if hint, _ := sc["hint"].(string); !strings.Contains(hint, "omitted") {
		t.Errorf("hint = %q", hint)
	}
	path, _ := sc["overflow_path"].(string)
	if path == "" {
		t.Fatal("overflow_path missing on an overflowing result")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading spill file: %v", err)
	}
	var spilled struct {
		Hits []Hit `json:"hits"`
	}
	if err := json.Unmarshal(raw, &spilled); err != nil {
		t.Fatalf("decoding spill file: %v", err)
	}
	if len(spilled.Hits) != len(hits) || spilled.Hits[0].ID != "d00" {
		t.Errorf("spill file has %d hits, want the full %d in order", len(spilled.Hits), len(hits))
	}
}

// TestKnowledgeToolArgumentValidation pins the MCP-edge barricade: missing
// required arguments are tool errors (isError=true), not backend calls.
func TestKnowledgeToolArgumentValidation(t *testing.T) {
	tests := []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{"ingest missing collection", ToolKnowledgeIngest, map[string]any{"source": "s", "harvest_id": "h"}, "collection"},
		{"ingest missing harvest_id", ToolKnowledgeIngest, map[string]any{"collection": "c", "source": "s"}, "harvest_id"},
		{"search missing collection", ToolKnowledgeSearch, map[string]any{"query": "q"}, "collection"},
		{"delete missing current_harvest_id", ToolKnowledgeDelete, map[string]any{"collection": "c", "source": "s"}, "current_harvest_id"},
		{"create missing name", ToolCreateCollection, map[string]any{"text_field": "abstract"}, "name"},
		{"update missing name", ToolUpdateCollection, map[string]any{"public": true}, "name"},
	}
	b := &knowledgeFake{}
	c := startServer(t, b)
	c.call("initialize", nil)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := callTool(t, c, tt.tool, tt.args)
			if result["isError"] != true {
				t.Fatalf("isError = %v, want true: %v", result["isError"], result)
			}
			text := result["content"].([]any)[0].(map[string]any)["text"].(string)
			if !strings.Contains(text, tt.want) {
				t.Errorf("error %q does not name the missing %q", text, tt.want)
			}
		})
	}
}

// TestKnowledgeToolBackendErrorsSurfaceAsToolErrors proves a backend failure
// (e.g. the gRPC barricade's self-correcting InvalidArgument text) reaches
// the caller verbatim as an isError result, never a protocol error.
func TestKnowledgeToolBackendErrorsSurfaceAsToolErrors(t *testing.T) {
	b := &knowledgeFake{err: errors.New(`unknown or unfilterable field "yr"; valid filterable fields: year`)}
	c := startServer(t, b)
	c.call("initialize", nil)

	result := callTool(t, c, ToolKnowledgeSearch, map[string]any{"collection": "papers", "query": "q"})
	if result["isError"] != true {
		t.Fatalf("isError = %v: %v", result["isError"], result)
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "valid filterable fields: year") {
		t.Errorf("self-correcting backend message lost: %q", text)
	}
}

// TestDW_6_5_MemoryToolsUnchanged pins the memory regression contract at the
// tool layer: memory_search still packs with the memory facet fields and the
// three memory tools still behave as before with the knowledge-capable fake.
func TestDW_6_5_MemoryToolsUnchanged(t *testing.T) {
	b := &knowledgeFake{fakeBackend: *newFakeBackend()}
	c := startServer(t, b)
	c.call("initialize", nil)

	ing := callTool(t, c, ToolIngest, map[string]any{"event_id": "e1", "text": "subject alpha"})
	if got := structured(t, ing)["id"]; got != "ep-e1" {
		t.Fatalf("memory_ingest id = %v, want ep-e1", got)
	}
	res := callTool(t, c, ToolSearch, map[string]any{"query": "alpha"})
	hits := searchLines(t, res)["hits"].([]any)
	if len(hits) != 1 || !strings.HasPrefix(fmt.Sprint(hits[0]), "episodic:ep-e1\t") {
		t.Errorf("memory_search hits = %v", hits)
	}
	st := callTool(t, c, ToolStatus, map[string]any{})
	if got := structured(t, st)["healthy"]; got != true {
		t.Errorf("memory_status healthy = %v", got)
	}
}

// Read records the (id, source) pair the tool edge resolved, so a test can
// assert how a self-addressing search id was split before dispatch.
func (f *knowledgeFake) Read(_ context.Context, id, source string) (ReadResult, error) {
	f.lastReadID, f.lastReadSource = id, source
	return ReadResult{ID: id, Source: source, Fields: map[string]any{"text": "x"}}, nil
}

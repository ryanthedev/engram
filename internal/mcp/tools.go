package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
)

// toolSchema is one MCP tool advertised by tools/list. InputSchema is a JSON
// Schema object describing the tool's arguments.
type toolSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Tool names — the stable client contract (plan Produces).
const (
	ToolIngest = "memory_ingest"
	ToolSearch = "memory_search"
	ToolStatus = "memory_status"

	ToolKnowledgeIngest      = "knowledge_ingest"
	ToolKnowledgeSearch      = "knowledge_search"
	ToolKnowledgeCollections = "knowledge_collections"
	ToolKnowledgeDelete      = "knowledge_delete"
	ToolCreateCollection     = "knowledge_create_collection"
	ToolUpdateCollection     = "knowledge_update_collection"
)

// toolSchemas is the advertised tool set. Kept as a function so each call
// gets a fresh map (no shared-mutable-state hazard).
func toolSchemas() []toolSchema {
	strProp := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}
	return []toolSchema{
		{
			Name:        ToolIngest,
			Description: "Append one event to Engram episodic memory. Extraction and reconciliation happen asynchronously; returns the durable episodic id.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"event_id": strProp("Client-supplied idempotency id (required)."),
					"text":     strProp("Raw event text to remember (required)."),
					"source":   strProp("Optional source identifier for provenance."),
				},
				"required": []any{"event_id", "text"},
			},
		},
		{
			Name:        ToolSearch,
			Description: "Hybrid (BM25 + vector) search over Engram memory. Returns fused, ranked hits.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": strProp("Natural-language query (required)."),
					"k":     map[string]any{"type": "integer", "description": "Max hits to return (default server-chosen)."},
				},
				"required": []any{"query"},
			},
		},
		{
			Name:        ToolStatus,
			Description: "Report Engram server health, the caller's identity, and per-tier document counts.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

// knowledgeToolSchemas is the knowledge-platform tool set advertised alongside
// the memory tools. Same fresh-map-per-call rule as toolSchemas.
func knowledgeToolSchemas() []toolSchema {
	strProp := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}
	// collectionSpecProps is the shared create/update argument shape,
	// mirroring mcp.CollectionSpec.
	collectionSpecProps := func() map[string]any {
		return map[string]any{
			"name":       strProp("Collection name (required)."),
			"text_field": strProp("Document field BM25 indexes the full-text body under (default: text)."),
			"mappings": map[string]any{
				"type":        "object",
				"description": "Declared document fields by name: {type, filterable, sortable}. type is an index type such as keyword, date, integer.",
			},
			"public": map[string]any{"type": "boolean", "description": "Readable by any authenticated caller."},
			"roles":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Read roles when not public."},
		}
	}
	return []toolSchema{
		{
			Name:        ToolKnowledgeIngest,
			Description: "Bulk-upsert one harvest batch of documents into a knowledge collection. Re-ingesting a document id overwrites it in place; requires the harvester or admin role.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"collection": strProp("Target collection name (required)."),
					"source":     strProp("Logical origin of the batch (required)."),
					"harvest_id": strProp("Identifier of this harvest run, stamped on every row for the sweep (required)."),
					"docs": map[string]any{
						"type":        "array",
						"description": "Documents: {id (required), title, text, source_version, fields}. fields carries collection-specific values matching the declared mappings.",
						"items":       map[string]any{"type": "object"},
					},
				},
				"required": []any{"collection", "source", "harvest_id", "docs"},
			},
		},
		{
			Name:        ToolKnowledgeSearch,
			Description: "BM25 search over one knowledge collection with generic field filters and sort. Returns budget-packed ranked hits; oversized result sets spill to overflow_path.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"collection": strProp("Collection to search (required)."),
					"query":      strProp("Full-text query; may be empty when filters alone select documents."),
					"filters": map[string]any{
						"type":        "array",
						"description": "Field filters: {field, op: term|range|prefix, value}. range takes value {gte, lte} (either bound optional). Fields must be declared filterable — knowledge_collections lists them.",
						"items":       map[string]any{"type": "object"},
					},
					"sort": map[string]any{
						"type":        "array",
						"description": "Sort keys: {field, order: asc|desc} over declared sortable fields.",
						"items":       map[string]any{"type": "object"},
					},
					"k": map[string]any{"type": "integer", "description": "Max hits to return (default server-chosen)."},
				},
				"required": []any{"collection"},
			},
		},
		{
			Name:        ToolKnowledgeCollections,
			Description: "List the knowledge collections the caller may read, each with its filterable/sortable field mappings, document count, and staleness (newest harvest time and newest document date).",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        ToolKnowledgeDelete,
			Description: "Mark-and-sweep delete: remove every document in a collection from the named source whose harvest_id differs from current_harvest_id. Requires the harvester or admin role.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"collection":         strProp("Collection to sweep (required)."),
					"source":             strProp("Source whose stale rows are removed (required)."),
					"current_harvest_id": strProp("Harvest id of the latest run; rows NOT carrying it are deleted (required)."),
				},
				"required": []any{"collection", "source", "current_harvest_id"},
			},
		},
		{
			Name:        ToolCreateCollection,
			Description: "Register a new knowledge collection and provision its backing index live (no restart). Requires the admin role; a duplicate name is a conflict.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": collectionSpecProps(),
				"required":   []any{"name"},
			},
		},
		{
			Name:        ToolUpdateCollection,
			Description: "Amend an existing knowledge collection's spec (new fields, access policy); mapping changes apply live. Requires the admin role.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": collectionSpecProps(),
				"required":   []any{"name"},
			},
		},
	}
}

// handleToolsList returns the advertised tool schemas: the three memory tools
// followed by the six knowledge tools.
func (s *Server) handleToolsList() (any, *rpcError) {
	return map[string]any{"tools": append(toolSchemas(), knowledgeToolSchemas()...)}, nil
}

// callParams is the tools/call request shape.
type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// handleToolsCall dispatches one tool invocation. Tool-level failures are
// returned as a successful JSON-RPC result with isError=true (per the MCP
// tools contract — the protocol call succeeded, the tool reported an error),
// while protocol misuse (unknown tool, bad params) is a JSON-RPC error.
func (s *Server) handleToolsCall(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var p callParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "invalid tools/call params"}
	}
	switch p.Name {
	case ToolIngest:
		return s.callIngest(ctx, p.Arguments)
	case ToolSearch:
		return s.callSearch(ctx, p.Arguments)
	case ToolStatus:
		return s.callStatus(ctx)
	case ToolKnowledgeIngest:
		return s.callKnowledgeIngest(ctx, p.Arguments)
	case ToolKnowledgeSearch:
		return s.callKnowledgeSearch(ctx, p.Arguments)
	case ToolKnowledgeCollections:
		return s.callKnowledgeCollections(ctx)
	case ToolKnowledgeDelete:
		return s.callKnowledgeDelete(ctx, p.Arguments)
	case ToolCreateCollection:
		return s.callCreateCollection(ctx, p.Arguments)
	case ToolUpdateCollection:
		return s.callUpdateCollection(ctx, p.Arguments)
	default:
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown tool: " + p.Name}
	}
}

func (s *Server) callIngest(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		EventID string `json:"event_id"`
		Text    string `json:"text"`
		Source  string `json:"source"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "invalid memory_ingest arguments"}
	}
	if args.EventID == "" || args.Text == "" {
		return toolError("memory_ingest requires non-empty event_id and text"), nil
	}
	id, err := s.backend.Ingest(ctx, args.EventID, args.Text, args.Source)
	if err != nil {
		return toolError(fmt.Sprintf("ingest failed: %v", err)), nil
	}
	return toolResult(map[string]any{"id": id}), nil
}

func (s *Server) callSearch(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Query string `json:"query"`
		K     int    `json:"k"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "invalid memory_search arguments"}
	}
	if args.Query == "" {
		return toolError("memory_search requires a non-empty query"), nil
	}
	k := args.K
	if k <= 0 {
		k = defaultRequestK // caller didn't ask for a specific count: request generously, pack tightly
	}
	hits, err := s.backend.Search(ctx, args.Query, k)
	if err != nil {
		return toolError(fmt.Sprintf("search failed: %v", err)), nil
	}
	return toolResult(packAndSpill(hits, memoryFacetFields, ToolSearch)), nil
}

// packAndSpill budget-packs hits (facet hints computed over facetFields) and,
// when the page omitted anything, spills the FULL result set to disk and
// attaches its overflow_path. hits (unsliced) is exactly the full slim result
// set: packed and remainder are an order-preserving split of it (budget.go),
// so no reconstruction is needed. A spill failure must never fail the search
// (DW-3.4) — log and return the capped page without overflow_path rather than
// propagating the error. Shared verbatim by memory_search and
// knowledge_search (DW-6.1); toolName only labels the degradation log line.
func packAndSpill(hits []Hit, facetFields []string, toolName string) searchResult {
	result := packSearchResult(hits, searchByteBudget(), facetFields)
	if result.Omitted > 0 {
		if path, spillErr := spillFullResult(hits); spillErr != nil {
			slog.Warn(toolName+": spilling full result set to disk failed; returning capped response without overflow_path", "error", spillErr)
		} else {
			result.OverflowPath = path
		}
	}
	return result
}

func (s *Server) callStatus(ctx context.Context) (any, *rpcError) {
	st, err := s.backend.Status(ctx)
	if err != nil {
		return toolError(fmt.Sprintf("status failed: %v", err)), nil
	}
	return toolResult(st), nil
}

// --- Knowledge tool handlers. Each validates the argument shape here (the
// MCP transport's edge of the barricade — required strings present, JSON
// decodes into the DTOs) and delegates everything semantic (collection
// existence, field validation, authorization) to the Backend; the gRPC
// handlers behind it return self-correcting messages that surface verbatim
// as tool errors so the LLM caller can fix its own call.

func (s *Server) callKnowledgeIngest(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Collection string         `json:"collection"`
		Source     string         `json:"source"`
		HarvestID  string         `json:"harvest_id"`
		Docs       []KnowledgeDoc `json:"docs"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "invalid knowledge_ingest arguments"}
	}
	if args.Collection == "" || args.Source == "" || args.HarvestID == "" {
		return toolError("knowledge_ingest requires non-empty collection, source, and harvest_id"), nil
	}
	indexed, err := s.backend.KnowledgeIngest(ctx, args.Collection, args.Source, args.HarvestID, args.Docs)
	if err != nil {
		return toolError(fmt.Sprintf("knowledge ingest failed: %v", err)), nil
	}
	return toolResult(map[string]any{"indexed": indexed}), nil
}

func (s *Server) callKnowledgeSearch(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Collection string      `json:"collection"`
		Query      string      `json:"query"`
		Filters    []Predicate `json:"filters"`
		Sort       []SortKey   `json:"sort"`
		K          int         `json:"k"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "invalid knowledge_search arguments"}
	}
	if args.Collection == "" {
		return toolError("knowledge_search requires a non-empty collection"), nil
	}
	k := args.K
	if k <= 0 {
		k = defaultRequestK // same request-generously-pack-tightly posture as memory_search
	}
	hits, err := s.backend.KnowledgeSearch(ctx, args.Collection, args.Query, args.Filters, args.Sort, k)
	if err != nil {
		return toolError(fmt.Sprintf("knowledge search failed: %v", err)), nil
	}
	// nil facet fields: collections declare their own field vocabulary at
	// runtime, so v1 reports the omitted count + hint without facet values.
	return toolResult(packAndSpill(hits, nil, ToolKnowledgeSearch)), nil
}

func (s *Server) callKnowledgeCollections(ctx context.Context) (any, *rpcError) {
	infos, err := s.backend.KnowledgeCollections(ctx)
	if err != nil {
		return toolError(fmt.Sprintf("knowledge collections failed: %v", err)), nil
	}
	// Wrap in an object (not a bare array) so count and staleness fields read
	// unambiguously and future envelope fields stay additive.
	return toolResult(map[string]any{"collections": infos}), nil
}

func (s *Server) callKnowledgeDelete(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Collection       string `json:"collection"`
		Source           string `json:"source"`
		CurrentHarvestID string `json:"current_harvest_id"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "invalid knowledge_delete arguments"}
	}
	if args.Collection == "" || args.Source == "" || args.CurrentHarvestID == "" {
		return toolError("knowledge_delete requires non-empty collection, source, and current_harvest_id"), nil
	}
	deleted, err := s.backend.KnowledgeDelete(ctx, args.Collection, args.Source, args.CurrentHarvestID)
	if err != nil {
		return toolError(fmt.Sprintf("knowledge delete failed: %v", err)), nil
	}
	return toolResult(map[string]any{"deleted": deleted}), nil
}

// decodeCollectionSpec parses the shared create/update argument shape.
func decodeCollectionSpec(raw json.RawMessage) (CollectionSpec, string) {
	var spec CollectionSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return spec, "invalid collection spec arguments"
	}
	if spec.Name == "" {
		return spec, "a collection spec requires a non-empty name"
	}
	return spec, ""
}

func (s *Server) callCreateCollection(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	spec, msg := decodeCollectionSpec(raw)
	if msg != "" {
		return toolError(ToolCreateCollection + ": " + msg), nil
	}
	if err := s.backend.CreateCollection(ctx, spec); err != nil {
		return toolError(fmt.Sprintf("create collection failed: %v", err)), nil
	}
	return toolResult(map[string]any{"created": spec.Name}), nil
}

func (s *Server) callUpdateCollection(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	spec, msg := decodeCollectionSpec(raw)
	if msg != "" {
		return toolError(ToolUpdateCollection + ": " + msg), nil
	}
	if err := s.backend.UpdateCollection(ctx, spec); err != nil {
		return toolError(fmt.Sprintf("update collection failed: %v", err)), nil
	}
	return toolResult(map[string]any{"updated": spec.Name}), nil
}

// toolResult wraps a structured payload as an MCP tool result: a text content
// block carrying the JSON, plus structuredContent for clients that consume it.
func toolResult(payload any) map[string]any {
	text, _ := json.Marshal(payload)
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(text)}},
		"structuredContent": payload,
		"isError":           false,
	}
}

// toolError wraps a tool-level failure message as an MCP error result.
func toolError(msg string) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": msg}},
		"isError": true,
	}
}

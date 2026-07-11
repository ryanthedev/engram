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
	ToolRead   = "memory_read"
)

// readSources are the source values memory_read accepts — validated at this
// entry (agent-supplied arguments are external input) before the id ever
// reaches the backend. "graph" is recognized but short-circuited: a graph
// hit's statement already IS the whole memory.
var readSources = map[string]bool{"episodic": true, "semantic": true, "graph": true}

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
			Name:        ToolRead,
			Description: "Read ONE memory record's full content by the id and source a memory_search result line exposes. Episodic returns the full untruncated text; semantic returns the fact plus its provenance and version history. Spends your context on the whole record — drill deliberately.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":     strProp("Record id exactly as shown in a memory_search result (required)."),
					"source": strProp(`Source tier of the id: "episodic" or "semantic" (required; "graph" has no drill-down).`),
				},
				"required": []any{"id", "source"},
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

// handleToolsList returns the advertised tool schemas.
func (s *Server) handleToolsList() (any, *rpcError) {
	return map[string]any{"tools": toolSchemas()}, nil
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
	case ToolRead:
		return s.callRead(ctx, p.Arguments)
	case ToolStatus:
		return s.callStatus(ctx)
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
	result := packSearchResult(hits, searchByteBudget())
	if result.Omitted > 0 {
		// hits (unsliced) is exactly the full slim result set: packed and
		// remainder are an order-preserving split of it (budget.go), so no
		// reconstruction is needed. A spill failure must never fail the
		// search (DW-3.4) — log and return the capped page without
		// overflow_path rather than propagating the error.
		if path, spillErr := spillFullResult(hits); spillErr != nil {
			slog.Warn("memory_search: spilling full result set to disk failed; returning capped response without overflow_path", "error", spillErr)
		} else {
			result.OverflowPath = path
		}
	}
	// Render AFTER packing/spilling: the byte budget above is enforced on the
	// raw (fields_json-escaped) form, which is provably larger than its
	// rendered compact-line/un-nested-fields form (no double-encoding, plus
	// episodic text is now truncated) — so "packed form fits budget" implies
	// "rendered form fits budget" without re-measuring here.
	rendered := renderSearchResult(result)
	return toolResultWithText(rendered, compactLines(rendered)), nil
}

// callRead is the memory_read tool: the deliberate full-record drill for an
// (id, source) pair surfaced by memory_search. Arguments are validated here
// at the tool barricade (they arrive from the agent — external input); the
// server re-validates and authorizes fail-closed on its side of the gRPC
// boundary, so a miss, mismatch, or denial surfaces as one opaque not-found.
func (s *Server) callRead(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		ID     string `json:"id"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "invalid memory_read arguments"}
	}
	if args.ID == "" || args.Source == "" {
		return toolError("memory_read requires non-empty id and source"), nil
	}
	if !readSources[args.Source] {
		return toolError(`memory_read source must be "episodic" or "semantic" (as shown in the memory_search result line)`), nil
	}
	if args.Source == "graph" {
		return toolError("graph records have no drill-down: the memory_search result already carries the full statement"), nil
	}
	result, err := s.backend.Read(ctx, args.ID, args.Source)
	if err != nil {
		return toolError(fmt.Sprintf("read failed: %v", err)), nil
	}
	return toolResult(result), nil
}

func (s *Server) callStatus(ctx context.Context) (any, *rpcError) {
	st, err := s.backend.Status(ctx)
	if err != nil {
		return toolError(fmt.Sprintf("status failed: %v", err)), nil
	}
	return toolResult(st), nil
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

// toolResultWithText wraps a structured payload as an MCP tool result whose
// content text block is the caller-supplied rendering (memory_search's
// compact-line format) rather than a straight marshal of payload — while
// structuredContent still carries the full structured payload for clients
// that consume it. toolResult stays the JSON-marshal-both-ways default;
// this is the divergent case.
func toolResultWithText(payload any, text string) map[string]any {
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": text}},
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

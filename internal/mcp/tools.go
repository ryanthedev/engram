package mcp

import (
	"context"
	"encoding/json"
	"fmt"
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
	hits, err := s.backend.Search(ctx, args.Query, args.K)
	if err != nil {
		return toolError(fmt.Sprintf("search failed: %v", err)), nil
	}
	return toolResult(map[string]any{"hits": hits}), nil
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

// toolError wraps a tool-level failure message as an MCP error result.
func toolError(msg string) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": msg}},
		"isError": true,
	}
}

// Package mcp is a hand-rolled Model Context Protocol server over stdio
// JSON-RPC 2.0. It exposes Engram's three memory tools (memory_ingest,
// memory_search, memory_status) mapped onto a small Backend seam, so the
// gRPC client wiring stays out of the protocol logic and the protocol is
// conformance-testable in-process against a reference client. Only the MCP
// subset Engram needs is implemented — initialize, notifications/initialized,
// tools/list, tools/call — which is small and stable; the official Go SDK can
// replace this behind the same Backend seam without touching callers (the
// design-it-twice decision in the phase discovery doc).
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// protocolVersion is the MCP revision this server speaks.
const protocolVersion = "2024-11-05"

// serverName / serverVersion identify this implementation in the initialize
// handshake.
const (
	serverName    = "engram-mcp"
	serverVersion = "0.1.0"
)

// Hit is one search result surfaced through the memory_search tool.
type Hit struct {
	ID     string  `json:"id"`
	Score  float64 `json:"score"`
	Source string  `json:"source"`
	Fields string  `json:"fields_json,omitempty"`
}

// Status is the memory_status payload.
type Status struct {
	Healthy           bool   `json:"healthy"`
	TenantID          string `json:"tenant_id"`
	UserID            string `json:"user_id"`
	AgentID           string `json:"agent_id"`
	EpisodicCount     int64  `json:"episodic_count"`
	SemanticCount     int64  `json:"semantic_count"`
	OpenSearchVersion string `json:"opensearch_version,omitempty"`
}

// Backend is the Engram capability the MCP tools map onto (consumer-defined
// seam). The gRPC client adapter satisfies it; tests use a fake.
type Backend interface {
	// Ingest appends one episodic event and returns its storage id.
	Ingest(ctx context.Context, eventID, text, source string) (id string, err error)
	// Search runs one hybrid query and returns fused hits.
	Search(ctx context.Context, query string, k int) ([]Hit, error)
	// Status reports server health, the caller's identity, and tier counts.
	Status(ctx context.Context) (Status, error)
}

// Server serves the MCP protocol for one client connection over a Backend.
type Server struct {
	backend Backend
}

// NewServer returns an MCP Server over backend.
func NewServer(backend Backend) *Server {
	return &Server{backend: backend}
}

// JSON-RPC 2.0 envelope types.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // absent on notifications
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// JSON-RPC standard error codes used here.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// Serve reads newline-delimited JSON-RPC messages from r and writes responses
// to w until r is exhausted or ctx is cancelled (the MCP stdio transport:
// one JSON message per line, no embedded newlines). Notifications (no id)
// produce no response. Malformed lines produce a parse-error response with a
// null id, per JSON-RPC.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // allow large tool payloads
	enc := json.NewEncoder(w)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		resp, emit := s.handleLine(ctx, line)
		if !emit {
			continue // notification: no reply
		}
		if err := enc.Encode(resp); err != nil {
			return fmt.Errorf("mcp: writing response: %w", err)
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("mcp: reading input: %w", err)
	}
	return nil
}

// handleLine parses and dispatches one message, returning the response and
// whether it should be emitted (false for notifications).
func (s *Server) handleLine(ctx context.Context, line []byte) (response, bool) {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return errorResponse(nil, codeParseError, "parse error"), true
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		return errorResponse(req.ID, codeInvalidRequest, "invalid request"), true
	}
	// Notifications carry no id and never get a reply.
	isNotification := len(req.ID) == 0
	result, rerr := s.dispatch(ctx, req)
	if isNotification {
		return response{}, false
	}
	if rerr != nil {
		return response{JSONRPC: "2.0", ID: req.ID, Error: rerr}, true
	}
	return response{JSONRPC: "2.0", ID: req.ID, Result: result}, true
}

// dispatch routes one method to its handler.
func (s *Server) dispatch(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return s.handleInitialize()
	case "notifications/initialized", "initialized":
		return nil, nil // client ack; no-op
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return s.handleToolsList()
	case "tools/call":
		return s.handleToolsCall(ctx, req.Params)
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}
	}
}

// handleInitialize returns the server capabilities and identity.
func (s *Server) handleInitialize() (any, *rpcError) {
	return map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    serverName,
			"version": serverVersion,
		},
	}, nil
}

// errorResponse builds a JSON-RPC error response.
func errorResponse(id json.RawMessage, code int, msg string) response {
	return response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

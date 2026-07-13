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
	"time"
)

// protocolVersion is the MCP revision this server speaks.
const protocolVersion = "2024-11-05"

// serverName / serverVersion identify this implementation in the initialize
// handshake.
const (
	serverName    = "engram-mcp"
	serverVersion = "0.1.0"
)

// Hit is one search result surfaced through the memory_search and
// knowledge_search tools. Source is the hit's origin: a memory tier
// ("episodic" | "semantic") or, for knowledge hits, the collection name.
type Hit struct {
	ID     string  `json:"id"`
	Score  float64 `json:"score"`
	Source string  `json:"source"`
	Fields string  `json:"fields_json,omitempty"`
}

// SearchResult is one memory_search answer in two labeled blocks. Hits are the
// query's MATCHED hits — at most k of them, and never a graph expansion. Expanded
// carries the graph expansions reached by traversing out of those matched hits:
// bonus context that is NOT counted against k, so an expansion can neither evict
// a match nor inflate the count the caller asked for.
//
// They are two fields rather than two positional return values precisely because
// a caller cannot transpose them by accident — and because every expanded hit is
// a per-call token cost, so it must be delimited and independently budgetable,
// never smuggled into Hits.
//
// Expanded is nil (not empty) whenever expansion is off, finds nothing, or the
// caller's Sources excluded "graph" — so an absent block is absent, not empty.
type SearchResult struct {
	Hits     []Hit `json:"hits"`
	Expanded []Hit `json:"expanded,omitempty"`
}

// ReadResult is one record's full content, surfaced through the memory_read
// tool — the deliberate drill-down for an (id, source) pair a memory_search
// compact line exposed. A full read spends the caller's context, so it is a
// separate explicit call, never inlined into search results.
type ReadResult struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	// Fields is the record's content as a real JSON object (never a
	// stringified fields_json): episodic carries the full untruncated text
	// plus kind/event_id/occurred_at/source_ids; semantic carries the target
	// version's statement/subject/predicate/object and validity interval.
	Fields map[string]any `json:"fields"`
	// Provenance (semantic only): who wrote the fact and from what.
	Provenance map[string]any `json:"provenance,omitempty"`
	// Versions (semantic only): the fact's full bi-temporal history,
	// oldest-first.
	Versions []map[string]any `json:"versions,omitempty"`
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

// KnowledgeDoc is one document in a knowledge_ingest batch. Fields carries
// collection-specific values matching the collection's declared mappings;
// the seam itself is schema-free.
type KnowledgeDoc struct {
	// ID is the upsert identity: re-ingesting an id overwrites in place.
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
	// Text is the full-text body indexed under the collection's text field.
	Text string `json:"text"`
	// SourceVersion is a source-side change marker (opaque to the server).
	SourceVersion string         `json:"source_version,omitempty"`
	Fields        map[string]any `json:"fields,omitempty"`
}

// Predicate is one generic knowledge-search filter. Op is "term" | "range" |
// "prefix". Value carries a scalar for term/prefix; for range it is a
// map with "gte" and/or "lte" scalar bounds (either may be absent).
type Predicate struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value"`
}

// SortKey orders knowledge-search results by one sortable field. Order is
// "asc" | "desc".
type SortKey struct {
	Field string `json:"field"`
	Order string `json:"order"`
}

// FieldSpec declares one collection field: its index type (e.g. "keyword",
// "date", "integer") and whether queries may filter or sort on it.
type FieldSpec struct {
	Type       string `json:"type"`
	Filterable bool   `json:"filterable,omitempty"`
	Sortable   bool   `json:"sortable,omitempty"`
}

// CollectionSpec describes one document collection: its name, the full-text
// field BM25 targets, declared field mappings, and the read-access policy
// (public, or restricted to the named roles). It carries no index or alias
// name — physical storage stays registry-internal.
type CollectionSpec struct {
	Name      string               `json:"name"`
	TextField string               `json:"text_field,omitempty"`
	Mappings  map[string]FieldSpec `json:"mappings,omitempty"`
	Public    bool                 `json:"public,omitempty"`
	Roles     []string             `json:"roles,omitempty"`
}

// CollectionInfo is one knowledge_collections entry: the collection's spec
// (so a caller can learn its filterable/sortable fields) plus size and
// staleness. Nil timestamps mean the collection is empty or undated.
type CollectionInfo struct {
	CollectionSpec
	Count             int64      `json:"count"`
	NewestHarvestedAt *time.Time `json:"newest_harvested_at,omitempty"`
	NewestDocDate     *time.Time `json:"newest_doc_date,omitempty"`
}

// SearchFilter narrows a memory_search. It is the FLAT, named-parameter filter
// surface the memory_search tool advertises — deliberately not a generic
// [{field, op, value}] array, which costs an LLM caller more tokens to emit and
// gives it more ways to be wrong. The generic predicate form is the internal
// representation the server compiles this into; it never reaches the caller.
//
// Every field is optional and its zero value means "no constraint", so a zero
// SearchFilter is exactly the unfiltered search this tool ran before filters
// existed.
//
// Each filter applies only to the tier that owns its field — Kind to episodic,
// the fact triple and ExtractorVersion to semantic — which is why filtering on
// one does not zero out the other. Since/Until bound the memory's event time
// (episodic occurred_at, semantic valid_at) on BOTH tiers. Sources that declare
// no filterable fields ("experience", "graph") cannot honor any of this, so any
// filter at all excludes them rather than letting their hits ride back
// unconstrained.
type SearchFilter struct {
	Kind             string
	Subject          string
	Predicate        string
	Object           string
	ExtractorVersion string
	// Since/Until are inclusive event-time bounds; a zero time.Time is an open
	// bound. Validated at the tool barricade: Since must not follow Until.
	Since time.Time
	Until time.Time
	// IncludeSuperseded widens the search to superseded and retracted historical
	// fact versions. False (the default) keeps today's current-state-only
	// behavior. It relaxes the VALIDITY filter only — tenancy and ACL scope are
	// enforced independently and are never widened by it.
	IncludeSuperseded bool
	// Sources restricts the search to the named tiers; nil searches every
	// source. An empty (non-nil) slice is a validation error, not a silent
	// "all".
	Sources []string
}

// Backend is the Engram capability the MCP tools map onto (consumer-defined
// seam). The gRPC client adapter satisfies it; tests use a fake.
type Backend interface {
	// Ingest appends one episodic event and returns its storage id.
	Ingest(ctx context.Context, eventID, text, source string) (id string, err error)
	// Search runs one hybrid query under f and returns the matched hits and
	// the graph expansions in separate blocks (see SearchResult): matched hits
	// are capped at k, expansions are not counted against it. f is validated at
	// the tool barricade before it arrives here.
	Search(ctx context.Context, query string, k int, f SearchFilter) (SearchResult, error)
	// Read fetches one record's full content by the (id, source) pair a
	// search hit exposed. Authorization is fail-closed server-side: an
	// unknown, cross-tenant, mismatched, or denied id errors NOT_FOUND.
	Read(ctx context.Context, id, source string) (ReadResult, error)
	// Status reports server health, the caller's identity, and tier counts.
	Status(ctx context.Context) (Status, error)

	// KnowledgeIngest bulk-upserts one harvest batch (collection, source,
	// harvestID) of documents and returns how many were indexed. Partial
	// failures surface as err; retrying the whole batch is idempotent.
	KnowledgeIngest(ctx context.Context, collection, source, harvestID string, docs []KnowledgeDoc) (indexed int, err error)
	// KnowledgeSearch runs one BM25 query over a collection with generic
	// field filters and sort; hits carry the collection name as Source.
	KnowledgeSearch(ctx context.Context, collection, query string, filters []Predicate, sort []SortKey, k int) ([]Hit, error)
	// KnowledgeCollections lists the collections the caller may read, with
	// field mappings, document count, and staleness.
	KnowledgeCollections(ctx context.Context) ([]CollectionInfo, error)
	// KnowledgeDelete sweeps the collection: it deletes rows from source
	// whose harvest id differs from currentHarvestID and reports the count.
	KnowledgeDelete(ctx context.Context, collection, source, currentHarvestID string) (deleted int, err error)
	// CreateCollection registers a new collection and provisions its backing
	// index live; a duplicate name is a conflict.
	CreateCollection(ctx context.Context, spec CollectionSpec) error
	// UpdateCollection amends an existing collection's spec; mapping changes
	// apply live.
	UpdateCollection(ctx context.Context, spec CollectionSpec) error
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

package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// fakeBackend is an in-memory Backend for conformance tests.
type fakeBackend struct {
	knowledgeStubs
	ingested map[string]string // event_id -> text
	failNext bool
}

func newFakeBackend() *fakeBackend { return &fakeBackend{ingested: map[string]string{}} }

func (b *fakeBackend) Ingest(_ context.Context, eventID, text, _ string) (string, error) {
	b.ingested[eventID] = text
	return "ep-" + eventID, nil
}

func (b *fakeBackend) Search(_ context.Context, query string, k int) ([]Hit, error) {
	var hits []Hit
	for id, text := range b.ingested {
		if strings.Contains(text, query) {
			hits = append(hits, Hit{ID: "ep-" + id, Score: 1.0, Source: "episodic", Fields: text})
		}
	}
	return hits, nil
}

func (b *fakeBackend) Status(context.Context) (Status, error) {
	return Status{Healthy: true, TenantID: "t1", UserID: "alice", EpisodicCount: int64(len(b.ingested))}, nil
}

// knowledgeStubs satisfies Backend's six knowledge methods (Phase 1) with
// benign zero values; every memory-focused fake embeds it. Tool handlers
// that exercise these land in Phase 6.
type knowledgeStubs struct{}

func (knowledgeStubs) KnowledgeIngest(context.Context, string, string, string, []KnowledgeDoc) (int, error) {
	return 0, nil
}

func (knowledgeStubs) KnowledgeSearch(context.Context, string, string, []Predicate, []SortKey, int) ([]Hit, error) {
	return nil, nil
}

func (knowledgeStubs) KnowledgeCollections(context.Context) ([]CollectionInfo, error) {
	return nil, nil
}

func (knowledgeStubs) KnowledgeDelete(context.Context, string, string, string) (int, error) {
	return 0, nil
}

func (knowledgeStubs) CreateCollection(context.Context, CollectionSpec) error { return nil }

func (knowledgeStubs) UpdateCollection(context.Context, CollectionSpec) error { return nil }

// Compile-time proof the fake tracks the full Backend seam (DW-1.2).
var _ Backend = (*fakeBackend)(nil)

// refClient is a minimal in-process MCP reference client driving the server
// over an io.Pipe pair — it exercises the exact wire framing a real client
// (Claude Code) uses.
type refClient struct {
	t    *testing.T
	in   io.Writer     // to server
	out  *bufio.Reader // from server
	next int
}

func startServer(t *testing.T, backend Backend) *refClient {
	t.Helper()
	clientR, serverW := io.Pipe() // server -> client
	serverR, clientW := io.Pipe() // client -> server
	srv := NewServer(backend)
	go func() {
		_ = srv.Serve(context.Background(), serverR, serverW)
		serverW.Close()
	}()
	t.Cleanup(func() { clientW.Close() })
	return &refClient{t: t, in: clientW, out: bufio.NewReader(clientR)}
}

// call sends a request and reads the next response line, decoding it.
func (c *refClient) call(method string, params any) map[string]any {
	c.t.Helper()
	c.next++
	req := map[string]any{"jsonrpc": "2.0", "id": c.next, "method": method}
	if params != nil {
		req["params"] = params
	}
	line, _ := json.Marshal(req)
	if _, err := c.in.Write(append(line, '\n')); err != nil {
		c.t.Fatalf("write %s: %v", method, err)
	}
	respLine, err := c.out.ReadBytes('\n')
	if err != nil {
		c.t.Fatalf("read response to %s: %v", method, err)
	}
	var resp map[string]any
	if err := json.Unmarshal(respLine, &resp); err != nil {
		c.t.Fatalf("decode response to %s: %v (%s)", method, err, respLine)
	}
	return resp
}

// notify sends a notification (no id, no response expected).
func (c *refClient) notify(method string) {
	c.t.Helper()
	line, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	if _, err := c.in.Write(append(line, '\n')); err != nil {
		c.t.Fatalf("notify %s: %v", method, err)
	}
}

// TestDW_3_5_ConformanceInitialize: initialize returns the protocol version,
// tools capability, and server info.
func TestDW_3_5_ConformanceInitialize(t *testing.T) {
	c := startServer(t, newFakeBackend())
	resp := c.call("initialize", map[string]any{"protocolVersion": protocolVersion})
	if resp["error"] != nil {
		t.Fatalf("initialize error: %v", resp["error"])
	}
	result, _ := resp["result"].(map[string]any)
	if result["protocolVersion"] != protocolVersion {
		t.Errorf("protocolVersion = %v, want %s", result["protocolVersion"], protocolVersion)
	}
	caps, _ := result["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Error("initialize did not advertise the tools capability")
	}
	info, _ := result["serverInfo"].(map[string]any)
	if info["name"] != serverName {
		t.Errorf("serverInfo.name = %v, want %s", info["name"], serverName)
	}
	// The initialized notification must be accepted silently (no reply).
	c.notify("notifications/initialized")
}

// TestDW_3_5_ConformanceListTools: tools/list advertises exactly the three
// Engram tools with input schemas.
func TestDW_3_5_ConformanceListTools(t *testing.T) {
	c := startServer(t, newFakeBackend())
	c.call("initialize", nil)
	resp := c.call("tools/list", nil)
	result, _ := resp["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	names := map[string]bool{}
	for _, tl := range tools {
		tm, _ := tl.(map[string]any)
		names[tm["name"].(string)] = true
		if _, ok := tm["inputSchema"]; !ok {
			t.Errorf("tool %v missing inputSchema", tm["name"])
		}
	}
	for _, want := range []string{ToolIngest, ToolSearch, ToolStatus} {
		if !names[want] {
			t.Errorf("tools/list missing %s", want)
		}
	}
	if len(names) != 3 {
		t.Errorf("advertised %d tools, want 3", len(names))
	}
}

// TestDW_3_5_ConformanceCallTool: tools/call drives ingest then search end to
// end through the backend and returns structured results.
func TestDW_3_5_ConformanceCallTool(t *testing.T) {
	c := startServer(t, newFakeBackend())
	c.call("initialize", nil)

	ing := c.call("tools/call", map[string]any{
		"name":      ToolIngest,
		"arguments": map[string]any{"event_id": "e1", "text": "the deploy key is rotated weekly"},
	})
	res, _ := ing["result"].(map[string]any)
	if res["isError"] == true {
		t.Fatalf("ingest reported error: %v", res)
	}
	sc, _ := res["structuredContent"].(map[string]any)
	if sc["id"] != "ep-e1" {
		t.Errorf("ingest id = %v, want ep-e1", sc["id"])
	}

	srch := c.call("tools/call", map[string]any{
		"name":      ToolSearch,
		"arguments": map[string]any{"query": "deploy key", "k": 5},
	})
	sres, _ := srch["result"].(map[string]any)
	scon, _ := sres["structuredContent"].(map[string]any)
	hits, _ := scon["hits"].([]any)
	if len(hits) != 1 {
		t.Fatalf("search returned %d hits, want 1", len(hits))
	}
}

// TestConformanceUnknownMethod: an unknown method returns JSON-RPC -32601.
func TestConformanceUnknownMethod(t *testing.T) {
	c := startServer(t, newFakeBackend())
	resp := c.call("does/not/exist", nil)
	rerr, _ := resp["error"].(map[string]any)
	if rerr == nil || int(rerr["code"].(float64)) != codeMethodNotFound {
		t.Fatalf("error = %v, want method-not-found %d", rerr, codeMethodNotFound)
	}
}

// TestCallToolValidationIsToolError: a tool call with missing required args
// returns a tool-level isError result (the RPC itself succeeds).
func TestCallToolValidationIsToolError(t *testing.T) {
	c := startServer(t, newFakeBackend())
	resp := c.call("tools/call", map[string]any{
		"name":      ToolIngest,
		"arguments": map[string]any{"event_id": "e1"}, // no text
	})
	res, _ := resp["result"].(map[string]any)
	if res == nil || res["isError"] != true {
		t.Fatalf("expected a tool-level isError result, got %v", resp)
	}
}

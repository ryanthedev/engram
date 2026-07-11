package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// callRead drives one memory_read tools/call over the wire and returns the
// MCP tool result map.
func callReadViaWire(t *testing.T, c *refClient, args map[string]any) map[string]any {
	t.Helper()
	resp := c.call("tools/call", map[string]any{"name": ToolRead, "arguments": args})
	res, _ := resp["result"].(map[string]any)
	if res == nil {
		t.Fatalf("memory_read returned no result: %v", resp)
	}
	return res
}

// contentText extracts the tool result's text content block.
func contentText(t *testing.T, res map[string]any) string {
	t.Helper()
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("tool result has no content: %v", res)
	}
	block, _ := content[0].(map[string]any)
	text, _ := block["text"].(string)
	return text
}

// TestToolsCall_DW_2_1_MemoryReadReturnsFullEpisodicText: an id surfaced by a
// Phase-1 memory_search compact line drills to the FULL untruncated text —
// longer than the search gist's 200-rune snippet cap, emoji intact.
func TestToolsCall_DW_2_1_MemoryReadReturnsFullEpisodicText(t *testing.T) {
	body := strings.Repeat("the 👻 ghost memory haunts the index; ", 20) // ≫ leadSnippetRunes
	backend := newFakeBackend()
	c := startServer(t, backend)
	c.call("initialize", nil)
	ing := c.call("tools/call", map[string]any{
		"name":      ToolIngest,
		"arguments": map[string]any{"event_id": "e1", "text": body},
	})
	ires, _ := ing["result"].(map[string]any)
	isc, _ := ires["structuredContent"].(map[string]any)
	id, _ := isc["id"].(string)
	if id == "" {
		t.Fatalf("ingest returned no id: %v", ing)
	}

	// The Phase-1 search line only carries the truncated gist...
	srch := c.call("tools/call", map[string]any{
		"name":      ToolSearch,
		"arguments": map[string]any{"query": "ghost memory"},
	})
	sres, _ := srch["result"].(map[string]any)
	if line := contentText(t, sres); strings.Contains(line, body) {
		t.Fatalf("search already carries the full body; the drill would be pointless")
	}

	// ...and memory_read on that (id, source) returns the whole record.
	res := callReadViaWire(t, c, map[string]any{"id": id, "source": "episodic"})
	if res["isError"] == true {
		t.Fatalf("memory_read errored: %v", res)
	}
	sc, _ := res["structuredContent"].(map[string]any)
	fields, _ := sc["fields"].(map[string]any)
	if fields["text"] != body {
		t.Errorf("fields.text = %.60q..., want the full untruncated %d-rune body", fields["text"], len([]rune(body)))
	}
}

// TestToolsCall_DW_2_5_MemoryReadEmitsStructuredJSON: the read output's text
// block is structured JSON whose fields value is a REAL object — never a
// stringified fields_json.
func TestToolsCall_DW_2_5_MemoryReadEmitsStructuredJSON(t *testing.T) {
	backend := newFakeBackend()
	c := startServer(t, backend)
	c.call("initialize", nil)
	c.call("tools/call", map[string]any{
		"name":      ToolIngest,
		"arguments": map[string]any{"event_id": "e1", "text": "full body here"},
	})
	res := callReadViaWire(t, c, map[string]any{"id": "ep-e1", "source": "episodic"})
	text := contentText(t, res)

	var decoded map[string]any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatalf("read content is not JSON: %v (%s)", err, text)
	}
	if _, stringified := decoded["fields_json"]; stringified {
		t.Error("read output carries a stringified fields_json key")
	}
	fields, isObject := decoded["fields"].(map[string]any)
	if !isObject {
		t.Fatalf("fields is %T, want a real JSON object", decoded["fields"])
	}
	if fields["text"] != "full body here" {
		t.Errorf("fields.text = %v", fields["text"])
	}
	if decoded["id"] != "ep-e1" || decoded["source"] != "episodic" {
		t.Errorf("addressing echo = id=%v source=%v", decoded["id"], decoded["source"])
	}
}

// TestToolsCallMemoryReadValidation (dirty): the tool barricade rejects
// missing/blank args and non-drillable sources as tool-level errors, and a
// backend NOT_FOUND surfaces as a tool error, not a protocol failure.
func TestToolsCallMemoryReadValidation(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"missing id", map[string]any{"source": "episodic"}, "non-empty id and source"},
		{"blank source", map[string]any{"id": "ep-1", "source": ""}, "non-empty id and source"},
		{"unknown source", map[string]any{"id": "ep-1", "source": "experience"}, `must be "episodic" or "semantic"`},
		{"graph has no drill", map[string]any{"id": "g-1", "source": "graph"}, "no drill-down"},
		{"unknown id", map[string]any{"id": "ep-missing", "source": "episodic"}, "read failed"},
	}
	c := startServer(t, newFakeBackend())
	c.call("initialize", nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := callReadViaWire(t, c, tc.args)
			if res["isError"] != true {
				t.Fatalf("expected a tool-level error, got %v", res)
			}
			if text := contentText(t, res); !strings.Contains(text, tc.want) {
				t.Errorf("error text %q does not mention %q", text, tc.want)
			}
		})
	}
}

// TestToolsCallMemoryReadWrongSourceIsOpaque (dirty): an id that exists under
// one source but is requested under the other yields the same opaque
// not-found as a nonexistent id — the tool layer adds no oracle of its own.
func TestToolsCallMemoryReadWrongSourceIsOpaque(t *testing.T) {
	c := startServer(t, newFakeBackend())
	c.call("initialize", nil)
	c.call("tools/call", map[string]any{
		"name":      ToolIngest,
		"arguments": map[string]any{"event_id": "e1", "text": "exists episodically"},
	})
	mismatch := callReadViaWire(t, c, map[string]any{"id": "ep-e1", "source": "semantic"})
	unknown := callReadViaWire(t, c, map[string]any{"id": "never-existed", "source": "semantic"})
	if mismatch["isError"] != true || unknown["isError"] != true {
		t.Fatalf("expected tool errors, got %v / %v", mismatch, unknown)
	}
	if a, b := contentText(t, mismatch), contentText(t, unknown); a != b {
		t.Errorf("mismatch (%q) and unknown (%q) are distinguishable: existence leak", a, b)
	}
}

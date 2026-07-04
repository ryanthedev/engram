//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"time"
)

// This file is a SAMPLE scenario pack (DW-3.6). It is added entirely in Phase
// 3 with ZERO edits to the harness core (registry.go / harness.go): it
// registers scenarios from init(), and the core discovers them. Phases 4–8 add
// their own packs the same way — this file is the worked example of the
// documented extension point.

func init() {
	RegisterScenario(Scenario{Name: "sample/mcp-round-trip", Run: sampleRoundTrip})
	RegisterScenario(Scenario{Name: "sample/retraction", Run: sampleRetraction})
}

// sampleRoundTrip: ingest a fact through MCP, then find it through MCP search
// once the async worker has extracted and reconciled it.
func sampleRoundTrip(h *Harness) error {
	tenant := uniqueTenant("sample-rt")
	raw, _, err := h.MintToken(tenant, "alice", "claude", time.Hour)
	if err != nil {
		return err
	}
	conn, err := h.MCP(raw)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.Initialize(); err != nil {
		return err
	}
	if _, err := conn.CallTool("memory_ingest", map[string]any{
		"event_id": tenant + "-e1",
		"text":     "fact: alice | prefers | dark-mode",
	}); err != nil {
		return err
	}
	return waitForHit(conn, "alice prefers dark-mode", "dark-mode")
}

// sampleRetraction: a fact is ingested then retracted; after reconciliation
// the retracted fact is no longer a live search hit.
func sampleRetraction(h *Harness) error {
	tenant := uniqueTenant("sample-rx")
	raw, _, err := h.MintToken(tenant, "bob", "claude", time.Hour)
	if err != nil {
		return err
	}
	conn, err := h.MCP(raw)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.Initialize(); err != nil {
		return err
	}
	if _, err := conn.CallTool("memory_ingest", map[string]any{
		"event_id": tenant + "-e1",
		"text":     "fact: service-x | owner | team-alpha @ 2026-01-01T00:00:00Z",
	}); err != nil {
		return err
	}
	if err := waitForHit(conn, "service-x owner team-alpha", "team-alpha"); err != nil {
		return err
	}
	// Reassign ownership: the live head becomes team-beta.
	if _, err := conn.CallTool("memory_ingest", map[string]any{
		"event_id": tenant + "-e2",
		"text":     "fact: service-x | owner | team-beta @ 2026-06-01T00:00:00Z",
	}); err != nil {
		return err
	}
	return waitForHit(conn, "service-x owner team-beta", "team-beta")
}

// waitForHit polls memory_search until a hit whose fields contain want appears.
func waitForHit(conn *MCPConn, query, want string) error {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		sc, err := conn.CallTool("memory_search", map[string]any{"query": query, "k": 10})
		if err != nil {
			return err
		}
		hits, _ := sc["hits"].([]any)
		for _, hit := range hits {
			hm, _ := hit.(map[string]any)
			fields, _ := hm["fields_json"].(string)
			if strings.Contains(fields, want) {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("search %q never returned a hit containing %q", query, want)
}

// uniqueTenant returns a per-run tenant id so scenarios never collide on a
// shared cluster (Search is tenant-scoped by the token identity).
func uniqueTenant(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

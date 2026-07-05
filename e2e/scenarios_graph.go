//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"time"
)

// Phase-6 incremental-graph scenario pack. Registered from init() with ZERO
// edits to the harness core (registry.go / harness.go) — the documented
// extension point (DW-3.6). Both scenarios drive the FULL local stack: MCP
// ingest -> worker extract/reconcile -> the graph worker stage (D20) upserts
// entities/edges from the landed facts -> MCP search fuses hits and the
// registered GraphExpander (Phase-4 RegisterPostHook seam) performs bounded
// connect-the-dots expansion, re-verified through the ACL before return.

func init() {
	RegisterScenario(Scenario{Name: "graph/connect-the-dots", Run: graphConnectTheDots})
	RegisterScenario(Scenario{Name: "graph/acl-blocked-edge", Run: graphACLBlockedEdge})
}

// graphConnectTheDots (DW-6.2): a fixture knowledge base of two facts,
// ingested in two separate sessions (A works_at B, then B located_in C) —
// the entity B must dedup-merge across both events for the chain to
// connect. A search that only directly matches the A-B fact still surfaces
// C through the graph expansion, the documented A->B->C answer path.
func graphConnectTheDots(h *Harness) error {
	tenant := uniqueTenant("graph-dots")
	raw, _, err := h.MintToken(tenant, "u1", "a1", time.Hour)
	if err != nil {
		return err
	}
	marker := strings.ReplaceAll(tenant, "-", "")
	a, b, c := "A"+marker, "B"+marker, "C"+marker

	sess1, err := h.MCP(raw)
	if err != nil {
		return err
	}
	defer sess1.Close()
	if _, err := sess1.Initialize(); err != nil {
		return err
	}
	if _, err := sess1.CallTool("memory_ingest", map[string]any{
		"event_id": tenant + "-e1",
		"text":     fmt.Sprintf("fact: %s | works_at | %s", a, b),
	}); err != nil {
		return err
	}
	// Wait for the A-B fact to land before ingesting the second episode, so
	// the graph stage's dedup sees B already resolved to one entity.
	if err := waitForHit(sess1, a+" works_at "+b, b); err != nil {
		return fmt.Errorf("A-B fact never landed: %w", err)
	}
	if _, err := sess1.CallTool("memory_ingest", map[string]any{
		"event_id": tenant + "-e2",
		"text":     fmt.Sprintf("fact: %s | located_in | %s", b, c),
	}); err != nil {
		return err
	}
	if err := waitForHit(sess1, b+" located_in "+c, c); err != nil {
		return fmt.Errorf("B-C fact never landed: %w", err)
	}

	// A LATER session searches for the A-B fact; the graph expansion should
	// surface C even though this query never mentions it.
	sess2, err := h.MCP(raw)
	if err != nil {
		return err
	}
	defer sess2.Close()
	if _, err := sess2.Initialize(); err != nil {
		return err
	}
	return waitForGraphHit(sess2, a+" works_at "+b, c)
}

// graphACLBlockedEdge (DW-6.4): u1 (agent a1) ingests A-B privately; a
// DIFFERENT, unrelated identity u2 (agent a2, no reachability grant to a1)
// ingests B-C privately, sharing the SAME entity B (dedup-merged). u1's
// search must expand through the A-B edge (its own) but NEVER surface the
// B-C edge or C — it is reachable only through an edge owned by an agent u1
// cannot read (fail-closed re-verification at the retriever).
func graphACLBlockedEdge(h *Harness) error {
	tenant := uniqueTenant("graph-aclblock")
	raw1, _, err := h.MintToken(tenant, "u1", "a1", time.Hour)
	if err != nil {
		return err
	}
	raw2, _, err := h.MintToken(tenant, "u2", "a2", time.Hour)
	if err != nil {
		return err
	}
	marker := strings.ReplaceAll(tenant, "-", "")
	a, b, c := "A"+marker, "B"+marker, "C"+marker

	sess1, err := h.MCP(raw1)
	if err != nil {
		return err
	}
	defer sess1.Close()
	if _, err := sess1.Initialize(); err != nil {
		return err
	}
	if _, err := sess1.CallTool("memory_ingest", map[string]any{
		"event_id": tenant + "-u1e1",
		"text":     fmt.Sprintf("fact: %s | works_at | %s", a, b),
	}); err != nil {
		return err
	}
	if err := waitForHit(sess1, a+" works_at "+b, b); err != nil {
		return fmt.Errorf("A-B fact never landed: %w", err)
	}

	// u2 (a2), NOT reachable by u1, ingests the connecting B-C fact
	// privately — no acl grant links u1 to a2.
	sess2, err := h.MCP(raw2)
	if err != nil {
		return err
	}
	defer sess2.Close()
	if _, err := sess2.Initialize(); err != nil {
		return err
	}
	if _, err := sess2.CallTool("memory_ingest", map[string]any{
		"event_id": tenant + "-u2e1",
		"text":     fmt.Sprintf("fact: %s | located_in | %s", b, c),
	}); err != nil {
		return err
	}
	// u2 must see it (sanity: it's not globally broken, just not shared).
	if err := waitForHit(sess2, b+" located_in "+c, c); err != nil {
		return fmt.Errorf("B-C fact never landed for its own owner: %w", err)
	}

	// u1 searches repeatedly; C must NEVER appear, in any hit, from any
	// source (a leaked graph expansion or otherwise).
	for i := 0; i < 6; i++ {
		hits, err := searchHitsMCP(sess1, a+" works_at "+b)
		if err != nil {
			return err
		}
		for _, hm := range hits {
			if fj, _ := hm["fields_json"].(string); strings.Contains(fj, c) {
				return fmt.Errorf("u1 saw entity %s, reachable only through a2's unauthorized edge: %v", c, hm)
			}
		}
		time.Sleep(400 * time.Millisecond)
	}
	return nil
}

// waitForGraphHit polls memory_search until a source=="graph" hit whose
// fields contain want appears (the expansion having run and survived ACL
// re-verification).
func waitForGraphHit(conn *MCPConn, query, want string) error {
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		hits, err := searchHitsMCP(conn, query)
		if err != nil {
			return err
		}
		for _, hm := range hits {
			src, _ := hm["source"].(string)
			fj, _ := hm["fields_json"].(string)
			if src == "graph" && strings.Contains(fj, want) {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("no graph-tier hit for %q containing %q within deadline", query, want)
}

//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"time"
)

// Phase-4 ACL scenario pack. Registered from init() with ZERO edits to the
// harness core (registry.go / harness.go) — the documented extension point
// (DW-3.6). Every scenario drives the full local stack through the real CLI
// (token + acl admin, scoped ingest, search, audit): scope enforcement, instant
// revocation, write-time denial, and the audit trail end to end.

func init() {
	RegisterScenario(Scenario{Name: "acl/scope-matrix", Run: aclScopeMatrix})
	RegisterScenario(Scenario{Name: "acl/revoke", Run: aclRevoke})
	RegisterScenario(Scenario{Name: "acl/write-denied", Run: aclWriteDenied})
	RegisterScenario(Scenario{Name: "acl/audit", Run: aclAudit})
}

// grant runs `engram acl grant` (OpenSearch-backed admin, no token needed).
func (h *Harness) grant(tenant, user string, args ...string) error {
	full := append([]string{"acl", "grant", "--tenant", tenant, "--user", user}, args...)
	if out, err := h.RunCLI(nil, full...); err != nil {
		return fmt.Errorf("acl grant %v: %w (%s)", args, err, out)
	}
	return nil
}

// ingestAs ingests one scoped event authenticated as raw, returning any error.
func (h *Harness) ingestAs(raw, eventID, text, scope, team string) (string, error) {
	env := map[string]string{"ENGRAM_TOKEN": raw}
	args := []string{"ingest", "--event-id", eventID, "--text", text}
	if scope != "" {
		args = append(args, "--scope", scope)
	}
	if team != "" {
		args = append(args, "--team", team)
	}
	return h.RunCLI(env, args...)
}

// visibleMarkers searches as raw and returns the set of probe markers present
// in the results (each fact's text carries a unique "MK-<label>" marker).
func (h *Harness) visibleMarkers(raw, query string, markers []string) (map[string]bool, error) {
	out, err := h.RunCLI(map[string]string{"ENGRAM_TOKEN": raw}, "search", query, "-k", "50")
	if err != nil {
		return nil, fmt.Errorf("search: %w (%s)", err, out)
	}
	seen := map[string]bool{}
	for _, m := range markers {
		if strings.Contains(out, m) {
			seen[m] = true
		}
	}
	return seen, nil
}

// aclScopeMatrix (DW-4.1): three identities × three scopes; each identity sees
// exactly its authorized set. Enforcement is on the episodic hits (which carry
// scope/owner/team), so it holds without waiting for async extraction.
func aclScopeMatrix(h *Harness) error {
	tenant := uniqueTenant("acl-matrix")
	a1, _, err := h.MintToken(tenant, "u1", "a1", time.Hour)
	if err != nil {
		return err
	}
	a2, _, err := h.MintToken(tenant, "u2", "a2", time.Hour)
	if err != nil {
		return err
	}
	a3, _, err := h.MintToken(tenant, "u3", "a3", time.Hour)
	if err != nil {
		return err
	}
	// Reachability: memberships, org-write grants, and u1 reaches a2.
	for _, g := range [][]string{
		{"u1", "--team", "teamX"}, {"u2", "--team", "teamX"}, {"u3", "--team", "teamY"},
		{"u1", "--org"}, {"u2", "--org"},
		{"u1", "--agent", "a2"},
	} {
		if err := h.grant(tenant, g[0], g[1:]...); err != nil {
			return err
		}
	}
	// Ingest the six facts (scope write-authorized above).
	probe := "aclprobe" + strings.ReplaceAll(tenant, "-", "")
	facts := []struct{ tok, ev, marker, scope, team string }{
		{a1, "e-a1p", "MK-a1priv", "private", ""},
		{a1, "e-a1t", "MK-a1team", "team", "teamX"},
		{a1, "e-a1o", "MK-a1org", "org", ""},
		{a2, "e-a2t", "MK-a2team", "team", "teamX"},
		{a2, "e-a2o", "MK-a2org", "org", ""},
		{a3, "e-a3t", "MK-a3team", "team", "teamY"},
	}
	all := make([]string, 0, len(facts))
	for _, f := range facts {
		text := fmt.Sprintf("%s %s marker", probe, f.marker)
		if out, err := h.ingestAs(f.tok, tenant+"-"+f.ev, text, f.scope, f.team); err != nil {
			return fmt.Errorf("ingest %s: %w (%s)", f.marker, err, out)
		}
		all = append(all, f.marker)
	}

	// Expected visibility per identity.
	want := map[string]map[string]bool{
		"u1": {"MK-a1priv": true, "MK-a1team": true, "MK-a1org": true, "MK-a2team": true, "MK-a2org": true},
		"u2": {"MK-a2team": true, "MK-a2org": true},
		"u3": {"MK-a3team": true},
	}
	toks := map[string]string{"u1": a1, "u2": a2, "u3": a3}
	for user, tok := range toks {
		// Poll until all expected-visible markers appear (indexing complete).
		if err := waitForMarkers(h, tok, probe, all, want[user]); err != nil {
			return fmt.Errorf("%s: %w", user, err)
		}
		seen, err := h.visibleMarkers(tok, probe, all)
		if err != nil {
			return err
		}
		for _, m := range all {
			if want[user][m] != seen[m] {
				return fmt.Errorf("%s visibility of %s = %v, want %v (all seen=%v)", user, m, seen[m], want[user][m], seen)
			}
		}
	}
	return nil
}

// waitForMarkers polls until every wanted marker is visible to tok (bounding
// the async/indexing delay) so the subsequent exact-set assertion is stable.
func waitForMarkers(h *Harness, tok, query string, all []string, want map[string]bool) error {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		seen, err := h.visibleMarkers(tok, query, all)
		if err != nil {
			return err
		}
		complete := true
		for m := range want {
			if !seen[m] {
				complete = false
				break
			}
		}
		if complete {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for markers %v", want)
}

// aclRevoke (DW-4.2): u1 can see a2's team/org facts via a user_agent edge;
// revoking the edge hides them on the next query, well within 5 s, no restart.
func aclRevoke(h *Harness) error {
	tenant := uniqueTenant("acl-revoke")
	a1, _, err := h.MintToken(tenant, "u1", "a1", time.Hour)
	if err != nil {
		return err
	}
	a2, _, err := h.MintToken(tenant, "u2", "a2", time.Hour)
	if err != nil {
		return err
	}
	for _, g := range [][]string{{"u1", "--team", "teamX"}, {"u2", "--team", "teamX"}, {"u2", "--org"}, {"u1", "--agent", "a2"}} {
		if err := h.grant(tenant, g[0], g[1:]...); err != nil {
			return err
		}
	}
	probe := "aclrev" + strings.ReplaceAll(tenant, "-", "")
	markers := []string{"MK-a2team", "MK-a2org"}
	if out, err := h.ingestAs(a2, tenant+"-r2t", probe+" MK-a2team marker", "team", "teamX"); err != nil {
		return fmt.Errorf("ingest team: %w (%s)", err, out)
	}
	if out, err := h.ingestAs(a2, tenant+"-r2o", probe+" MK-a2org marker", "org", ""); err != nil {
		return fmt.Errorf("ingest org: %w (%s)", err, out)
	}
	if err := waitForMarkers(h, a1, probe, markers, map[string]bool{"MK-a2team": true, "MK-a2org": true}); err != nil {
		return fmt.Errorf("precondition (u1 sees a2 facts): %w", err)
	}

	// Revoke u1→a2 and confirm the hits vanish on the next query, ≤5 s.
	start := time.Now()
	if out, err := h.RunCLI(nil, "acl", "revoke", "--tenant", tenant, "--user", "u1", "--agent", "a2"); err != nil {
		return fmt.Errorf("revoke: %w (%s)", err, out)
	}
	seen, err := h.visibleMarkers(a1, probe, markers)
	if err != nil {
		return err
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		return fmt.Errorf("revocation took %v to bite, want <=5s", elapsed)
	}
	if seen["MK-a2team"] || seen["MK-a2org"] {
		return fmt.Errorf("after revoke u1 still sees a2 facts: %v", seen)
	}
	return nil
}

// aclWriteDenied (DW-4.3): an agent writing a scope it does not reach gets a
// typed denial from the server (the CLI surfaces a non-zero exit + error).
func aclWriteDenied(h *Harness) error {
	tenant := uniqueTenant("acl-deny")
	a1, _, err := h.MintToken(tenant, "u1", "a1", time.Hour)
	if err != nil {
		return err
	}
	// u1 is a member of NO team and holds no org grant. A private write works;
	// a team write to a team it does not belong to must be denied.
	if out, err := h.ingestAs(a1, tenant+"-ok", "acldeny private ok", "private", ""); err != nil {
		return fmt.Errorf("private write should succeed: %w (%s)", err, out)
	}
	out, err := h.ingestAs(a1, tenant+"-bad", "acldeny team nope", "team", "teamX")
	if err == nil {
		return fmt.Errorf("team write to a non-member team was NOT denied; output: %s", out)
	}
	// Then org without a grant is also denied.
	if out, err := h.ingestAs(a1, tenant+"-bad2", "acldeny org nope", "org", ""); err == nil {
		return fmt.Errorf("org write without a grant was NOT denied; output: %s", out)
	}
	return nil
}

// aclAudit (DW-4.6): ingest a fact, let it distill, then `engram audit <id>`
// shows its provenance and version history.
func aclAudit(h *Harness) error {
	tenant := uniqueTenant("acl-audit")
	raw, _, err := h.MintToken(tenant, "u1", "a1", time.Hour)
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
		"event_id": tenant + "-au1",
		"text":     "fact: audit-svc | owner | team-gamma",
	}); err != nil {
		return err
	}
	// Wait for the distilled semantic fact and capture its id.
	factID, err := waitForSemanticID(conn, "audit-svc owner team-gamma", "team-gamma")
	if err != nil {
		return err
	}
	out, err := h.RunCLI(map[string]string{"ENGRAM_TOKEN": raw}, "audit", factID)
	if err != nil {
		return fmt.Errorf("audit %s: %w (%s)", factID, err, out)
	}
	// Provenance (owner agent) and at least one version must be present.
	if !strings.Contains(out, "\"owner_agent_id\": \"a1\"") {
		return fmt.Errorf("audit output missing provenance owner a1:\n%s", out)
	}
	if !strings.Contains(out, "\"versions\"") || !strings.Contains(out, "audit-svc") {
		return fmt.Errorf("audit output missing version history:\n%s", out)
	}
	return nil
}

// waitForSemanticID polls memory_search until a semantic-tier hit whose fields
// contain want appears, returning its doc id.
func waitForSemanticID(conn *MCPConn, query, want string) (string, error) {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		sc, err := conn.CallTool("memory_search", map[string]any{"query": query, "k": 10})
		if err != nil {
			return "", err
		}
		hits, _ := sc["hits"].([]any)
		for _, hit := range hits {
			hm, _ := hit.(map[string]any)
			src, _ := hm["source"].(string)
			fields, _ := hm["fields_json"].(string)
			if src == "semantic" && strings.Contains(fields, want) {
				id, _ := hm["id"].(string)
				if id != "" {
					return id, nil
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return "", fmt.Errorf("no semantic hit for %q containing %q", query, want)
}

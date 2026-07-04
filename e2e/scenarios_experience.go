//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"time"

	"github.com/ryanthedev/engram/internal/experience"
)

// Phase-5 experience-memory scenario pack. Registered from init() with ZERO
// edits to the harness core (DW-3.6 extension point). It drives the full local
// stack: an agent completes a task (a task-completion event carrying both a fact
// and an `experience:` directive), the distillation worker stage gates the
// experience, and a LATER MCP session retrieves the admitted skill through the
// gated experience tier (DW-5.6). A second scenario proves a poisoned experience
// is quarantined and never retrievable.
//
// Note on the fact+experience event shape: the distillation stage runs as part
// of the post-fact worker pipeline, so a distillable task-completion event must
// also yield at least one extracted fact — a completed task naturally emits a
// completion fact, so the event carries a `fact:` line alongside the
// `experience:` line.

func init() {
	RegisterScenario(Scenario{Name: "experience/distill-gate-retrieve", Run: expDistillGateRetrieve})
	RegisterScenario(Scenario{Name: "experience/poison-quarantined", Run: expPoisonQuarantined})
}

// expDistillGateRetrieve (DW-5.6): ingest a task-completion experience in one
// MCP session; retrieve the distilled skill from a second session.
func expDistillGateRetrieve(h *Harness) error {
	tenant := uniqueTenant("exp-dgr")
	raw, _, err := h.MintToken(tenant, "u1", "a1", time.Hour)
	if err != nil {
		return err
	}
	marker := "EXPSKILL" + strings.ReplaceAll(tenant, "-", "")
	text := strings.Join([]string{
		"fact: taskX | status | done",
		"experience: optimize taskX | " + marker + " warm the pipeline cache | success | phi=0.8 | evidence=p95-verified;trace9 | signals=tests_passed",
	}, "\n")

	sess1, err := h.MCP(raw)
	if err != nil {
		return err
	}
	defer sess1.Close()
	if _, err := sess1.Initialize(); err != nil {
		return err
	}
	if _, err := sess1.CallTool("memory_ingest", map[string]any{"event_id": tenant + "-t1", "text": text}); err != nil {
		return err
	}

	// A LATER session retrieves the distilled experience through the gated tier.
	sess2, err := h.MCP(raw)
	if err != nil {
		return err
	}
	defer sess2.Close()
	if _, err := sess2.Initialize(); err != nil {
		return err
	}
	return waitForExperienceHit(sess2, marker+" warm the pipeline cache optimize taskX", marker)
}

// expPoisonQuarantined (DW-5.6 / DW-5.1 end-to-end): a self-reported success
// with no evidence is quarantined — never retrievable — and appears in
// `engram quarantine list`.
func expPoisonQuarantined(h *Harness) error {
	tenant := uniqueTenant("exp-poison")
	raw, _, err := h.MintToken(tenant, "u1", "a1", time.Hour)
	if err != nil {
		return err
	}
	marker := "POISON" + strings.ReplaceAll(tenant, "-", "")
	// A fact to carry the event through the stage pipeline + a poisoned (no
	// evidence) experience directive.
	text := strings.Join([]string{
		"fact: taskY | status | done",
		"experience: dubious taskY | " + marker + " trust me it works | success | phi=0.9",
	}, "\n")

	conn, err := h.MCP(raw)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.Initialize(); err != nil {
		return err
	}
	if _, err := conn.CallTool("memory_ingest", map[string]any{"event_id": tenant + "-y1", "text": text}); err != nil {
		return err
	}

	// It must land in quarantine (poll the CLI admin surface), keyed by fingerprint.
	fp := experience.Fingerprint(tenant, "dubious taskY")
	if err := waitForQuarantine(h, tenant, fp); err != nil {
		return err
	}

	// And it must never be retrievable — search several times; the poison marker
	// must not appear as an experience hit.
	for i := 0; i < 6; i++ {
		hits, err := searchHitsMCP(conn, marker+" trust me dubious taskY")
		if err != nil {
			return err
		}
		for _, hm := range hits {
			if src, _ := hm["source"].(string); src == "experience" {
				if fj, _ := hm["fields_json"].(string); strings.Contains(fj, marker) {
					return fmt.Errorf("quarantined experience became retrievable: %v", hm)
				}
			}
		}
		time.Sleep(400 * time.Millisecond)
	}
	return nil
}

// waitForExperienceHit polls memory_search until an experience-tier hit whose
// fields contain want appears.
func waitForExperienceHit(conn *MCPConn, query, want string) error {
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		hits, err := searchHitsMCP(conn, query)
		if err != nil {
			return err
		}
		for _, hm := range hits {
			src, _ := hm["source"].(string)
			fj, _ := hm["fields_json"].(string)
			if src == "experience" && strings.Contains(fj, want) {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("no experience-tier hit for %q containing %q within deadline", query, want)
}

// searchHitsMCP runs memory_search and returns the raw hit maps.
func searchHitsMCP(conn *MCPConn, query string) ([]map[string]any, error) {
	sc, err := conn.CallTool("memory_search", map[string]any{"query": query, "k": 25})
	if err != nil {
		return nil, err
	}
	raw, _ := sc["hits"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if hm, ok := r.(map[string]any); ok {
			out = append(out, hm)
		}
	}
	return out, nil
}

// waitForQuarantine polls `engram quarantine list` until fingerprint appears.
func waitForQuarantine(h *Harness, tenant, fingerprint string) error {
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		out, err := h.RunCLI(nil, "quarantine", "list", "--tenant", tenant)
		if err == nil && strings.Contains(out, fingerprint) {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("fingerprint %s never appeared in quarantine list for tenant %s", fingerprint, tenant)
}

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/eval"
	"github.com/ryanthedev/engram/internal/eval/gate"
	"github.com/ryanthedev/engram/internal/retrieval"
	"github.com/ryanthedev/engram/internal/store"
)

// This file is Phase 8's e2e/eval scenario pack (Constraint 4 — every phase
// extends the e2e suite): it registers the release-gate pipeline exercised
// end-to-end against the local stack, added entirely via RegisterScenario
// with zero edits to the harness core, exactly like every prior phase's
// pack. It ingests through the REAL pipeline (MCP -> RuleExtractor/stub LLM
// -> reconcile -> OpenSearch), then reads back and runs the same
// eval.RunHallucinationSuite + gate.CheckHallucination the CI release gate
// uses (internal/eval/gate/live_test.go) — proving the gate logic and the
// real extraction pipeline agree, not just the gate logic in isolation.
//
// This scenario only ever asserts grounded facts (the extractor faithfully
// echoes exactly what it was told, so nothing here can be "unsupported"). It
// is intentionally safe to run against real staging (make e2e-cloud, D22) —
// unlike the adversarial bad-release drill, which is opt-in only (see
// drill_bad_release_test.go).

func init() {
	RegisterScenario(Scenario{Name: "eval/hallucination-gate-green", Run: hallucinationGateGreen})
}

// evalFactDirective is one fact this scenario ingests via MCP; Query is the
// probe used to retrieve it back for grounding.
type evalFactDirective struct {
	subject, predicate, object, query string
}

var evalGreenFacts = []evalFactDirective{
	{"engram-mcp", "ships-as", "a-stdio-server", "what does engram-mcp ship as"},
	{"engram-cli", "supports", "token-admin-commands", "does the engram cli support token admin commands"},
	{"the-eval-harness", "measures", "recall-mrr-and-ndcg", "what does the eval harness measure"},
	{"the-release-gate", "combines", "three-independent-detectors", "what does the release gate combine"},
}

// hallucinationGateGreen ingests evalGreenFacts through the real pipeline,
// waits for them to become searchable, then runs the exact same
// hallucination-suite + gate check the CI release gate runs, asserting Pass
// — the gate pipeline, exercised end-to-end against the local stack.
func hallucinationGateGreen(h *Harness) error {
	tenant := uniqueTenant("eval-halu-green")
	raw, _, err := h.MintToken(tenant, "eval-bot", "claude", time.Hour)
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

	gs := eval.HaluGoldSet{}
	for i, f := range evalGreenFacts {
		text := fmt.Sprintf("fact: %s | %s | %s", f.subject, f.predicate, f.object)
		if _, err := conn.CallTool("memory_ingest", map[string]any{
			"event_id": fmt.Sprintf("%s-e%d", tenant, i),
			"text":     text,
		}); err != nil {
			return fmt.Errorf("ingesting fixture %d: %w", i, err)
		}
		statement := fmt.Sprintf("%s %s %s", f.subject, f.predicate, f.object)
		gs.Corpus = append(gs.Corpus, eval.Doc{ID: fmt.Sprintf("f%d", i), Text: statement})
		gs.Cases = append(gs.Cases, eval.HaluCase{ID: fmt.Sprintf("f%d", i), Query: f.query})
		if err := waitForHit(conn, f.query, f.object); err != nil {
			return fmt.Errorf("fixture %d never became searchable: %w", i, err)
		}
	}

	embedder := embed.NewFakeEmbedder(store.EmbeddingDim, nil)
	httpClient := &http.Client{Timeout: 10 * time.Second}
	r := retrieval.NewOpenSearchRetriever(httpClient, h.OSURL, embedder)
	// TenantID scopes the measurement to exactly this scenario run's facts —
	// required on a shared cluster where every other scenario/tenant's data
	// coexists in the same production-shaped indices (DW-1.4's tenancy
	// filter, reused here for isolation rather than security).
	filter := retrieval.Filter{TenantID: tenant, ValidOnly: true}
	rep, err := eval.RunHallucinationSuite(context.Background(), r, gs, eval.RuleHaluJudge{}, 10, filter)
	if err != nil {
		return fmt.Errorf("hallucination measurement: %w", err)
	}
	res := gate.CheckHallucination(rep, gate.DefaultThresholds().HallucinationRateMax)
	if res.Blocks() {
		return fmt.Errorf("eval/hallucination-gate-green: gate reported %s on a faithful release: %s (details=%+v)",
			res.Verdict, res.Detail, rep.Details)
	}
	return nil
}

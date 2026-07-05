//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/eval"
	"github.com/ryanthedev/engram/internal/eval/gate"
	"github.com/ryanthedev/engram/internal/retrieval"
	"github.com/ryanthedev/engram/internal/store"
)

// TestBadReleaseDrill_HallucinationGateBlocks is DW-8.4's e2e-level bad-
// release drill: it ingests poison directives through the REAL, unmodified
// pipeline (MCP -> RuleExtractor/stub LLM -> reconcile -> OpenSearch — the
// same faithful-echo extractor eval/hallucination-gate-green exercises) and
// asserts the hallucination gate correctly blocks the resulting "release."
//
// It is opt-in via ENGRAM_EVAL_DRILL=1 and skipped otherwise — by default,
// under ANY invocation (`make e2e`, `make e2e-cloud`, or a bare
// `go test -tags=e2e`). Boot()'s external mode (ENGRAM_E2E_ADDR) is used
// identically by the local compose stack and by real staging (D22) — there
// is no reliable runtime signal to tell them apart — so "skip unless
// external" is not a safe guard here. An explicit, dedicated opt-in is: only
// `make eval-drill` sets ENGRAM_EVAL_DRILL, so this drill can never fire as
// a side effect of a routine e2e run or a real deploy pipeline's
// `make e2e-cloud` step against persistent staging. See the Design doc's
// Decision 4.
func TestBadReleaseDrill_HallucinationGateBlocks(t *testing.T) {
	if os.Getenv("ENGRAM_EVAL_DRILL") == "" {
		t.Skip("opt-in only: set ENGRAM_EVAL_DRILL=1 (see `make eval-drill`) — never runs automatically, including against staging")
	}

	tenant := uniqueTenant("eval-halu-drill")
	raw, _, err := harness.MintToken(tenant, "eval-bot", "claude", time.Hour)
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	conn, err := harness.MCP(raw)
	if err != nil {
		t.Fatalf("spawn mcp: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Initialize(); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	// Poison directives: facts sharing zero vocabulary with anything in
	// eval.HaluFixtures()'s known-good corpus. RuleExtractor/the stub LLM
	// take directives at face value (syntax, not truth) — exactly what a
	// degraded extraction prompt would produce (see the Design doc,
	// Decision 3). No internal/ingest or cmd/engram-stub-llm edit needed.
	poison := []struct{ subject, predicate, object, query string }{
		{"shadow-directorate", "secretly-controls", "the-nuclear-launch-codes", "shadow-directorate nuclear launch codes"},
		{"phantom-vendor-nine", "has-silent-root-access-to", "every-customer-payment-token", "phantom-vendor-nine payment tokens"},
	}
	gs := eval.HaluGoldSet{Corpus: eval.HaluFixtures().Corpus} // judge against the REAL known-good corpus
	for i, p := range poison {
		if _, err := conn.CallTool("memory_ingest", map[string]any{
			"event_id": tenant + "-drill-" + p.subject,
			"text":     "fact: " + p.subject + " | " + p.predicate + " | " + p.object,
		}); err != nil {
			t.Fatalf("ingesting poison fixture %d: %v", i, err)
		}
		gs.Cases = append(gs.Cases, eval.HaluCase{ID: p.subject, Query: p.query})
		if err := waitForHit(conn, p.query, p.object); err != nil {
			t.Fatalf("poison fixture %d never became searchable: %v", i, err)
		}
	}

	embedder := embed.NewFakeEmbedder(store.EmbeddingDim, nil)
	httpClient := &http.Client{Timeout: 10 * time.Second}
	r := retrieval.NewOpenSearchRetriever(httpClient, harness.OSURL, embedder)
	filter := retrieval.Filter{TenantID: tenant, ValidOnly: true}
	rep, err := eval.RunHallucinationSuite(context.Background(), r, gs, eval.RuleHaluJudge{}, 10, filter)
	if err != nil {
		t.Fatalf("hallucination measurement: %v", err)
	}
	res := gate.CheckHallucination(rep, gate.DefaultThresholds().HallucinationRateMax)
	if !res.Blocks() {
		t.Fatalf("BAD-RELEASE DRILL FAILED: the hallucination gate let a poisoned release through end-to-end "+
			"(rate=%.4f, details=%+v) — evidence: this test's exit code and log", rep.Rate, rep.Details)
	}
	t.Logf("e2e bad-release drill: gate correctly blocked (rate=%.4f, verdict=%s) — evidence in this CI job's log", rep.Rate, res.Verdict)
}

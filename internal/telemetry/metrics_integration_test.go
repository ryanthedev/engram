//go:build integration

package telemetry

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/ryanthedev/engram/internal/experience"
	"github.com/ryanthedev/engram/internal/graph"
	"github.com/ryanthedev/engram/internal/testutil"
)

// sanitize turns a test name into an OpenSearch-safe index-name suffix
// (lowercase alnum, everything else becomes '-') — mirrors the identical
// helper in internal/experience's and internal/graph's own
// opensearch_integration_test.go (each package keeps its own copy, per this
// codebase's self-contained-per-package convention).
func sanitize(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r+('a'-'A'))
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}

// TestIntegration_MetricsExposesDurableInventoryAndGraphGauges is the DW-3.3
// evidence: against the LIVE dev cluster, admitting one experience and
// upserting one graph entity makes the durable inventory gauges
// (engram_experience_admitted_count / _quarantined_count) and the graph
// gauge (engram_graph_entity_count) appear on a real /metrics scrape with
// non-garbage (matching, non-zero where expected) values — proving the
// whole wiring (Backend _count -> Store method -> Recorder -> Gauges ->
// Prometheus exposition) end to end, not just each layer in isolation.
func TestIntegration_MetricsExposesDurableInventoryAndGraphGauges(t *testing.T) {
	ctx := context.Background()
	base := testutil.OpenSearchURL()
	name := sanitize(t.Name())

	// --- experience T3 scratch indices ---
	if err := experience.Apply(ctx, testutil.HTTPClient, base); err != nil {
		t.Fatalf("apply experience templates: %v", err)
	}
	admittedIdx := "engram-experience-000001-" + name
	quarantineIdx := "engram-experience-quarantine-000001-" + name
	testutil.CreateScratchIndex(t, base, admittedIdx)
	testutil.CreateScratchIndex(t, base, quarantineIdx)
	t.Cleanup(func() {
		testutil.DeleteIndex(t, base, admittedIdx)
		testutil.DeleteIndex(t, base, quarantineIdx)
	})
	expBackend := experience.NewOpenSearchBackend(testutil.HTTPClient, base, experience.WithIndices(admittedIdx, quarantineIdx))
	expStore, err := experience.NewStore(experience.RuleGatekeeper{}, expBackend, slog.Default())
	if err != nil {
		t.Fatalf("experience.NewStore: %v", err)
	}
	exp := experience.Experience{
		TenantID: "ten", OwnerAgentID: "ag", Scope: "private",
		Task: "metrics integration probe", DistilledSkill: "prove the gauge renders", Utility: 0.9,
		Outcome: experience.Outcome{Success: true, Evidence: []string{"scrape shows the value"}},
	}
	if v, err := expStore.Admit(ctx, exp); err != nil || v != experience.Admit {
		t.Fatalf("Admit => %v, %v; want Admit", v, err)
	}

	// --- graph T4 scratch indices ---
	if err := graph.Apply(ctx, testutil.HTTPClient, base); err != nil {
		t.Fatalf("apply graph templates: %v", err)
	}
	entityIdx := "engram-graph-entities-000001-" + name
	edgeIdx := "engram-graph-edges-000001-" + name
	testutil.CreateScratchIndex(t, base, entityIdx)
	testutil.CreateScratchIndex(t, base, edgeIdx)
	t.Cleanup(func() {
		testutil.DeleteIndex(t, base, entityIdx)
		testutil.DeleteIndex(t, base, edgeIdx)
	})
	graphBackend := graph.NewOpenSearchBackend(testutil.HTTPClient, base, graph.WithIndices(entityIdx, edgeIdx))
	dedup, err := graph.NewDeduper(graph.RuleJudge{})
	if err != nil {
		t.Fatalf("graph.NewDeduper: %v", err)
	}
	graphStore := graph.NewStore(graphBackend, dedup, nil, slog.Default())
	if _, _, err := graphStore.UpsertMention(ctx, graph.Mention{TenantID: "ten", Name: "acme", Context: "acme ships widgets", SourceID: "ev-1"}); err != nil {
		t.Fatalf("UpsertMention: %v", err)
	}

	// --- wire the Recorder exactly as main.go does and poll once ---
	provider, err := NewProvider()
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	gauges, err := NewGauges(provider.Meter("integration-test"))
	if err != nil {
		t.Fatalf("NewGauges: %v", err)
	}
	rec := &Recorder{Gauges: gauges, GateInventory: expStore, Graph: graphStore, Logger: slog.Default()}
	rec.Poll(ctx)

	body := scrape(t, provider)
	for _, name := range []string{"engram_experience_admitted_count", "engram_experience_quarantined_count", "engram_graph_entity_count"} {
		if !strings.Contains(body, name) {
			t.Errorf("scrape missing gauge %s:\n%s", name, body)
		}
	}
	wantGauge(t, body, "engram_experience_admitted_count", 1)
	wantGauge(t, body, "engram_experience_quarantined_count", 0)
	wantGauge(t, body, "engram_graph_entity_count", 1)
}

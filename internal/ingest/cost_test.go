package ingest_test

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/ryanthedev/engram/internal/ingest"
	"github.com/ryanthedev/engram/internal/memory"
)

// TestCostMeterMath pins the pricing arithmetic, including how batching
// counts toward the per-event metric: one call covering many events divides
// its tokens across all of them.
func TestCostMeterMath(t *testing.T) {
	p := ingest.Pricing{Model: "test", InputUSDPer1M: 2.0, OutputUSDPer1M: 10.0}
	m := &ingest.CostMeter{}

	if got := m.Snapshot().CostPer1kEventsUSD(p); got != 0 {
		t.Fatalf("zero-event cost = %v, want 0", got)
	}

	m.Record(4, 1_000_000, 100_000) // one batched call over 4 events
	u := m.Snapshot()
	wantCost := 2.0 + 1.0 // $2 input + $1 output
	if got := u.CostUSD(p); math.Abs(got-wantCost) > 1e-9 {
		t.Errorf("CostUSD = %v, want %v", got, wantCost)
	}
	wantPer1k := wantCost / 4 * 1000
	if got := u.CostPer1kEventsUSD(p); math.Abs(got-wantPer1k) > 1e-9 {
		t.Errorf("CostPer1kEventsUSD = %v, want %v", got, wantPer1k)
	}
}

// TestDW_2_6_ExtractionCostGate is the S1 budget gate: on a fixed synthetic
// workload of 1,000 events through the metered cheap-model path (simulated
// token counts on the deterministic extractor — the plumbing DW-2.6 gates at
// this stage), the measured extraction cost must be ≤ $5 per 1k events at
// the pinned model's list price (ingest.DefaultPricing).
func TestDW_2_6_ExtractionCostGate(t *testing.T) {
	meter := &ingest.CostMeter{}
	ex := &ingest.RuleExtractor{Meter: meter}
	ctx := context.Background()

	const events = 1000
	for i := 0; i < events; i++ {
		// A representative event: some prose plus two extractable facts.
		ev := memory.Episodic{
			EventID:  fmt.Sprintf("cost-ev-%d", i),
			TenantID: "t1",
			Text: fmt.Sprintf("deploy pipeline run %d finished; artifacts uploaded to the registry\n"+
				"fact: service-%d | owner | team-core\nfact: service-%d | tier | gold", i, i%50, i%50),
		}
		if _, err := ex.Extract(ctx, []memory.Episodic{ev}); err != nil {
			t.Fatalf("extracting event %d: %v", i, err)
		}
	}

	u := meter.Snapshot()
	if u.Events != events {
		t.Fatalf("meter recorded %d events, want %d", u.Events, events)
	}
	if u.PromptTokens == 0 || u.CompletionTokens == 0 {
		t.Fatal("meter recorded zero tokens — the cost plumbing is not measuring")
	}
	cost := u.CostPer1kEventsUSD(ingest.DefaultPricing)
	t.Logf("extraction cost on %s: $%.4f per 1k events (prompt=%d completion=%d tokens)",
		ingest.DefaultPricing.Model, cost, u.PromptTokens, u.CompletionTokens)
	if cost <= 0 {
		t.Fatal("measured cost must be positive")
	}
	if cost > 5.0 {
		t.Fatalf("extraction cost $%.4f per 1k events exceeds the $5 S1 budget gate (DW-2.6)", cost)
	}
}

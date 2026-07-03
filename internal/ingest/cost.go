package ingest

import "sync"

// Pricing is the list price of the pinned extraction model (DW-2.6's cost
// gate computes against it). Configuration, not discovery: the gate must be
// stable and auditable, so the price is pinned here and overridable by flags.
type Pricing struct {
	// Model is the pinned cheap-model identifier.
	Model string
	// InputUSDPer1M / OutputUSDPer1M are USD list prices per million tokens.
	InputUSDPer1M  float64
	OutputUSDPer1M float64
}

// DefaultPricing pins the skeleton's cheap extraction model: gpt-4o-mini at
// its public list price (USD per 1M tokens, verified 2026-07). The S1 budget
// gate is ≤ $5 per 1k events (DW-2.6).
var DefaultPricing = Pricing{Model: "gpt-4o-mini", InputUSDPer1M: 0.15, OutputUSDPer1M: 0.60}

// Usage is an immutable snapshot of accumulated extraction usage.
type Usage struct {
	Events           int64
	PromptTokens     int64
	CompletionTokens int64
}

// CostUSD prices the snapshot at p.
func (u Usage) CostUSD(p Pricing) float64 {
	return float64(u.PromptTokens)/1e6*p.InputUSDPer1M + float64(u.CompletionTokens)/1e6*p.OutputUSDPer1M
}

// CostPer1kEventsUSD is the DW-2.6 gate metric: extraction cost normalized
// per 1,000 events. Batching counts toward it naturally — a batched call
// records its token totals against every event in the batch. Zero events
// yields zero.
func (u Usage) CostPer1kEventsUSD(p Pricing) float64 {
	if u.Events == 0 {
		return 0
	}
	return u.CostUSD(p) / float64(u.Events) * 1000
}

// CostMeter accumulates extraction usage across workers (goroutine-safe).
// The HTTP extractor records real token counts from the model's usage block;
// the deterministic fake records simulated counts — the metric plumbing is
// identical either way (DW-2.6 gates the plumbing at this stage).
type CostMeter struct {
	mu sync.Mutex
	u  Usage
}

// Record adds one extraction call's usage: how many events it covered and
// the prompt/completion token counts it consumed.
func (m *CostMeter) Record(events, promptTokens, completionTokens int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.u.Events += int64(events)
	m.u.PromptTokens += int64(promptTokens)
	m.u.CompletionTokens += int64(completionTokens)
}

// Snapshot returns the accumulated usage.
func (m *CostMeter) Snapshot() Usage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.u
}

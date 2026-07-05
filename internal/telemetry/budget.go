package telemetry

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/ryanthedev/engram/internal/memory"
)

// BudgetAlarm evaluates the extraction cost-per-1k-events metric against a
// USD threshold (DW-7.6). It is a plain value comparator — Evaluate takes the
// already-computed metric rather than reaching into ingest.CostMeter itself,
// so it has no dependency on the ingest package and is trivially unit
// testable with synthetic overspend values.
type BudgetAlarm struct {
	// ThresholdUSD is the DW-2.6 budget ($5/1k events at S1, overridable for
	// staging/prod tuning).
	ThresholdUSD float64
	// Switch, when non-nil, is tripped the moment the alarm first fires and
	// reset the moment cost falls back under threshold — the extraction
	// kill-switch (DW-7.6). Nil means "alarm without a kill-switch" (alerting
	// only, no enforcement) — a valid configuration for a dry-run/staging
	// alarm that should page without halting extraction.
	Switch *KillSwitch

	firing atomic.Bool
}

// Evaluate records whether costPer1k breaches ThresholdUSD, trips/resets the
// kill-switch accordingly, and returns the new firing state.
func (a *BudgetAlarm) Evaluate(costPer1k float64) bool {
	firing := costPer1k > a.ThresholdUSD
	a.firing.Store(firing)
	if a.Switch != nil {
		if firing {
			a.Switch.Trip()
		} else {
			a.Switch.Reset()
		}
	}
	return firing
}

// Firing reports the alarm's last-evaluated state.
func (a *BudgetAlarm) Firing() bool {
	return a.firing.Load()
}

// KillSwitch is a process-wide atomic flag: tripped, it makes
// GatedExtractor.Extract fail closed instead of calling through to the real
// (billed) extractor. It never drops events — a failed Extract call returns
// to the worker's normal retry/backoff/dead-letter path (DW-7.6's "kill the
// spend, don't lose the event").
type KillSwitch struct {
	tripped atomic.Bool
}

// Trip halts extraction calls through any GatedExtractor wrapping this switch.
func (k *KillSwitch) Trip() { k.tripped.Store(true) }

// Reset resumes extraction calls.
func (k *KillSwitch) Reset() { k.tripped.Store(false) }

// Tripped reports the current state.
func (k *KillSwitch) Tripped() bool { return k.tripped.Load() }

// ErrBudgetExceeded is returned by GatedExtractor when the kill-switch is
// tripped. It is a plain sentinel (not wrapped further) so callers can
// errors.Is against it in tests and ops logs.
var ErrBudgetExceeded = errors.New("telemetry: extraction budget exceeded; kill-switch tripped")

// extractor is the minimal shape GatedExtractor needs — duck-typed against
// ingest.Extractor so this package depends on ingest's exported interface
// (allowed: ingest does not import telemetry back) without needing the
// concrete ingest.HTTPExtractor/RuleExtractor types.
type extractor interface {
	Extract(ctx context.Context, events []memory.Episodic) ([]memory.SemanticFact, error)
}

// GatedExtractor wraps an ingest.Extractor with a KillSwitch: Extract fails
// fast with ErrBudgetExceeded while the switch is tripped, and delegates
// normally otherwise. It satisfies ingest.Extractor itself, so it drops in
// wherever an Extractor is wired (cmd/engram-server).
type GatedExtractor struct {
	Extractor extractor
	Switch    *KillSwitch
}

// Extract implements ingest.Extractor.
func (g *GatedExtractor) Extract(ctx context.Context, events []memory.Episodic) ([]memory.SemanticFact, error) {
	if g.Switch != nil && g.Switch.Tripped() {
		return nil, fmt.Errorf("%w", ErrBudgetExceeded)
	}
	return g.Extractor.Extract(ctx, events)
}

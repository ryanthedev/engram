package telemetry

import (
	"context"
	"errors"
	"testing"

	"github.com/ryanthedev/engram/internal/memory"
)

// TestBudgetAlarm_Fires is the DW-7.6 synthetic-overspend test: a cost
// reading above threshold fires the alarm; a subsequent reading back under
// threshold clears it.
func TestBudgetAlarm_Fires(t *testing.T) {
	a := &BudgetAlarm{ThresholdUSD: 5.0}

	if a.Evaluate(4.99) {
		t.Fatal("alarm should not fire under threshold")
	}
	if a.Firing() {
		t.Fatal("Firing() should be false under threshold")
	}

	if !a.Evaluate(5.01) {
		t.Fatal("alarm should fire over threshold (synthetic overspend)")
	}
	if !a.Firing() {
		t.Fatal("Firing() should be true after a breach")
	}

	if a.Evaluate(1.0) {
		t.Fatal("alarm should clear once cost falls back under threshold")
	}
}

// TestBudgetAlarm_KillSwitchTrips proves the alarm trips its bound
// KillSwitch on breach and resets it on recovery (DW-7.6's kill-switch).
func TestBudgetAlarm_KillSwitchTrips(t *testing.T) {
	sw := &KillSwitch{}
	a := &BudgetAlarm{ThresholdUSD: 5.0, Switch: sw}

	a.Evaluate(1.0)
	if sw.Tripped() {
		t.Fatal("switch should not be tripped under threshold")
	}

	a.Evaluate(10.0)
	if !sw.Tripped() {
		t.Fatal("switch should trip on breach")
	}

	a.Evaluate(1.0)
	if sw.Tripped() {
		t.Fatal("switch should reset once cost recovers")
	}
}

type stubExtractor struct {
	calls int
}

func (s *stubExtractor) Extract(context.Context, []memory.Episodic) ([]memory.SemanticFact, error) {
	s.calls++
	return nil, nil
}

// TestGatedExtractor_TrippedSwitchBlocksExtraction proves the kill-switch
// actually halts extraction calls (no silent pass-through, no event loss —
// the caller sees ErrBudgetExceeded and the worker's existing retry/DLQ path
// takes over, same as any other extractor failure).
func TestGatedExtractor_TrippedSwitchBlocksExtraction(t *testing.T) {
	sw := &KillSwitch{}
	inner := &stubExtractor{}
	gated := &GatedExtractor{Extractor: inner, Switch: sw}

	if _, err := gated.Extract(context.Background(), nil); err != nil {
		t.Fatalf("expected no error while switch is reset, got %v", err)
	}
	if inner.calls != 1 {
		t.Fatalf("expected inner extractor called once, got %d", inner.calls)
	}

	sw.Trip()
	_, err := gated.Extract(context.Background(), nil)
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("expected ErrBudgetExceeded, got %v", err)
	}
	if inner.calls != 1 {
		t.Fatalf("expected inner extractor NOT called while tripped, got %d calls", inner.calls)
	}

	sw.Reset()
	if _, err := gated.Extract(context.Background(), nil); err != nil {
		t.Fatalf("expected no error after reset, got %v", err)
	}
	if inner.calls != 2 {
		t.Fatalf("expected inner extractor called again after reset, got %d", inner.calls)
	}
}

// TestGatedExtractor_NilSwitchPassesThrough proves a GatedExtractor with no
// bound switch behaves as a plain pass-through (an alarm-only, no-kill-switch
// configuration is valid — see BudgetAlarm.Switch's doc comment).
func TestGatedExtractor_NilSwitchPassesThrough(t *testing.T) {
	inner := &stubExtractor{}
	gated := &GatedExtractor{Extractor: inner}
	if _, err := gated.Extract(context.Background(), nil); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if inner.calls != 1 {
		t.Fatalf("expected inner extractor called, got %d calls", inner.calls)
	}
}

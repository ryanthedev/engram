package gate_test

import (
	"testing"

	"github.com/ryanthedev/engram/internal/eval/gate"
)

// TestIsFlaky_TwoConsecutiveContradictions is the DW-8.5 dirty test: a gate
// whose last two runs already alternate (Fail, Pass) and now flips again
// (back to Fail) is flaky.
func TestIsFlaky_TwoConsecutiveContradictions(t *testing.T) {
	history := []gate.Verdict{gate.Pass, gate.Fail, gate.Pass}
	if !gate.IsFlaky(history, gate.Fail) {
		t.Fatal("two consecutive alternations must be flagged flaky")
	}
}

func TestIsFlaky_StableHistoryNotFlagged(t *testing.T) {
	cases := []struct {
		name    string
		history []gate.Verdict
		next    gate.Verdict
	}{
		{"all passing, stays passing", []gate.Verdict{gate.Pass, gate.Pass, gate.Pass}, gate.Pass},
		{"a single fix (one flip) is not flaky", []gate.Verdict{gate.Fail, gate.Fail, gate.Pass}, gate.Pass},
		{"a single new regression (one flip) is not flaky", []gate.Verdict{gate.Pass, gate.Pass, gate.Pass}, gate.Fail},
		{"too little history", []gate.Verdict{gate.Fail}, gate.Pass},
		{"empty history", nil, gate.Pass},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if gate.IsFlaky(c.history, c.next) {
				t.Fatalf("history=%v next=%v must not be flagged flaky", c.history, c.next)
			}
		})
	}
}

// TestApplyQuarantine_StillBlocksButLabelsFlaky proves quarantining a gate
// does not silently un-gate it: the overridden verdict must still Blocks().
func TestApplyQuarantine_StillBlocksButLabelsFlaky(t *testing.T) {
	history := []gate.Verdict{gate.Pass, gate.Fail}
	result := gate.Result{Gate: "hallucination", Verdict: gate.Pass, Detail: "rate ok"}
	got := gate.ApplyQuarantine(result, history)
	if got.Verdict != gate.Quarantined {
		t.Fatalf("flaky history must quarantine, got %+v", got)
	}
	if !got.Blocks() {
		t.Fatal("a quarantined gate must still Blocks() — quarantine is not un-gating")
	}
}

func TestApplyQuarantine_StableHistoryPassesThrough(t *testing.T) {
	history := []gate.Verdict{gate.Pass, gate.Pass}
	result := gate.Result{Gate: "hallucination", Verdict: gate.Pass, Detail: "rate ok"}
	got := gate.ApplyQuarantine(result, history)
	if got.Verdict != gate.Pass {
		t.Fatalf("stable-pass history must not be quarantined, got %+v", got)
	}
	if got.Blocks() {
		t.Fatal("a genuinely passing, non-flaky gate must not block")
	}
}

func TestResult_Blocks(t *testing.T) {
	if (gate.Result{Verdict: gate.Pass}).Blocks() {
		t.Fatal("Pass must not block")
	}
	if !(gate.Result{Verdict: gate.Fail}).Blocks() {
		t.Fatal("Fail must block")
	}
	if !(gate.Result{Verdict: gate.Quarantined}).Blocks() {
		t.Fatal("Quarantined must block")
	}
}

package gate_test

import (
	"testing"

	"github.com/ryanthedev/engram/internal/eval/gate"
)

func TestCheckFollowingCorrelation_WithinBandPasses(t *testing.T) {
	res := gate.CheckFollowingCorrelation(0.6, 0.3)
	if res.Verdict != gate.Pass {
		t.Fatalf("correlation above the floor must pass, got %+v", res)
	}
}

func TestCheckFollowingCorrelation_BelowBandFails(t *testing.T) {
	res := gate.CheckFollowingCorrelation(0.1, 0.3)
	if res.Verdict != gate.Fail {
		t.Fatalf("correlation below the floor must fail, got %+v", res)
	}
}

// TestCheckFollowingCorrelation_AtBandFloorBoundary: correlation exactly at
// the floor passes (a ">=" contract).
func TestCheckFollowingCorrelation_AtBandFloorBoundary(t *testing.T) {
	res := gate.CheckFollowingCorrelation(0.3, 0.3)
	if res.Verdict != gate.Pass {
		t.Fatalf("correlation exactly at the floor must pass, got %+v", res)
	}
	res = gate.CheckFollowingCorrelation(0.2999, 0.3)
	if res.Verdict != gate.Fail {
		t.Fatalf("correlation just under the floor must fail, got %+v", res)
	}
}

// TestCheckFollowingCorrelation_NegativeCorrelationFails is a dirty test: a
// poisoned store (D5's threat model) drives following correlation toward or
// below zero — the gate must catch that, not just extreme negatives.
func TestCheckFollowingCorrelation_NegativeCorrelationFails(t *testing.T) {
	res := gate.CheckFollowingCorrelation(-0.9, 0.3)
	if res.Verdict != gate.Fail {
		t.Fatalf("negative correlation must fail, got %+v", res)
	}
}

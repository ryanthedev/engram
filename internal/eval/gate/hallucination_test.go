package gate_test

import (
	"testing"

	"github.com/ryanthedev/engram/internal/eval"
	"github.com/ryanthedev/engram/internal/eval/gate"
)

func TestCheckHallucination_PassesUnderThreshold(t *testing.T) {
	rep := eval.HaluReport{Cases: 2, Assertions: 20, Hallucinated: 1, Rate: 0.05}
	res := gate.CheckHallucination(rep, 0.10)
	if res.Verdict != gate.Pass {
		t.Fatalf("rate under threshold must pass, got %+v", res)
	}
}

func TestCheckHallucination_BreachFails(t *testing.T) {
	rep := eval.HaluReport{Cases: 2, Assertions: 10, Hallucinated: 5, Rate: 0.5}
	res := gate.CheckHallucination(rep, 0.10)
	if res.Verdict != gate.Fail {
		t.Fatalf("rate over threshold must fail, got %+v", res)
	}
}

// TestCheckHallucination_AtThresholdBoundary: rate exactly at maxRate must
// pass (a "<=" contract).
func TestCheckHallucination_AtThresholdBoundary(t *testing.T) {
	rep := eval.HaluReport{Cases: 1, Assertions: 100, Hallucinated: 10, Rate: 0.10}
	res := gate.CheckHallucination(rep, 0.10)
	if res.Verdict != gate.Pass {
		t.Fatalf("rate exactly at the ceiling must pass, got %+v", res)
	}
	rep.Rate = 0.1001
	res = gate.CheckHallucination(rep, 0.10)
	if res.Verdict != gate.Fail {
		t.Fatalf("rate just over the ceiling must fail, got %+v", res)
	}
}

func TestCheckHallucination_ZeroAssertionsPasses(t *testing.T) {
	res := gate.CheckHallucination(eval.HaluReport{Cases: 1, Assertions: 0, Rate: 0}, 0.10)
	if res.Verdict != gate.Pass {
		t.Fatalf("zero assertions (nothing to hallucinate) must pass, got %+v", res)
	}
}

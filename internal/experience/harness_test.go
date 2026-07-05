package experience

import (
	"math"
	"testing"
)

// TestDW_5_4_CuratedSmallBeatsLargeNoisy: the reproduced memory-management
// result — a small curated experience set yields higher retrieval uplift than a
// large noisy one, with the gate removing the poison. The A/B report is emitted.
func TestDW_5_4_CuratedSmallBeatsLargeNoisy(t *testing.T) {
	report := ABEval(RuleGatekeeper{}, CuratedSmall(), LargeNoisy())
	report.Emit(nil) // "logged by the harness"

	if !report.CuratedBeatsNoisy {
		t.Fatalf("curated did not beat noisy: %+v", report)
	}
	if report.CuratedUplift <= report.NoisyUplift {
		t.Fatalf("curated uplift %.3f <= noisy uplift %.3f", report.CuratedUplift, report.NoisyUplift)
	}
	// The gate must have kept poison out of the noisy arm: its admitted count is
	// well below its input size.
	if report.NoisyAdmitted >= len(LargeNoisy()) {
		t.Fatalf("noisy arm admitted everything (%d); gate should have caught poison", report.NoisyAdmitted)
	}
	t.Logf("A/B: curated uplift=%.3f (n=%d) vs noisy uplift=%.3f (n=%d)",
		report.CuratedUplift, report.CuratedAdmitted, report.NoisyUplift, report.NoisyAdmitted)
}

// TestDW_5_5_FollowingCorrelationEmitted: the experience-following correlation
// is computed and emitted. High-utility followed experiences correlate with
// success (r > 0) — the health signal a poisoned store would invert.
func TestDW_5_5_FollowingCorrelationEmitted(t *testing.T) {
	samples := []FollowingSample{
		{Utility: 0.9, Followed: true, OutcomeSuccess: true},
		{Utility: 0.8, Followed: true, OutcomeSuccess: true},
		{Utility: 0.2, Followed: true, OutcomeSuccess: false},
		{Utility: 0.1, Followed: true, OutcomeSuccess: false},
		{Utility: 0.95, Followed: false, OutcomeSuccess: false}, // not followed → ignored
	}
	r := FollowingCorrelation(samples)
	if r <= 0 {
		t.Fatalf("following correlation = %.3f, want > 0 (following good experiences helps)", r)
	}
	m := Metrics{Following: r, Admitted: 2, Quarantined: 2, Rejected: 0}
	m.Emit(nil) // emitted to the harness/ops logs
}

// TestFollowingCorrelation_Degenerate: a degenerate sample is NaN-safe (returns
// 0, never NaN), so the metric never poisons a downstream gate.
func TestFollowingCorrelation_Degenerate(t *testing.T) {
	for _, s := range [][]FollowingSample{
		nil,
		{{Utility: 0.5, Followed: true, OutcomeSuccess: true}},                                                       // < 2 points
		{{Utility: 0.5, Followed: true, OutcomeSuccess: true}, {Utility: 0.5, Followed: true, OutcomeSuccess: true}}, // zero variance
	} {
		if r := FollowingCorrelation(s); math.IsNaN(r) || r != 0 {
			t.Fatalf("degenerate sample => %v, want 0", r)
		}
	}
}

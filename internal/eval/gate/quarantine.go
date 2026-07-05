package gate

// This file implements the DW-8.5 flaky-gate edge case: "two consecutive
// contradictory runs -> auto-quarantine the gate + alert, don't silently
// un-gate." A gate is flaky when its last two recorded verdicts disagree
// (Pass then Fail, or Fail then Pass) AND the run about to be recorded
// disagrees with the most recent of those too — i.e. three verdicts in a
// row that alternate. A single flip (one disagreement) is not enough to
// quarantine; that is normal — a gate that was Fail and is now Pass because
// someone fixed the regression is the whole point of a gate. Two
// alternations running is the flapping signature a real quarantine should
// catch instead of trusting either verdict.

// IsFlaky reports whether appending next to history's most recent verdicts
// would produce two consecutive alternations (A,B,A pattern with A != B) —
// the flapping signature. history is ordered oldest-first; only the last two
// entries matter.
func IsFlaky(history []Verdict, next Verdict) bool {
	n := len(history)
	if n < 2 {
		return false
	}
	last, prev := history[n-1], history[n-2]
	return prev != last && last != next
}

// ApplyQuarantine overrides result's verdict to Quarantined when history
// shows the gate is flaky (per IsFlaky), preserving the original verdict in
// Detail for the record. Quarantined still Blocks() — see Result.Blocks
// — so quarantining a gate never silently un-gates a release; it surfaces
// the instability loudly instead of trusting a single contradictory run
// either way.
func ApplyQuarantine(result Result, history []Verdict) Result {
	if !IsFlaky(history, result.Verdict) {
		return result
	}
	original := result.Verdict
	result.Verdict = Quarantined
	result.Detail = "FLAKY: two consecutive contradictory runs — quarantined for investigation, still blocking promotion. Last raw verdict: " +
		string(original) + ". " + result.Detail
	return result
}

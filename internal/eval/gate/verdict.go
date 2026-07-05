package gate

import "time"

// Verdict is a single gate's outcome.
type Verdict string

const (
	// Pass means the metric met its threshold.
	Pass Verdict = "pass"
	// Fail means the metric breached its threshold — blocks promotion.
	Fail Verdict = "fail"
	// Quarantined means the gate's recent history is flapping (two
	// consecutive contradictory runs, DW-8.5's edge case) and is being
	// investigated rather than trusted outright. Quarantined still blocks
	// promotion (see Result.Blocks) — "auto-quarantine ... don't
	// silently un-gate."
	Quarantined Verdict = "quarantined"
)

// Result is one gate check's outcome: which gate, what verdict, the
// measured metric, a human-readable description of the threshold it was
// checked against, and free-form detail (e.g. which query classes lost, or
// which statements were flagged).
type Result struct {
	Gate      string
	Verdict   Verdict
	Metric    float64
	Threshold string
	Detail    string
	RanAt     time.Time
}

// Blocks reports whether this result should block release promotion. Both
// Fail and Quarantined block — quarantining a flaky gate never means
// silently letting a release through; it means "investigate," not
// "disabled."
func (r Result) Blocks() bool {
	return r.Verdict != Pass
}

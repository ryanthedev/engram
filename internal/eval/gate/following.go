package gate

import (
	"fmt"
	"time"
)

// CheckFollowingCorrelation is the DW-8.3 gate: the per-release
// experience-following correlation (internal/experience.FollowingCorrelation
// — Pearson r between followed-experience utility and task outcome) must not
// fall below min. A correlation exactly at min passes (a ">=" contract,
// pinned by an at-threshold boundary test). This function is a pure scalar
// check on purpose: it does not know how the correlation was computed, so a
// later swap from fixture-derived samples to live production telemetry
// changes nothing about the gate itself (see the Design doc's Decision 7).
func CheckFollowingCorrelation(correlation, floor float64) Result {
	res := Result{
		Gate:      "experience-following",
		Metric:    correlation,
		Threshold: fmt.Sprintf(">= %.4f", floor),
		RanAt:     time.Now().UTC(),
	}
	if correlation < floor {
		res.Verdict = Fail
		res.Detail = fmt.Sprintf("following correlation %.4f fell below the documented band floor %.4f", correlation, floor)
		return res
	}
	res.Verdict = Pass
	res.Detail = fmt.Sprintf("following correlation %.4f is within the documented band (floor %.4f)", correlation, floor)
	return res
}

package gate

import (
	"fmt"
	"time"

	"github.com/ryanthedev/engram/internal/eval"
)

// CheckHallucination is the DW-8.1 gate: a release candidate's measured
// hallucination rate must not exceed maxRate. A rate exactly at maxRate
// passes (a "<=" contract, symmetric with CheckRegression's ">="
// non-inferiority shape) — pinned by an at-threshold boundary test.
func CheckHallucination(rep eval.HaluReport, maxRate float64) Result {
	res := Result{
		Gate:      "hallucination",
		Metric:    rep.Rate,
		Threshold: fmt.Sprintf("<= %.4f", maxRate),
		RanAt:     time.Now().UTC(),
	}
	if rep.Rate > maxRate {
		res.Verdict = Fail
		res.Detail = fmt.Sprintf("hallucination rate %.4f (%d/%d assertions) exceeds the %.4f ceiling",
			rep.Rate, rep.Hallucinated, rep.Assertions, maxRate)
		return res
	}
	res.Verdict = Pass
	res.Detail = fmt.Sprintf("hallucination rate %.4f (%d/%d assertions) is within the %.4f ceiling",
		rep.Rate, rep.Hallucinated, rep.Assertions, maxRate)
	return res
}

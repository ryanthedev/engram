// Package gate turns the Phase-0/1 eval harness and the Phase-5 experience
// harness into CI/CD release gates (Phase 8, D9): a hallucination-rate
// ceiling, a retrieval-regression non-inferiority check against a versioned
// baseline, and an experience-following correlation band — three
// independent detectors (no single technique exceeds ~75% detection; combine
// them) whose thresholds are documented and versioned in-repo, not
// hand-tuned per run. This file owns the threshold config: defaults plus a
// checked-in JSON override (eval/goldset/thresholds.json, see
// docs/eval/thresholds.md for the rationale behind each number).
package gate

import (
	"encoding/json"
	"fmt"
	"os"
)

// Thresholds are the release-gate pass/fail boundaries. All three are
// "boundary is acceptable" contracts (>= / <=, never strict), documented at
// each Check function and pinned by an at-threshold boundary test.
type Thresholds struct {
	// HallucinationRateMax is the highest hallucination rate (Hallucinated /
	// Assertions) a release candidate may have. A rate AT the max passes.
	HallucinationRateMax float64 `json:"hallucination_rate_max"`
	// RegressionMarginPP is the DW-1.3 non-inferiority margin, in
	// percentage points of recall@k, current is allowed to fall below the
	// versioned baseline. current == baseline - margin passes.
	RegressionMarginPP float64 `json:"regression_margin_pp"`
	// FollowingCorrelationMin is the lowest acceptable experience-following
	// Pearson correlation. A correlation AT the min passes.
	FollowingCorrelationMin float64 `json:"following_correlation_min"`
}

// DefaultThresholds are the documented, versioned defaults (see
// docs/eval/thresholds.md): hallucination rate <=10% (HaluMem-style suites
// commonly tolerate a nonzero rate from paraphrase/summary noise even in a
// healthy system; 10% leaves real headroom below the ~25%+ rates a broken
// extractor produces), regression margin matches DW-1.3 exactly (2pp, do not
// silently loosen the contract this phase promotes), and following
// correlation floor of 0.3 (weakly-positive; internal/experience's own
// DW-5.5 test asserts r>0 on healthy fixtures, 0.3 leaves margin above zero
// before alerting).
func DefaultThresholds() Thresholds {
	return Thresholds{
		HallucinationRateMax:    0.10,
		RegressionMarginPP:      0.02,
		FollowingCorrelationMin: 0.3,
	}
}

// LoadThresholds reads and validates a thresholds JSON file (the versioned,
// checked-in eval/goldset/thresholds.json). A missing or malformed file, or
// one with an out-of-range value, is an error — never silently falls back to
// defaults, since a gate whose thresholds silently reset is a gate someone
// can silently disable.
func LoadThresholds(path string) (Thresholds, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Thresholds{}, fmt.Errorf("gate: reading thresholds %s: %w", path, err)
	}
	var t Thresholds
	if err := json.Unmarshal(b, &t); err != nil {
		return Thresholds{}, fmt.Errorf("gate: parsing thresholds %s: %w", path, err)
	}
	if err := t.Validate(); err != nil {
		return Thresholds{}, fmt.Errorf("gate: invalid thresholds %s: %w", path, err)
	}
	return t, nil
}

// Validate checks every threshold is in its legal range.
func (t Thresholds) Validate() error {
	if t.HallucinationRateMax < 0 || t.HallucinationRateMax > 1 {
		return fmt.Errorf("hallucination_rate_max must be in [0,1], got %v", t.HallucinationRateMax)
	}
	if t.RegressionMarginPP < 0 || t.RegressionMarginPP > 1 {
		return fmt.Errorf("regression_margin_pp must be in [0,1], got %v", t.RegressionMarginPP)
	}
	if t.FollowingCorrelationMin < -1 || t.FollowingCorrelationMin > 1 {
		return fmt.Errorf("following_correlation_min must be in [-1,1], got %v", t.FollowingCorrelationMin)
	}
	return nil
}

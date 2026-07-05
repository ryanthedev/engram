package gate

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/ryanthedev/engram/internal/eval"
)

// RegressionBaseline is the versioned, checked-in recording of a past-good
// hybrid-retrieval measurement on the DW-1.3 holdout split
// (eval/goldset/baseline.json) — the "recorded baseline" DW-8.2 holds the
// non-inferiority contract against. Re-recording it (an intentional
// relevance change, e.g. a new fusion algorithm) is a deliberate act with
// sign-off — see docs/eval/baseline-rerecord-runbook.md — never an
// automatic side effect of a gate run.
type RegressionBaseline struct {
	// Version is a human label for this baseline recording (e.g. "v1").
	Version string `json:"version"`
	// GitRev is the commit the baseline was measured against.
	GitRev string `json:"git_rev"`
	// RecordedAt is when the measurement was taken (RFC3339).
	RecordedAt string `json:"recorded_at"`
	// Split is the gold-set split measured (must be eval.SplitHoldout — the
	// pre-registered split DW-1.3 measures on).
	Split string `json:"split"`
	// K is the rank cutoff used.
	K int `json:"k"`
	// Queries is the number of queries scored (a sanity check against
	// eval/goldset/seed.json drifting size without re-recording).
	Queries int `json:"queries"`

	RecallAtK float64 `json:"recall_at_k"`
	MRR       float64 `json:"mrr"`
	NDCGAtK   float64 `json:"ndcg_at_k"`
}

// LoadBaseline reads and validates the versioned baseline file.
func LoadBaseline(path string) (RegressionBaseline, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return RegressionBaseline{}, fmt.Errorf("gate: reading baseline %s: %w", path, err)
	}
	var rb RegressionBaseline
	if err := json.Unmarshal(b, &rb); err != nil {
		return RegressionBaseline{}, fmt.Errorf("gate: parsing baseline %s: %w", path, err)
	}
	if err := rb.Validate(); err != nil {
		return RegressionBaseline{}, fmt.Errorf("gate: invalid baseline %s: %w", path, err)
	}
	return rb, nil
}

// Validate checks the baseline's structural invariants.
func (rb RegressionBaseline) Validate() error {
	if rb.Version == "" {
		return fmt.Errorf("baseline missing version")
	}
	if rb.Split != eval.SplitHoldout {
		return fmt.Errorf("baseline split = %q, want %q (DW-1.3 measures on the holdout)", rb.Split, eval.SplitHoldout)
	}
	if rb.K <= 0 {
		return fmt.Errorf("baseline k must be positive, got %d", rb.K)
	}
	if rb.Queries <= 0 {
		return fmt.Errorf("baseline queries must be positive, got %d", rb.Queries)
	}
	if rb.RecallAtK < 0 || rb.RecallAtK > 1 {
		return fmt.Errorf("baseline recall_at_k out of [0,1]: %v", rb.RecallAtK)
	}
	return nil
}

// SaveBaseline writes rb to path as indented JSON — the mechanism the
// baseline re-record runbook drives (see cmd invocation in
// docs/eval/baseline-rerecord-runbook.md). Never called by a gate check
// itself; only by the explicit re-record procedure.
func SaveBaseline(path string, rb RegressionBaseline) error {
	if err := rb.Validate(); err != nil {
		return fmt.Errorf("gate: refusing to save invalid baseline: %w", err)
	}
	b, err := json.MarshalIndent(rb, "", "  ")
	if err != nil {
		return fmt.Errorf("gate: encoding baseline: %w", err)
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// NewRegressionBaseline builds a RegressionBaseline from a freshly-measured
// eval.Report on the holdout split — the shape a re-record run produces
// before SaveBaseline persists it.
func NewRegressionBaseline(version, gitRev string, rep eval.Report) RegressionBaseline {
	return RegressionBaseline{
		Version:    version,
		GitRev:     gitRev,
		RecordedAt: time.Now().UTC().Format(time.RFC3339),
		Split:      eval.SplitHoldout,
		K:          rep.K,
		Queries:    rep.Queries,
		RecallAtK:  rep.RecallAtK,
		MRR:        rep.MRR,
		NDCGAtK:    rep.NDCGAtK,
	}
}

// CheckRegression is the DW-8.2 gate: current recall@k must be non-inferior
// to the versioned baseline within marginPP percentage points — the exact
// DW-1.3 contract (current >= baseline - marginPP), just graded against a
// frozen historical number instead of a same-run comparator. Non-inferior
// means the recorded release did not get worse; it says nothing about
// improving. A current recall/split/k mismatch against the baseline's is a
// Fail (comparing across mismatched configurations is meaningless, not
// silently skipped).
func CheckRegression(current eval.Report, baseline RegressionBaseline, marginPP float64) Result {
	res := Result{Gate: "retrieval-regression", RanAt: time.Now().UTC(),
		Threshold: fmt.Sprintf(">= baseline(%.4f) - %.4fpp", baseline.RecallAtK, marginPP)}
	if current.Split != eval.SplitHoldout {
		res.Verdict = Fail
		res.Detail = fmt.Sprintf("current split = %q, want %q", current.Split, eval.SplitHoldout)
		return res
	}
	if current.K != baseline.K {
		res.Verdict = Fail
		res.Detail = fmt.Sprintf("current k=%d does not match baseline k=%d", current.K, baseline.K)
		return res
	}
	res.Metric = current.RecallAtK
	floor := baseline.RecallAtK - marginPP
	if current.RecallAtK < floor {
		res.Verdict = Fail
		res.Detail = fmt.Sprintf("recall@%d=%.4f fell below the non-inferiority floor %.4f (baseline %.4f, margin %.4fpp)",
			current.K, current.RecallAtK, floor, baseline.RecallAtK, marginPP)
		return res
	}
	res.Verdict = Pass
	res.Detail = fmt.Sprintf("recall@%d=%.4f (MRR=%.4f nDCG=%.4f) is non-inferior to baseline %.4f",
		current.K, current.RecallAtK, current.MRR, current.NDCGAtK, baseline.RecallAtK)
	return res
}

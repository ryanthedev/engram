package gate_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ryanthedev/engram/internal/eval"
	"github.com/ryanthedev/engram/internal/eval/gate"
)

func baseline() gate.RegressionBaseline {
	return gate.RegressionBaseline{
		Version: "v1", GitRev: "abc123", RecordedAt: "2026-07-05T00:00:00Z",
		Split: eval.SplitHoldout, K: 10, Queries: 61,
		RecallAtK: 0.90, MRR: 0.85, NDCGAtK: 0.88,
	}
}

func TestCheckRegression_NonInferiorPasses(t *testing.T) {
	current := eval.Report{K: 10, Split: eval.SplitHoldout, Queries: 61, RecallAtK: 0.90, MRR: 0.85, NDCGAtK: 0.88}
	res := gate.CheckRegression(current, baseline(), 0.02)
	if res.Verdict != gate.Pass {
		t.Fatalf("equal-to-baseline recall must pass, got %+v", res)
	}
}

func TestCheckRegression_ImprovedRecallPasses(t *testing.T) {
	current := eval.Report{K: 10, Split: eval.SplitHoldout, Queries: 61, RecallAtK: 0.95, MRR: 0.9, NDCGAtK: 0.92}
	res := gate.CheckRegression(current, baseline(), 0.02)
	if res.Verdict != gate.Pass {
		t.Fatalf("improved recall must pass, got %+v", res)
	}
}

// TestCheckRegression_AtThresholdBoundary: current == baseline - margin
// EXACTLY must pass (a ">=" contract, not ">"), matching DW-1.3's own
// "hybrid >= best-single - 2pp" phrasing.
func TestCheckRegression_AtThresholdBoundary(t *testing.T) {
	b := baseline()
	current := eval.Report{K: b.K, Split: eval.SplitHoldout, Queries: b.Queries, RecallAtK: b.RecallAtK - 0.02}
	res := gate.CheckRegression(current, b, 0.02)
	if res.Verdict != gate.Pass {
		t.Fatalf("recall exactly at the non-inferiority floor must pass, got %+v", res)
	}
	// One unit below the floor must fail.
	current.RecallAtK -= 0.0001
	res = gate.CheckRegression(current, b, 0.02)
	if res.Verdict != gate.Fail {
		t.Fatalf("recall just below the floor must fail, got %+v", res)
	}
}

func TestCheckRegression_BreachFails(t *testing.T) {
	current := eval.Report{K: 10, Split: eval.SplitHoldout, Queries: 61, RecallAtK: 0.5, MRR: 0.4, NDCGAtK: 0.45}
	res := gate.CheckRegression(current, baseline(), 0.02)
	if res.Verdict != gate.Fail {
		t.Fatalf("large recall drop must fail, got %+v", res)
	}
}

// TestCheckRegression_MismatchedKFails is a dirty test: comparing across
// different k values is meaningless and must fail loudly, not silently pass.
func TestCheckRegression_MismatchedKFails(t *testing.T) {
	current := eval.Report{K: 5, Split: eval.SplitHoldout, Queries: 61, RecallAtK: 0.99}
	res := gate.CheckRegression(current, baseline(), 0.02)
	if res.Verdict != gate.Fail {
		t.Fatalf("mismatched k must fail, got %+v", res)
	}
}

// TestCheckRegression_MismatchedSplitFails is a dirty test.
func TestCheckRegression_MismatchedSplitFails(t *testing.T) {
	current := eval.Report{K: 10, Split: eval.SplitTrain, Queries: 61, RecallAtK: 0.99}
	res := gate.CheckRegression(current, baseline(), 0.02)
	if res.Verdict != gate.Fail {
		t.Fatalf("measuring on the train split instead of holdout must fail, got %+v", res)
	}
}

func TestLoadBaseline_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.json")
	want := baseline()
	if err := gate.SaveBaseline(path, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := gate.LoadBaseline(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// TestLoadBaseline_MissingFile is a dirty test.
func TestLoadBaseline_MissingFile(t *testing.T) {
	if _, err := gate.LoadBaseline(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("missing baseline file must error")
	}
}

// TestSaveBaseline_RejectsInvalid is a dirty test: refuse to persist a
// baseline that doesn't validate (e.g. wrong split) — a bad re-record must
// not silently corrupt the versioned file.
func TestSaveBaseline_RejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.json")
	bad := baseline()
	bad.Split = "bogus"
	if err := gate.SaveBaseline(path, bad); err == nil {
		t.Fatal("invalid baseline must be rejected")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("no file should be written for a rejected baseline")
	}
}

// TestLoadBaseline_WrongSplitRejected is a dirty test.
func TestLoadBaseline_WrongSplitRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.json")
	bad := baseline()
	bad.Split = eval.SplitTrain
	b, _ := json.Marshal(bad)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.LoadBaseline(path); err == nil {
		t.Fatal("a baseline recorded on the train split must be rejected")
	}
}

func TestNewRegressionBaseline_CopiesReportFields(t *testing.T) {
	rep := eval.Report{K: 10, Split: eval.SplitHoldout, Queries: 61, RecallAtK: 0.91, MRR: 0.8, NDCGAtK: 0.85}
	rb := gate.NewRegressionBaseline("v2", "deadbeef", rep)
	if rb.K != rep.K || rb.Queries != rep.Queries || rb.RecallAtK != rep.RecallAtK || rb.MRR != rep.MRR || rb.NDCGAtK != rep.NDCGAtK {
		t.Fatalf("baseline %+v did not copy report %+v faithfully", rb, rep)
	}
	if rb.Version != "v2" || rb.GitRev != "deadbeef" || rb.Split != eval.SplitHoldout {
		t.Fatalf("baseline metadata wrong: %+v", rb)
	}
}

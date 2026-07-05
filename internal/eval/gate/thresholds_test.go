package gate_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ryanthedev/engram/internal/eval/gate"
)

func TestDefaultThresholds_AllValid(t *testing.T) {
	if err := gate.DefaultThresholds().Validate(); err != nil {
		t.Fatalf("default thresholds must validate: %v", err)
	}
}

func TestLoadThresholds_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "thresholds.json")
	want := gate.Thresholds{HallucinationRateMax: 0.1, RegressionMarginPP: 0.02, FollowingCorrelationMin: 0.3}
	b, _ := json.Marshal(want)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := gate.LoadThresholds(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// TestLoadThresholds_MissingFile is a dirty test.
func TestLoadThresholds_MissingFile(t *testing.T) {
	if _, err := gate.LoadThresholds(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("missing file must error")
	}
}

// TestLoadThresholds_MalformedJSON is a dirty test.
func TestLoadThresholds_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "thresholds.json")
	if err := os.WriteFile(path, []byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.LoadThresholds(path); err == nil {
		t.Fatal("malformed JSON must error")
	}
}

// TestLoadThresholds_OutOfRangeRejected is a dirty test: a thresholds file
// that would silently disable a gate (e.g. rate max > 1) must be rejected,
// not clamped.
func TestLoadThresholds_OutOfRangeRejected(t *testing.T) {
	cases := map[string]gate.Thresholds{
		"hallucination rate > 1":    {HallucinationRateMax: 1.5, RegressionMarginPP: 0.02, FollowingCorrelationMin: 0.3},
		"hallucination rate < 0":    {HallucinationRateMax: -0.1, RegressionMarginPP: 0.02, FollowingCorrelationMin: 0.3},
		"regression margin > 1":     {HallucinationRateMax: 0.1, RegressionMarginPP: 1.2, FollowingCorrelationMin: 0.3},
		"following min out of band": {HallucinationRateMax: 0.1, RegressionMarginPP: 0.02, FollowingCorrelationMin: -2},
	}
	for name, th := range cases {
		t.Run(name, func(t *testing.T) {
			if err := th.Validate(); err == nil {
				t.Fatalf("thresholds %+v should be rejected", th)
			}
		})
	}
}

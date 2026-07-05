package sizing_test

import (
	"testing"

	"github.com/ryanthedev/engram/deploy/aws/sizing"
)

// TestBytesPerVectorSQfp16_MatchesPlanFormula pins the exact GREENFIELD
// formula (1.1*(2*dim+8*m)) at Engram's pinned dim=1024, m=16 (D15 +
// internal/store's semantic template) so a future edit can't silently drift
// from the plan's documented sizing math.
func TestBytesPerVectorSQfp16_MatchesPlanFormula(t *testing.T) {
	got := sizing.BytesPerVectorSQfp16(1024, 16)
	want := 1.1 * (2*1024 + 8*16) // = 1.1 * 2176 = 2393.6
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("BytesPerVectorSQfp16(1024,16) = %v, want %v", got, want)
	}
}

func TestEstimatedVectorRAMBytes_ScalesWithVectorsAndReplicas(t *testing.T) {
	perVector := sizing.BytesPerVectorSQfp16(1024, 16)
	got := sizing.EstimatedVectorRAMBytes(1_000_000, 1024, 16, 2)
	want := perVector * 1_000_000 * 2
	if got != want {
		t.Fatalf("EstimatedVectorRAMBytes = %v, want %v", got, want)
	}
	// Replicas < 1 defaults to 1 (a domain always has at least one copy).
	if got := sizing.EstimatedVectorRAMBytes(100, 1024, 16, 0); got != perVector*100 {
		t.Fatalf("EstimatedVectorRAMBytes with replicas=0 = %v, want the replicas=1 value %v", got, perVector*100)
	}
}

func TestWithinTolerance(t *testing.T) {
	cases := []struct {
		name                string
		measured, estimated float64
		tolerancePct        float64
		want                bool
	}{
		{"exact match", 100, 100, 20, true},
		{"10pct over, 20pct tolerance", 110, 100, 20, true},
		{"25pct over, 20pct tolerance", 125, 100, 20, false},
		{"20pct under, 20pct tolerance (boundary)", 80, 100, 20, true},
		{"zero estimate, zero measured", 0, 0, 20, true},
		{"zero estimate, nonzero measured", 5, 0, 20, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sizing.WithinTolerance(c.measured, c.estimated, c.tolerancePct); got != c.want {
				t.Errorf("WithinTolerance(%v,%v,%v) = %v, want %v", c.measured, c.estimated, c.tolerancePct, got, c.want)
			}
		})
	}
}

package goldgen_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ryanthedev/engram/internal/eval"
	"github.com/ryanthedev/engram/internal/eval/goldgen"
	"github.com/ryanthedev/engram/internal/testutil"
)

// TestGenerateIsDeterministic guards the pre-registration property: two
// generator runs produce identical output (including split assignment).
func TestGenerateIsDeterministic(t *testing.T) {
	a, b := goldgen.Generate(), goldgen.Generate()
	if !reflect.DeepEqual(a, b) {
		t.Fatal("Generate() is not deterministic")
	}
	if err := a.Validate(); err != nil {
		t.Fatalf("generated gold set invalid: %v", err)
	}
}

// TestCheckedInSeedMatchesGenerator guards the frozen split: the checked-in
// seed.json must be exactly what the generator produces, so nobody can hand-
// tune the holdout after Phase-1 measurements start without the diff showing.
func TestCheckedInSeedMatchesGenerator(t *testing.T) {
	path := filepath.Join(testutil.RepoRoot(t), "eval", "goldset", "seed.json")
	checked, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading checked-in seed: %v", err)
	}
	var got eval.GoldSet
	if err := json.Unmarshal(checked, &got); err != nil {
		t.Fatalf("parsing checked-in seed: %v", err)
	}
	if want := goldgen.Generate(); !reflect.DeepEqual(got, want) {
		t.Fatal("eval/goldset/seed.json does not match the generator output; run `go run ./cmd/engram-goldgen` (only legitimate before Phase-1 measurements exist)")
	}
}

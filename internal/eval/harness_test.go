package eval_test

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"testing"

	"github.com/ryanthedev/engram/internal/eval"
	"github.com/ryanthedev/engram/internal/retrieval"
	"github.com/ryanthedev/engram/internal/testutil"
)

func loadSeed(t *testing.T) eval.GoldSet {
	t.Helper()
	gs, err := eval.LoadGoldSet(filepath.Join(testutil.RepoRoot(t), "eval", "goldset", "seed.json"))
	if err != nil {
		t.Fatalf("loading seed gold set: %v", err)
	}
	return gs
}

// TestDW_0_5_HarnessLoadsSeedAndEmitsRecallAtK proves the eval-harness
// skeleton end-to-end: the checked-in seed gold set loads and validates, and
// a run emits recall@k — 0 against the NullRetriever, by construction, until
// Phase 1 provides a real Retriever.
func TestDW_0_5_HarnessLoadsSeedAndEmitsRecallAtK(t *testing.T) {
	gs := loadSeed(t)
	rep, err := eval.Run(context.Background(), eval.NullRetriever{}, gs, 10, "")
	if err != nil {
		t.Fatalf("harness run: %v", err)
	}
	if rep.Queries != len(gs.Queries) {
		t.Errorf("scored %d queries, want %d", rep.Queries, len(gs.Queries))
	}
	if rep.RecallAtK != 0 || rep.MRR != 0 || rep.NDCGAtK != 0 {
		t.Errorf("null retriever must score 0, got recall=%v mrr=%v ndcg=%v",
			rep.RecallAtK, rep.MRR, rep.NDCGAtK)
	}
	if len(rep.PerQuery) != len(gs.Queries) {
		t.Errorf("per-query results = %d, want %d", len(rep.PerQuery), len(gs.Queries))
	}
}

// TestDW_0_5_SeedSplitPreRegistered verifies the pre-registered train/holdout
// split frozen in the checked-in file: both splits populated, holdout large
// enough for the DW-1.3 gate (≥50 queries), split labels legal, and every
// query class represented in the holdout.
func TestDW_0_5_SeedSplitPreRegistered(t *testing.T) {
	gs := loadSeed(t)
	train, holdout := gs.SplitCounts()
	if train == 0 || holdout == 0 {
		t.Fatalf("both splits must be populated, got train=%d holdout=%d", train, holdout)
	}
	if holdout < 50 {
		t.Errorf("holdout = %d, want >= 50 (DW-1.3 measures on >=50 held-out queries)", holdout)
	}
	classes := map[string]int{}
	for _, q := range gs.Queries {
		if q.Split == eval.SplitHoldout {
			classes[q.Class]++
		}
	}
	for _, c := range []string{"exact-id", "keyword", "paraphrase", "multi-term"} {
		if classes[c] == 0 {
			t.Errorf("query class %q absent from holdout — per-class fusion analysis impossible", c)
		}
	}
}

// fixedRetriever returns a canned ranking per query text.
type fixedRetriever map[string][]string

// Search implements retrieval.Retriever.
func (f fixedRetriever) Search(_ context.Context, q retrieval.Query, _ retrieval.Filter) ([]retrieval.Hit, error) {
	ids, ok := f[q.Text]
	if !ok {
		return nil, errors.New("boom")
	}
	hits := make([]retrieval.Hit, len(ids))
	for i, id := range ids {
		hits[i] = retrieval.Hit{ID: id, Score: 1 / float64(i+1)}
	}
	return hits, nil
}

// TestHarnessScoresKnownRankings pins the metric math on hand-computable
// rankings, and verifies a per-query retrieval error scores 0 without
// failing the run.
func TestHarnessScoresKnownRankings(t *testing.T) {
	gs := eval.GoldSet{
		Corpus: []eval.Doc{{ID: "d1", Text: "one"}, {ID: "d2", Text: "two"}, {ID: "d3", Text: "three"}},
		Queries: []eval.Query{
			{ID: "q1", Text: "hit-first", ExpectedIDs: []string{"d1"}, Split: "train"},
			{ID: "q2", Text: "hit-second", ExpectedIDs: []string{"d2"}, Split: "holdout"},
			{ID: "q3", Text: "miss", ExpectedIDs: []string{"d3"}, Split: "holdout"},
			{ID: "q4", Text: "error", ExpectedIDs: []string{"d3"}, Split: "train"},
		},
	}
	if err := gs.Validate(); err != nil {
		t.Fatalf("fixture invalid: %v", err)
	}
	r := fixedRetriever{
		"hit-first":  {"d1", "d2"},
		"hit-second": {"d1", "d2"},
		"miss":       {"d1", "d2"},
	}
	rep, err := eval.Run(context.Background(), r, gs, 10, "")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// recall: q1=1, q2=1, q3=0, q4=0 (error) → 0.5
	if math.Abs(rep.RecallAtK-0.5) > 1e-9 {
		t.Errorf("recall@10 = %v, want 0.5", rep.RecallAtK)
	}
	// MRR: 1 + 0.5 + 0 + 0 → 0.375
	if math.Abs(rep.MRR-0.375) > 1e-9 {
		t.Errorf("MRR = %v, want 0.375", rep.MRR)
	}
	// nDCG: q1=1, q2=1/log2(3)≈0.6309, q3=q4=0 → ≈0.4077
	want := (1 + 1/math.Log2(3)) / 4
	if math.Abs(rep.NDCGAtK-want) > 1e-9 {
		t.Errorf("nDCG@10 = %v, want %v", rep.NDCGAtK, want)
	}
	var errored int
	for _, qr := range rep.PerQuery {
		if qr.Err != "" {
			errored++
		}
	}
	if errored != 1 {
		t.Errorf("want exactly 1 per-query error recorded, got %d", errored)
	}

	// Split filtering narrows the query set.
	hold, err := eval.Run(context.Background(), r, gs, 10, eval.SplitHoldout)
	if err != nil {
		t.Fatalf("holdout run: %v", err)
	}
	if hold.Queries != 2 {
		t.Errorf("holdout run scored %d queries, want 2", hold.Queries)
	}
}

// TestRunRejectsBadK covers the k<=0 error path.
func TestRunRejectsBadK(t *testing.T) {
	if _, err := eval.Run(context.Background(), eval.NullRetriever{}, eval.GoldSet{}, 0, ""); err == nil {
		t.Fatal("k=0 must error")
	}
}

// TestGoldSetValidateRejectsCorruptSets covers the validator's error paths.
func TestGoldSetValidateRejectsCorruptSets(t *testing.T) {
	base := func() eval.GoldSet {
		return eval.GoldSet{
			Corpus:  []eval.Doc{{ID: "d1", Text: "x"}},
			Queries: []eval.Query{{ID: "q1", Text: "y", ExpectedIDs: []string{"d1"}, Split: "train"}},
		}
	}
	cases := map[string]func(*eval.GoldSet){
		"empty corpus":        func(gs *eval.GoldSet) { gs.Corpus = nil },
		"empty queries":       func(gs *eval.GoldSet) { gs.Queries = nil },
		"dup doc id":          func(gs *eval.GoldSet) { gs.Corpus = append(gs.Corpus, eval.Doc{ID: "d1"}) },
		"dup query id":        func(gs *eval.GoldSet) { gs.Queries = append(gs.Queries, gs.Queries[0]) },
		"unknown expected id": func(gs *eval.GoldSet) { gs.Queries[0].ExpectedIDs = []string{"nope"} },
		"bad split":           func(gs *eval.GoldSet) { gs.Queries[0].Split = "test" },
		"no expected ids":     func(gs *eval.GoldSet) { gs.Queries[0].ExpectedIDs = nil },
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			gs := base()
			corrupt(&gs)
			if err := gs.Validate(); err == nil {
				t.Errorf("Validate accepted a gold set with %s", name)
			}
		})
	}
}

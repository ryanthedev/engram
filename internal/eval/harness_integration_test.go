//go:build integration

package eval_test

import (
	"context"
	"math"
	"path/filepath"
	"testing"

	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/eval"
	"github.com/ryanthedev/engram/internal/eval/seed"
	"github.com/ryanthedev/engram/internal/retrieval"
	"github.com/ryanthedev/engram/internal/store"
	"github.com/ryanthedev/engram/internal/testutil"
)

// TestDW_1_3_HybridNonInferiorToSingleSignalOnHoldout is the DW-1.3 gate: on
// the pre-registered holdout split (>=50 queries — TestDW_0_5_SeedSplitPre
// Registered already pins it at 61), hybrid recall@10 must be non-inferior
// (>= best single signal - 2pp), with MRR/nDCG reported and any query class
// where fusion loses logged for analysis.
func TestDW_1_3_HybridNonInferiorToSingleSignalOnHoldout(t *testing.T) {
	gs, err := eval.LoadGoldSet(filepath.Join(testutil.RepoRoot(t), "eval", "goldset", "seed.json"))
	if err != nil {
		t.Fatalf("loading gold set: %v", err)
	}

	base := testutil.OpenSearchURL()
	if _, err := store.Apply(context.Background(), testutil.HTTPClient, base); err != nil {
		t.Fatalf("applying cluster contract: %v", err)
	}
	epIdx := testutil.ScratchIndexName("episodic", t.Name())
	semIdx := testutil.ScratchIndexName("semantic", t.Name())
	for _, idx := range []string{epIdx, semIdx} {
		testutil.DeleteIndex(t, base, idx)
		t.Cleanup(func(idx string) func() { return func() { testutil.DeleteIndex(t, base, idx) } }(idx))
		testutil.CreateScratchIndex(t, base, idx)
	}

	// The fixture-keyed fake embedder makes each corpus statement and its
	// gold queries (exact-id/keyword/paraphrase/multi-term) share a vector —
	// the fake kNN signal needs that correlation to be meaningful without a
	// real embedding model (see the plan's Phase-1 embeddings note).
	embedder := embed.NewFakeEmbedder(store.EmbeddingDim, eval.FixtureKeys(gs))
	if err := seed.Corpus(context.Background(), testutil.HTTPClient, base, semIdx, gs, embedder, "", ""); err != nil {
		t.Fatalf("seeding gold corpus: %v", err)
	}
	testutil.RefreshIndex(t, base, semIdx)

	const k = 10
	hybrid := retrieval.NewOpenSearchRetriever(testutil.HTTPClient, base, embedder, retrieval.WithIndices(epIdx, semIdx))
	bm25 := retrieval.NewOpenSearchRetriever(testutil.HTTPClient, base, embedder,
		retrieval.WithIndices(epIdx, semIdx), retrieval.WithMode(retrieval.ModeBM25Only))
	knn := retrieval.NewOpenSearchRetriever(testutil.HTTPClient, base, embedder,
		retrieval.WithIndices(epIdx, semIdx), retrieval.WithMode(retrieval.ModeKNNOnly))

	ctx := context.Background()
	repHybrid, err := eval.Run(ctx, hybrid, gs, k, eval.SplitHoldout)
	if err != nil {
		t.Fatalf("hybrid run: %v", err)
	}
	repBM25, err := eval.Run(ctx, bm25, gs, k, eval.SplitHoldout)
	if err != nil {
		t.Fatalf("bm25 run: %v", err)
	}
	repKNN, err := eval.Run(ctx, knn, gs, k, eval.SplitHoldout)
	if err != nil {
		t.Fatalf("knn run: %v", err)
	}

	t.Logf("finding: holdout(n=%d) recall@%d hybrid=%.4f bm25=%.4f knn=%.4f | MRR hybrid=%.4f bm25=%.4f knn=%.4f | nDCG@%d hybrid=%.4f bm25=%.4f knn=%.4f",
		repHybrid.Queries, k, repHybrid.RecallAtK, repBM25.RecallAtK, repKNN.RecallAtK,
		repHybrid.MRR, repBM25.MRR, repKNN.MRR, k, repHybrid.NDCGAtK, repBM25.NDCGAtK, repKNN.NDCGAtK)

	best := math.Max(repBM25.RecallAtK, repKNN.RecallAtK)
	if repHybrid.RecallAtK < best-0.02 {
		t.Errorf("hybrid recall@%d=%.4f is not non-inferior to the best single signal=%.4f (2pp margin)", k, repHybrid.RecallAtK, best)
	}

	hByClass, bByClass, kByClass := classRecall(repHybrid), classRecall(repBM25), classRecall(repKNN)
	for class, hVals := range hByClass {
		hAvg := avg(hVals)
		bestClass := math.Max(avg(bByClass[class]), avg(kByClass[class]))
		if hAvg+1e-9 < bestClass {
			t.Logf("finding: fusion loses on class %q: hybrid=%.4f < best-single=%.4f", class, hAvg, bestClass)
		} else {
			t.Logf("finding: class %q: hybrid=%.4f >= best-single=%.4f", class, hAvg, bestClass)
		}
	}
}

func classRecall(rep eval.Report) map[string][]float64 {
	m := map[string][]float64{}
	for _, qr := range rep.PerQuery {
		m[qr.Class] = append(m[qr.Class], qr.Recall)
	}
	return m
}

func avg(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	var s float64
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

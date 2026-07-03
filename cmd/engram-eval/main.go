// Command engram-eval runs the retrieval eval harness (D9) on a gold set and
// prints recall@k, MRR, and nDCG@k. Until Phase 1 lands a real Retriever it
// scores the NullRetriever and reports 0 — proving the harness runs.
//
// Usage:
//
//	go run ./cmd/engram-eval [-gold eval/goldset/seed.json] [-k 10] [-split holdout]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/ryanthedev/engram/internal/eval"
)

func main() {
	gold := flag.String("gold", "eval/goldset/seed.json", "gold set JSON path")
	k := flag.Int("k", 10, "rank cutoff for recall@k / nDCG@k")
	split := flag.String("split", "", "restrict to one split: train | holdout (default: all)")
	perQuery := flag.Bool("per-query", false, "include per-query results in the JSON output")
	flag.Parse()

	gs, err := eval.LoadGoldSet(*gold)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	train, holdout := gs.SplitCounts()
	fmt.Fprintf(os.Stderr, "gold set: %d docs, %d queries (train=%d holdout=%d, pre-registered)\n",
		len(gs.Corpus), len(gs.Queries), train, holdout)

	// Phase 0: no Retriever implementation exists yet; score the null
	// retriever so the pipeline is exercised end-to-end (DW-0.5).
	rep, err := eval.Run(context.Background(), eval.NullRetriever{}, gs, *k, *split)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if !*perQuery {
		rep.PerQuery = nil
	}
	out, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Println(string(out))
}

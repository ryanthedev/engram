// Command engram-goldgen regenerates the checked-in seed gold set from the
// deterministic generator. The checked-in output IS the pre-registered
// train/holdout split; regeneration is byte-identical unless the seed facts
// change (which re-registers the split before any measurement uses it).
//
// Usage:
//
//	go run ./cmd/engram-goldgen [-out eval/goldset/seed.json]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/ryanthedev/engram/internal/eval/goldgen"
)

func main() {
	out := flag.String("out", "eval/goldset/seed.json", "output path")
	flag.Parse()

	gs := goldgen.Generate()
	b, err := json.MarshalIndent(gs, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	train, holdout := gs.SplitCounts()
	fmt.Printf("wrote %s: %d docs, %d queries (train=%d holdout=%d)\n",
		*out, len(gs.Corpus), len(gs.Queries), train, holdout)
}

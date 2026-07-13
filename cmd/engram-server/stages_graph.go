package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/graph"
	"github.com/ryanthedev/engram/internal/retrieval"
	"github.com/ryanthedev/engram/internal/worker"
)

// graphConfig tunes the Phase-6 incremental-graph wiring.
type graphConfig struct {
	// judgeURL is the OpenAI-compatible dedup tie-break judge endpoint;
	// empty selects the deterministic RuleJudge (dev/e2e), mirroring
	// -gate-url / -extract-url.
	judgeURL   string
	judgeModel string
	// expandDepth bounds GraphExpander's traversal (<=2, D8).
	expandDepth int
}

// wireGraph stands up the Phase-6 incremental graph (T4) and plugs it into
// the Phase-3/4 seams WITHOUT touching the worker or retriever cores: it
// applies the entity/edge index contract, builds the dedup routine (rule or
// LLM judge, config-selected), registers the graph worker stage (D20
// RegisterStage) and the bounded expander (Phase-4 RegisterPostHook). The
// worker stage lives in this file per the D20 convention (each phase
// registers its stages in its own file). wireGraph returns the constructed
// *graph.Store (mirroring wireExperience's shape) so main.go can wire its
// CountAllEntities into the Phase-7/3 telemetry recorder (the graph entity
// gauge, DW-3.2) without this file's callers reaching past the registration
// seams for anything else.
func wireGraph(ctx context.Context, httpClient *http.Client, osURL string, cfg graphConfig,
	embedder embed.Embedder, wk *worker.Worker, retriever *retrieval.MultiRetriever, logger *slog.Logger) (*graph.Store, error) {

	if err := graph.Apply(ctx, httpClient, osURL); err != nil {
		return nil, fmt.Errorf("applying graph contract: %w", err)
	}

	// The dedup tie-break judge: LLM judge when configured, deterministic
	// rule judge otherwise. Both resolve failure to "distinct" (fail-safe —
	// see graph.Judge's doc comment on why that direction, not fail-closed).
	var judge graph.Judge
	if cfg.judgeURL != "" {
		judge = graph.NewHTTPJudge(httpClient, cfg.judgeURL, cfg.judgeModel)
		logger.Info("graph dedup judge: LLM judge", "url", cfg.judgeURL, "model", cfg.judgeModel)
	} else {
		judge = graph.RuleJudge{}
		logger.Info("graph dedup judge: deterministic rule judge (no -graph-judge-url)")
	}
	dedup, err := graph.NewDeduper(judge)
	if err != nil {
		return nil, fmt.Errorf("building graph deduper: %w", err)
	}

	backend := graph.NewOpenSearchBackend(httpClient, osURL)
	// Name-keyed dedup (finding #2) is enabled ONLY for the deterministic
	// dev/e2e fake embedder — tied directly to the embedder's own concrete
	// identity (not a separately-threaded config flag that could drift out
	// of sync with reality). Production's real embedder (embed.HTTPEmbedder)
	// never enables this, so real-embedder homonym separation via fact
	// context is unaffected. See graph.WithNameKeyedDedup's doc comment.
	var storeOpts []graph.StoreOption
	if _, ok := embedder.(*embed.FakeEmbedder); ok {
		storeOpts = append(storeOpts, graph.WithNameKeyedDedup())
		logger.Info("graph dedup: name-keyed mention embedding enabled (deterministic dev/fake embedder detected)")
	}
	store := graph.NewStore(backend, dedup, embedder, logger, storeOpts...)

	// Graph worker stage (D20): registered in its own file, no worker-core
	// edits. Derives entity/edge upserts from each event's landed facts.
	wk.RegisterStage("graph-upsert", graph.NewStage(store, logger))

	// Bounded (<=2-hop) connect-the-dots expansion (Phase-4 seam): runs
	// INSIDE the ACL boundary — the retriever re-verifies every hit the
	// expander adds, so an edge reachable only through an unauthorized
	// owner is dropped before the caller ever sees it.
	expander, err := graph.NewExpander(store, cfg.expandDepth, logger)
	if err != nil {
		return nil, fmt.Errorf("building graph expander: %w", err)
	}
	retriever.RegisterPostHook("graph", expander)

	return store, nil
}

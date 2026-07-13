package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/ryanthedev/engram/internal/experience"
	"github.com/ryanthedev/engram/internal/retrieval"
	"github.com/ryanthedev/engram/internal/worker"
)

// experienceConfig tunes the Phase-5 experience-memory wiring.
type experienceConfig struct {
	// gateURL is the OpenAI-compatible LLM-judge endpoint; empty selects the
	// deterministic RuleGatekeeper (dev/e2e), mirroring -extract-url.
	gateURL   string
	gateModel string
	// prune thresholds and cadence.
	prunePhi       float64
	pruneRetrieval int
	pruneInterval  time.Duration
}

// wireExperience stands up the Phase-5 T3 experience memory and plugs it into
// the Phase-3/4 seams WITHOUT touching the worker core or the retriever
// package: it applies the T3 index contract, builds the mandatory write-gate
// (rule or LLM judge, config-selected), registers the distillation stage (D20
// RegisterStage) and the gated experience tier (Phase-4 RegisterTier), and
// starts the background utility-prune job. The distillation stage lives in this
// file per the D20 convention (each phase registers its stages in its own file).
// wireExperience returns the constructed *experience.Store so main.go can
// wire its VerdictCounts into the Phase-7 telemetry recorder (DW-7.8's
// gate-verdict-rate gauge) without this file's callers reaching past the
// registration seams for anything else.
func wireExperience(ctx context.Context, httpClient *http.Client, osURL string, cfg experienceConfig,
	wk *worker.Worker, retriever *retrieval.MultiRetriever, logger *slog.Logger) (*experience.Store, error) {

	if err := experience.Apply(ctx, httpClient, osURL); err != nil {
		return nil, fmt.Errorf("applying experience contract: %w", err)
	}

	// The write-gate: LLM judge when configured, deterministic rule judge
	// otherwise. Both are fail-closed (timeout/error → Quarantine, never Admit).
	var gate experience.Gatekeeper
	if cfg.gateURL != "" {
		gate = experience.NewHTTPGatekeeper(httpClient, cfg.gateURL, cfg.gateModel)
		logger.Info("experience gate: LLM judge", "url", cfg.gateURL, "model", cfg.gateModel)
	} else {
		gate = experience.RuleGatekeeper{}
		logger.Info("experience gate: deterministic rule judge (no -gate-url)")
	}

	backend := experience.NewOpenSearchBackend(httpClient, osURL)
	store, err := experience.NewStore(gate, backend, logger)
	if err != nil {
		return nil, fmt.Errorf("building experience store: %w", err)
	}

	// Distillation stage (D20): registered in its own file, no worker-core edits.
	wk.RegisterStage("experience-distill", experience.NewDistillStage(experience.RuleDistiller{}, store, logger))
	// Gated retrieval tier (Phase-4 seam): admitted experiences only, ACL-verified.
	retriever.RegisterTier("experience", experience.NewTier(store, logger))

	// Background utility prune (soft-expire low-Φ, never delete).
	prune := &experience.PruneJob{Store: store, PhiMax: cfg.prunePhi, RetrievalMax: cfg.pruneRetrieval, Logger: logger}
	go prune.Run(ctx, cfg.pruneInterval)

	return store, nil
}

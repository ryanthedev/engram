// Command engram-graph-rebuild wipes and rebuilds Engram's derived graph
// tier (T4: entities + edges) from the CURRENT live semantic facts for one
// tenant. It exists because Phase 2's write-path fix (closing a
// superseded fact's edge, graph.Store.CloseEdge) is not self-healing:
// every zombie edge written BEFORE that fix landed stays live forever
// unless the graph is rebuilt from scratch. Re-running is always safe —
// the graph is derived data (D2/D3); this command only ever addresses
// EntityIndex/EdgeIndex and the semantic index's READ path, and never
// writes to the episodic or semantic tiers, which are append-only and out
// of its reach entirely.
//
// Usage:
//
//	go run ./cmd/engram-graph-rebuild -tenant <id> -confirm [-url http://localhost:9200]
//
// Refuses to run without -confirm: a rebuild is destructive to the graph
// tier's CURRENT state (trivially recoverable by re-running, but still a
// wipe), so an operator must say so explicitly rather than the flag
// defaulting on.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/graph"
	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/store"
)

// config is every flag this command accepts, gathered so validation and
// wiring can each be tested without touching flag.CommandLine.
type config struct {
	url, tenant          string
	confirm              bool
	embedURL             string
	judgeURL, judgeModel string
}

// validate checks everything that can be decided WITHOUT any network call
// — the -confirm gate (DW-3.3) lives here, deliberately before run()
// builds an HTTP client or issues a single request.
func (c config) validate() error {
	if !c.confirm {
		return fmt.Errorf("refusing to run without -confirm: this drops and rebuilds the graph tier for tenant %q (safely re-runnable, but destructive to the graph's CURRENT state)", c.tenant)
	}
	if c.tenant == "" {
		return fmt.Errorf("-tenant is required")
	}
	return nil
}

func main() {
	def := os.Getenv("ENGRAM_OPENSEARCH_URL")
	if def == "" {
		def = "http://localhost:9200"
	}
	var cfg config
	flag.StringVar(&cfg.url, "url", def, "OpenSearch base URL")
	flag.StringVar(&cfg.tenant, "tenant", "", "tenant id to rebuild the graph for (required)")
	flag.BoolVar(&cfg.confirm, "confirm", false, "required: acknowledges this drops and rebuilds the graph tier for -tenant")
	flag.StringVar(&cfg.embedURL, "embed-url", "", "real embedding service base URL (TEI-style); empty uses the deterministic fake embedder")
	flag.StringVar(&cfg.judgeURL, "graph-judge-url", "", "optional OpenAI-compatible dedup tie-break judge endpoint; empty uses the deterministic RuleJudge")
	flag.StringVar(&cfg.judgeModel, "graph-judge-model", "", "dedup judge model name (used with -graph-judge-url)")
	flag.Parse()

	if err := run(context.Background(), cfg, os.Stdout, slog.Default()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run wires the real OpenSearch-backed dependencies and drives one
// rebuild. Split out of main so tests can exercise validation and the full
// wiring against a scratch/fake server without a live cluster.
func run(ctx context.Context, cfg config, out io.Writer, logger *slog.Logger) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	if logger == nil {
		logger = slog.Default()
	}

	client := &http.Client{Timeout: store.DefaultTimeout}
	semanticStore := store.NewOpenSearchStore(client, cfg.url)

	var judge graph.Judge
	if cfg.judgeURL != "" {
		judge = graph.NewHTTPJudge(client, cfg.judgeURL, cfg.judgeModel)
	} else {
		judge = graph.RuleJudge{}
	}
	dedup, err := graph.NewDeduper(judge)
	if err != nil {
		return fmt.Errorf("building graph deduper: %w", err)
	}

	var embedder embed.Embedder
	if cfg.embedURL != "" {
		embedder = embed.NewHTTPEmbedder(client, cfg.embedURL, embed.ModelInfo{Dim: store.EmbeddingDim})
	} else {
		embedder = embed.NewFakeEmbedder(store.EmbeddingDim, nil)
	}

	backend := graph.NewOpenSearchBackend(client, cfg.url)
	gstore := graph.NewStore(backend, dedup, embedder, logger)
	stage := graph.NewStage(gstore, logger)
	dropper := graph.NewOpenSearchIndexDropper(client, cfg.url)
	scanner := &factScannerAdapter{store: semanticStore}

	report, err := graph.Rebuild(ctx, dropper, scanner, stage, cfg.tenant, logger)
	if err != nil {
		return fmt.Errorf("rebuilding graph for tenant %q: %w", cfg.tenant, err)
	}
	fmt.Fprintf(out, "rebuilt graph for tenant %s: %d live facts replayed\n", cfg.tenant, report.FactsReplayed)
	return nil
}

// factScannerAdapter bridges store.OpenSearchStore.ScanLiveFacts to
// graph.FactScanner's narrower shape. internal/graph must not import
// internal/store (see graph.go's architecture-boundary doc comment), so
// this command's wiring layer — free to import both, being the top-level
// glue — does the translation. ScanLiveFacts is the ONLY method this
// command exposes to graph.Rebuild from the semantic store side: a single
// read (POST .../_search), never Create/Update/Append/ClaimBatch/etc —
// the structural half of DW-3.4's "never writes episodic/semantic"
// guarantee (the other half is that graph.Rebuild's FactScanner parameter
// has no writer-shaped method to misuse even if it wanted to).
type factScannerAdapter struct {
	store *store.OpenSearchStore
}

func (a *factScannerAdapter) ScanLiveFacts(ctx context.Context, tenantID string, cursor graph.FactCursor) ([]memory.SemanticFact, graph.FactCursor, error) {
	var sc store.FactCursor
	if cursor.CreatedAtUnixMilli != 0 { // zero means "start from the beginning" (see graph.FactCursor's doc comment)
		sc = store.FactCursor{CreatedAt: time.UnixMilli(cursor.CreatedAtUnixMilli).UTC(), ContentKey: cursor.ContentKey}
	}
	vfs, next, err := a.store.ScanLiveFacts(ctx, tenantID, sc, 0)
	if err != nil {
		return nil, graph.FactCursor{}, err
	}
	facts := make([]memory.SemanticFact, len(vfs))
	for i, vf := range vfs {
		facts[i] = vf.Fact
	}
	var nc graph.FactCursor
	if !next.CreatedAt.IsZero() {
		nc = graph.FactCursor{CreatedAtUnixMilli: next.CreatedAt.UnixMilli(), ContentKey: next.ContentKey}
	}
	return facts, nc, nil
}

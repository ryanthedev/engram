// Command engram-server runs the Engram gRPC service (Ingest + Search) over
// OpenSearch, plus the Phase-2 async write path: the outbox worker pool
// (claim → extract → reconcile → bi-temporal write) and the repair sweep. It
// applies the cluster contract on startup (idempotent — safe to run
// alongside make apply-templates), validates the configured embedder against
// the index template's dimension (D15), and starts the episodic
// embedding-enrichment job.
//
// Usage:
//
//	go run ./cmd/engram-server [-addr :7070] [-url http://localhost:9200] [-embed-url ""] [-extract-url ""]
//
// With no -embed-url, the deterministic fake embedder is used; with no
// -extract-url, the deterministic rule-based extractor is used (no real
// BGE-M3 or LLM endpoint is required for the walking skeleton).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/enrich"
	"github.com/ryanthedev/engram/internal/ingest"
	"github.com/ryanthedev/engram/internal/retrieval"
	"github.com/ryanthedev/engram/internal/server"
	"github.com/ryanthedev/engram/internal/store"
	"github.com/ryanthedev/engram/internal/worker"
)

func main() {
	osURLDefault := os.Getenv("ENGRAM_OPENSEARCH_URL")
	if osURLDefault == "" {
		osURLDefault = "http://localhost:9200"
	}

	addr := flag.String("addr", ":7070", "gRPC listen address")
	osURL := flag.String("url", osURLDefault, "OpenSearch base URL")
	embedURL := flag.String("embed-url", "", "real embedding service base URL (TEI-style); empty uses the deterministic fake embedder")
	embedModel := flag.String("embed-model", "BAAI/bge-m3", "embedding model id (D15)")
	embedRevision := flag.String("embed-revision", "unpinned-dev", "embedding model revision (D15)")
	enrichInterval := flag.Duration("enrich-interval", 2*time.Second, "embedding-enrichment poll interval")
	enrichBatch := flag.Int("enrich-batch", 50, "embedding-enrichment batch size")
	extractURL := flag.String("extract-url", "", "OpenAI-compatible extraction endpoint base URL (e.g. https://api.openai.com/v1); empty uses the deterministic rule extractor")
	extractModel := flag.String("extract-model", ingest.DefaultPricing.Model, "extraction model id (the pinned cheap model)")
	extractorVersion := flag.String("extractor-version", "v1", "extraction pipeline version (ledger key component, D13)")
	workers := flag.Int("workers", 2, "outbox worker pool size (D12)")
	claimBatch := flag.Int("claim-batch", 16, "outbox events claimed per scan")
	claimLease := flag.Duration("claim-lease", time.Minute, "outbox claim lease (also the retry backoff clock)")
	pollInterval := flag.Duration("poll-interval", 2*time.Second, "outbox poll cadence when idle")
	maxAttempts := flag.Int("max-attempts", 5, "processing attempts before dead-lettering")
	sweepInterval := flag.Duration("sweep-interval", 30*time.Second, "repair sweep cadence (D10 convergence SLO <=5m at S1)")
	priceIn := flag.Float64("extract-price-in", ingest.DefaultPricing.InputUSDPer1M, "extraction model list price, USD per 1M input tokens")
	priceOut := flag.Float64("extract-price-out", ingest.DefaultPricing.OutputUSDPer1M, "extraction model list price, USD per 1M output tokens")
	flag.Parse()

	httpClient := &http.Client{Timeout: store.DefaultTimeout}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if _, err := store.Apply(ctx, httpClient, *osURL); err != nil {
		fmt.Fprintln(os.Stderr, "error applying cluster contract:", err)
		os.Exit(1)
	}

	var embedder embed.Embedder
	if *embedURL != "" {
		embedder = embed.NewHTTPEmbedder(httpClient, *embedURL, embed.ModelInfo{Model: *embedModel, Revision: *embedRevision, Dim: store.EmbeddingDim})
	} else {
		embedder = embed.NewFakeEmbedder(store.EmbeddingDim, nil)
	}
	if err := embed.ValidateInfo(embedder.Info(), store.EmbeddingDim); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	st := store.NewOpenSearchStore(httpClient, *osURL)
	retriever := retrieval.NewOpenSearchRetriever(httpClient, *osURL, embedder)

	job := &enrich.Job{Store: st, Embedder: embedder}
	go job.Run(ctx, *enrichInterval, *enrichBatch)

	// The async write path (Phase 2): outbox worker pool + repair sweep.
	meter := &ingest.CostMeter{}
	pricing := ingest.Pricing{Model: *extractModel, InputUSDPer1M: *priceIn, OutputUSDPer1M: *priceOut}
	var extractor ingest.Extractor
	if *extractURL != "" {
		httpEx := ingest.NewHTTPExtractor(httpClient, *extractURL, *extractModel)
		httpEx.Meter = meter
		extractor = httpEx
	} else {
		extractor = &ingest.RuleExtractor{Meter: meter}
	}
	wk := worker.New(st, extractor, ingest.RuleReconciler{}, embedder, worker.Config{
		ExtractorVersion: *extractorVersion,
		BatchSize:        *claimBatch,
		ClaimLease:       *claimLease,
		PollInterval:     *pollInterval,
		MaxAttempts:      *maxAttempts,
		Workers:          *workers,
	}, slog.Default())
	go wk.Run(ctx)
	sweeper := &worker.Sweeper{Store: st, Worker: wk}
	go sweeper.Run(ctx, *sweepInterval)

	// Extraction cost is the dominant variable cost line (DW-2.6): report
	// the per-1k-events figure periodically so drift is visible in ops logs.
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if u := meter.Snapshot(); u.Events > 0 {
					slog.Info("extraction cost", "model", pricing.Model, "events", u.Events,
						"usd_per_1k_events", u.CostPer1kEventsUSD(pricing))
				}
			}
		}
	}()

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error listening on", *addr, ":", err)
		os.Exit(1)
	}
	grpcServer := grpc.NewServer()
	engrampb.RegisterEngramServer(grpcServer, server.New(st, retriever))

	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()

	slog.Info("engram-server listening", "addr", *addr, "opensearch", *osURL, "embedder", embedder.Info())
	if err := grpcServer.Serve(lis); err != nil {
		fmt.Fprintln(os.Stderr, "error serving:", err)
		os.Exit(1)
	}
}

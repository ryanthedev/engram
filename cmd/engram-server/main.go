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
	"github.com/ryanthedev/engram/internal/acl"
	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/authgrpc"
	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/enrich"
	"github.com/ryanthedev/engram/internal/graph"
	"github.com/ryanthedev/engram/internal/ingest"
	"github.com/ryanthedev/engram/internal/retrieval"
	"github.com/ryanthedev/engram/internal/server"
	"github.com/ryanthedev/engram/internal/store"
	"github.com/ryanthedev/engram/internal/telemetry"
	"github.com/ryanthedev/engram/internal/telemetrygrpc"
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
	embedTimeout := flag.Duration("embed-timeout", 5*time.Minute, "per-request budget for the embedding service")
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
	gateURL := flag.String("gate-url", "", "OpenAI-compatible experience write-gate (LLM judge) endpoint; empty uses the deterministic rule judge (fail-closed either way)")
	gateModel := flag.String("gate-model", "gpt-4o-mini", "experience write-gate judge model id")
	prunePhi := flag.Float64("prune-phi-max", 0.2, "experience prune: soft-expire admitted experiences with utility <= this")
	pruneRetrievalMax := flag.Int("prune-retrieval-max", 0, "experience prune: only soft-expire experiences with retrieval_count <= this")
	pruneInterval := flag.Duration("prune-interval", 5*time.Minute, "experience utility-prune cadence")
	graphJudgeURL := flag.String("graph-judge-url", "", "OpenAI-compatible dedup tie-break judge endpoint; empty uses the deterministic rule judge (fail-safe either way)")
	graphJudgeModel := flag.String("graph-judge-model", "gpt-4o-mini", "graph dedup judge model id")
	graphExpandDepth := flag.Int("graph-expand-depth", graph.MaxDepth, "graph connect-the-dots expansion depth (<=2, D8)")
	metricsAddr := flag.String("metrics-addr", ":9464", "Prometheus /metrics scrape endpoint address (Phase 7 telemetry, DW-7.8)")
	metricsInterval := flag.Duration("metrics-interval", 5*time.Second, "domain-gauge poll cadence")
	budgetPer1k := flag.Float64("budget-per-1k-usd", 5.0, "extraction cost budget alarm threshold, USD per 1k events (DW-2.6/DW-7.6); a breach trips the extraction kill-switch")
	episodicIndex := flag.String("episodic-index", store.EpisodicIndex, "episodic (T1) index name — override to run an isolated instance (e.g. a load test or failure drill) against scratch indices on a shared cluster")
	semanticIndex := flag.String("semantic-index", store.SemanticIndex, "semantic (T2) index name — see -episodic-index")
	ledgerIndex := flag.String("ledger-index", store.LedgerIndex, "extraction-ledger index name — see -episodic-index")
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
		// A separate client on purpose: httpClient carries store.DefaultTimeout,
		// which is sized for OpenSearch round-trips, and model inference is not
		// that shape. A full -enrich-batch of long documents can take minutes,
		// and because the enrichment scans are oldest-first, a batch that
		// cannot finish inside the budget is re-fetched every tick forever —
		// the backlog deadlocks rather than merely crawling. The budget belongs
		// to the embedder, so it is tunable without loosening every OpenSearch
		// call in the process.
		embedClient := &http.Client{Timeout: *embedTimeout}
		embedder = embed.NewHTTPEmbedder(embedClient, *embedURL, embed.ModelInfo{Model: *embedModel, Revision: *embedRevision, Dim: store.EmbeddingDim})
	} else {
		embedder = embed.NewFakeEmbedder(store.EmbeddingDim, nil)
	}
	if err := embed.ValidateInfo(embedder.Info(), store.EmbeddingDim); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	// Phase 4 ACL: the reachability store backs both the query-time read filter
	// (compiled inside the retriever) and the write-time scope guard registered
	// on the store — scope enforced at read AND write (defense in depth).
	edgeStore := store.NewACLEdgeStore(httpClient, *osURL)
	aclFilter := acl.NewFilter(edgeStore, slog.Default())

	st := store.NewOpenSearchStore(httpClient, *osURL,
		store.WithEpisodicIndex(*episodicIndex), store.WithSemanticIndex(*semanticIndex), store.WithLedgerIndex(*ledgerIndex))
	// Materialize the CONFIGURED index names now that Apply has PUT the
	// templates: a -semantic/-episodic/-ledger-index override pointing at a
	// not-yet-created index is created here so the first read/reconcile does
	// not race a missing index (the read paths also treat a missing index as
	// empty — belt and suspenders). A name that does not match its template
	// pattern is rejected loudly rather than silently mis-mapped.
	if _, err := st.EnsureIndices(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error ensuring configured indices:", err)
		os.Exit(1)
	}
	st.RegisterWriteGuard(acl.NewScopeGuard(edgeStore))
	retriever := retrieval.NewOpenSearchRetriever(httpClient, *osURL, embedder,
		retrieval.WithACL(aclFilter), retrieval.WithIndices(*episodicIndex, *semanticIndex))
	tokenStore := store.NewAuthTokenStore(httpClient, *osURL)
	authenticator := auth.NewAuthenticator(tokenStore, nil)

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
	// Phase 7 cost control (DW-7.6): a budget breach trips the kill-switch,
	// which makes the gated extractor fail closed instead of spending further
	// — the worker's existing retry/backoff/dead-letter path takes over, same
	// as any other extractor failure, so a breach costs availability, not data.
	killSwitch := &telemetry.KillSwitch{}
	budgetAlarm := &telemetry.BudgetAlarm{ThresholdUSD: *budgetPer1k, Switch: killSwitch}
	extractor = &telemetry.GatedExtractor{Extractor: extractor, Switch: killSwitch}

	wk := worker.New(st, extractor, ingest.RuleReconciler{}, embedder, worker.Config{
		ExtractorVersion: *extractorVersion,
		BatchSize:        *claimBatch,
		ClaimLease:       *claimLease,
		PollInterval:     *pollInterval,
		MaxAttempts:      *maxAttempts,
		Workers:          *workers,
	}, slog.Default())
	// Phase 5: experience memory (T3) + mandatory write-gate. Registers the
	// distillation stage (D20) and the gated experience tier (Phase-4 seam) and
	// starts the utility-prune job — all without editing the worker/retriever
	// cores. Registration must precede wk.Run so the stage is active for the
	// first claimed batch.
	experienceStore, err := wireExperience(ctx, httpClient, *osURL, experienceConfig{
		gateURL: *gateURL, gateModel: *gateModel,
		prunePhi: *prunePhi, pruneRetrieval: *pruneRetrievalMax, pruneInterval: *pruneInterval,
	}, wk, retriever, slog.Default())
	if err != nil {
		fmt.Fprintln(os.Stderr, "error wiring experience memory:", err)
		os.Exit(1)
	}
	// Phase 6: incremental graph (T4) + bounded connect-the-dots expansion.
	// Registers the graph worker stage (D20) and the ACL-bounded post-hook
	// expander (Phase-4 seam) — no worker/retriever core edits. Registration
	// must precede wk.Run so the stage is active for the first claimed batch.
	graphStore, err := wireGraph(ctx, httpClient, *osURL, graphConfig{
		judgeURL: *graphJudgeURL, judgeModel: *graphJudgeModel, expandDepth: *graphExpandDepth,
	}, embedder, wk, retriever, slog.Default())
	if err != nil {
		fmt.Fprintln(os.Stderr, "error wiring graph memory:", err)
		os.Exit(1)
	}

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

	// Phase 7: OTel/Prometheus telemetry (DW-7.8's domain gauges) + the
	// budget alarm's live cost feed (DW-7.6). Every source is the same live
	// object the request/worker paths already use — no shadow bookkeeping.
	telemetryProvider, err := telemetry.NewProvider()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error building telemetry provider:", err)
		os.Exit(1)
	}
	gauges, err := telemetry.NewGauges(telemetryProvider.Meter("engram-server"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error building telemetry gauges:", err)
		os.Exit(1)
	}
	red, err := telemetry.NewRED(telemetryProvider.Meter("engram-server"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error building RED metrics:", err)
		os.Exit(1)
	}
	recorder := &telemetry.Recorder{
		Gauges:        gauges,
		Outbox:        st,
		DLQ:           st,
		Repair:        sweeper,
		Gate:          experienceStore,
		GateInventory: experienceStore,
		Graph:         graphStore,
		Cost:          costSourceFunc(func() float64 { return meter.Snapshot().CostPer1kEventsUSD(pricing) }),
		Alarm:         budgetAlarm,
		Logger:        slog.Default(),
	}
	go recorder.Run(ctx, *metricsInterval)
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", telemetryProvider.Handler())
	metricsServer := &http.Server{Addr: *metricsAddr, Handler: metricsMux}
	go func() {
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("metrics server failed", "error", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = metricsServer.Shutdown(shutdownCtx)
	}()

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error listening on", *addr, ":", err)
		os.Exit(1)
	}
	// The auth interceptor is the barricade: every gRPC call is authenticated
	// and its Identity injected before any handler runs (D17). The RED
	// interceptor (Phase 7) records every call's duration/outcome regardless
	// of auth result — an auth rejection is itself an observable RED event.
	grpcServer := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			telemetrygrpc.UnaryServerInterceptor(red),
			authgrpc.UnaryServerInterceptor(authenticator, slog.Default()),
		),
	)
	svc := server.New(st, retriever)
	svc.Probe = st
	svc.Auditor = st
	svc.ACL = aclFilter
	// Knowledge platform (Phase 6): runtime collection registry, bulk
	// document writer, and BM25-only retriever — all over the same cluster,
	// never touching the memory tiers. Apply already PUT the
	// knowledge-collections meta template above. KnowledgeAuth's zero value
	// enforces the per-collection role policy at the handlers.
	knowledgeRegistry := store.NewCollectionRegistry(httpClient, *osURL)
	svc.Registry = knowledgeRegistry
	svc.KnowledgeWriter = store.NewKnowledgeStore(httpClient, *osURL)
	svc.KnowledgeReader = retrieval.NewKnowledgeRetriever(httpClient, *osURL, knowledgeRegistry)
	// The Read RPC's episodic branch drills into the same store search reads
	// from; its semantic branch reuses the Auditor above.
	svc.Episodic = st
	// The Export RPC pages over the same graph store the worker stage and
	// expander use — no shadow read path; tenant/ACL are enforced in the
	// handler on top of the store's own tenant-scoped scan. The episodic
	// stage reads the same store search does, behind its own seam (the graph
	// store cannot reach the episodic index).
	svc.Exporter = graphStore
	svc.EpisodicExporter = st
	// MemoryPurge: the memory tier's only delete path, gated on the
	// memory-admin role at the handler. Same store as every other memory
	// operation — the purge queries are just by-query deletes/tombstones over
	// the episodic, ledger, and semantic indices it already owns.
	svc.Purger = st
	engrampb.RegisterEngramServer(grpcServer, svc)

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

// Command engram-loadtest is the Phase-7 measure-first load test (DW-7.2):
// it stands up a real in-process engramd (gRPC Ingest+Search, the outbox
// worker pool, and the repair sweep — the same code paths cmd/engram-server
// runs, minus the auth interceptor and the experience/graph stages, exactly
// like the existing cmd/engram-perf tool bypasses auth) against a scratch
// corpus on the local OpenSearch cluster, then drives it at the S1 model's
// 10x sustained pace (>=500k events/day) plus a 5x burst, reporting
// p50/p95/p99 latency and error rate per endpoint (Ingest, Search), worker
// lag (outbox backlog/age + repair backlog/convergence age), and a
// measured-vs-SQfp16-formula vector RAM comparison (DW-7.3) pulled from the
// real k-NN plugin stats API.
//
// This tool measures; it does not tune (performance-optimization's
// measure-first gate) — its output is the evidence DW-7.2/DW-7.3 cite, not a
// claim of production capacity on real AWS (that confirmation is a
// documented manual step — see the phase report).
//
// Usage:
//
//	go run ./cmd/engram-loadtest [-url http://localhost:9200] [-events-per-day 500000] [-sustained 60s] [-burst 15s] [-search-clients 8]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/ingest"
	"github.com/ryanthedev/engram/internal/retrieval"
	"github.com/ryanthedev/engram/internal/server"
	"github.com/ryanthedev/engram/internal/store"
	"github.com/ryanthedev/engram/internal/telemetry"
	"github.com/ryanthedev/engram/internal/worker"
)

func main() {
	osURLDefault := os.Getenv("ENGRAM_OPENSEARCH_URL")
	if osURLDefault == "" {
		osURLDefault = "http://localhost:9200"
	}
	url := flag.String("url", osURLDefault, "OpenSearch base URL")
	addr := flag.String("addr", "127.0.0.1:0", "gRPC listen address (port 0 = OS-assigned)")
	seedDocs := flag.Int("seed-docs", 200000, "episodic docs pre-seeded with embeddings, for a realistic search corpus and vector-RAM measurement")
	eventsPerDay := flag.Float64("events-per-day", 500000, "sustained ingest pace target, S1 events/day units (10x S1's >=500k/day floor, DW-7.2)")
	burstMultiplier := flag.Float64("burst-multiplier", 5, "burst phase ingest-rate multiplier over the sustained pace")
	sustainedFor := flag.Duration("sustained", 60*time.Second, "sustained-phase duration")
	burstFor := flag.Duration("burst", 15*time.Second, "burst-phase duration")
	searchClients := flag.Int("search-clients", 8, "concurrent Search clients per phase")
	suffix := flag.String("suffix", fmt.Sprintf("loadtest-%d", time.Now().Unix()), "scratch-index name suffix (keeps runs isolated and cleans up predictably)")
	metricsAddr := flag.String("metrics-addr", ":9465", "Prometheus /metrics scrape endpoint for the run's own telemetry.Recorder (DW-7.8 — proves the SAME production gauges move during this specific load test, not just in isolated unit tests)")
	flag.Parse()

	ctx := context.Background()
	httpClient := &http.Client{Timeout: 60 * time.Second}

	if _, err := store.Apply(ctx, httpClient, *url); err != nil {
		fatal("applying cluster contract: %v", err)
	}

	epIdx := "engram-episodic-" + *suffix
	semIdx := "engram-semantic-" + *suffix
	ledIdx := "engram-ledger-" + *suffix
	for _, idx := range []string{epIdx, semIdx, ledIdx} {
		if err := recreateScratchIndex(ctx, httpClient, *url, idx); err != nil {
			fatal("creating scratch index %s: %v", idx, err)
		}
	}

	embedder := embed.NewFakeEmbedder(store.EmbeddingDim, nil)

	fmt.Fprintf(os.Stderr, "seeding %d episodic docs into %s ...\n", *seedDocs, epIdx)
	seedStart := time.Now()
	if err := bulkSeedEpisodic(ctx, httpClient, *url, epIdx, *seedDocs, embedder); err != nil {
		fatal("seeding: %v", err)
	}
	fmt.Fprintf(os.Stderr, "seeded %d docs in %s\n", *seedDocs, time.Since(seedStart))

	st := store.NewOpenSearchStore(httpClient, *url,
		store.WithEpisodicIndex(epIdx), store.WithSemanticIndex(semIdx),
		store.WithLedgerIndex(ledIdx))
	retriever := retrieval.NewOpenSearchRetriever(httpClient, *url, embedder, retrieval.WithIndices(epIdx, semIdx))

	meter := &ingest.CostMeter{}
	extractor := &ingest.RuleExtractor{Meter: meter}
	wk := worker.New(st, extractor, ingest.RuleReconciler{}, embedder, worker.Config{
		BatchSize: 32, Workers: 4, PollInterval: 250 * time.Millisecond,
	}, nil)
	workerCtx, stopWorker := context.WithCancel(ctx)
	go wk.Run(workerCtx)
	sweeper := &worker.Sweeper{Store: st, Worker: wk}
	go sweeper.Run(workerCtx, 2*time.Second)
	defer stopWorker()

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		fatal("listen: %v", err)
	}
	grpcServer := grpc.NewServer()
	engrampb.RegisterEngramServer(grpcServer, server.New(st, retriever))
	go func() { _ = grpcServer.Serve(lis) }()
	defer grpcServer.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fatal("dial: %v", err)
	}
	defer conn.Close()
	client := engrampb.NewEngramClient(conn)

	sustainedRate := *eventsPerDay / 86400 // events/sec
	burstRate := sustainedRate * *burstMultiplier

	lagSampler := &lagSampler{store: st, sweeper: sweeper}
	go lagSampler.run(workerCtx, time.Second)

	// Phase 7 telemetry (DW-7.8): wire the SAME Provider/Gauges/Recorder
	// cmd/engram-server runs in production, against this run's real store
	// and sweeper, and serve /metrics for the run's duration. This is what
	// makes "the gauges move during THIS load test" a directly checkable
	// claim rather than something only proven in isolated unit tests —
	// gaugeSnapshot below scrapes it before and after the phases run.
	telemetryProvider, err := telemetry.NewProvider()
	if err != nil {
		fatal("building telemetry provider: %v", err)
	}
	gauges, err := telemetry.NewGauges(telemetryProvider.Meter("engram-loadtest"))
	if err != nil {
		fatal("building telemetry gauges: %v", err)
	}
	recorder := &telemetry.Recorder{Gauges: gauges, Outbox: st, DLQ: st, Repair: sweeper}
	go recorder.Run(workerCtx, time.Second)
	metricsServer := &http.Server{Addr: *metricsAddr, Handler: telemetryProvider.Handler()}
	go func() { _ = metricsServer.ListenAndServe() }()
	defer metricsServer.Close()
	time.Sleep(1500 * time.Millisecond) // let the recorder's first tick land
	beforeGauges := scrapeGauges(*metricsAddr)

	fmt.Fprintf(os.Stderr, "sustained phase: %.2f events/sec for %s, %d search clients\n", sustainedRate, *sustainedFor, *searchClients)
	sustained := runPhase(ctx, client, sustainedRate, *sustainedFor, *searchClients)

	fmt.Fprintf(os.Stderr, "burst phase: %.2f events/sec for %s, %d search clients\n", burstRate, *burstFor, (*searchClients)*int(*burstMultiplier))
	burst := runPhase(ctx, client, burstRate, *burstFor, (*searchClients)*int(*burstMultiplier))

	afterGauges := scrapeGauges(*metricsAddr)

	// Let the outbox drain before checking for data loss / DLQ.
	time.Sleep(3 * time.Second)
	dlq, _ := st.DeadLetteredCount(ctx)
	backlog, lagAge, _ := st.PendingBacklog(ctx)

	ram, err := measureVectorRAM(ctx, httpClient, *url, epIdx, *seedDocs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: vector-RAM measurement failed: %v\n", err)
	}

	report := map[string]any{
		"sustained_events_per_sec": sustainedRate,
		"burst_events_per_sec":     burstRate,
		"sustained":                sustained,
		"burst":                    burst,
		"worker_lag": map[string]any{
			"max_backlog_observed":      lagSampler.maxBacklog(),
			"max_lag_seconds_observed":  lagSampler.maxLagSeconds(),
			"max_repair_backlog":        lagSampler.maxRepairBacklog(),
			"final_outbox_backlog":      backlog,
			"final_outbox_lag_seconds":  lagAge.Seconds(),
			"final_dead_lettered_count": dlq,
		},
		"vector_ram": ram,
		// gauges_before/after are real /metrics scrapes of this run's own
		// Recorder, taken immediately before the sustained phase and
		// immediately after the burst phase — the DW-7.8 evidence that the
		// production gauges genuinely move during this specific load test.
		"gauges_before_phases": beforeGauges,
		"gauges_after_phases":  afterGauges,
	}
	out, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(out))

	if dlq > 0 {
		fmt.Fprintln(os.Stderr, "WARNING: dead-lettered events present after the load test — investigate before treating this run as a clean pass")
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}

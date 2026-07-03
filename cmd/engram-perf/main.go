// Command engram-perf runs the Phase-1 DW-1.5 performance harness: it seeds N
// synthetic episodic docs directly into a scratch OpenSearch index (bulk,
// bypassing gRPC so seeding time isn't counted), starts a real Engram gRPC
// server over that data, and drives it with C concurrent clients issuing
// Search calls, reporting p50/p95/p99 latency and error rate measured at the
// gRPC boundary — including query embedding, since the retriever embeds
// query text server-side (the co-located D15 budget).
//
// Usage:
//
//	go run ./cmd/engram-perf [-docs 100000] [-clients 8] [-queries 50] [-url http://localhost:9200]
//
// Intended for the perf environment, not CI (DW-1.5).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/retrieval"
	"github.com/ryanthedev/engram/internal/server"
	"github.com/ryanthedev/engram/internal/store"
)

const vocabSize = 200

func main() {
	osURLDefault := os.Getenv("ENGRAM_OPENSEARCH_URL")
	if osURLDefault == "" {
		osURLDefault = "http://localhost:9200"
	}

	url := flag.String("url", osURLDefault, "OpenSearch base URL")
	addr := flag.String("addr", "127.0.0.1:0", "gRPC listen address (port 0 = OS-assigned)")
	docs := flag.Int("docs", 100000, "number of episodic docs to seed")
	clients := flag.Int("clients", 8, "concurrent gRPC clients")
	queriesPerClient := flag.Int("queries", 50, "search calls per client")
	index := flag.String("index", "engram-episodic-perf", "scratch episodic index (matches the engram-episodic* template pattern)")
	flag.Parse()

	ctx := context.Background()
	httpClient := &http.Client{Timeout: 60 * time.Second}

	if _, err := store.Apply(ctx, httpClient, *url); err != nil {
		fatal("applying cluster contract: %v", err)
	}
	if err := createScratchIndex(ctx, httpClient, *url, *index); err != nil {
		fatal("creating scratch index %s: %v", *index, err)
	}

	embedder := embed.NewFakeEmbedder(store.EmbeddingDim, nil)
	vocab := makeVocab(vocabSize)

	fmt.Fprintf(os.Stderr, "seeding %d docs into %s ...\n", *docs, *index)
	seedStart := time.Now()
	if err := bulkSeed(ctx, httpClient, *url, *index, *docs, vocab, embedder); err != nil {
		fatal("seeding: %v", err)
	}
	fmt.Fprintf(os.Stderr, "seeded %d docs in %s\n", *docs, time.Since(seedStart))

	retriever := retrieval.NewOpenSearchRetriever(httpClient, *url, embedder, retrieval.WithIndices(*index, store.SemanticIndex))
	st := store.NewOpenSearchStore(httpClient, *url, store.WithEpisodicIndex(*index))

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		fatal("listen: %v", err)
	}
	grpcServer := grpc.NewServer()
	engrampb.RegisterEngramServer(grpcServer, server.New(st, retriever))
	go func() { _ = grpcServer.Serve(lis) }()
	defer grpcServer.Stop()

	fmt.Fprintln(os.Stderr, "warm-up ...")
	warmConn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fatal("dial: %v", err)
	}
	warmClient := engrampb.NewEngramClient(warmConn)
	for i := 0; i < 20; i++ {
		_, _ = warmClient.Search(ctx, &engrampb.SearchRequest{Query: vocab[i%len(vocab)], K: 10})
	}
	_ = warmConn.Close()

	fmt.Fprintf(os.Stderr, "running %d clients x %d queries ...\n", *clients, *queriesPerClient)
	var (
		mu        sync.Mutex
		latencies []time.Duration
		errCount  int64
		total     int64
	)
	var wg sync.WaitGroup
	for c := 0; c < *clients; c++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				atomic.AddInt64(&errCount, int64(*queriesPerClient))
				atomic.AddInt64(&total, int64(*queriesPerClient))
				return
			}
			defer conn.Close()
			cl := engrampb.NewEngramClient(conn)
			r := rand.New(rand.NewSource(seed))
			local := make([]time.Duration, 0, *queriesPerClient)
			for i := 0; i < *queriesPerClient; i++ {
				q := vocab[r.Intn(len(vocab))]
				start := time.Now()
				_, err := cl.Search(ctx, &engrampb.SearchRequest{Query: q, K: 10})
				d := time.Since(start)
				atomic.AddInt64(&total, 1)
				if err != nil {
					atomic.AddInt64(&errCount, 1)
					continue
				}
				local = append(local, d)
			}
			mu.Lock()
			latencies = append(latencies, local...)
			mu.Unlock()
		}(int64(c) + 1)
	}
	wg.Wait()

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	report := map[string]any{
		"docs_seeded":        *docs,
		"clients":            *clients,
		"queries_per_client": *queriesPerClient,
		"total_queries":      total,
		"errors":             errCount,
		"error_rate":         float64(errCount) / float64(max64(total, 1)),
		"p50_ms":             percentileMS(latencies, 0.50),
		"p95_ms":             percentileMS(latencies, 0.95),
		"p99_ms":             percentileMS(latencies, 0.99),
	}
	out, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(out))
}

// createScratchIndex drops and recreates index empty, matching the
// engram-episodic* template pattern (settings/mappings come from the
// already-applied template).
func createScratchIndex(ctx context.Context, client *http.Client, baseURL, index string) error {
	delReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, baseURL+"/"+index, nil)
	if err != nil {
		return err
	}
	if resp, err := client.Do(delReq); err == nil {
		resp.Body.Close()
	}
	putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, baseURL+"/"+index, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(putReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status %s: %s", resp.Status, string(b))
	}
	return nil
}

// bulkSeed indexes n synthetic episodic docs via the OpenSearch _bulk API in
// batches, with text_embedding pre-computed via the (fast, in-process) fake
// embedder so the perf run measures search, not enrichment lag.
func bulkSeed(ctx context.Context, client *http.Client, baseURL, index string, n int, vocab []string, embedder embed.Embedder) error {
	const batchSize = 500
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for start := 0; start < n; start += batchSize {
		end := min(start+batchSize, n)
		texts := make([]string, end-start)
		for i := start; i < end; i++ {
			texts[i-start] = vocab[i%len(vocab)]
		}
		vectors, err := embedder.Embed(ctx, texts)
		if err != nil {
			return fmt.Errorf("embedding batch [%d,%d): %w", start, end, err)
		}
		var buf bytes.Buffer
		for i := start; i < end; i++ {
			action, _ := json.Marshal(map[string]any{"index": map[string]any{"_index": index}})
			buf.Write(action)
			buf.WriteByte('\n')
			doc, _ := json.Marshal(map[string]any{
				"event_id":       fmt.Sprintf("perf-%d", i),
				"tenant_id":      fmt.Sprintf("tenant-%d", i%10),
				"owner_agent_id": fmt.Sprintf("agent-%d", i%50),
				"kind":           "perf",
				"text":           texts[i-start],
				"text_embedding": vectors[i-start],
				"occurred_at":    now,
				"created_at":     now,
				"attempts":       0,
			})
			buf.Write(doc)
			buf.WriteByte('\n')
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/_bulk", &buf)
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-ndjson")
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("bulk request [%d,%d): %w", start, end, err)
		}
		var result struct {
			Errors bool `json:"errors"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		if result.Errors {
			return fmt.Errorf("bulk seed batch [%d,%d) reported item errors", start, end)
		}
		if (start/batchSize)%20 == 0 {
			fmt.Fprintf(os.Stderr, "  seeded %d/%d\n", end, n)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/"+index+"/_refresh", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// makeVocab deterministically generates n short synthetic query/doc texts
// from a small realistic-looking vocabulary.
func makeVocab(n int) []string {
	words := []string{
		"orders-svc", "payments-api", "checkout-web", "incident", "deploy",
		"timeout", "latency", "connection", "pool", "leak", "rotation",
		"token", "quota", "rate", "limit", "postgres", "kafka", "cdn",
		"oncall", "terraform",
	}
	r := rand.New(rand.NewSource(42))
	vocab := make([]string, n)
	for i := range vocab {
		a, b, c := words[r.Intn(len(words))], words[r.Intn(len(words))], words[r.Intn(len(words))]
		vocab[i] = fmt.Sprintf("%s %s %s incident-%d", a, b, c, i)
	}
	return vocab
}

// percentileMS returns the p-th percentile latency in milliseconds from a
// sorted (ascending) slice.
func percentileMS(sorted []time.Duration, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := min(int(p*float64(len(sorted))), len(sorted)-1)
	return float64(sorted[idx].Microseconds()) / 1000.0
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}

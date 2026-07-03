// Command engram-server runs the Engram gRPC service (Ingest + Search) over
// OpenSearch (Phase 1). It applies the cluster contract on startup
// (idempotent — safe to run alongside make apply-templates), validates the
// configured embedder against the index template's dimension (D15), starts
// the episodic embedding-enrichment job, and serves engrampb.EngramServer.
//
// Usage:
//
//	go run ./cmd/engram-server [-addr :7070] [-url http://localhost:9200] [-embed-url ""]
//
// With no -embed-url, the deterministic fake embedder is used (no real
// BGE-M3 service is required for the walking skeleton's Phase-1 tests).
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
	"github.com/ryanthedev/engram/internal/retrieval"
	"github.com/ryanthedev/engram/internal/server"
	"github.com/ryanthedev/engram/internal/store"
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

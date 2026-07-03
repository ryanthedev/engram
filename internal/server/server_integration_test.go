//go:build integration

package server_test

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/enrich"
	"github.com/ryanthedev/engram/internal/retrieval"
	"github.com/ryanthedev/engram/internal/server"
	"github.com/ryanthedev/engram/internal/store"
	"github.com/ryanthedev/engram/internal/testutil"
)

// TestDW_1_1_IngestThenSearchEndToEnd is the full walking-skeleton
// round-trip: gRPC Ingest appends to episodic and returns a durable id; an
// immediate Search finds it via BM25; a kNN-only search finds it once the
// background enrichment job fills text_embedding, within the dev-harness
// 30s lag budget (DW-1.1).
func TestDW_1_1_IngestThenSearchEndToEnd(t *testing.T) {
	base := testutil.OpenSearchURL()
	if _, err := store.Apply(context.Background(), testutil.HTTPClient, base); err != nil {
		t.Fatalf("applying cluster contract: %v", err)
	}
	epIdx := testutil.ScratchIndexName("episodic", t.Name())
	semIdx := testutil.ScratchIndexName("semantic", t.Name())
	for _, idx := range []string{epIdx, semIdx} {
		testutil.DeleteIndex(t, base, idx)
		t.Cleanup(func(idx string) func() { return func() { testutil.DeleteIndex(t, base, idx) } }(idx))
		testutil.CreateScratchIndex(t, base, idx)
	}

	embedder := embed.NewFakeEmbedder(store.EmbeddingDim, nil)
	st := store.NewOpenSearchStore(testutil.HTTPClient, base, store.WithEpisodicIndex(epIdx), store.WithSemanticIndex(semIdx))
	hybridRetriever := retrieval.NewOpenSearchRetriever(testutil.HTTPClient, base, embedder, retrieval.WithIndices(epIdx, semIdx))
	knnRetriever := retrieval.NewOpenSearchRetriever(testutil.HTTPClient, base, embedder,
		retrieval.WithIndices(epIdx, semIdx), retrieval.WithMode(retrieval.ModeKNNOnly))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job := &enrich.Job{Store: st, Embedder: embedder}
	go job.Run(ctx, 500*time.Millisecond, 50)

	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	engrampb.RegisterEngramServer(grpcServer, server.New(st, hybridRetriever))
	go func() { _ = grpcServer.Serve(lis) }()
	defer grpcServer.Stop()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	client := engrampb.NewEngramClient(conn)

	const text = "the payments-api rollback runbook is RB-99"
	ingestResp, err := client.Ingest(context.Background(), &engrampb.IngestRequest{
		EventId: "ev-e2e-1", TenantId: "dw11", Text: text,
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if ingestResp.GetId() == "" {
		t.Fatal("Ingest returned empty id")
	}

	testutil.RefreshIndex(t, base, epIdx)
	searchResp, err := client.Search(context.Background(), &engrampb.SearchRequest{
		Query: "payments-api rollback runbook", K: 10, TenantId: "dw11",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !containsHitID(searchResp.GetHits(), ingestResp.GetId()) {
		t.Fatalf("BM25 search did not find the ingested event %s; hits=%v", ingestResp.GetId(), searchResp.GetHits())
	}

	deadline := time.Now().Add(30 * time.Second)
	var knnFound bool
	for time.Now().Before(deadline) {
		testutil.RefreshIndex(t, base, epIdx)
		hits, err := knnRetriever.Search(context.Background(), retrieval.Query{Text: text, K: 10}, retrieval.Filter{TenantID: "dw11"})
		if err != nil {
			t.Fatalf("knn poll search: %v", err)
		}
		for _, h := range hits {
			if h.ID == ingestResp.GetId() {
				knnFound = true
			}
		}
		if knnFound {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !knnFound {
		t.Fatal("event was not kNN-searchable within the 30s dev-harness lag budget (DW-1.1)")
	}
}

func containsHitID(hits []*engrampb.Hit, id string) bool {
	for _, h := range hits {
		if h.GetId() == id {
			return true
		}
	}
	return false
}

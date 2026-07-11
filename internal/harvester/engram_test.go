package harvester_test

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/harvester"
	"github.com/ryanthedev/engram/internal/mcp"
)

type mockEngramServer struct {
	engrampb.UnimplementedEngramServer

	collections []*engrampb.CollectionInfo
	indexed     int32
	deleted     int64

	lastIngestReq *engrampb.KnowledgeIngestRequest
	lastDeleteReq *engrampb.KnowledgeDeleteRequest
}

func (s *mockEngramServer) KnowledgeCollections(ctx context.Context, req *engrampb.KnowledgeCollectionsRequest) (*engrampb.KnowledgeCollectionsResponse, error) {
	return &engrampb.KnowledgeCollectionsResponse{
		Collections: s.collections,
	}, nil
}

func (s *mockEngramServer) KnowledgeIngest(ctx context.Context, req *engrampb.KnowledgeIngestRequest) (*engrampb.KnowledgeIngestResponse, error) {
	s.lastIngestReq = req
	return &engrampb.KnowledgeIngestResponse{
		Indexed: s.indexed,
	}, nil
}

func (s *mockEngramServer) KnowledgeDelete(ctx context.Context, req *engrampb.KnowledgeDeleteRequest) (*engrampb.KnowledgeDeleteResponse, error) {
	s.lastDeleteReq = req
	return &engrampb.KnowledgeDeleteResponse{
		Deleted: s.deleted,
	}, nil
}

func TestEngramClientAdapter(t *testing.T) {
	// Start gRPC server on a random port.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	mockSrv := &mockEngramServer{
		collections: []*engrampb.CollectionInfo{
			{
				Spec: &engrampb.CollectionSpec{
					Name:      "test-collection",
					TextField: "text",
				},
				Count: 10,
			},
		},
		indexed: 5,
		deleted: 2,
	}

	grpcServer := grpc.NewServer()
	engrampb.RegisterEngramServer(grpcServer, mockSrv)

	go func() {
		_ = grpcServer.Serve(lis)
	}()
	defer grpcServer.Stop()

	// Dial using our harvester DialEngram.
	client, err := harvester.DialEngram(lis.Addr().String(), "test-token")
	if err != nil {
		t.Fatalf("DialEngram failed: %v", err)
	}

	// Test Collections.
	cols, err := client.Collections(context.Background())
	if err != nil {
		t.Fatalf("Collections failed: %v", err)
	}
	if len(cols) != 1 || cols[0].Name != "test-collection" {
		t.Errorf("unexpected collections: %v", cols)
	}

	// Test Ingest.
	docs := []mcp.KnowledgeDoc{
		{ID: "doc-1", Text: "Hello world"},
	}
	indexed, err := client.Ingest(context.Background(), "test-collection", "test-source", "h-123", docs)
	if err != nil {
		t.Fatalf("Ingest failed: %v", err)
	}
	if indexed != 5 {
		t.Errorf("expected 5 indexed docs, got %d", indexed)
	}
	if mockSrv.lastIngestReq == nil || mockSrv.lastIngestReq.Collection != "test-collection" {
		t.Errorf("unexpected ingest request received: %v", mockSrv.lastIngestReq)
	}

	// Test Delete.
	deleted, err := client.Delete(context.Background(), "test-collection", "test-source", "h-123")
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if deleted != 2 {
		t.Errorf("expected 2 deleted docs, got %d", deleted)
	}
	if mockSrv.lastDeleteReq == nil || mockSrv.lastDeleteReq.Collection != "test-collection" {
		t.Errorf("unexpected delete request received: %v", mockSrv.lastDeleteReq)
	}

	// Test Close.
	closer, ok := client.(interface{ Close() error })
	if !ok {
		t.Error("expected client to support Close()")
	} else {
		if err := closer.Close(); err != nil {
			t.Errorf("Close failed: %v", err)
		}
	}
}

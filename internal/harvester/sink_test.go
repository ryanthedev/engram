package harvester_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ryanthedev/engram/internal/harvester"
	"github.com/ryanthedev/engram/internal/mcp"
)

type ingestCall struct {
	collection string
	source     string
	harvestID  string
	docs       []mcp.KnowledgeDoc
}

type deleteCall struct {
	collection string
	source     string
	harvestID  string
}

type testEngramClient struct {
	ingestCalls []ingestCall
	deleteCalls []deleteCall

	ingestErr   error
	ingestCount int // if non-zero, returns this count
	deleteErr   error
	deleteCount int

	collectionsResult []mcp.CollectionInfo
	collectionsErr    error
}

func (f *testEngramClient) Collections(ctx context.Context) ([]mcp.CollectionInfo, error) {
	if f.collectionsErr != nil {
		return nil, f.collectionsErr
	}
	return f.collectionsResult, nil
}

func (f *testEngramClient) Ingest(ctx context.Context, collection, source, harvestID string, docs []mcp.KnowledgeDoc) (int, error) {
	// Copy docs to avoid mutations sharing state
	copied := make([]mcp.KnowledgeDoc, len(docs))
	copy(copied, docs)
	f.ingestCalls = append(f.ingestCalls, ingestCall{
		collection: collection,
		source:     source,
		harvestID:  harvestID,
		docs:       copied,
	})
	if f.ingestErr != nil {
		return 0, f.ingestErr
	}
	if f.ingestCount > 0 {
		return f.ingestCount, nil
	}
	return len(docs), nil
}

func (f *testEngramClient) Delete(ctx context.Context, collection, source, currentHarvestID string) (int, error) {
	f.deleteCalls = append(f.deleteCalls, deleteCall{
		collection: collection,
		source:     source,
		harvestID:  currentHarvestID,
	})
	if f.deleteErr != nil {
		return 0, f.deleteErr
	}
	return f.deleteCount, nil
}

func TestSink_Batching(t *testing.T) {
	t.Run("DW-2.1: Add triggers auto-flush on batchSize", func(t *testing.T) {
		ec := &testEngramClient{}
		sink := harvester.ExportNewBatchSink(ec, "my-col", "my-src", "h-1", 3, nil)

		// Add 2 docs, should not ingest
		if err := sink.Add(mcp.KnowledgeDoc{ID: "d1"}); err != nil {
			t.Fatalf("Add failed: %v", err)
		}
		if err := sink.Add(mcp.KnowledgeDoc{ID: "d2"}); err != nil {
			t.Fatalf("Add failed: %v", err)
		}
		if len(ec.ingestCalls) != 0 {
			t.Errorf("expected 0 ingest calls, got %d", len(ec.ingestCalls))
		}

		// Add 3rd doc, should auto-flush
		if err := sink.Add(mcp.KnowledgeDoc{ID: "d3"}); err != nil {
			t.Fatalf("Add failed: %v", err)
		}
		if len(ec.ingestCalls) != 1 {
			t.Fatalf("expected 1 ingest call, got %d", len(ec.ingestCalls))
		}
		if len(ec.ingestCalls[0].docs) != 3 {
			t.Errorf("expected 3 docs in batch, got %d", len(ec.ingestCalls[0].docs))
		}
		if sink.Indexed() != 3 {
			t.Errorf("expected Indexed to be 3, got %d", sink.Indexed())
		}
	})

	t.Run("Flush sends remaining buffer", func(t *testing.T) {
		ec := &testEngramClient{}
		sink := harvester.ExportNewBatchSink(ec, "my-col", "my-src", "h-1", 5, nil)

		_ = sink.Add(mcp.KnowledgeDoc{ID: "d1"})
		_ = sink.Add(mcp.KnowledgeDoc{ID: "d2"})

		if err := sink.Flush(context.Background()); err != nil {
			t.Fatalf("Flush failed: %v", err)
		}

		if len(ec.ingestCalls) != 1 {
			t.Fatalf("expected 1 ingest call, got %d", len(ec.ingestCalls))
		}
		if len(ec.ingestCalls[0].docs) != 2 {
			t.Errorf("expected 2 docs in flushed batch, got %d", len(ec.ingestCalls[0].docs))
		}
	})

	t.Run("DW-2.3: Ingest error causes fail-fast", func(t *testing.T) {
		ec := &testEngramClient{
			ingestErr: errors.New("database down"),
		}
		sink := harvester.ExportNewBatchSink(ec, "my-col", "my-src", "h-1", 2, nil)

		_ = sink.Add(mcp.KnowledgeDoc{ID: "d1"})
		err := sink.Add(mcp.KnowledgeDoc{ID: "d2"}) // triggers flush
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !errors.Is(err, ec.ingestErr) {
			t.Errorf("expected error wrapping %v, got %v", ec.ingestErr, err)
		}

		// Subsequent calls should fail fast with same error
		err2 := sink.Add(mcp.KnowledgeDoc{ID: "d3"})
		if err2 == nil {
			t.Fatal("expected error on subsequent Add, got nil")
		}
		if !errors.Is(err2, ec.ingestErr) {
			t.Errorf("expected subsequent error to wrap %v, got %v", ec.ingestErr, err2)
		}

		err3 := sink.Flush(context.Background())
		if err3 == nil {
			t.Fatal("expected error on Flush, got nil")
		}
	})

	t.Run("DW-2.3: Ingest short count causes partial failure", func(t *testing.T) {
		ec := &testEngramClient{
			ingestCount: 1, // sends 2, returns 1
		}
		sink := harvester.ExportNewBatchSink(ec, "my-col", "my-src", "h-1", 2, nil)

		_ = sink.Add(mcp.KnowledgeDoc{ID: "d1"})
		err := sink.Add(mcp.KnowledgeDoc{ID: "d2"}) // triggers flush
		if err == nil {
			t.Fatal("expected partial failure error, got nil")
		}
		if !errors.Is(err, harvester.ErrPartialIngest) {
			t.Errorf("expected ErrPartialIngest, got %v", err)
		}
		if sink.Indexed() != 1 {
			t.Errorf("expected Indexed to accumulate the returned count 1, got %d", sink.Indexed())
		}
	})

	t.Run("boundary: batchSize <= 0 defaults to sane value", func(t *testing.T) {
		ec := &testEngramClient{}
		sink := harvester.ExportNewBatchSink(ec, "my-col", "my-src", "h-1", 0, nil)
		// Default should be 500. Let's verify by adding 499 docs, no flush.
		for i := 0; i < 499; i++ {
			_ = sink.Add(mcp.KnowledgeDoc{ID: "d"})
		}
		if len(ec.ingestCalls) != 0 {
			t.Fatalf("expected no flush at 499 docs with default batch size, got %d calls", len(ec.ingestCalls))
		}
		// 500th doc should trigger flush.
		_ = sink.Add(mcp.KnowledgeDoc{ID: "d"})
		if len(ec.ingestCalls) != 1 {
			t.Fatalf("expected 1 flush at 500 docs, got %d calls", len(ec.ingestCalls))
		}
	})

	t.Run("boundary: context cancellation surfaced", func(t *testing.T) {
		ec := &testEngramClient{}
		sink := harvester.ExportNewBatchSink(ec, "my-col", "my-src", "h-1", 2, nil)
		_ = sink.Add(mcp.KnowledgeDoc{ID: "d1"})

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := sink.Flush(ctx)
		// Should surface context cancellation or standard cancellation
		if err == nil {
			t.Fatal("expected context cancelled error, got nil")
		}
	})
}

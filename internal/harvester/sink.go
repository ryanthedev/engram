package harvester

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/ryanthedev/engram/internal/mcp"
)

// ErrPartialIngest is returned when the engram server indexes fewer documents than expected.
var ErrPartialIngest = errors.New("harvester: partial ingest: server indexed fewer documents than sent")

type batchSink struct {
	ec         EngramClient
	collection string
	source     string
	harvestID  string
	batchSize  int
	logger     *slog.Logger

	ctx context.Context // set by Runner to propagate run context to Add

	mu      sync.Mutex
	buf     []mcp.KnowledgeDoc
	indexed int
	err     error
}

var _ Sink = (*batchSink)(nil)

// TestBatchSink is a helper interface for testing batchSink from external test packages.
type TestBatchSink interface {
	Sink
	Indexed() int
}

// ExportNewBatchSink is for testing purposes only.
func ExportNewBatchSink(ec EngramClient, collection, source, harvestID string, batchSize int, logger *slog.Logger) TestBatchSink {
	return newBatchSink(ec, collection, source, harvestID, batchSize, logger)
}

func newBatchSink(ec EngramClient, collection, source, harvestID string, batchSize int, logger *slog.Logger) *batchSink {
	if logger == nil {
		logger = slog.Default()
	}
	if batchSize <= 0 {
		batchSize = 500
	}
	return &batchSink{
		ec:         ec,
		collection: collection,
		source:     source,
		harvestID:  harvestID,
		batchSize:  batchSize,
		logger:     logger,
	}
}

// Indexed returns the total number of successfully indexed documents.
func (s *batchSink) Indexed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.indexed
}

// Add buffers the document, auto-flushing if the batch size is reached.
func (s *batchSink) Add(doc mcp.KnowledgeDoc) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.err != nil {
		return s.err
	}

	s.buf = append(s.buf, doc)
	if len(s.buf) >= s.batchSize {
		ctx := s.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		if err := s.flushWithContextLocked(ctx); err != nil {
			s.err = err
			return err
		}
	}
	return nil
}

// Flush sends any remaining buffered documents to the server.
func (s *batchSink) Flush(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.err != nil {
		return s.err
	}

	if err := s.flushWithContextLocked(ctx); err != nil {
		s.err = err
		return err
	}
	return nil
}

func (s *batchSink) flushWithContextLocked(ctx context.Context) error {
	if len(s.buf) == 0 {
		return nil
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("harvester: ingesting batch: %w", err)
	}

	batch := s.buf
	s.buf = nil // Clear buffer before making the I/O call to handle error fast

	n, err := s.ec.Ingest(ctx, s.collection, s.source, s.harvestID, batch)
	if err != nil {
		return fmt.Errorf("harvester: ingesting batch: %w", err)
	}

	s.indexed += n

	if n < len(batch) {
		return fmt.Errorf("harvester: ingesting batch: %w", ErrPartialIngest)
	}

	return nil
}

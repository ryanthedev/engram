package harvester

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/ryanthedev/engram/internal/mcp"
)

var globalCounter uint64

// Report contains metrics from a harvest execution.
type Report struct {
	Indexed   int
	Deleted   int
	HarvestID string
}

// Runner coordinates the harvesting of documents from a Source into an EngramClient.
type Runner struct {
	ec        EngramClient
	batchSize int
	logger    *slog.Logger
}

// NewRunner creates a new Runner.
func NewRunner(ec EngramClient, batchSize int, logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{
		ec:        ec,
		batchSize: batchSize,
		logger:    logger,
	}
}

// Run executes a harvest run for a given collection and source.
func (r *Runner) Run(ctx context.Context, collection string, source Source) (Report, error) {
	counter := atomic.AddUint64(&globalCounter, 1)
	harvestID := fmt.Sprintf("%s-%d#%s", time.Now().UTC().Format(time.RFC3339Nano), counter, source.Type())

	r.logger.InfoContext(ctx, "harvester: starting harvest run",
		slog.String("collection", collection),
		slog.String("source", source.Type()),
		slog.String("harvest_id", harvestID),
	)

	sink := newBatchSink(r.ec, collection, source.Type(), harvestID, r.batchSize, r.logger)
	sink.ctx = ctx

	if err := source.Harvest(ctx, sink); err != nil {
		return Report{HarvestID: harvestID, Indexed: sink.Indexed()}, fmt.Errorf("harvester: harvesting source: %w", err)
	}

	if err := sink.Flush(ctx); err != nil {
		return Report{HarvestID: harvestID, Indexed: sink.Indexed()}, fmt.Errorf("harvester: flushing sink: %w", err)
	}

	indexed := sink.Indexed()
	var deleted int

	if source.Mode() == FullHarvest {
		if indexed > 0 {
			n, err := r.ec.Delete(ctx, collection, source.Type(), harvestID)
			if err != nil {
				return Report{HarvestID: harvestID, Indexed: indexed}, fmt.Errorf("harvester: sweep deletion: %w", err)
			}
			deleted = n
		} else {
			r.logger.InfoContext(ctx, "harvester: skipped delete sweep: full harvest indexed zero documents",
				slog.String("collection", collection),
				slog.String("source", source.Type()),
				slog.String("harvest_id", harvestID),
			)
		}
	}

	report := Report{
		Indexed:   indexed,
		Deleted:   deleted,
		HarvestID: harvestID,
	}

	r.logger.InfoContext(ctx, "harvester: harvest run finished",
		slog.String("collection", collection),
		slog.String("source", source.Type()),
		slog.String("harvest_id", harvestID),
		slog.Int("indexed", report.Indexed),
		slog.Int("deleted", report.Deleted),
	)

	return report, nil
}

// Collections returns the list of collections from the Engram client.
func (r *Runner) Collections(ctx context.Context) ([]mcp.CollectionInfo, error) {
	infos, err := r.ec.Collections(ctx)
	if err != nil {
		return nil, fmt.Errorf("harvester: fetching collections: %w", err)
	}
	return infos, nil
}

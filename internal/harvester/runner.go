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

// Run executes a harvest run for a given collection and source. A source that
// implements ScopedSource is harvested and swept per config-derived scope (so
// one scope's run never deletes another's docs); a plain Source is harvested as
// a single scope equal to Type() (the pre-existing behavior).
func (r *Runner) Run(ctx context.Context, collection string, source Source) (Report, error) {
	counter := atomic.AddUint64(&globalCounter, 1)
	harvestID := fmt.Sprintf("%s-%d#%s", time.Now().UTC().Format(time.RFC3339Nano), counter, source.Type())

	r.logger.InfoContext(ctx, "harvester: starting harvest run",
		slog.String("collection", collection),
		slog.String("source", source.Type()),
		slog.String("harvest_id", harvestID),
	)

	if scoped, ok := source.(ScopedSource); ok {
		return r.runScoped(ctx, collection, source, scoped, harvestID)
	}
	return r.runSingleScope(ctx, collection, source, source.Type(), harvestID)
}

// runSingleScope harvests a source under one sweep scope and, for a FullHarvest,
// runs the not-current sweep — guarded by the zero-doc empty guard so an empty
// successful full harvest never wipes the collection to zero.
func (r *Runner) runSingleScope(ctx context.Context, collection string, source Source, scope, harvestID string) (Report, error) {
	sink := newBatchSink(r.ec, collection, scope, harvestID, r.batchSize, r.logger)
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
			n, err := r.ec.Delete(ctx, collection, scope, harvestID)
			if err != nil {
				return Report{HarvestID: harvestID, Indexed: indexed}, fmt.Errorf("harvester: sweep deletion: %w", err)
			}
			deleted = n
		} else {
			r.logger.InfoContext(ctx, "harvester: skipped delete sweep: full harvest indexed zero documents",
				slog.String("collection", collection),
				slog.String("source", scope),
				slog.String("harvest_id", harvestID),
			)
		}
	}

	report := Report{Indexed: indexed, Deleted: deleted, HarvestID: harvestID}
	r.logFinished(ctx, collection, source.Type(), report)
	return report, nil
}

// runScoped harvests every config-declared scope of a ScopedSource under its own
// `source` value, then (for a FullHarvest) sweeps each scope independently.
//
// Fail-safe: ALL scopes are harvested and flushed before ANY sweep — if any
// scope errors, the run aborts before every sweep, so a partial run never
// deletes live rows (correctness on a delete path, matching the single-scope
// path). Already-ingested docs from earlier scopes are harmless (idempotent
// upsert-by-id; reconciled on the next clean run).
//
// Unlike the single-scope path there is NO aggregate zero-doc guard: each scope
// is swept regardless of how many docs IT emitted, because the scope set comes
// from config, not from emitted docs. A repo whose files were all deleted (zero
// docs this run) thus has its own stale docs swept, and one scope's sweep can
// never touch another scope's rows.
func (r *Runner) runScoped(ctx context.Context, collection string, source Source, scoped ScopedSource, harvestID string) (Report, error) {
	scopes := scoped.SweepScopes()

	var totalIndexed int
	for _, scope := range scopes {
		if err := ctx.Err(); err != nil {
			return Report{HarvestID: harvestID, Indexed: totalIndexed}, fmt.Errorf("harvester: harvesting scope %q: %w", scope, err)
		}
		sink := newBatchSink(r.ec, collection, scope, harvestID, r.batchSize, r.logger)
		sink.ctx = ctx

		if err := scoped.HarvestScope(ctx, scope, sink); err != nil {
			return Report{HarvestID: harvestID, Indexed: totalIndexed}, fmt.Errorf("harvester: harvesting scope %q: %w", scope, err)
		}
		if err := sink.Flush(ctx); err != nil {
			return Report{HarvestID: harvestID, Indexed: totalIndexed}, fmt.Errorf("harvester: flushing scope %q: %w", scope, err)
		}
		totalIndexed += sink.Indexed()
	}

	var totalDeleted int
	if source.Mode() == FullHarvest {
		for _, scope := range scopes {
			n, err := r.ec.Delete(ctx, collection, scope, harvestID)
			if err != nil {
				return Report{HarvestID: harvestID, Indexed: totalIndexed, Deleted: totalDeleted}, fmt.Errorf("harvester: sweep deletion for scope %q: %w", scope, err)
			}
			totalDeleted += n
		}
	}

	report := Report{Indexed: totalIndexed, Deleted: totalDeleted, HarvestID: harvestID}
	r.logFinished(ctx, collection, source.Type(), report)
	return report, nil
}

// logFinished emits the run-completion log line shared by both harvest paths.
func (r *Runner) logFinished(ctx context.Context, collection, sourceType string, report Report) {
	r.logger.InfoContext(ctx, "harvester: harvest run finished",
		slog.String("collection", collection),
		slog.String("source", sourceType),
		slog.String("harvest_id", report.HarvestID),
		slog.Int("indexed", report.Indexed),
		slog.Int("deleted", report.Deleted),
	)
}

// Collections returns the list of collections from the Engram client.
func (r *Runner) Collections(ctx context.Context) ([]mcp.CollectionInfo, error) {
	infos, err := r.ec.Collections(ctx)
	if err != nil {
		return nil, fmt.Errorf("harvester: fetching collections: %w", err)
	}
	return infos, nil
}

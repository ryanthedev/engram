package experience

import (
	"context"
	"log/slog"
	"time"
)

// PruneJob is the background utility-prune loop: it periodically soft-expires
// admitted experiences whose utility Φ and retrieval count are both at/below the
// configured floors, so low-value memory stops crowding retrieval without ever
// being deleted (D3 bi-temporal — the record stays recoverable via
// Store.Recover). It mirrors enrich.Job's poll/Tick shape: a failed
// tick is logged, never fatal.
type PruneJob struct {
	Store *Store
	// PhiMax is the utility ceiling for pruning (inclusive). An experience with
	// Utility <= PhiMax AND RetrievalCount <= RetrievalMax is prunable.
	PhiMax float64
	// RetrievalMax is the retrieval-count ceiling for pruning (inclusive).
	RetrievalMax int
	// Logger receives tick outcomes; defaults to slog.Default() when nil.
	Logger *slog.Logger
}

// Tick runs one prune pass and returns the number soft-expired.
func (j *PruneJob) Tick(ctx context.Context) (int, error) {
	return j.Store.Prune(ctx, j.PhiMax, j.RetrievalMax)
}

// Run polls Tick every interval until ctx is done. A transient error is logged,
// not fatal — a background job must not die on one bad tick.
func (j *PruneJob) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := j.Tick(ctx); err != nil {
				j.logger().ErrorContext(ctx, "experience prune tick failed", "error", err)
			}
		}
	}
}

func (j *PruneJob) logger() *slog.Logger {
	if j.Logger != nil {
		return j.Logger
	}
	return slog.Default()
}

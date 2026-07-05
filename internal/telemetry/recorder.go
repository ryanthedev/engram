package telemetry

import (
	"context"
	"log/slog"
	"time"
)

// OutboxSource is the outbox/worker-lag gauge's data source. It is satisfied
// structurally by *store.OpenSearchStore (PendingBacklog) — no import of
// this package from internal/store.
type OutboxSource interface {
	PendingBacklog(ctx context.Context) (count int64, oldestAge time.Duration, err error)
}

// DLQSource is the DLQ-depth gauge's data source. Satisfied structurally by
// *store.OpenSearchStore (DeadLetteredCount).
type DLQSource interface {
	DeadLetteredCount(ctx context.Context) (int64, error)
}

// RepairSource is the repair-backlog + convergence-age gauges' data source.
// Satisfied structurally by *worker.Sweeper (Backlog, ConvergenceAge).
type RepairSource interface {
	Backlog(ctx context.Context) (int, error)
	ConvergenceAge() time.Duration
}

// GateSource is the gate-verdict-rate gauges' data source. Satisfied
// structurally by *experience.Store (VerdictCounts). These rates are
// in-process and reset on restart — see GateInventorySource for the durable
// counterpart.
type GateSource interface {
	VerdictCounts() (admit, quarantine, reject int64)
}

// GateInventorySource is the durable experience-inventory gauges' data
// source (Phase 3). Satisfied structurally by *experience.Store
// (InventoryCounts) — a per-poll OpenSearch _count of the admitted and
// quarantine indices, so the gauges reflect real live composition across a
// server restart instead of resetting to 0 like GateSource's in-process
// rates.
type GateInventorySource interface {
	InventoryCounts(ctx context.Context) (admitted, quarantined int64, err error)
}

// GraphSource is the graph entity-count gauge's data source (Phase 3).
// Satisfied structurally by *graph.Store (CountAllEntities) — a per-poll
// OpenSearch _count of live entities across all tenants, the DW-6.3
// stability signal wired onto /metrics.
type GraphSource interface {
	CountAllEntities(ctx context.Context) (int, error)
}

// CostSource is the extraction-cost gauge's data source. main.go supplies a
// small closure over *ingest.CostMeter + ingest.Pricing — CostMeter itself
// carries no notion of "current price," so the closure is the natural seam.
type CostSource interface {
	CostPer1kEventsUSD() float64
}

// Recorder polls the configured sources on each Poll call and records their
// values into Gauges. Every source is optional (nil-safe): a Recorder can be
// built with only the sources available in a given context (e.g. a unit test
// exercising just the gate-verdict gauges).
type Recorder struct {
	Gauges *Gauges

	Outbox        OutboxSource
	DLQ           DLQSource
	Repair        RepairSource
	Gate          GateSource
	GateInventory GateInventorySource
	Graph         GraphSource
	Cost          CostSource
	Alarm         *BudgetAlarm

	// Logger receives poll errors (best-effort observability; a failed poll
	// is not fatal — the next tick retries).
	Logger *slog.Logger
}

func (r *Recorder) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

// Poll reads every configured source once and records the results into
// Gauges. A single source's failure is logged and does not block the others.
func (r *Recorder) Poll(ctx context.Context) {
	if r.Outbox != nil {
		count, age, err := r.Outbox.PendingBacklog(ctx)
		if err != nil {
			r.logger().WarnContext(ctx, "telemetry: polling outbox backlog failed", "error", err)
		} else {
			r.Gauges.OutboxBacklog.Record(ctx, float64(count))
			r.Gauges.OutboxLagSeconds.Record(ctx, age.Seconds())
		}
	}
	if r.DLQ != nil {
		count, err := r.DLQ.DeadLetteredCount(ctx)
		if err != nil {
			r.logger().WarnContext(ctx, "telemetry: polling DLQ depth failed", "error", err)
		} else {
			r.Gauges.DLQDepth.Record(ctx, float64(count))
		}
	}
	if r.Repair != nil {
		backlog, err := r.Repair.Backlog(ctx)
		if err != nil {
			r.logger().WarnContext(ctx, "telemetry: polling repair backlog failed", "error", err)
		} else {
			r.Gauges.RepairBacklog.Record(ctx, float64(backlog))
		}
		r.Gauges.RepairConvergenceAge.Record(ctx, r.Repair.ConvergenceAge().Seconds())
	}
	if r.Gate != nil {
		admit, quarantine, reject := r.Gate.VerdictCounts()
		total := admit + quarantine + reject
		// Always record (0 before any verdict exists) so the gauge renders
		// on the very first scrape instead of being absent from the
		// exposition until the first Admit call — a dashboard panel with no
		// series at all reads as broken, not "zero."
		var admitRate, quarantineRate, rejectRate float64
		if total > 0 {
			admitRate = float64(admit) / float64(total)
			quarantineRate = float64(quarantine) / float64(total)
			rejectRate = float64(reject) / float64(total)
		}
		r.Gauges.GateAdmitRate.Record(ctx, admitRate)
		r.Gauges.GateQuarantineRate.Record(ctx, quarantineRate)
		r.Gauges.GateRejectRate.Record(ctx, rejectRate)
	}
	if r.GateInventory != nil {
		admitted, quarantined, err := r.GateInventory.InventoryCounts(ctx)
		if err != nil {
			r.logger().WarnContext(ctx, "telemetry: polling experience inventory failed", "error", err)
		} else {
			r.Gauges.ExperienceAdmittedInventory.Record(ctx, float64(admitted))
			r.Gauges.ExperienceQuarantinedInventory.Record(ctx, float64(quarantined))
		}
	}
	if r.Graph != nil {
		count, err := r.Graph.CountAllEntities(ctx)
		if err != nil {
			r.logger().WarnContext(ctx, "telemetry: polling graph entity count failed", "error", err)
		} else {
			r.Gauges.GraphEntityCount.Record(ctx, float64(count))
		}
	}
	if r.Cost != nil {
		costPer1k := r.Cost.CostPer1kEventsUSD()
		r.Gauges.ExtractionCostPer1k.Record(ctx, costPer1k)
		if r.Alarm != nil {
			firing := r.Alarm.Evaluate(costPer1k)
			if firing {
				r.Gauges.BudgetAlarmFiring.Record(ctx, 1)
			} else {
				r.Gauges.BudgetAlarmFiring.Record(ctx, 0)
			}
		}
	}
}

// Run polls every interval until ctx is done — the production wiring for
// main.go (mirrors the existing cost-report ticker's shape).
func (r *Recorder) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.Poll(ctx)
		}
	}
}

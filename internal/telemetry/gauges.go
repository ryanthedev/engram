package telemetry

import (
	"fmt"

	"go.opentelemetry.io/otel/metric"
)

// Gauges holds the DW-7.8 domain instruments: one synchronous Float64Gauge
// per gauge listed in the phase's IN scope. Synchronous gauges (OTel metric
// API's Meter.Float64Gauge, not an observable callback) are used because the
// Recorder already owns a poll loop — recording a value directly on each poll
// is simpler than wiring an async callback, and it is exactly as visible to
// a scrape.
type Gauges struct {
	OutboxBacklog        metric.Float64Gauge // count of unprocessed, non-dead-lettered episodic docs
	OutboxLagSeconds     metric.Float64Gauge // age of the oldest unprocessed episodic doc
	RepairBacklog        metric.Float64Gauge // count of chains/keys the repair sweep still needs to converge
	RepairConvergenceAge metric.Float64Gauge // seconds since the repair sweep last completed a pass
	DLQDepth             metric.Float64Gauge // count of dead-lettered episodic docs
	GateAdmitRate        metric.Float64Gauge // fraction of gate verdicts that were Admit
	GateQuarantineRate   metric.Float64Gauge // fraction of gate verdicts that were Quarantine
	GateRejectRate       metric.Float64Gauge // fraction of gate verdicts that were Reject
	ExtractionCostPer1k  metric.Float64Gauge // USD per 1,000 ingested events
	BudgetAlarmFiring    metric.Float64Gauge // 1 when the budget alarm is firing, else 0
}

// NewGauges creates every DW-7.8 instrument on meter. An error from any
// instrument constructor aborts the whole set — a partially wired gauge set
// would silently under-report, which is worse than failing at startup.
func NewGauges(meter metric.Meter) (*Gauges, error) {
	g := &Gauges{}
	var err error
	specs := []struct {
		name string
		desc string
		dst  *metric.Float64Gauge
	}{
		{"engram_outbox_backlog", "unprocessed, non-dead-lettered episodic docs", &g.OutboxBacklog},
		{"engram_outbox_lag_seconds", "age of the oldest unprocessed episodic doc", &g.OutboxLagSeconds},
		{"engram_repair_backlog", "chains/keys the repair sweep still needs to converge", &g.RepairBacklog},
		{"engram_repair_convergence_age_seconds", "seconds since the repair sweep last completed a pass", &g.RepairConvergenceAge},
		{"engram_dlq_depth", "dead-lettered episodic docs", &g.DLQDepth},
		{"engram_gate_admit_rate", "fraction of experience-gate verdicts that were Admit", &g.GateAdmitRate},
		{"engram_gate_quarantine_rate", "fraction of experience-gate verdicts that were Quarantine", &g.GateQuarantineRate},
		{"engram_gate_reject_rate", "fraction of experience-gate verdicts that were Reject", &g.GateRejectRate},
		{"engram_extraction_cost_usd_per_1k_events", "extraction cost, USD per 1,000 ingested events", &g.ExtractionCostPer1k},
		{"engram_budget_alarm_firing", "1 when the extraction budget alarm is firing, else 0", &g.BudgetAlarmFiring},
	}
	for _, s := range specs {
		*s.dst, err = meter.Float64Gauge(s.name, metric.WithDescription(s.desc))
		if err != nil {
			return nil, fmt.Errorf("telemetry: creating gauge %s: %w", s.name, err)
		}
	}
	return g, nil
}

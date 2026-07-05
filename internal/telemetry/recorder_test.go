package telemetry

import (
	"context"
	"io"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeOutbox/fakeDLQ/fakeRepair/fakeGate/fakeCost let tests drive each
// gauge's data source independently and deterministically.
type fakeOutbox struct {
	count int64
	age   time.Duration
	err   error
}

func (f fakeOutbox) PendingBacklog(context.Context) (int64, time.Duration, error) {
	return f.count, f.age, f.err
}

type fakeDLQ struct {
	count int64
	err   error
}

func (f fakeDLQ) DeadLetteredCount(context.Context) (int64, error) { return f.count, f.err }

type fakeRepair struct {
	backlog int
	age     time.Duration
	err     error
}

func (f fakeRepair) Backlog(context.Context) (int, error) { return f.backlog, f.err }
func (f fakeRepair) ConvergenceAge() time.Duration        { return f.age }

type fakeGate struct{ admit, quarantine, reject int64 }

func (f fakeGate) VerdictCounts() (int64, int64, int64) { return f.admit, f.quarantine, f.reject }

type fakeGateInventory struct {
	admitted, quarantined int64
	err                   error
}

func (f fakeGateInventory) InventoryCounts(context.Context) (int64, int64, error) {
	return f.admitted, f.quarantined, f.err
}

type fakeGraph struct {
	count int
	err   error
}

func (f fakeGraph) CountAllEntities(context.Context) (int, error) { return f.count, f.err }

type fakeCost struct{ perK float64 }

func (f fakeCost) CostPer1kEventsUSD() float64 { return f.perK }

func newTestProvider(t *testing.T) (*Provider, *Gauges) {
	t.Helper()
	p, err := NewProvider()
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	g, err := NewGauges(p.Meter("test"))
	if err != nil {
		t.Fatalf("NewGauges: %v", err)
	}
	return p, g
}

// scrape serves the Provider's real HTTP handler and returns the exposition
// body — the same bytes a Prometheus scraper would see, so gauge assertions
// below match the standard `metric_name{labels} value` text format.
func scrape(t *testing.T, p *Provider) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	p.Handler().ServeHTTP(rec, req)
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatalf("reading scrape body: %v", err)
	}
	return string(body)
}

// gaugeValue extracts name's value from a Prometheus exposition body,
// tolerant of any label set the OTel bridge attaches (e.g. otel_scope_name).
func gaugeValue(t *testing.T, body, name string) float64 {
	t.Helper()
	re := regexp.MustCompile(regexp.QuoteMeta(name) + `(?:\{[^}]*\})?\s+(-?[0-9.eE+]+)\n`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("gauge %s not found in scrape body:\n%s", name, body)
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatalf("parsing gauge %s value %q: %v", name, m[1], err)
	}
	return v
}

// wantGauge asserts name's scraped value equals want (within float epsilon).
func wantGauge(t *testing.T, body, name string, want float64) {
	t.Helper()
	got := gaugeValue(t, body, name)
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("gauge %s: got %v, want %v", name, got, want)
	}
}

// TestRecorder_GaugesMoveOnPoll is the DW-7.8 evidence: every domain gauge
// renders on the scrape endpoint and its value changes between two polls fed
// different source readings.
func TestRecorder_GaugesMoveOnPoll(t *testing.T) {
	p, g := newTestProvider(t)
	rec := &Recorder{
		Gauges: g,
		Outbox: fakeOutbox{count: 3, age: 5 * time.Second},
		DLQ:    fakeDLQ{count: 1},
		Repair: fakeRepair{backlog: 2, age: 10 * time.Second},
		Gate:   fakeGate{admit: 8, quarantine: 1, reject: 1},
		Cost:   fakeCost{perK: 1.5},
		Alarm:  &BudgetAlarm{ThresholdUSD: 5.0},
	}

	rec.Poll(context.Background())
	first := scrape(t, p)
	for _, want := range []string{
		"engram_outbox_backlog", "engram_outbox_lag_seconds",
		"engram_repair_backlog", "engram_repair_convergence_age_seconds",
		"engram_dlq_depth",
		"engram_gate_admit_rate", "engram_gate_quarantine_rate", "engram_gate_reject_rate",
		"engram_extraction_cost_usd_per_1k_events", "engram_budget_alarm_firing",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("first scrape missing gauge %s\n%s", want, first)
		}
	}
	wantGauge(t, first, "engram_outbox_backlog", 3)
	wantGauge(t, first, "engram_gate_admit_rate", 0.8)
	wantGauge(t, first, "engram_budget_alarm_firing", 0)

	// Change every source's reading and poll again — every gauge must move.
	rec.Outbox = fakeOutbox{count: 30, age: 90 * time.Second}
	rec.DLQ = fakeDLQ{count: 9}
	rec.Repair = fakeRepair{backlog: 20, age: 300 * time.Second}
	rec.Gate = fakeGate{admit: 1, quarantine: 8, reject: 1}
	rec.Cost = fakeCost{perK: 9.9} // breaches the 5.0 threshold
	rec.Poll(context.Background())
	second := scrape(t, p)

	if gaugeValue(t, second, "engram_outbox_backlog") == 3 {
		t.Errorf("outbox backlog did not move:\n%s", second)
	}
	wantGauge(t, second, "engram_outbox_backlog", 30)
	wantGauge(t, second, "engram_dlq_depth", 9)
	wantGauge(t, second, "engram_repair_backlog", 20)
	wantGauge(t, second, "engram_gate_admit_rate", 0.1)
	wantGauge(t, second, "engram_budget_alarm_firing", 1)
	if !rec.Alarm.Firing() {
		t.Error("expected Alarm.Firing() true after breach")
	}
}

// TestRecorder_NilSourcesSkipped proves a Recorder with only some sources
// configured does not panic and only records the gauges it has data for.
func TestRecorder_NilSourcesSkipped(t *testing.T) {
	_, g := newTestProvider(t)
	rec := &Recorder{Gauges: g, Gate: fakeGate{admit: 1}}
	rec.Poll(context.Background()) // must not panic with Outbox/DLQ/Repair/Cost nil
}

// TestRecorder_SourceErrorDoesNotBlockOthers proves one failing source
// (e.g. a transient OpenSearch read) does not prevent the others from
// recording — a gauge with no fresh data simply keeps its last value rather
// than the whole poll aborting.
func TestRecorder_SourceErrorDoesNotBlockOthers(t *testing.T) {
	p, g := newTestProvider(t)
	rec := &Recorder{
		Gauges: g,
		Outbox: fakeOutbox{err: context.DeadlineExceeded},
		DLQ:    fakeDLQ{count: 4},
	}
	rec.Poll(context.Background())
	wantGauge(t, scrape(t, p), "engram_dlq_depth", 4)
}

// TestDW_3_1_DurableInventoryGaugesReflectBackendNotZero is the Phase-3
// evidence: the durable experience-inventory gauges render the
// GateInventorySource's counts on the very first poll — proving they do not
// depend on any in-process accumulation the way GateSource's rates do.
func TestDW_3_1_DurableInventoryGaugesReflectBackendNotZero(t *testing.T) {
	p, g := newTestProvider(t)
	rec := &Recorder{
		Gauges:        g,
		GateInventory: fakeGateInventory{admitted: 42, quarantined: 7},
	}
	rec.Poll(context.Background())
	body := scrape(t, p)
	wantGauge(t, body, "engram_experience_admitted_count", 42)
	wantGauge(t, body, "engram_experience_quarantined_count", 7)
}

// TestDW_3_1_GateRateGaugeDescriptionsStateResetOnRestart proves every
// in-process gate-verdict-rate gauge's HELP text now says it resets on
// restart, so its description no longer misstates what it computes (it does
// NOT reflect durable OpenSearch state — that's what the new inventory
// gauges are for).
func TestDW_3_1_GateRateGaugeDescriptionsStateResetOnRestart(t *testing.T) {
	p, g := newTestProvider(t)
	rec := &Recorder{Gauges: g, Gate: fakeGate{admit: 1}}
	rec.Poll(context.Background())
	body := scrape(t, p)
	for _, name := range []string{"engram_gate_admit_rate", "engram_gate_quarantine_rate", "engram_gate_reject_rate"} {
		help := helpLine(t, body, name)
		if !strings.Contains(help, "resets to 0 on restart") {
			t.Errorf("gauge %s HELP text = %q, want it to state it resets on restart", name, help)
		}
	}
}

// helpLine returns the "# HELP <name> ..." line for name from a Prometheus
// exposition body.
func helpLine(t *testing.T, body, name string) string {
	t.Helper()
	prefix := "# HELP " + name + " "
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatalf("no HELP line found for %s in scrape body:\n%s", name, body)
	return ""
}

// TestDW_3_2_GraphEntityGaugeTracksCount is the Phase-3 evidence: the graph
// entity gauge is registered on /metrics and its recorded value moves as the
// GraphSource's count changes across polls (the "increments as entities are
// added" requirement).
func TestDW_3_2_GraphEntityGaugeTracksCount(t *testing.T) {
	p, g := newTestProvider(t)
	rec := &Recorder{Gauges: g, Graph: fakeGraph{count: 3}}
	rec.Poll(context.Background())
	first := scrape(t, p)
	if !strings.Contains(first, "engram_graph_entity_count") {
		t.Fatalf("first scrape missing engram_graph_entity_count gauge:\n%s", first)
	}
	wantGauge(t, first, "engram_graph_entity_count", 3)

	rec.Graph = fakeGraph{count: 9}
	rec.Poll(context.Background())
	second := scrape(t, p)
	wantGauge(t, second, "engram_graph_entity_count", 9)
}

// TestRecorder_GateInventoryAndGraphNilSourcesSkipped proves a Recorder with
// GateInventory/Graph left nil (unconfigured) does not panic — mirroring
// TestRecorder_NilSourcesSkipped for the two Phase-3 sources.
func TestRecorder_GateInventoryAndGraphNilSourcesSkipped(t *testing.T) {
	_, g := newTestProvider(t)
	rec := &Recorder{Gauges: g, Gate: fakeGate{admit: 1}}
	rec.Poll(context.Background()) // must not panic with GateInventory/Graph nil
}

// TestDW_3_4_TelemetrySourceErrorDoesNotBlockOthers proves a failing
// GateInventory/Graph source (e.g. a transient OpenSearch read, or the
// genuine-error path DW-3.4 also covers at the backend layer) does not
// block the other gauges from recording.
func TestDW_3_4_TelemetrySourceErrorDoesNotBlockOthers(t *testing.T) {
	p, g := newTestProvider(t)
	rec := &Recorder{
		Gauges:        g,
		GateInventory: fakeGateInventory{err: context.DeadlineExceeded},
		Graph:         fakeGraph{err: context.DeadlineExceeded},
		DLQ:           fakeDLQ{count: 4},
	}
	rec.Poll(context.Background())
	wantGauge(t, scrape(t, p), "engram_dlq_depth", 4)
}

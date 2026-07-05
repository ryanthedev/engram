package gate_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/eval/gate"
)

func TestAppendAndLoadTrend_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	recs := []gate.Record{
		{Timestamp: time.Now().UTC(), Gate: "hallucination", Verdict: gate.Pass, Metric: 0.02, Detail: "ok", GitRev: "aaa111"},
		{Timestamp: time.Now().UTC(), Gate: "hallucination", Verdict: gate.Fail, Metric: 0.5, Detail: "bad", GitRev: "bbb222"},
		{Timestamp: time.Now().UTC(), Gate: "retrieval-regression", Verdict: gate.Pass, Metric: 0.9, Detail: "ok", GitRev: "aaa111"},
	}
	for _, r := range recs {
		if err := gate.AppendTrend(path, r); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	got, err := gate.LoadTrend(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != len(recs) {
		t.Fatalf("loaded %d records, want %d", len(got), len(recs))
	}
	for i, r := range got {
		if r.Gate != recs[i].Gate || r.Verdict != recs[i].Verdict || r.Metric != recs[i].Metric {
			t.Fatalf("record %d = %+v, want %+v", i, r, recs[i])
		}
	}
}

// TestLoadTrend_MissingFileIsEmptyHistory: a gate's very first run has no
// prior trend file — that must not be an error.
func TestLoadTrend_MissingFileIsEmptyHistory(t *testing.T) {
	recs, err := gate.LoadTrend(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil {
		t.Fatalf("missing trend file must not error: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("want empty history, got %v", recs)
	}
}

// TestLoadTrend_CorruptLineErrors is a dirty test: a corrupt trend log must
// be noticed (the flaky-quarantine logic depends on trustworthy history),
// not silently skipped.
func TestLoadTrend_CorruptLineErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	if err := os.WriteFile(path, []byte("{\"gate\":\"x\"}\nnot json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.LoadTrend(path); err == nil {
		t.Fatal("a corrupt trend line must error")
	}
}

func TestHistoryFor_FiltersByGate(t *testing.T) {
	recs := []gate.Record{
		{Gate: "a", Verdict: gate.Pass},
		{Gate: "b", Verdict: gate.Fail},
		{Gate: "a", Verdict: gate.Fail},
	}
	got := gate.HistoryFor(recs, "a")
	if len(got) != 2 || got[0] != gate.Pass || got[1] != gate.Fail {
		t.Fatalf("got %v, want [Pass Fail]", got)
	}
}

// TestRenderDashboard_ShowsTrendAcrossRuns is the DW-8.6 proof at the unit
// level: >=3 records for one gate must all appear as distinct rows — a
// dashboard that only shows the latest run is not a trend.
func TestRenderDashboard_ShowsTrendAcrossRuns(t *testing.T) {
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	recs := []gate.Record{
		{Timestamp: base, Gate: "hallucination", Verdict: gate.Pass, Metric: 0.01, Detail: "run 1", GitRev: "rev1"},
		{Timestamp: base.Add(time.Hour), Gate: "hallucination", Verdict: gate.Pass, Metric: 0.02, Detail: "run 2", GitRev: "rev2"},
		{Timestamp: base.Add(2 * time.Hour), Gate: "hallucination", Verdict: gate.Fail, Metric: 0.9, Detail: "run 3 — drill", GitRev: "rev3"},
	}
	out := gate.RenderDashboard(recs)
	for _, want := range []string{"hallucination", "rev1", "rev2", "rev3", "run 1", "run 2", "run 3", "3 run(s) recorded"} {
		if !strings.Contains(out, want) {
			t.Errorf("dashboard output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderDashboard_EmptyHistory(t *testing.T) {
	out := gate.RenderDashboard(nil)
	if !strings.Contains(out, "No gate runs recorded") {
		t.Fatalf("empty history must say so plainly, got:\n%s", out)
	}
}

// TestRenderDashboard_SeparatesGatesIntoOwnSections: multiple gates must not
// bleed into one table (each gate's trend is independent).
func TestRenderDashboard_SeparatesGatesIntoOwnSections(t *testing.T) {
	recs := []gate.Record{
		{Gate: "hallucination", Verdict: gate.Pass, Timestamp: time.Now()},
		{Gate: "retrieval-regression", Verdict: gate.Pass, Timestamp: time.Now()},
		{Gate: "experience-following", Verdict: gate.Pass, Timestamp: time.Now()},
	}
	out := gate.RenderDashboard(recs)
	for _, g := range []string{"hallucination", "retrieval-regression", "experience-following"} {
		if !strings.Contains(out, "## "+g) {
			t.Errorf("missing section heading for %q:\n%s", g, out)
		}
	}
}

func TestWriteDashboard_WritesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dashboard.md")
	if err := gate.WriteDashboard(path, []gate.Record{{Gate: "g", Verdict: gate.Pass}}); err != nil {
		t.Fatalf("write: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(b), "g") {
		t.Fatalf("written dashboard missing content: %s", b)
	}
}

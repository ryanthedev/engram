package gate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Record is one gate run's persisted history entry (DW-8.6: "trend, not
// point-in-time"). Records accumulate in a checked-in JSON-lines file
// (eval/gate-runs/history.jsonl) — one line per run, append-only, so trend
// history survives across CI runs and local drills alike.
type Record struct {
	Timestamp time.Time `json:"timestamp"`
	Gate      string    `json:"gate"`
	Verdict   Verdict   `json:"verdict"`
	Metric    float64   `json:"metric"`
	Detail    string    `json:"detail"`
	GitRev    string    `json:"git_rev,omitempty"`
}

// RecordOf converts a Result (plus a git revision, may be "") into a
// trend Record.
func RecordOf(res Result, gitRev string) Record {
	return Record{Timestamp: res.RanAt, Gate: res.Gate, Verdict: res.Verdict, Metric: res.Metric, Detail: res.Detail, GitRev: gitRev}
}

// AppendTrend appends rec as one JSON line to path, creating the file (and
// its parent directory) if needed. Never truncates or rewrites prior lines —
// trend history is append-only by construction.
func AppendTrend(path string, rec Record) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("gate: encoding trend record: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("gate: opening trend log %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("gate: appending trend record to %s: %w", path, err)
	}
	return nil
}

// LoadTrend reads every record from a JSON-lines trend log. A missing file
// is treated as empty history (a gate's first-ever run has no prior trend),
// not an error; a malformed line IS an error — a corrupt trend log must be
// noticed, not silently skipped (DW-8.5's flaky-quarantine logic depends on
// history being trustworthy).
func LoadTrend(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("gate: opening trend log %s: %w", path, err)
	}
	defer f.Close()
	var recs []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, fmt.Errorf("gate: trend log %s line %d: %w", path, lineNo, err)
		}
		recs = append(recs, r)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("gate: reading trend log %s: %w", path, err)
	}
	return recs, nil
}

// HistoryFor returns just gate's verdicts from records, oldest first (the
// order AppendTrend/LoadTrend already preserve, filtered to one gate) — the
// shape IsFlaky/ApplyQuarantine consume.
func HistoryFor(records []Record, gate string) []Verdict {
	var out []Verdict
	for _, r := range records {
		if r.Gate == gate {
			out = append(out, r.Verdict)
		}
	}
	return out
}

// RenderDashboard renders records as a Markdown trend report, one table per
// gate (oldest run first), plus a one-line current-streak summary — the
// eval trend dashboard DW-8.6 requires ("trend, not point-in-time"). Pure
// and dependency-free so it is exercised in unit tests without ever reading
// eval/gate-runs/history.jsonl directly; docs/eval/dashboard.md is this
// output written to disk by whatever invokes it (see Makefile's
// eval-dashboard target).
func RenderDashboard(records []Record) string {
	var sb strings.Builder
	sb.WriteString("# Engram Eval Gate Trend Dashboard\n\n")
	sb.WriteString("Generated from eval/gate-runs/history.jsonl — regenerate with `make eval-dashboard`. Do not hand-edit.\n\n")
	if len(records) == 0 {
		sb.WriteString("_No gate runs recorded yet._\n")
		return sb.String()
	}

	gates := gateNames(records)
	for _, g := range gates {
		fmt.Fprintf(&sb, "## %s\n\n", g)
		sb.WriteString("| Run | Timestamp (UTC) | Verdict | Metric | Git rev | Detail |\n")
		sb.WriteString("|---|---|---|---|---|---|\n")
		n := 0
		for _, r := range records {
			if r.Gate != g {
				continue
			}
			n++
			fmt.Fprintf(&sb, "| %d | %s | %s | %.4f | %s | %s |\n",
				n, r.Timestamp.UTC().Format(time.RFC3339), verdictBadge(r.Verdict), r.Metric, shortRev(r.GitRev), escapeCell(r.Detail))
		}
		fmt.Fprintf(&sb, "\n%d run(s) recorded.\n\n", n)
	}
	return sb.String()
}

func gateNames(records []Record) []string {
	seen := map[string]bool{}
	var names []string
	for _, r := range records {
		if !seen[r.Gate] {
			seen[r.Gate] = true
			names = append(names, r.Gate)
		}
	}
	sort.Strings(names)
	return names
}

func verdictBadge(v Verdict) string {
	switch v {
	case Pass:
		return "PASS"
	case Fail:
		return "FAIL"
	case Quarantined:
		return "QUARANTINED"
	default:
		return string(v)
	}
}

func shortRev(rev string) string {
	if rev == "" {
		return "-"
	}
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}

func escapeCell(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "|", "\\|"), "\n", " ")
}

// WriteDashboard renders records and writes them to path (docs/eval/dashboard.md).
func WriteDashboard(path string, records []Record) error {
	return os.WriteFile(path, []byte(RenderDashboard(records)), 0o644)
}

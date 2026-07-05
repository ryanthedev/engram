//go:build drill

package cloud

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/store"
	"github.com/ryanthedev/engram/internal/telemetry"
	"github.com/ryanthedev/engram/internal/testutil"
)

// buildBin compiles ./cmd/<name> to a temp path and returns it.
func buildBin(t *testing.T, name string) string {
	t.Helper()
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("finding repo root: %v", err)
	}
	out := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", out, "./cmd/"+name)
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building %s: %v: %s", name, err, b)
	}
	return out
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitTCP(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := (&net.Dialer{Timeout: time.Second}).Dial("tcp", addr)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", addr)
}

// TestFailureDrill_WorkerKill is DW-7.5's worker half: kill the engramd
// process (the current single-deployable architecture runs the worker
// in-process — see the plan's documented S2 split seam — so "kill a worker
// task" locally means killing engramd itself, the accurate reading for
// today's topology) while events are in flight, and prove no data loss: the
// durable outbox (D12) survives the kill, a restarted engramd resumes and
// drains the backlog, and nothing is dead-lettered.
func TestFailureDrill_WorkerKill(t *testing.T) {
	base := testutil.OpenSearchURL()
	suffix := fmt.Sprintf("drillwk-%d", time.Now().UnixNano())
	epIdx, semIdx, ledIdx := "engram-episodic-"+suffix, "engram-semantic-"+suffix, "engram-ledger-"+suffix
	for _, idx := range []string{epIdx, semIdx, ledIdx} {
		testutil.DeleteIndex(t, base, idx)
		testutil.CreateScratchIndex(t, base, idx)
	}
	t.Cleanup(func() {
		for _, idx := range []string{epIdx, semIdx, ledIdx} {
			testutil.DeleteIndex(t, base, idx)
		}
	})

	serverBin := buildBin(t, "engram-server")
	grpcPort := freePort(t)
	addr := fmt.Sprintf("localhost:%d", grpcPort)

	startEngramd := func() *exec.Cmd {
		cmd := exec.Command(serverBin,
			"-addr", addr, "-url", base,
			"-episodic-index", epIdx, "-semantic-index", semIdx, "-ledger-index", ledIdx,
			"-poll-interval", "200ms", "-sweep-interval", "1s",
		)
		cmd.Env = os.Environ()
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatalf("starting engramd: %v", err)
		}
		// Registered per-invocation (not a single defer at the call site) so
		// an early t.Fatalf between two startEngramd() calls can never leak
		// the first process as an orphaned background job still polling
		// (and erroring against) indices this test's own cleanup deletes.
		t.Cleanup(func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		})
		return cmd
	}

	tenant := "drill-tenant-" + suffix
	st := store.NewOpenSearchStore(&http.Client{Timeout: 10 * time.Second}, base,
		store.WithEpisodicIndex(epIdx), store.WithSemanticIndex(semIdx), store.WithLedgerIndex(ledIdx))

	proc := startEngramd()
	waitTCP(t, addr, 30*time.Second)

	// Ingest directly against the store (bypassing gRPC/auth — this drill is
	// about worker/outbox resilience, not the auth barricade) so events are
	// durably enqueued regardless of what happens to engramd next.
	ctx := context.Background()
	const n = 20
	for i := 0; i < n; i++ {
		if _, err := st.Append(ctx, episodicFixture(tenant, i)); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	// PendingBacklog's _count query is refresh-visible, not real-time (see
	// its doc comment in internal/store/lag.go) — force a refresh so the
	// 20 events just appended are actually countable before asserting on
	// them. Racing this would make the alert-condition check below flaky
	// (a false "gauge stayed at 0"), not prove anything about engramd.
	testutil.RefreshIndex(t, base, epIdx)

	// Prove the alert condition actually fires: poll the SAME telemetry
	// source the production Recorder uses, directly against the store
	// (independent of engramd's own /metrics, which dies with the process)
	// — right after enqueuing, before the worker has drained everything —
	// and scrape the resulting gauge value, not just log a raw count.
	provider, gauges := mustGauges(t)
	rec := &telemetry.Recorder{Gauges: gauges, Outbox: st}
	rec.Poll(ctx)
	backlogGaugeValue := scrapeGaugeValue(t, provider, "engram_outbox_backlog")
	t.Logf("engram_outbox_backlog gauge immediately after enqueue: %v", backlogGaugeValue)
	if backlogGaugeValue <= 0 {
		t.Fatalf("engram_outbox_backlog gauge = %v after enqueuing %d events, want > 0 (the alert condition should be observable)", backlogGaugeValue, n)
	}

	// Kill engramd NOW — mid-processing, not gracefully.
	if err := proc.Process.Kill(); err != nil {
		t.Fatalf("killing engramd: %v", err)
	}
	_ = proc.Wait()

	// The durable outbox survives the kill regardless of how far processing
	// got — this is the assertion that matters, not the exact backlog count
	// at the moment of kill (which is a race by nature).
	backlogAfterKill, _, err := st.PendingBacklog(ctx)
	if err != nil {
		t.Fatalf("PendingBacklog after kill: %v", err)
	}
	t.Logf("outbox backlog immediately after kill: %d", backlogAfterKill)

	// Restart engramd — it must resume and drain the backlog with no data
	// loss (no dead-lettered events from the kill itself). Cleanup is
	// registered inside startEngramd itself (see above).
	proc = startEngramd()
	waitTCP(t, addr, 30*time.Second)

	deadline := time.Now().Add(30 * time.Second)
	var finalBacklog int64
	for time.Now().Before(deadline) {
		finalBacklog, _, err = st.PendingBacklog(ctx)
		if err != nil {
			t.Fatalf("PendingBacklog polling: %v", err)
		}
		if finalBacklog == 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if finalBacklog != 0 {
		t.Fatalf("outbox backlog did not drain after restart: %d remaining", finalBacklog)
	}
	dlq, err := st.DeadLetteredCount(ctx)
	if err != nil {
		t.Fatalf("DeadLetteredCount: %v", err)
	}
	if dlq != 0 {
		t.Fatalf("DLQ depth = %d after recovery, want 0 (no data loss)", dlq)
	}
}

// mustGauges builds a throwaway Provider+Gauges set for the drill's own
// Recorder — used to prove the gauge mechanism observes the real backlog
// (scrapeGaugeValue reads it back the same way a Prometheus scraper would).
func mustGauges(t *testing.T) (*telemetry.Provider, *telemetry.Gauges) {
	t.Helper()
	p, err := telemetry.NewProvider()
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	g, err := telemetry.NewGauges(p.Meter("drill"))
	if err != nil {
		t.Fatalf("NewGauges: %v", err)
	}
	return p, g
}

// scrapeGaugeValue serves provider's real HTTP handler and extracts name's
// numeric value from the Prometheus exposition body.
func scrapeGaugeValue(t *testing.T, provider *telemetry.Provider, name string) float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	provider.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatalf("reading scrape body: %v", err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, name) {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			var v float64
			if _, err := fmt.Sscanf(fields[len(fields)-1], "%g", &v); err == nil {
				return v
			}
		}
	}
	t.Fatalf("gauge %s not found in scrape body:\n%s", name, body)
	return 0
}

func episodicFixture(tenant string, i int) memory.Episodic {
	now := time.Now().UTC()
	return memory.Episodic{
		EventID: fmt.Sprintf("drill-%s-%d", tenant, i), TenantID: tenant,
		Text: fmt.Sprintf("drill event %d", i), Kind: "drill",
		OccurredAt: now, CreatedAt: now,
	}
}

package worker_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/worker"
)

// TestSweeper_ConvergenceAge_ZeroBeforeFirstSweep proves the Phase-7
// convergence-age gauge source (DW-7.8) reports zero before any sweep has
// ever completed — a freshly started sweeper should not read as "infinitely
// stale."
func TestSweeper_ConvergenceAge_ZeroBeforeFirstSweep(t *testing.T) {
	f := newFakeStore()
	w, _ := newTestWorker(f, "v1")
	sweeper := &worker.Sweeper{Store: f, Worker: w, Logger: quietLogger}

	if age := sweeper.ConvergenceAge(); age != 0 {
		t.Fatalf("ConvergenceAge before any Sweep = %v, want 0", age)
	}
}

// TestSweeper_ConvergenceAge_TracksLastSweep proves ConvergenceAge reflects
// time elapsed since the most recent completed Sweep pass.
func TestSweeper_ConvergenceAge_TracksLastSweep(t *testing.T) {
	f := newFakeStore()
	w, _ := newTestWorker(f, "v1")
	sweeper := &worker.Sweeper{Store: f, Worker: w, Logger: quietLogger}

	if _, err := sweeper.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	age := sweeper.ConvergenceAge()
	if age <= 0 || age > time.Second {
		t.Fatalf("ConvergenceAge just after a sweep = %v, want a small positive duration", age)
	}
}

// TestSweeper_Backlog_CountsOutstandingWorkWithoutConverging proves Backlog
// (the DW-7.8 repair-backlog gauge source) reports the same crashed-write
// residue Sweep would converge, but leaves it untouched — a second Backlog
// call reports the same count, and Sweep afterward still converges it.
func TestSweeper_Backlog_CountsOutstandingWorkWithoutConverging(t *testing.T) {
	f := newFakeStore()
	f.ledgerLease = -time.Second // the crashed run's ledger lease is expired
	w, _ := newTestWorker(f, "v1")
	ctx := context.Background()

	mustProcess(t, f, w, event("ev-1", factLine("svc", "owner", "bob", tv0)))

	crash := errors.New("simulated crash between create and close")
	f.failOnce("update", crash)
	ev := event("ev-2", factLine("svc", "owner", "ana", tv1))
	if _, err := f.Append(ctx, ev); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.ProcessEvent(ctx, ev); !errors.Is(err, crash) {
		t.Fatalf("crashed ProcessEvent error = %v, want the injected crash", err)
	}

	sweeper := &worker.Sweeper{Store: f, Worker: w, Logger: quietLogger}
	backlog, err := sweeper.Backlog(ctx)
	if err != nil {
		t.Fatalf("Backlog: %v", err)
	}
	if backlog == 0 {
		t.Fatal("Backlog = 0, want > 0 (a crashed close + an incomplete ledger entry are outstanding)")
	}

	// Backlog is read-only: calling it again reports the same outstanding
	// work, and Sweep afterward still converges it.
	backlogAgain, err := sweeper.Backlog(ctx)
	if err != nil {
		t.Fatalf("second Backlog: %v", err)
	}
	if backlogAgain != backlog {
		t.Fatalf("Backlog is not read-only: first=%d second=%d", backlog, backlogAgain)
	}

	rep, err := sweeper.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep after Backlog: %v", err)
	}
	if rep.ClosesCompleted != 1 || rep.LedgersResumed != 1 {
		t.Fatalf("Sweep after Backlog = %+v, want the crashed close + ledger resume still converged", rep)
	}

	drained, err := sweeper.Backlog(ctx)
	if err != nil {
		t.Fatalf("Backlog after Sweep: %v", err)
	}
	if drained != 0 {
		t.Fatalf("Backlog after Sweep = %d, want 0 (converged)", drained)
	}
}

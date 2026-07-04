package worker_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/ryanthedev/engram/internal/memory"
)

// recordingStage captures what the D20 seam hands each registered stage.
type recordingStage struct {
	calls    atomic.Int64
	lastEv   string
	lastN    int
	failNext bool
}

func (s *recordingStage) Process(_ context.Context, ev memory.Episodic, facts []memory.SemanticFact) error {
	s.calls.Add(1)
	s.lastEv = ev.EventID
	s.lastN = len(facts)
	if s.failNext {
		s.failNext = false
		return errors.New("stage boom")
	}
	return nil
}

// TestDW_3_stageSeamRunsAfterFactsLand: a registered stage receives the event
// and its landed facts exactly once per successful event (D20).
func TestStageSeamRunsAfterFactsLand(t *testing.T) {
	f := newFakeStore()
	w, _ := newTestWorker(f, "v1")
	st := &recordingStage{}
	w.RegisterStage("test-stage", st)

	mustProcess(t, f, w, event("ev-1", factLine("svc", "owner", "ana", tv1)))

	if st.calls.Load() != 1 {
		t.Fatalf("stage calls = %d, want 1", st.calls.Load())
	}
	if st.lastEv != "ev-1" {
		t.Errorf("stage saw event %q, want ev-1", st.lastEv)
	}
	if st.lastN != 1 {
		t.Errorf("stage saw %d facts, want 1", st.lastN)
	}
}

// TestStageErrorRequeuesEvent: a stage error fails the event (the outbox
// retries) and the ledger is NOT marked complete — proving at-least-once
// semantics and that stages gate completion.
func TestStageErrorRequeuesEvent(t *testing.T) {
	f := newFakeStore()
	w, _ := newTestWorker(f, "v1")
	st := &recordingStage{failNext: true}
	w.RegisterStage("flaky-stage", st)
	ctx := context.Background()

	ev := event("ev-1", factLine("svc", "owner", "ana", tv1))
	if _, err := f.Append(ctx, ev); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.ProcessEvent(ctx, ev); err == nil {
		t.Fatal("ProcessEvent succeeded despite a stage error")
	}
	if rec, _ := f.eventRow("ev-1"); rec.ProcessedAt != nil {
		t.Fatal("event marked processed despite the stage failure")
	}

	// Retry: the stage now succeeds; the event completes and the fact is
	// intact (reconciliation is idempotent via content addressing).
	if err := w.ProcessEvent(ctx, ev); err != nil {
		t.Fatalf("retry ProcessEvent: %v", err)
	}
	if rec, _ := f.eventRow("ev-1"); rec.ProcessedAt == nil {
		t.Fatal("event not completed on retry")
	}
	singleLiveHead(t, f, "svc", "owner")
}

// TestNoStagesUnchanged: with no stage registered the pipeline behaves exactly
// as before (anchoring — the existing worker tests must be unaffected).
func TestNoStagesUnchanged(t *testing.T) {
	f := newFakeStore()
	w, _ := newTestWorker(f, "v1")
	mustProcess(t, f, w, event("ev-1", factLine("svc", "owner", "ana", tv1)))
	if _, head := singleLiveHead(t, f, "svc", "owner"); head.Object != "ana" {
		t.Fatalf("head = %+v, want ana", head)
	}
}

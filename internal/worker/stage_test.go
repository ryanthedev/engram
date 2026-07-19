package worker_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/ingest"
	"github.com/ryanthedev/engram/internal/memory"
)

// recordingStage captures what the D20 seam hands each registered stage: the
// event and one ingest.FactOutcome per extracted fact (DW-1.1).
type recordingStage struct {
	calls    atomic.Int64
	lastEv   string
	lastN    int
	last     []ingest.FactOutcome
	failNext bool
}

func (s *recordingStage) Process(_ context.Context, ev memory.Episodic, outcomes []ingest.FactOutcome) error {
	s.calls.Add(1)
	s.lastEv = ev.EventID
	s.lastN = len(outcomes)
	s.last = outcomes
	if s.failNext {
		s.failNext = false
		return errors.New("stage boom")
	}
	return nil
}

// only returns the stage's single recorded outcome, failing if the last call
// did not carry exactly one.
func (s *recordingStage) only(t *testing.T) ingest.FactOutcome {
	t.Helper()
	if len(s.last) != 1 {
		t.Fatalf("stage saw %d outcomes, want exactly 1: %+v", len(s.last), s.last)
	}
	return s.last[0]
}

func retractLine(s, p string, at time.Time) string {
	return fmt.Sprintf("retract: %s | %s @ %s", s, p, at.Format(time.RFC3339))
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
		t.Errorf("stage saw %d outcomes, want 1", st.lastN)
	}
}

// TestDW_1_1_StageSeamCarriesTheLandedFact: the seam hands stages the fact AS
// LANDED inside a FactOutcome — not the pre-reconciliation candidate — so a
// derived projection sees exactly what the semantic tier stored. The old
// []memory.SemanticFact signature is gone (compile-enforced by the
// var _ worker.Stage assertions in internal/graph and internal/experience).
func TestDW_1_1_StageSeamCarriesTheLandedFact(t *testing.T) {
	f := newFakeStore()
	w, _ := newTestWorker(f, "v1")
	st := &recordingStage{}
	w.RegisterStage("test-stage", st)

	mustProcess(t, f, w, event("ev-1", factLine("svc", "owner", "ana", tv1)))
	mustProcess(t, f, w, event("ev-2", factLine("svc", "owner", "bob", tv2)))

	got := st.only(t)
	if got.Fact.Object != "bob" || !got.Fact.ValidAt.Equal(tv2) {
		t.Fatalf("outcome fact = %+v, want bob @ tv2", got.Fact)
	}
	// "As landed": the supersedes link the writer stamped is visible to the
	// stage, which the pre-reconciliation slice never carried.
	oldID := memory.FactDocID(got.Predecessor.ContentKey, got.Predecessor.ValidAt)
	if got.Fact.Supersedes != oldID {
		t.Errorf("outcome fact supersedes = %q, want the predecessor doc id %q", got.Fact.Supersedes, oldID)
	}
}

// TestDW_1_2_OutcomeDecisionPerOpKind: Decision is the reconciler's actual
// Op.Kind for every fact, and Predecessor is non-nil for EXACTLY the UPDATE
// and INVALIDATE cases.
func TestDW_1_2_OutcomeDecisionPerOpKind(t *testing.T) {
	tests := []struct {
		name        string
		prior       string // event processed first (empty = none)
		final       string // the event whose outcome is asserted
		wantKind    ingest.OpKind
		wantPredObj string // "" = Predecessor must be nil
	}{
		{
			name:     "add: nothing to supersede",
			final:    factLine("svc", "owner", "ana", tv1),
			wantKind: ingest.OpAdd,
		},
		{
			name:     "noop: the assertion is already current",
			prior:    factLine("svc", "owner", "ana", tv1),
			final:    factLine("svc", "owner", "ana", tv1),
			wantKind: ingest.OpNoop,
		},
		{
			name:        "update: a new object supersedes the live head",
			prior:       factLine("svc", "owner", "ana", tv1),
			final:       factLine("svc", "owner", "bob", tv2),
			wantKind:    ingest.OpUpdate,
			wantPredObj: "ana",
		},
		{
			name:        "invalidate: a retraction closes the live head",
			prior:       factLine("svc", "owner", "ana", tv1),
			final:       retractLine("svc", "owner", tv2),
			wantKind:    ingest.OpInvalidate,
			wantPredObj: "ana",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeStore()
			w, _ := newTestWorker(f, "v1")
			st := &recordingStage{}
			w.RegisterStage("test-stage", st)

			if tt.prior != "" {
				mustProcess(t, f, w, event("ev-1", tt.prior))
			}
			mustProcess(t, f, w, event("ev-2", tt.final))

			got := st.only(t)
			if got.Decision != tt.wantKind {
				t.Errorf("Decision = %q, want %q", got.Decision, tt.wantKind)
			}
			switch {
			case tt.wantPredObj == "" && got.Predecessor != nil:
				t.Errorf("Predecessor = %+v, want nil for %s", *got.Predecessor, tt.wantKind)
			case tt.wantPredObj != "" && got.Predecessor == nil:
				t.Fatalf("Predecessor = nil, want the superseded fact (%s)", tt.wantPredObj)
			case tt.wantPredObj != "":
				if got.Predecessor.Object != tt.wantPredObj {
					t.Errorf("Predecessor.Object = %q, want %q", got.Predecessor.Object, tt.wantPredObj)
				}
				if got.Predecessor.Subject != "svc" || got.Predecessor.Predicate != "owner" {
					t.Errorf("Predecessor = %+v, want the svc/owner head", *got.Predecessor)
				}
			}
		})
	}
}

// TestDW_1_4_ResumedActionYieldsReplayedOutcome: an action whose doc id is
// already in the ledger's CompletedActions is NOT re-reconciled, so its
// original decision is gone. The seam must still hand the stage a well-defined
// outcome — OpReplayed, never a zero value — so a projection can tell "already
// accounted for" from "newly added".
func TestDW_1_4_ResumedActionYieldsReplayedOutcome(t *testing.T) {
	f := newFakeStore()
	w, _ := newTestWorker(f, "v1")
	st := &recordingStage{failNext: true} // fail AFTER the fact lands
	w.RegisterStage("resume-stage", st)
	ctx := context.Background()

	ev := event("ev-1", factLine("svc", "owner", "ana", tv1))
	if _, err := f.Append(ctx, ev); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// Attempt 1: the fact lands and is recorded in CompletedActions, then the
	// stage fails and re-queues the event.
	if err := w.ProcessEvent(ctx, ev); err == nil {
		t.Fatal("ProcessEvent succeeded despite a stage error")
	}
	if first := st.only(t); first.Decision != ingest.OpAdd {
		t.Fatalf("first attempt Decision = %q, want %q", first.Decision, ingest.OpAdd)
	}

	// Attempt 2: the action is resumed, not re-reconciled.
	if err := w.ProcessEvent(ctx, ev); err != nil {
		t.Fatalf("retry ProcessEvent: %v", err)
	}
	got := st.only(t)
	if got.Decision == "" {
		t.Fatal("resumed action produced a ZERO-VALUE outcome; want an explicit decision")
	}
	if got.Decision != ingest.OpReplayed {
		t.Errorf("Decision = %q, want %q", got.Decision, ingest.OpReplayed)
	}
	if got.Predecessor != nil {
		t.Errorf("Predecessor = %+v, want nil (a resumed action knows of none)", *got.Predecessor)
	}
	if got.Fact.Subject != "svc" || got.Fact.Object != "ana" {
		t.Errorf("outcome fact = %+v, want the resumed svc/owner/ana fact", got.Fact)
	}
}

// TestOutcomeParityWithExtractedFacts: one outcome per extracted fact, in the
// writer's deterministic (valid_at, content_key) order — including NOOPs, so a
// stage never has to guess which fact an outcome belongs to. Also pins the
// in-batch contradiction rule: two facts for the same subject+predicate in ONE
// event resolve to add-then-update, with the earlier fact as the predecessor.
func TestOutcomeParityWithExtractedFacts(t *testing.T) {
	f := newFakeStore()
	w, _ := newTestWorker(f, "v1")
	st := &recordingStage{}
	w.RegisterStage("test-stage", st)

	// Emitted newest-first; the writer sorts them oldest-first before landing.
	mustProcess(t, f, w, event("ev-1",
		factLine("svc", "owner", "bob", tv2)+"\n"+factLine("svc", "owner", "ana", tv1)))

	if len(st.last) != 2 {
		t.Fatalf("outcomes = %d, want 2 (one per extracted fact)", len(st.last))
	}
	first, second := st.last[0], st.last[1]
	if first.Fact.Object != "ana" || !first.Fact.ValidAt.Equal(tv1) {
		t.Fatalf("outcome[0] = %+v, want ana @ tv1 (sorted by valid_at)", first.Fact)
	}
	if first.Decision != ingest.OpAdd || first.Predecessor != nil {
		t.Errorf("outcome[0] = %q/%v, want add with no predecessor", first.Decision, first.Predecessor)
	}
	if second.Decision != ingest.OpUpdate {
		t.Fatalf("outcome[1] Decision = %q, want update (bob supersedes ana in-batch)", second.Decision)
	}
	if second.Predecessor == nil || second.Predecessor.Object != "ana" {
		t.Fatalf("outcome[1] Predecessor = %+v, want the in-batch ana fact", second.Predecessor)
	}
}

// TestNoopOutcomeReportsTheDiscardedFact: a NOOP wrote nothing, but it is
// still a well-defined outcome carrying the discarded candidate and no
// predecessor — a projection must be able to skip it, not mistake it for an
// add.
func TestNoopOutcomeReportsTheDiscardedFact(t *testing.T) {
	f := newFakeStore()
	w, _ := newTestWorker(f, "v1")
	st := &recordingStage{}
	w.RegisterStage("test-stage", st)

	mustProcess(t, f, w, event("ev-1", factLine("svc", "owner", "ana", tv1)))
	mustProcess(t, f, w, event("ev-2", factLine("svc", "owner", "ana", tv1)))

	got := st.only(t)
	if got.Decision != ingest.OpNoop {
		t.Fatalf("Decision = %q, want noop", got.Decision)
	}
	if got.Fact.Object != "ana" {
		t.Errorf("outcome fact = %+v, want the discarded ana candidate", got.Fact)
	}
	if got.Fact.Supersedes != "" {
		t.Errorf("NOOP outcome fact supersedes %q; nothing was written", got.Fact.Supersedes)
	}
}

// TestLateArrivalOutcomeIsDistinguishable: a superseding fact that arrives
// OLDER than the fact it supersedes (D10 step 4) reports UPDATE with a
// predecessor — but the predecessor was deliberately NOT closed (it stays the
// live head). A projection that acts on supersession must be able to tell this
// apart from a real one, and can: Fact.ValidAt < Predecessor.ValidAt.
func TestLateArrivalOutcomeIsDistinguishable(t *testing.T) {
	f := newFakeStore()
	w, _ := newTestWorker(f, "v1")
	st := &recordingStage{}
	w.RegisterStage("test-stage", st)

	mustProcess(t, f, w, event("ev-1", factLine("svc", "owner", "bob", tv2)))
	mustProcess(t, f, w, event("ev-2", factLine("svc", "owner", "ana", tv1))) // late arrival

	got := st.only(t)
	if got.Decision != ingest.OpUpdate {
		t.Fatalf("Decision = %q, want update", got.Decision)
	}
	if got.Predecessor == nil {
		t.Fatal("Predecessor = nil, want the live head it decided against")
	}
	if !got.Fact.ValidAt.Before(got.Predecessor.ValidAt) {
		t.Fatalf("late arrival not recognizable: fact @%v is not before predecessor @%v",
			got.Fact.ValidAt, got.Predecessor.ValidAt)
	}
	// The predecessor really was left live — the outcome must not be read as a
	// completed supersession.
	if _, head := singleLiveHead(t, f, "svc", "owner"); head.Object != "bob" {
		t.Fatalf("live head = %+v, want bob (the late arrival must not close it)", head)
	}
	// The late arrival landed bounded, not superseding.
	if got.Fact.Supersedes != "" {
		t.Errorf("late arrival supersedes = %q, want empty (it is bounded at index time)", got.Fact.Supersedes)
	}
	if got.Fact.InvalidAt == nil || !got.Fact.InvalidAt.Equal(tv2) {
		t.Errorf("late arrival invalid_at = %v, want tv2 (bounded by its successor)", got.Fact.InvalidAt)
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

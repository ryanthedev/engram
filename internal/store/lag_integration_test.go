//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/store"
	"github.com/ryanthedev/engram/internal/testutil"
)

// TestDW_7_8_PendingBacklogAndDeadLetteredCount proves the Phase-7 telemetry
// data sources against the real cluster: PendingBacklog counts unprocessed
// events and reports the oldest one's age, dead-lettering removes an event
// from the backlog, and DeadLetteredCount reflects the DLQ depth.
func TestDW_7_8_PendingBacklogAndDeadLetteredCount(t *testing.T) {
	s, base, epIdx := liveOutboxStore(t, t.Name(), store.DefaultLedgerLease)
	ctx := context.Background()

	// An empty outbox reports zero count and zero age.
	count, age, err := s.PendingBacklog(ctx)
	if err != nil {
		t.Fatalf("PendingBacklog (empty): %v", err)
	}
	if count != 0 || age != 0 {
		t.Fatalf("empty backlog = (%d, %v), want (0, 0)", count, age)
	}
	if dlq, err := s.DeadLetteredCount(ctx); err != nil || dlq != 0 {
		t.Fatalf("empty DLQ = (%d, %v), want (0, nil)", dlq, err)
	}

	oldest := time.Now().UTC().Add(-90 * time.Second)
	for i, id := range []string{"lag-a", "lag-b", "lag-c"} {
		_, err := s.Append(ctx, memory.Episodic{
			EventID: id, TenantID: "t1", Text: "backlog " + id,
			CreatedAt: oldest.Add(time.Duration(i) * time.Second),
		})
		if err != nil {
			t.Fatalf("Append %s: %v", id, err)
		}
	}
	testutil.RefreshIndex(t, base, epIdx)

	count, age, err = s.PendingBacklog(ctx)
	if err != nil {
		t.Fatalf("PendingBacklog: %v", err)
	}
	if count != 3 {
		t.Fatalf("backlog count = %d, want 3", count)
	}
	if age < 90*time.Second {
		t.Errorf("oldest age = %v, want >= 90s (oldest event was backdated 90s)", age)
	}

	// Completing one event drops the backlog count but leaves the DLQ
	// untouched; dead-lettering another moves it into the DLQ instead.
	if err := s.Complete(ctx, "lag-a"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := s.DeadLetter(ctx, "lag-b", "backlog fixture"); err != nil {
		t.Fatalf("DeadLetter: %v", err)
	}
	testutil.RefreshIndex(t, base, epIdx)

	count, _, err = s.PendingBacklog(ctx)
	if err != nil {
		t.Fatalf("PendingBacklog after drain: %v", err)
	}
	if count != 1 {
		t.Fatalf("backlog after drain = %d, want 1 (only lag-c pending)", count)
	}
	dlq, err := s.DeadLetteredCount(ctx)
	if err != nil {
		t.Fatalf("DeadLetteredCount: %v", err)
	}
	if dlq != 1 {
		t.Fatalf("DLQ depth = %d, want 1", dlq)
	}
}

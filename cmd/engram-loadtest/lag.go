package main

import (
	"context"
	"sync"
	"time"

	"github.com/ryanthedev/engram/internal/store"
	"github.com/ryanthedev/engram/internal/worker"
)

// lagSampler periodically polls the same worker/outbox-lag and repair-
// backlog sources the Phase-7 telemetry Recorder polls in production
// (internal/telemetry), tracking the maximum observed values across a load
// test run — the DW-7.2 "report worker lag" evidence.
type lagSampler struct {
	store   *store.OpenSearchStore
	sweeper *worker.Sweeper

	mu                sync.Mutex
	maxBacklogSeen    int64
	maxLagSecondsSeen float64
	maxRepairSeen     int
}

func (l *lagSampler) run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.poll(ctx)
		}
	}
}

func (l *lagSampler) poll(ctx context.Context) {
	count, age, err := l.store.PendingBacklog(ctx)
	if err == nil {
		l.mu.Lock()
		if count > l.maxBacklogSeen {
			l.maxBacklogSeen = count
		}
		if age.Seconds() > l.maxLagSecondsSeen {
			l.maxLagSecondsSeen = age.Seconds()
		}
		l.mu.Unlock()
	}
	if backlog, err := l.sweeper.Backlog(ctx); err == nil {
		l.mu.Lock()
		if backlog > l.maxRepairSeen {
			l.maxRepairSeen = backlog
		}
		l.mu.Unlock()
	}
}

func (l *lagSampler) maxBacklog() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.maxBacklogSeen
}

func (l *lagSampler) maxLagSeconds() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.maxLagSecondsSeen
}

func (l *lagSampler) maxRepairBacklog() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.maxRepairSeen
}

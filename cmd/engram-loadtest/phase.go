package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ryanthedev/engram/api/engrampb"
)

// endpointStats is one endpoint's (Ingest or Search) latency/error summary
// for one phase.
type endpointStats struct {
	Total    int64   `json:"total"`
	Errors   int64   `json:"errors"`
	ErrorPct float64 `json:"error_rate"`
	P50Ms    float64 `json:"p50_ms"`
	P95Ms    float64 `json:"p95_ms"`
	P99Ms    float64 `json:"p99_ms"`
}

// phaseReport is one phase's (sustained or burst) full result.
type phaseReport struct {
	DurationSeconds float64       `json:"duration_seconds"`
	Ingest          endpointStats `json:"ingest"`
	Search          endpointStats `json:"search"`
}

// runPhase drives ratePerSec Ingest calls and searchClients concurrent
// Search clients against client for duration, recording per-endpoint
// latencies. It returns once duration elapses and every in-flight call has
// been accounted for.
func runPhase(ctx context.Context, client engrampb.EngramClient, ratePerSec float64, duration time.Duration, searchClients int) phaseReport {
	phaseCtx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()

	ingest := &latencyRecorder{}
	search := &latencyRecorder{}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		driveIngest(phaseCtx, client, ratePerSec, ingest)
	}()

	for c := 0; c < searchClients; c++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			driveSearch(phaseCtx, client, seed, search)
		}(int64(c) + 1)
	}
	wg.Wait()

	return phaseReport{
		DurationSeconds: duration.Seconds(),
		Ingest:          ingest.summarize(),
		Search:          search.summarize(),
	}
}

// driveIngest issues one Ingest call per tick at ratePerSec until ctx is
// done. A rate <= 0 issues nothing (a valid configuration for a search-only
// phase, though the load test always configures a positive rate).
func driveIngest(ctx context.Context, client engrampb.EngramClient, ratePerSec float64, rec *latencyRecorder) {
	if ratePerSec <= 0 {
		return
	}
	interval := time.Duration(float64(time.Second) / ratePerSec)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	r := rand.New(rand.NewSource(1))
	i := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			i++
			start := time.Now()
			_, err := client.Ingest(ctx, &engrampb.IngestRequest{
				EventId:      fmt.Sprintf("load-%d-%d", start.UnixNano(), i),
				TenantId:     fmt.Sprintf("tenant-%d", i%10),
				OwnerAgentId: fmt.Sprintf("agent-%d", i%50),
				Kind:         "load",
				Text:         syntheticText(r, i),
			})
			if isPhaseBoundaryArtifact(ctx, err) {
				continue // the phase ended mid-call — not a service failure
			}
			rec.record(time.Since(start), err)
		}
	}
}

// isPhaseBoundaryArtifact reports whether err is just the phase's own
// context deadline firing mid-call (the last call of a phase racing the
// timeout) rather than a genuine service failure — such calls are excluded
// from the error rate so the SLO evidence isn't polluted by the test
// harness's own timing.
func isPhaseBoundaryArtifact(ctx context.Context, err error) bool {
	return err != nil && ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded)
}

// driveSearch issues Search calls back-to-back (no client-side pacing —
// this is the concurrency-bound latency measurement cmd/engram-perf also
// uses) until ctx is done.
func driveSearch(ctx context.Context, client engrampb.EngramClient, seed int64, rec *latencyRecorder) {
	r := rand.New(rand.NewSource(seed))
	for {
		select {
		case <-ctx.Done():
			return
		default:
			q := syntheticText(r, r.Intn(1_000_000))
			start := time.Now()
			_, err := client.Search(ctx, &engrampb.SearchRequest{Query: q, K: 10})
			if isPhaseBoundaryArtifact(ctx, err) {
				return // the phase ended mid-call — not a service failure
			}
			rec.record(time.Since(start), err)
		}
	}
}

// latencyRecorder collects latencies and error counts across goroutines.
type latencyRecorder struct {
	mu        sync.Mutex
	latencies []time.Duration
	errors    atomic.Int64
	total     atomic.Int64
}

func (r *latencyRecorder) record(d time.Duration, err error) {
	r.total.Add(1)
	if err != nil {
		r.errors.Add(1)
		return
	}
	r.mu.Lock()
	r.latencies = append(r.latencies, d)
	r.mu.Unlock()
}

func (r *latencyRecorder) summarize() endpointStats {
	r.mu.Lock()
	sorted := append([]time.Duration(nil), r.latencies...)
	r.mu.Unlock()
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	total := r.total.Load()
	errs := r.errors.Load()
	var errPct float64
	if total > 0 {
		errPct = float64(errs) / float64(total)
	}
	return endpointStats{
		Total: total, Errors: errs, ErrorPct: errPct,
		P50Ms: percentileMS(sorted, 0.50),
		P95Ms: percentileMS(sorted, 0.95),
		P99Ms: percentileMS(sorted, 0.99),
	}
}

func percentileMS(sorted []time.Duration, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := min(int(p*float64(len(sorted))), len(sorted)-1)
	return float64(sorted[idx].Microseconds()) / 1000.0
}

// Package worker implements the async write path (Phase 2): the outbox
// worker pool that claims episodic events (D12), extracts facts through the
// claim-first ledger (D13), reconciles them against T2, and lands them
// bi-temporally under the write protocol (D10/D11) — plus the repair sweep
// that converges partial writes. The gRPC append is the enqueue; this
// package is everything after it.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/ingest"
	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/store"
)

// Store is the write-path capability the worker needs: the shared Store seam
// plus the realtime fact reads reconciliation depends on. OpenSearchStore
// satisfies it; tests use an in-memory fake (the enrich.Store consumer-side
// seam precedent).
type Store interface {
	store.Store
	// GetFact fetches one fact by doc id with realtime visibility (never
	// refresh-dependent) — the 409 re-read and in-batch candidate merge.
	GetFact(ctx context.Context, id string) (store.VersionedFact, bool, error)
	// Candidates returns up to k live reconciliation candidates for f,
	// carrying the guarded-close concurrency tokens.
	Candidates(ctx context.Context, f memory.SemanticFact, k int) ([]store.VersionedFact, error)
	// ValidTimeNeighbors returns f's nearest chain versions by valid time,
	// INCLUDING closed history (excluding record-retracted rows and selfID) —
	// the read D10's neighbor-aware late-arrival insertion requires.
	ValidTimeNeighbors(ctx context.Context, f memory.SemanticFact, selfID string) (pred, succ *store.VersionedFact, err error)
}

// Config tunes one Worker. Zero values select the defaults noted per field.
type Config struct {
	// ExtractorVersion keys the ledger (D13): bumping it deliberately
	// reprocesses events under a new ledger identity. Default "v1".
	ExtractorVersion string
	// CandidateK bounds candidate retrieval (DW-2.2). Default 10.
	CandidateK int
	// MaxAttempts bounds outbox retries before dead-lettering. Default 5.
	MaxAttempts int
	// BatchSize bounds one ClaimBatch. Default 16.
	BatchSize int
	// ClaimLease is the outbox lease per claim — it is also the retry
	// backoff clock: an abandoned event becomes reclaimable only after it
	// expires. Default 1m.
	ClaimLease time.Duration
	// PollInterval paces idle outbox scans (D12: low cadence to limit
	// segment churn). Default 2s.
	PollInterval time.Duration
	// Workers sizes the Run pool. Default 2.
	Workers int
}

func (c Config) withDefaults() Config {
	if c.ExtractorVersion == "" {
		c.ExtractorVersion = "v1"
	}
	if c.CandidateK <= 0 {
		c.CandidateK = 10
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 5
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 16
	}
	if c.ClaimLease <= 0 {
		c.ClaimLease = time.Minute
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 2 * time.Second
	}
	if c.Workers <= 0 {
		c.Workers = 2
	}
	return c
}

// Metrics counts the write protocol's conflict paths — the observability
// DW-2.5 asserts on (a silent conflict path is an untested one).
type Metrics struct {
	// CreateConflicts counts op_type=create 409s (duplicate extraction or
	// replay — D11).
	CreateConflicts atomic.Int64
	// CloseConflicts counts guarded-close losses (two workers closing one
	// predecessor — D7).
	CloseConflicts atomic.Int64
	// EventsProcessed counts events that reached ledger-complete.
	EventsProcessed atomic.Int64
	// EventsDeadLettered counts events parked after MaxAttempts.
	EventsDeadLettered atomic.Int64
}

// Stage is a post-write extension of the async pipeline (D20). After an
// event's facts are extracted, reconciled, and landed bi-temporally, every
// registered stage runs against the event and its facts — this is where later
// phases plug in experience distillation (P5) and incremental graph upsert
// (P6), each in its own file, without editing the worker core. A stage runs
// AT-LEAST-ONCE: it executes before the ledger is marked complete, so a stage
// error fails the event and the outbox retries it (resuming from the cached
// extraction). Stages MUST therefore be idempotent.
type Stage interface {
	// Process handles one completed event and its landed facts. A non-nil
	// error re-queues the whole event (idempotent replay), so it must be
	// reserved for genuinely retryable failures.
	Process(ctx context.Context, ev memory.Episodic, facts []memory.SemanticFact) error
}

// namedStage pairs a stage with its registration name for logging.
type namedStage struct {
	name  string
	stage Stage
}

// Worker drives the async write path. Construct with New; run the pool with
// Run, or drive deterministically with Tick / ProcessEvent (tests, repair).
type Worker struct {
	store      Store
	extractor  ingest.Extractor
	reconciler ingest.Reconciler
	embedder   embed.Embedder // optional: nil skips fact embeddings
	cfg        Config
	logger     *slog.Logger
	stages     []namedStage

	// Metrics is exported state, safe for concurrent reads.
	Metrics Metrics
}

// RegisterStage adds a post-write pipeline stage (D20). Call it at wiring time
// before Run; it is not safe to call concurrently with a running pool. Stages
// run in registration order after each event's facts land.
func (w *Worker) RegisterStage(name string, s Stage) {
	w.stages = append(w.stages, namedStage{name: name, stage: s})
}

// runStages executes every registered stage for a completed event. A stage
// error is returned so the caller fails the event and lets the outbox retry;
// stages are documented at-least-once and idempotent.
func (w *Worker) runStages(ctx context.Context, ev memory.Episodic, facts []memory.SemanticFact) error {
	for _, s := range w.stages {
		if err := s.stage.Process(ctx, ev, facts); err != nil {
			return fmt.Errorf("worker: stage %q processing %s: %w", s.name, ev.EventID, err)
		}
	}
	return nil
}

// New returns a Worker over the given seams. embedder may be nil (facts are
// then BM25-only until a later enrichment fills vectors); logger nil uses
// slog.Default().
func New(st Store, ex ingest.Extractor, rec ingest.Reconciler, embedder embed.Embedder, cfg Config, logger *slog.Logger) *Worker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{store: st, extractor: ex, reconciler: rec, embedder: embedder, cfg: cfg.withDefaults(), logger: logger}
}

// Run operates the worker pool until ctx is done: cfg.Workers goroutines,
// each looping claim → process, sleeping a jittered PollInterval when the
// outbox is empty. Event-level failures are logged and left to the
// lease/attempts retry machinery — one poisoned event never stops the pool.
func (w *Worker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < w.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				n, err := w.Tick(ctx)
				if err != nil && ctx.Err() == nil {
					w.logger.ErrorContext(ctx, "worker tick failed", "error", err)
				}
				if n > 0 && err == nil {
					continue // drain while there is work
				}
				jitter := time.Duration(rand.Int64N(int64(w.cfg.PollInterval) / 2))
				select {
				case <-ctx.Done():
					return
				case <-time.After(w.cfg.PollInterval + jitter):
				}
			}
		}()
	}
	wg.Wait()
}

// Tick claims one batch and processes it, returning how many events were
// claimed. Per-event errors are logged, counted against attempts by the next
// claim, and do not fail the tick; only claim-level failures return an error.
func (w *Worker) Tick(ctx context.Context) (int, error) {
	batch, err := w.store.ClaimBatch(ctx, w.cfg.BatchSize, w.cfg.ClaimLease)
	if err != nil {
		return 0, fmt.Errorf("worker: claiming outbox batch: %w", err)
	}
	for _, ev := range batch {
		if ev.Attempts > w.cfg.MaxAttempts {
			reason := fmt.Sprintf("attempts exhausted (%d > %d)", ev.Attempts, w.cfg.MaxAttempts)
			if err := w.store.DeadLetter(ctx, ev.EventID, reason); err != nil {
				w.logger.ErrorContext(ctx, "dead-lettering failed", "event_id", ev.EventID, "error", err)
				continue
			}
			w.Metrics.EventsDeadLettered.Add(1)
			w.logger.WarnContext(ctx, "event dead-lettered", "event_id", ev.EventID, "reason", reason)
			continue
		}
		if err := w.ProcessEvent(ctx, ev); err != nil {
			// Abandon: the lease expires and the outbox retries; MaxAttempts
			// bounds it. This is the LLM-timeout retry/backoff path too.
			w.logger.ErrorContext(ctx, "event processing failed; will retry after lease expiry",
				"event_id", ev.EventID, "attempts", ev.Attempts, "error", err)
		}
	}
	return len(batch), nil
}

// ProcessEvent runs the D13 → D10 protocol for one claimed event:
//
//  1. Claim the ledger entry; an existing complete entry short-circuits, an
//     existing extracted entry resumes from the cached extraction (the LLM
//     is never re-called — D13), an unexpired foreign claim defers.
//  2. Extract, then persist the extraction into the ledger BEFORE any
//     semantic write.
//  3. Per fact (deterministic order): reconcile and execute the write
//     protocol (create-first with supersedes; guarded close; late arrivals
//     bounded at index time, predecessor untouched).
//  4. Mark the ledger complete, then the outbox event processed.
func (w *Worker) ProcessEvent(ctx context.Context, ev memory.Episodic) error {
	if ev.EventID == "" || ev.TenantID == "" {
		return fmt.Errorf("worker: event missing event_id or tenant_id: %+v", ev)
	}
	key := memory.LedgerKey{TenantID: ev.TenantID, EventID: ev.EventID, ExtractorVersion: w.cfg.ExtractorVersion}
	entry, err := w.store.ClaimLedger(ctx, key)
	if err != nil {
		return fmt.Errorf("worker: claiming ledger for %s: %w", ev.EventID, err)
	}

	state := entry.State
	var facts []memory.SemanticFact
	switch {
	case !entry.Claimed && state.Phase == store.LedgerComplete:
		// Replay short-circuit (DW-2.4): the work is done; just drain the
		// outbox copy.
		return w.complete(ctx, ev.EventID)

	case !entry.Claimed && state.Phase == store.LedgerExtracted:
		// Crash resume (D13): reuse the cached extraction; never re-extract.
		if err := json.Unmarshal(state.Extraction, &facts); err != nil {
			return fmt.Errorf("worker: cached extraction for %s is corrupt: %w", ev.EventID, err)
		}

	case !entry.Claimed && state.Phase == store.LedgerClaimed && time.Now().Before(entry.LeaseUntil):
		// Another run holds a live claim; defer to it.
		return nil

	default:
		// Fresh claim, or a crashed claim past its lease with nothing cached
		// (a pre-cache crash re-extracts by design — no semantic write can
		// have happened).
		facts, err = w.extractor.Extract(ctx, []memory.Episodic{ev})
		switch {
		case errors.Is(err, ingest.ErrNoFacts):
			// Fact-free events complete with nothing indexed.
			state = store.LedgerState{Phase: store.LedgerComplete, Extraction: []byte("[]")}
			if err := w.store.UpdateLedger(ctx, key, state); err != nil {
				return fmt.Errorf("worker: completing empty ledger for %s: %w", ev.EventID, err)
			}
			return w.complete(ctx, ev.EventID)
		case err != nil:
			// Malformed output or transport failure: rejected, never indexed
			// (DW-2.7); the outbox retries and dead-letters after MaxAttempts.
			return fmt.Errorf("worker: extracting %s: %w", ev.EventID, err)
		}
		w.stampFacts(ev, facts)
		raw, err := json.Marshal(facts)
		if err != nil {
			return fmt.Errorf("worker: encoding extraction for %s: %w", ev.EventID, err)
		}
		state = store.LedgerState{Phase: store.LedgerExtracted, Extraction: raw, CompletedActions: state.CompletedActions}
		if err := w.store.UpdateLedger(ctx, key, state); err != nil {
			return fmt.Errorf("worker: caching extraction for %s: %w", ev.EventID, err)
		}
	}

	// Deterministic order (DW-2.7): contradictory in-batch facts resolve by
	// (valid_at, content_key) — the last by valid time ends up the live head.
	sort.Slice(facts, func(i, j int) bool {
		if !facts[i].ValidAt.Equal(facts[j].ValidAt) {
			return facts[i].ValidAt.Before(facts[j].ValidAt)
		}
		return facts[i].ContentKey < facts[j].ContentKey
	})

	written := make(map[string]bool, len(facts))
	done := make(map[string]bool, len(state.CompletedActions))
	for _, id := range state.CompletedActions {
		done[id] = true
	}
	for _, f := range facts {
		docID := memory.FactDocID(f.ContentKey, f.ValidAt)
		if done[docID] {
			written[docID] = true // resumed: this action already landed
			continue
		}
		if err := w.reconcileFact(ctx, f, docID, written); err != nil {
			return fmt.Errorf("worker: reconciling fact %s for %s: %w", docID, ev.EventID, err)
		}
		done[docID] = true
		state.CompletedActions = append(state.CompletedActions, docID)
		if err := w.store.UpdateLedger(ctx, key, state); err != nil {
			return fmt.Errorf("worker: recording action %s for %s: %w", docID, ev.EventID, err)
		}
	}

	// Post-write stages (D20) run before the ledger is marked complete, so a
	// stage failure re-queues the event rather than silently dropping the
	// derived work. Phase 3 registers none; P5/P6 register theirs.
	if err := w.runStages(ctx, ev, facts); err != nil {
		return err
	}

	state.Phase = store.LedgerComplete
	if err := w.store.UpdateLedger(ctx, key, state); err != nil {
		return fmt.Errorf("worker: completing ledger for %s: %w", ev.EventID, err)
	}
	return w.complete(ctx, ev.EventID)
}

func (w *Worker) complete(ctx context.Context, eventID string) error {
	if err := w.store.Complete(ctx, eventID); err != nil {
		return fmt.Errorf("worker: completing event %s: %w", eventID, err)
	}
	w.Metrics.EventsProcessed.Add(1)
	return nil
}

// stampFacts fills the writer-owned fields on freshly extracted facts:
// tenancy/provenance from the source event (D16), valid_at defaulted to the
// event's occurrence time when the content carried none (bi-temporal
// semantics), the extractor version, and the D11 content key. System
// transaction time is stamped at write time, not here.
func (w *Worker) stampFacts(ev memory.Episodic, facts []memory.SemanticFact) {
	for i := range facts {
		f := &facts[i]
		f.TenantID, f.TeamID, f.Scope, f.OwnerAgentID = ev.TenantID, ev.TeamID, ev.Scope, ev.OwnerAgentID
		f.SourceIDs = []string{ev.EventID}
		f.ExtractorVersion = w.cfg.ExtractorVersion
		if f.ValidAt.IsZero() {
			f.ValidAt = ev.OccurredAt.UTC()
		}
		f.ValidAt = f.ValidAt.UTC()
		f.ContentKey = memory.ContentKey(f.TenantID, f.Subject, f.Predicate, f.Object)
	}
}

// maxReconcileAttempts bounds the re-read + re-reconcile loop (D10 step 3)
// and the guarded-close retry (step 4).
const maxReconcileAttempts = 3

// reconcileFact runs the decision + write protocol for one stamped fact.
// Every optimistic loss re-reads and re-decides; nothing is ever overwritten
// blindly and nothing is ever deleted.
func (w *Worker) reconcileFact(ctx context.Context, f memory.SemanticFact, docID string, written map[string]bool) error {
	for attempt := 0; attempt < maxReconcileAttempts; attempt++ {
		cands, err := w.candidates(ctx, f, docID, written)
		if err != nil {
			return err
		}
		op, err := w.reconciler.Reconcile(ctx, f, cands)
		if err != nil {
			return fmt.Errorf("reconcile decision: %w", err)
		}

		switch op.Kind {
		case ingest.OpNoop:
			return nil

		case ingest.OpAdd:
			f.Supersedes, f.InvalidAt = "", nil
			err := w.createFact(ctx, docID, f)
			if errors.Is(err, store.ErrConflict) {
				w.Metrics.CreateConflicts.Add(1)
				continue // D10 step 3: re-read, re-reconcile
			}
			if err == nil {
				written[docID] = true
			}
			return err

		case ingest.OpUpdate, ingest.OpInvalidate:
			pred := findCandidate(cands, op.PredecessorID)
			if pred == nil {
				return fmt.Errorf("reconciler chose predecessor %s not in the candidate set", op.PredecessorID)
			}
			if f.ValidAt.Before(pred.Fact.ValidAt) {
				// Late arrival (D10 step 4): the fact is historical —
				// neighbor-aware insertion against the chain's TRUE valid-time
				// neighbors (closed history included), never just the live
				// head. The live head itself is never touched.
				err := w.insertHistorical(ctx, f, docID, *pred, written)
				if errors.Is(err, store.ErrConflict) {
					w.Metrics.CreateConflicts.Add(1)
					continue
				}
				if err == nil {
					written[docID] = true
				}
				return err
			}
			f.Supersedes, f.InvalidAt = op.PredecessorID, nil
			err := w.createFact(ctx, docID, f) // step 3: NEW fact first, supersedes durable
			if errors.Is(err, store.ErrConflict) {
				w.Metrics.CreateConflicts.Add(1)
				continue
			}
			if err != nil {
				return err
			}
			written[docID] = true
			return w.closePredecessor(ctx, *pred, f.ValidAt) // step 4: guarded close

		default:
			return fmt.Errorf("reconciler returned unknown op kind %q", op.Kind)
		}
	}
	return fmt.Errorf("did not settle after %d attempts: %w", maxReconcileAttempts, store.ErrConflict)
}

// createFact stamps transaction time, best-effort embeds the statement, and
// indexes the fact with op_type=create.
func (w *Worker) createFact(ctx context.Context, docID string, f memory.SemanticFact) error {
	f.CreatedAt = time.Now().UTC()
	if w.embedder != nil {
		if vecs, err := w.embedder.Embed(ctx, []string{f.Statement}); err == nil && len(vecs) == 1 {
			f.FactEmbedding = vecs[0]
		} else if err != nil {
			// Degraded, not fatal: BM25 serves the fact; enrichment can
			// backfill. Silent-failure rule: log it.
			w.logger.WarnContext(ctx, "fact embedding failed; indexing text-only", "doc_id", docID, "error", err)
		}
	}
	return w.store.Create(ctx, docID, f)
}

// closePredecessor executes the guarded close (plan step 4, neighbor-aware):
// predecessor.invalid_at = min(requested bound, the predecessor's nearest
// valid-time successor's valid_at), stamped with invalidated_tx_at (the one
// sanctioned in-place mutation). The min matters: a record can land INSIDE a
// crash/divergence window (created between this chain's create and its
// close), and closing at the requested successor's valid_at alone would
// swallow it — permanently overlapping closed intervals. Every close in the
// system funnels through here (worker step 4 and the sweep's tryClose), so
// this one choke point keeps all close bounds neighbor-aware. A stale guard
// re-reads: if another worker already closed it, that IS success (no lost
// update — last state wins by guard, not by overwrite).
func (w *Worker) closePredecessor(ctx context.Context, pred store.VersionedFact, validAt time.Time) error {
	bound, err := w.closeBound(ctx, pred, validAt)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < maxReconcileAttempts; attempt++ {
		if pred.Fact.InvalidAt != nil {
			return nil // already closed by a concurrent worker or the sweep
		}
		if bound.Before(pred.Fact.ValidAt) {
			// Interval-inversion guard: closing a predecessor to before its
			// own valid_at can only come from a protocol bug upstream.
			return fmt.Errorf("refusing inverted close of %s: close %s < valid_at %s", pred.ID, bound, pred.Fact.ValidAt)
		}
		now := time.Now().UTC()
		closed := pred.Fact
		v := bound
		closed.InvalidAt, closed.InvalidatedTxAt = &v, &now
		err := w.store.Update(ctx, pred.ID, closed, pred.SeqNo, pred.PrimaryTerm)
		if err == nil {
			return nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
		w.Metrics.CloseConflicts.Add(1)
		fresh, ok, err := w.store.GetFact(ctx, pred.ID)
		if err != nil {
			return fmt.Errorf("re-reading predecessor %s after close conflict: %w", pred.ID, err)
		}
		if !ok {
			return fmt.Errorf("predecessor %s vanished during close (facts are never deleted): %w", pred.ID, store.ErrNotFound)
		}
		pred = fresh
	}
	return fmt.Errorf("guarded close of %s did not settle after %d attempts: %w", pred.ID, maxReconcileAttempts, store.ErrConflict)
}

// closeBound returns the neighbor-aware close instant for pred (plan step
// 4): min(requested, nearest valid-time successor's valid_at across ALL
// non-expired chain records — closed history included). The query's
// successor is strictly after pred's own valid_at, so the min can never
// invert pred's interval. Records that land after this read are covered by
// the other half of the invariant: their own insert path trims pred
// (insertHistorical), so both orders converge to disjoint intervals.
func (w *Worker) closeBound(ctx context.Context, pred store.VersionedFact, requested time.Time) (time.Time, error) {
	_, succ, err := w.store.ValidTimeNeighbors(ctx, pred.Fact, pred.ID)
	if err != nil {
		return time.Time{}, fmt.Errorf("reading close-bound neighbors of %s: %w", pred.ID, err)
	}
	if succ != nil && succ.Fact.ValidAt.Before(requested) {
		return succ.Fact.ValidAt, nil
	}
	return requested, nil
}

// insertHistorical executes the late-arrival path (D10 step 4, neighbor-
// aware): the historical fact is bounded by its TRUE nearest valid-time
// successor within the chain — which may be a CLOSED version, not the live
// head — and the closed valid-time predecessor whose interval would contain
// it is trimmed down to the new fact's valid_at, so the chain's intervals
// stay pairwise disjoint (bounding against the live head alone overlaps
// closed history once a chain has ≥2 prior versions). The live head is never
// touched.
//
// Ordering deviation from D10 step 3's create-first, deliberate and local to
// this path: the trim runs BEFORE the create. A historical record carries no
// supersedes link, so no sweep rule can repair a crash that lands the create
// but not the trim (the outer 409 re-reconcile NOOPs and the overlap would
// be permanent). Trim-first makes every crash window converge: a crash after
// the trim leaves a valid-time GAP — never two overlapping truths — and the
// cached ledger extraction fills it on resume.
func (w *Worker) insertHistorical(ctx context.Context, f memory.SemanticFact, docID string, head store.VersionedFact, written map[string]bool) error {
	pred, succ, err := w.store.ValidTimeNeighbors(ctx, f, docID)
	if err != nil {
		return fmt.Errorf("late arrival %s: reading valid-time neighbors: %w", docID, err)
	}
	// Merge realtime reads the neighbor search may lag behind: the reconciler
	// head and this batch's own writes.
	consider := []store.VersionedFact{head}
	for id := range written {
		if id == docID {
			continue
		}
		vf, ok, err := w.store.GetFact(ctx, id)
		if err != nil {
			return fmt.Errorf("late arrival %s: realtime neighbor read %s: %w", docID, id, err)
		}
		if ok {
			consider = append(consider, vf)
		}
	}
	for i := range consider {
		vf := &consider[i]
		if vf.ID == docID || vf.Fact.ExpiredAt != nil ||
			vf.Fact.TenantID != f.TenantID || vf.Fact.Subject != f.Subject || vf.Fact.Predicate != f.Predicate {
			continue
		}
		if vf.Fact.ValidAt.After(f.ValidAt) {
			if succ == nil || succBeats(*vf, *succ) {
				succ = vf
			}
		} else if pred == nil || predBeats(*vf, *pred) {
			pred = vf
		}
	}
	if succ == nil {
		// Unreachable by construction: the reconciler head qualifies
		// (f.ValidAt < head.ValidAt is this branch's precondition). Surfaced
		// loudly rather than indexed unbounded — an unbounded historical
		// record would shadow the live head.
		return fmt.Errorf("late arrival %s: no valid-time successor found (head %s)", docID, head.ID)
	}

	// 1. Trim the CLOSED containing neighbor so intervals never overlap. A
	// LIVE valid-time predecessor is concurrent divergence — the repair
	// sweep owns converging that; never close a live fact from this path.
	if pred != nil && pred.Fact.InvalidAt != nil && pred.Fact.InvalidAt.After(f.ValidAt) {
		if err := w.trimInterval(ctx, *pred, f.ValidAt); err != nil {
			return fmt.Errorf("late arrival %s: trimming valid-time predecessor %s: %w", docID, pred.ID, err)
		}
	}

	// 2. Index the bounded historical record. No supersedes link — the sweep
	// must never "complete" a close this path never owed.
	inv := succ.Fact.ValidAt
	f.Supersedes, f.InvalidAt = "", &inv
	return w.createFact(ctx, docID, f)
}

// succBeats reports whether a is the nearer successor than b (earliest wins;
// ties by doc id).
func succBeats(a, b store.VersionedFact) bool {
	if !a.Fact.ValidAt.Equal(b.Fact.ValidAt) {
		return a.Fact.ValidAt.Before(b.Fact.ValidAt)
	}
	return a.ID < b.ID
}

// predBeats reports whether a is the nearer predecessor than b (latest wins;
// ties by doc id).
func predBeats(a, b store.VersionedFact) bool {
	if !a.Fact.ValidAt.Equal(b.Fact.ValidAt) {
		return a.Fact.ValidAt.After(b.Fact.ValidAt)
	}
	return a.ID > b.ID
}

// trimInterval shrinks a CLOSED fact's invalid_at down to at (guarded; the
// sanctioned in-place mutation, re-stamped with invalidated_tx_at). It is
// monotone — it only ever narrows an interval — so concurrent trims converge
// to the earliest bound; a fact already within the bound is a no-op, and a
// live fact is left alone (closing live facts is the write protocol's and
// the sweep's job, never a trim's).
func (w *Worker) trimInterval(ctx context.Context, vf store.VersionedFact, at time.Time) error {
	for attempt := 0; attempt < maxReconcileAttempts; attempt++ {
		switch {
		case vf.Fact.InvalidAt == nil:
			return nil // live: concurrent divergence, the sweep converges it
		case !vf.Fact.InvalidAt.After(at):
			return nil // already within the bound
		case at.Before(vf.Fact.ValidAt):
			// Interval-inversion guard: trimming below the fact's own
			// valid_at can only come from a protocol bug upstream.
			return fmt.Errorf("refusing inverted trim of %s: trim %s < valid_at %s", vf.ID, at, vf.Fact.ValidAt)
		}
		now := time.Now().UTC()
		trimmed := vf.Fact
		v := at
		trimmed.InvalidAt, trimmed.InvalidatedTxAt = &v, &now
		err := w.store.Update(ctx, vf.ID, trimmed, vf.SeqNo, vf.PrimaryTerm)
		if err == nil {
			return nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
		w.Metrics.CloseConflicts.Add(1)
		fresh, ok, err := w.store.GetFact(ctx, vf.ID)
		if err != nil {
			return fmt.Errorf("re-reading %s after trim conflict: %w", vf.ID, err)
		}
		if !ok {
			return fmt.Errorf("fact %s vanished during trim (facts are never deleted): %w", vf.ID, store.ErrNotFound)
		}
		vf = fresh
	}
	return fmt.Errorf("trim of %s did not settle after %d attempts: %w", vf.ID, maxReconcileAttempts, store.ErrConflict)
}

// candidates fetches the searchable candidate set and merges realtime reads
// of this fact's own doc id plus everything written earlier in this batch —
// correctness must never depend on index refresh timing.
func (w *Worker) candidates(ctx context.Context, f memory.SemanticFact, docID string, written map[string]bool) ([]ingest.Candidate, error) {
	found, err := w.store.Candidates(ctx, f, w.cfg.CandidateK)
	if err != nil {
		return nil, fmt.Errorf("retrieving candidates: %w", err)
	}
	seen := make(map[string]bool, len(found)+len(written)+1)
	out := make([]ingest.Candidate, 0, len(found)+len(written)+1)
	for _, vf := range found {
		seen[vf.ID] = true
		out = append(out, toCandidate(vf))
	}
	realtime := []string{docID}
	for id := range written {
		realtime = append(realtime, id)
	}
	for _, id := range realtime {
		if seen[id] {
			continue
		}
		vf, ok, err := w.store.GetFact(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("realtime candidate read %s: %w", id, err)
		}
		if ok {
			seen[id] = true
			out = append(out, toCandidate(vf))
		}
	}
	return out, nil
}

func toCandidate(vf store.VersionedFact) ingest.Candidate {
	return ingest.Candidate{ID: vf.ID, Fact: vf.Fact, SeqNo: vf.SeqNo, PrimaryTerm: vf.PrimaryTerm}
}

func findCandidate(cands []ingest.Candidate, id string) *store.VersionedFact {
	for _, c := range cands {
		if c.ID == id {
			return &store.VersionedFact{ID: c.ID, Fact: c.Fact, SeqNo: c.SeqNo, PrimaryTerm: c.PrimaryTerm}
		}
	}
	return nil
}

// Package store defines the Store seam: the write-side contract every later
// phase shares. OpenSearch is one implementation behind it (Phase 1); no
// vendor types appear in these signatures. The method set encodes the plan's
// write protocol (D10/D11), outbox queue (D12), and claim-first extraction
// ledger (D13).
package store

import (
	"context"
	"errors"
	"time"

	"github.com/ryanthedev/engram/internal/memory"
)

// ErrConflict is returned when an optimistic write loses: an op_type=create
// hits an existing doc _id (HTTP 409 — concurrent identical extraction, or a
// replayed ledger claim), or a guarded Update's if_seq_no/if_primary_term is
// stale. Callers re-read and re-reconcile; they never overwrite blindly.
var ErrConflict = errors.New("store: version or create conflict")

// LedgerPhase is the lifecycle state of an extraction-ledger entry (D13).
type LedgerPhase string

// Ledger lifecycle: claimed before extraction, extraction output cached, then
// complete once every reconciliation action has landed.
const (
	// LedgerClaimed marks a claimed entry whose extraction has not yet been
	// persisted; a crash here means the retry re-extracts (nothing cached yet,
	// and no semantic write can have happened).
	LedgerClaimed LedgerPhase = "claimed"
	// LedgerExtracted marks an entry whose extraction output is cached; a
	// retry resumes reconciliation from the cache and never re-calls the LLM.
	LedgerExtracted LedgerPhase = "extracted"
	// LedgerComplete marks an entry whose reconciliation actions all landed;
	// a replayed event short-circuits here.
	LedgerComplete LedgerPhase = "complete"
)

// LedgerState is the mutable payload of a ledger entry: the phase, the cached
// extraction (persisted BEFORE any semantic write — D13), and which
// per-fact actions have landed, so a crashed run resumes where it stopped.
type LedgerState struct {
	Phase LedgerPhase `json:"phase"`
	// Extraction is the serialized extractor output, cached at
	// LedgerExtracted so retries are mechanical, never LLM-behavioral.
	Extraction []byte `json:"extraction,omitempty"`
	// CompletedActions lists the semantic doc _ids whose reconciliation
	// action (create / guarded close) has landed.
	CompletedActions []string `json:"completed_actions,omitempty"`
}

// LedgerEntry is one extraction-ledger row keyed by (tenant_id, event_id,
// extractor_version).
type LedgerEntry struct {
	Key   memory.LedgerKey `json:"key"`
	State LedgerState      `json:"state"`
	// Claimed reports whether THIS call created the entry (won the claim).
	// False means the entry already existed: resume from State per D13.
	Claimed bool `json:"-"`
	// LeaseUntil bounds how long the claim holder may work before the repair
	// sweep considers the run crashed and resumes it.
	LeaseUntil time.Time `json:"lease_until"`
}

// Store is the durable write seam shared by the ingest worker (Phase 2), the
// gRPC service (Phase 1), and the repair sweep. Implementations must surface
// every optimistic-concurrency loss as ErrConflict (wrapped or direct) and
// must never hard-delete a semantic fact.
type Store interface {
	// Append durably appends an episodic record to T1 and returns its doc id.
	// The append is the enqueue (outbox, D12): once Append returns, the event
	// will eventually be claimed by ClaimBatch even across crashes.
	Append(ctx context.Context, rec memory.Episodic) (id string, err error)

	// Create indexes a new semantic fact with op_type=create under the given
	// doc id (memory.FactDocID). An existing id yields ErrConflict — the D11
	// duplicate-extraction collision; the caller re-reads and re-reconciles.
	Create(ctx context.Context, id string, f memory.SemanticFact) error

	// Update rewrites the fact at id guarded by if_seq_no/if_primary_term
	// (optimistic concurrency, D7). Its one sanctioned use is the write
	// protocol's guarded close: stamping invalid_at + invalidated_tx_at on a
	// predecessor. A stale guard yields ErrConflict; callers re-read and
	// retry with bounded attempts.
	Update(ctx context.Context, id string, f memory.SemanticFact, ifSeqNo, ifPrimaryTerm int64) error

	// ClaimBatch atomically claims up to n unprocessed, unleased, non-dead-
	// lettered episodic events for the caller (scan-and-claim, D12), setting
	// claim_lease_until = now + lease and incrementing attempts. Events whose
	// lease has expired are reclaimable.
	ClaimBatch(ctx context.Context, n int, lease time.Duration) ([]memory.Episodic, error)

	// Complete marks the episodic event processed (processed_at = now),
	// removing it from future outbox scans.
	Complete(ctx context.Context, eventID string) error

	// DeadLetter permanently parks the episodic event with a reason after
	// attempts are exhausted; dead-lettered events are excluded from
	// ClaimBatch and surfaced for operator review.
	DeadLetter(ctx context.Context, eventID string, reason string) error

	// ClaimLedger claims the extraction-ledger entry for key with
	// op_type=create (claim-first, D13). If the entry already exists the
	// existing entry is returned with Claimed=false and the caller resumes
	// from its State: LedgerComplete → stop; LedgerExtracted → reconcile from
	// the cached extraction without re-calling the LLM.
	ClaimLedger(ctx context.Context, key memory.LedgerKey) (LedgerEntry, error)

	// UpdateLedger persists the entry's state transition (cache the
	// extraction, record completed actions, mark complete). The extraction
	// MUST be persisted here before any semantic write (D13).
	UpdateLedger(ctx context.Context, key memory.LedgerKey, state LedgerState) error

	// ScanIncomplete returns ledger entries that are past their lease and not
	// LedgerComplete — crashed runs the repair sweep resumes from their
	// cached extraction. The sweep (D10) also completes half-done UPDATE
	// chains (live supersedes target) and collapses duplicate live content
	// keys; convergence SLO ≤ 5 min at S1.
	ScanIncomplete(ctx context.Context) ([]LedgerEntry, error)
}

// Package ingest defines the async write-path seams (Phase 2 implements
// them): Extractor turns episodic events into candidate facts (LLM-gated,
// D4) and Reconciler decides how each candidate lands against existing
// memory (ADD / UPDATE / INVALIDATE / NOOP — the Mem0-lineage four-way
// decision, written bi-temporally per D10/D11).
package ingest

import (
	"context"

	"github.com/ryanthedev/engram/internal/memory"
)

// Candidate is an existing semantic fact retrieved as reconciliation context,
// carrying the optimistic-concurrency tokens (D7) a guarded close needs.
type Candidate struct {
	// ID is the fact's doc _id (memory.FactDocID form).
	ID   string
	Fact memory.SemanticFact
	// SeqNo and PrimaryTerm guard the predecessor close (if_seq_no /
	// if_primary_term); stale values surface as store.ErrConflict.
	SeqNo       int64
	PrimaryTerm int64
}

// OpKind is the reconciler's four-way decision (D4/D10).
type OpKind string

// The four reconciliation outcomes. UPDATE and INVALIDATE both index the new
// fact first (op_type=create, supersedes set) and then close the predecessor
// under guard; nothing is ever hard-deleted.
const (
	// OpNoop discards the candidate fact: it is already known or adds nothing.
	OpNoop OpKind = "noop"
	// OpAdd indexes the fact as new knowledge with no predecessor.
	OpAdd OpKind = "add"
	// OpUpdate supersedes a predecessor with revised content: new fact first,
	// then predecessor.invalid_at = new.valid_at (guarded).
	OpUpdate OpKind = "update"
	// OpInvalidate retracts a predecessor without asserting a successor
	// truth: the new record documents the retraction via supersedes.
	OpInvalidate OpKind = "invalidate"
)

// Op is one reconciliation decision. PredecessorID names the candidate being
// superseded — required for OpUpdate and OpInvalidate, empty otherwise. The
// late-arrival rule (D10 step 4) applies downstream: a fact older than its
// predecessor never touches it and is bounded at index time instead.
type Op struct {
	Kind OpKind
	// PredecessorID is the doc _id of the candidate this op supersedes.
	PredecessorID string
}

// Extractor turns a batch of episodic events into candidate semantic facts
// via the (nondeterministic) extraction LLM. Callers MUST persist the output
// into the extraction ledger before any semantic write, and retries MUST
// reuse that cached output rather than re-calling Extract (D13). A malformed
// or empty extraction returns an error; it is never indexed.
type Extractor interface {
	// Extract returns candidate facts for the events; candidates carry
	// content but no system timestamps (the writer stamps those).
	Extract(ctx context.Context, events []memory.Episodic) ([]memory.SemanticFact, error)
}

// Reconciler decides how one candidate fact lands against the retrieved
// candidates. Decisions must be deterministic for identical inputs so that
// contradictory in-batch facts resolve reproducibly.
type Reconciler interface {
	// Reconcile returns the four-way decision for fact given its candidate
	// context. It only decides; the caller executes the write protocol
	// (create-first, guarded close, ledger bookkeeping — D10/D11/D13).
	Reconcile(ctx context.Context, fact memory.SemanticFact, candidates []Candidate) (Op, error)
}

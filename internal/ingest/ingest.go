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

// OpKind is what happened to one candidate fact (D4/D10). A Reconciler
// returns exactly one of the four decisions below; the WRITER additionally
// reports OpReplayed for an action it resumed rather than re-decided, so that
// every landed fact carries a well-defined outcome (never a zero value).
type OpKind string

// The four reconciliation decisions, plus the writer-only OpReplayed. UPDATE
// and INVALIDATE both index the new fact first (op_type=create, supersedes
// set) and then close the predecessor under guard; nothing is ever
// hard-deleted.
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
	// OpReplayed marks a fact whose write already landed in an earlier attempt
	// at the same event (its doc id is in the ledger's CompletedActions, D13):
	// the writer skipped reconciliation, so the original decision is not
	// recoverable and no predecessor is known. A Reconciler MUST NEVER return
	// it — the writer rejects an unknown op kind — and derived projections
	// MUST treat it as "already accounted for", never re-applying a
	// supersession side effect.
	OpReplayed OpKind = "replayed"
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

// FactOutcome is what actually happened to one fact on the write path — the
// reconciler's decision, surrendered to the post-write stages (D20) instead of
// discarded. The reconciler is the single owner of fact lifecycle; derived
// projections (the graph tier) are FED that decision rather than re-deriving
// it by re-reading the semantic store.
//
// Invariants, which every stage may rely on:
//   - Decision is never the zero value. A fact the writer resumed rather than
//     re-decided reports OpReplayed.
//   - Predecessor is non-nil for EXACTLY OpUpdate and OpInvalidate — the two
//     kinds that supersede an existing fact — and nil for OpAdd, OpNoop and
//     OpReplayed.
//   - Fact is the fact AS LANDED — Supersedes and InvalidAt carry the values
//     actually written — not the pre-reconciliation candidate. (System
//     transaction time is stamped inside the store write and is NOT reflected
//     here.) For OpNoop nothing was written and Fact is the discarded
//     candidate; for OpReplayed it is the cached extraction's copy of the fact
//     that landed on an earlier attempt.
//   - A late arrival — a superseding fact OLDER than the fact it supersedes
//     (D10 step 4) — reports OpUpdate/OpInvalidate with a non-nil Predecessor,
//     yet the predecessor was deliberately NOT closed: the new fact was bounded
//     at index time instead and the predecessor stays live. A projection that
//     acts on supersession must exclude it, which is derivable from the outcome
//     alone: Fact.ValidAt.Before(Predecessor.ValidAt).
type FactOutcome struct {
	// Fact is the fact as landed (see invariants above).
	Fact memory.SemanticFact
	// Decision is the reconciler's actual Op.Kind for this fact, or OpReplayed.
	Decision OpKind
	// Predecessor is the fact this one superseded — a copy, carrying no
	// optimistic-concurrency tokens (those stay inside the writer).
	Predecessor *memory.SemanticFact
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

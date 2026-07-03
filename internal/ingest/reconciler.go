package ingest

import (
	"context"
	"fmt"

	"github.com/ryanthedev/engram/internal/memory"
)

// RuleReconciler is the deterministic Reconciler: a pure function of the
// fact and its candidates (no LLM, no clock, no I/O), so identical inputs —
// including contradictory in-batch replays — always resolve identically
// (DW-2.7). Rules, in order:
//
//  1. A candidate (live or closed) with the same content key AND valid time
//     is this fact's own content-addressed record → NOOP. This is the 409
//     re-reconcile landing: the record already exists, whoever wrote it.
//  2. A LIVE candidate with the same content key → NOOP: the assertion is
//     already current (content-key dedup, DW-2.4).
//  3. An empty Object is retraction intent: INVALIDATE the live head for the
//     same subject+predicate, or NOOP when nothing is live to retract —
//     retraction of nothing asserts nothing and must not be indexed.
//  4. A live head for the same subject+predicate with a different object →
//     UPDATE (supersede it).
//  5. Otherwise → ADD.
//
// The live head is the live same-subject+predicate candidate with the
// greatest (valid_at, content_key, id) — a total order, so head selection is
// deterministic even among concurrent divergent writes.
type RuleReconciler struct{}

var _ Reconciler = RuleReconciler{}

// Reconcile implements Reconciler; see RuleReconciler for the decision
// rules. The fact must carry a content key (the worker stamps it before
// reconciling) — a missing key is a programming error, not bad LLM input.
func (RuleReconciler) Reconcile(_ context.Context, fact memory.SemanticFact, candidates []Candidate) (Op, error) {
	if fact.ContentKey == "" || fact.Subject == "" || fact.Predicate == "" {
		return Op{}, fmt.Errorf("ingest: reconcile called with an unstamped fact (content_key/subject/predicate required)")
	}

	var head *Candidate
	for i := range candidates {
		c := &candidates[i]
		if c.Fact.TenantID != fact.TenantID {
			continue
		}
		// Rule 1: this fact's own record (content key + valid time ⇒ same
		// content-addressed doc id), live or closed.
		if c.Fact.ContentKey == fact.ContentKey && c.Fact.ValidAt.Equal(fact.ValidAt) {
			return Op{Kind: OpNoop}, nil
		}
		if !isLive(c.Fact) {
			continue
		}
		// Rule 2: the assertion is already current.
		if c.Fact.ContentKey == fact.ContentKey {
			return Op{Kind: OpNoop}, nil
		}
		if c.Fact.Subject == fact.Subject && c.Fact.Predicate == fact.Predicate && laterHead(c, head) {
			head = c
		}
	}

	switch {
	case fact.Object == "": // rule 3: retraction intent
		if head == nil {
			return Op{Kind: OpNoop}, nil
		}
		return Op{Kind: OpInvalidate, PredecessorID: head.ID}, nil
	case head != nil: // rule 4
		return Op{Kind: OpUpdate, PredecessorID: head.ID}, nil
	default: // rule 5
		return Op{Kind: OpAdd}, nil
	}
}

// isLive reports whether f is currently open on both time axes.
func isLive(f memory.SemanticFact) bool {
	return f.InvalidAt == nil && f.ExpiredAt == nil
}

// laterHead reports whether c should replace cur as the live head, under the
// total order (valid_at, content_key, id).
func laterHead(c, cur *Candidate) bool {
	if cur == nil {
		return true
	}
	if !c.Fact.ValidAt.Equal(cur.Fact.ValidAt) {
		return c.Fact.ValidAt.After(cur.Fact.ValidAt)
	}
	if c.Fact.ContentKey != cur.Fact.ContentKey {
		return c.Fact.ContentKey > cur.Fact.ContentKey
	}
	return c.ID > cur.ID
}

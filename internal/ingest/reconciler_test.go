package ingest_test

import (
	"context"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/ingest"
	"github.com/ryanthedev/engram/internal/memory"
)

var (
	t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 = t0.Add(24 * time.Hour)
	t2 = t0.Add(48 * time.Hour)
)

// fact builds a stamped candidate fact.
func fact(subject, predicate, object string, validAt time.Time) memory.SemanticFact {
	return memory.SemanticFact{
		Subject: subject, Predicate: predicate, Object: object,
		Statement:  subject + " " + predicate + " " + object,
		TenantID:   "t1",
		ContentKey: memory.ContentKey("t1", subject, predicate, object),
		ValidAt:    validAt,
	}
}

func live(id string, f memory.SemanticFact) ingest.Candidate {
	return ingest.Candidate{ID: id, Fact: f, SeqNo: 1, PrimaryTerm: 1}
}

func closed(id string, f memory.SemanticFact, at time.Time) ingest.Candidate {
	f.InvalidAt = &at
	return ingest.Candidate{ID: id, Fact: f, SeqNo: 2, PrimaryTerm: 1}
}

// TestDW_2_3_ReconcilerFourWayDecisions pins every RuleReconciler decision
// path: the four op kinds, tenant scoping, head selection, and the
// mechanical-idempotency NOOPs the 409 re-reconcile depends on.
func TestDW_2_3_ReconcilerFourWayDecisions(t *testing.T) {
	rec := ingest.RuleReconciler{}
	tests := []struct {
		name       string
		fact       memory.SemanticFact
		candidates []ingest.Candidate
		wantKind   ingest.OpKind
		wantPred   string
	}{
		{"no candidates → ADD", fact("svc", "owner", "ana", t1), nil, ingest.OpAdd, ""},
		{"unrelated live candidate → ADD", fact("svc", "owner", "ana", t1),
			[]ingest.Candidate{live("c1", fact("db", "owner", "bob", t0))}, ingest.OpAdd, ""},
		{"same subject+predicate, new object → UPDATE head", fact("svc", "owner", "ana", t1),
			[]ingest.Candidate{live("c1", fact("svc", "owner", "bob", t0))}, ingest.OpUpdate, "c1"},
		{"UPDATE picks the LATEST live head", fact("svc", "owner", "cid", t2),
			[]ingest.Candidate{live("old", fact("svc", "owner", "ana", t0)), live("new", fact("svc", "owner", "bob", t1))},
			ingest.OpUpdate, "new"},
		{"retraction with a live head → INVALIDATE", fact("svc", "owner", "", t1),
			[]ingest.Candidate{live("c1", fact("svc", "owner", "bob", t0))}, ingest.OpInvalidate, "c1"},
		{"retraction of nothing → NOOP (never indexed)", fact("svc", "owner", "", t1), nil, ingest.OpNoop, ""},
		{"live identical content key → NOOP (content-key dedup)", fact("svc", "owner", "ana", t1),
			[]ingest.Candidate{live("c1", fact("svc", "owner", "ana", t0))}, ingest.OpNoop, ""},
		{"own record already present (same key+valid_at), even closed → NOOP", fact("svc", "owner", "ana", t1),
			[]ingest.Candidate{closed("c1", fact("svc", "owner", "ana", t1), t2)}, ingest.OpNoop, ""},
		{"closed head does not block re-assertion → ADD", fact("svc", "owner", "ana", t1),
			[]ingest.Candidate{closed("c1", fact("svc", "owner", "bob", t0), t0)}, ingest.OpAdd, ""},
		{"other tenant's candidates are invisible → ADD", fact("svc", "owner", "ana", t1),
			[]ingest.Candidate{live("c1", func() memory.SemanticFact {
				f := fact("svc", "owner", "bob", t0)
				f.TenantID = "t-other"
				return f
			}())}, ingest.OpAdd, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			op, err := rec.Reconcile(context.Background(), tt.fact, tt.candidates)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if op.Kind != tt.wantKind || op.PredecessorID != tt.wantPred {
				t.Errorf("op = %+v, want kind=%s pred=%q", op, tt.wantKind, tt.wantPred)
			}
		})
	}
}

// TestReconcilerIsDeterministic: identical inputs in any candidate order
// produce the identical decision (the DW-2.7 determinism precondition).
func TestReconcilerIsDeterministic(t *testing.T) {
	rec := ingest.RuleReconciler{}
	f := fact("svc", "owner", "cid", t2)
	a := live("a", fact("svc", "owner", "ana", t1))
	b := live("b", fact("svc", "owner", "bob", t1)) // same valid_at: tie broken by content_key/id
	op1, err1 := rec.Reconcile(context.Background(), f, []ingest.Candidate{a, b})
	op2, err2 := rec.Reconcile(context.Background(), f, []ingest.Candidate{b, a})
	if err1 != nil || err2 != nil {
		t.Fatalf("Reconcile: %v / %v", err1, err2)
	}
	if op1 != op2 {
		t.Errorf("candidate order changed the decision: %+v vs %+v", op1, op2)
	}
}

// TestReconcilerRejectsUnstampedFact: a fact without its content key is a
// programming error, surfaced loudly rather than mis-reconciled.
func TestReconcilerRejectsUnstampedFact(t *testing.T) {
	rec := ingest.RuleReconciler{}
	if _, err := rec.Reconcile(context.Background(), memory.SemanticFact{Subject: "svc", Predicate: "owner"}, nil); err == nil {
		t.Fatal("want an error for a fact with no content key")
	}
}

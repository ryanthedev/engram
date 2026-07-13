package graph

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ryanthedev/engram/internal/ingest"
	"github.com/ryanthedev/engram/internal/memory"
)

// Stage is the incremental-graph worker.Stage (D20): after an event's facts
// land, it derives an entity mention from each fact's Subject and (when
// present) Object — no internal/ingest edit needed, since the extraction
// stage already emits these on every SemanticFact — upserts both entities
// through the dedup routine, then upserts the edge between them. It is
// registered via Worker.RegisterStage from
// cmd/engram-server/stages_graph.go, in its own file, with no worker-core
// edits (mirrors experience.DistillStage).
//
// It is idempotent under at-least-once replay: entity dedup always
// re-resolves an identical mention to the same entity (Store.UpsertMention),
// and edge upsert is keyed deterministically on (tenant, from, predicate,
// to) — so a replayed event never grows the graph (D2: incremental, no
// recompute of anything the event didn't itself touch).
//
// A retraction fact (empty Object, per ingest.ParseExtraction's convention)
// still yields a Subject entity mention but no edge — there is nothing to
// connect, matching the fact's own "no assertion" semantics.
type Stage struct {
	store  *Store
	logger *slog.Logger
}

// NewStage returns a Stage over store. logger nil uses slog.Default().
func NewStage(store *Store, logger *slog.Logger) *Stage {
	if logger == nil {
		logger = slog.Default()
	}
	return &Stage{store: store, logger: logger}
}

// Process implements worker.Stage. Provenance/scope are taken from the
// source event (never re-derived from fact content), matching every other
// stage's convention.
//
// It receives each fact's reconciliation outcome (ingest.FactOutcome), not
// just the fact: the landed fact's triple is upserted as before, and — when
// the reconciler superseded a predecessor — that predecessor's edge is closed,
// so a corrected fact's stale relation stops being served next to the
// correction that replaced it (see closeSupersededEdge for the exclusions).
func (g *Stage) Process(ctx context.Context, ev memory.Episodic, outcomes []ingest.FactOutcome) error {
	for _, o := range outcomes {
		f := o.Fact
		if f.Subject == "" {
			continue
		}
		subjID, subjDec, err := g.store.UpsertMention(ctx, Mention{
			TenantID: f.TenantID, TeamID: f.TeamID, Scope: f.Scope, OwnerAgentID: f.OwnerAgentID,
			Name: f.Subject, Context: f.Statement, SourceID: ev.EventID,
		})
		if err != nil {
			return fmt.Errorf("graph: upserting subject mention %q for %s: %w", f.Subject, ev.EventID, err)
		}

		// A retraction fact (empty Object) asserts no relation, so it writes no
		// edge — but it still SUPERSEDES one (OpInvalidate), which is exactly
		// the case the close below exists for. edgeID stays empty: there is no
		// just-written edge to protect from the close.
		var edgeID string
		if f.Object != "" {
			objID, objDec, err := g.store.UpsertMention(ctx, Mention{
				TenantID: f.TenantID, TeamID: f.TeamID, Scope: f.Scope, OwnerAgentID: f.OwnerAgentID,
				Name: f.Object, Context: f.Statement, SourceID: ev.EventID,
			})
			if err != nil {
				return fmt.Errorf("graph: upserting object mention %q for %s: %w", f.Object, ev.EventID, err)
			}
			edgeID, err = g.store.UpsertEdge(ctx, EdgeSpec{
				TenantID: f.TenantID, TeamID: f.TeamID, Scope: f.Scope, OwnerAgentID: f.OwnerAgentID,
				FromEntityID: subjID, ToEntityID: objID, Predicate: f.Predicate, Statement: f.Statement,
				SourceID: ev.EventID, ValidAt: f.ValidAt,
			})
			if err != nil {
				return fmt.Errorf("graph: upserting edge %s-%s->%s for %s: %w", subjID, f.Predicate, objID, ev.EventID, err)
			}
			g.logger.DebugContext(ctx, "graph stage upserted",
				"event_id", ev.EventID, "edge_id", edgeID,
				"subject_merged", subjDec.Merge, "object_merged", objDec.Merge)
		}

		if err := g.closeSupersededEdge(ctx, o, edgeID); err != nil {
			return fmt.Errorf("graph: closing superseded edge for %s: %w", ev.EventID, err)
		}
	}
	return nil
}

// closeSupersededEdge retires the edge the superseded PREDECESSOR fact
// asserted, mirroring the reconciler's close of that fact in the semantic tier
// — the graph is a projection of fact lifecycle, so a fact that is no longer
// live must not keep a live edge. newEdgeID is the edge this outcome's own
// fact just wrote, or empty when it wrote none.
//
// Five exclusions, each a case where closing would be WRONG rather than merely
// unnecessary:
//
//  1. No predecessor — OpAdd, OpNoop, and OpReplayed all carry a nil one. For
//     OpReplayed this is load-bearing, not incidental: the write already landed
//     on an earlier attempt, its close (if any) already ran, and the original
//     decision is unrecoverable — so a replay must never re-apply a
//     supersession side effect.
//  2. A decision other than UPDATE/INVALIDATE — the invariant already implies
//     a nil predecessor, so this only fires if that invariant ever drifts.
//     Cheaper to hold the line here than to close an edge on a guess.
//  3. A predecessor with no Subject or no Object never produced an edge (the
//     retraction convention: an empty Object asserts nothing to connect).
//  4. The LATE-ARRIVAL case. ingest's historical-insert path reports OpUpdate
//     with a non-nil Predecessor yet deliberately does NOT close it: the fact
//     that just landed is OLDER than the one already live, so it was bounded at
//     index time and the predecessor remains the live head. Closing its edge
//     here would retire a relation that is still true. Detected exactly as
//     ingest.FactOutcome documents it: Fact.ValidAt.Before(Predecessor.ValidAt).
//  5. The predecessor's edge IS the edge we just wrote. An UPDATE that only
//     rewords its object ("billing-db" → "Billing DB") has both objects dedup
//     to one entity, so both triples fingerprint identically — closing would
//     kill the edge this very call just upserted.
//
// The predecessor's entity ids are recovered read-only (resolveEntityID), never
// re-upserted: re-mentioning them would inflate their MentionCount. An
// unresolvable subject or object means no live entity corresponds to it, hence
// no edge to close — a no-op, like the CloseEdge miss it would otherwise become.
func (g *Stage) closeSupersededEdge(ctx context.Context, o ingest.FactOutcome, newEdgeID string) error {
	p := o.Predecessor
	if p == nil { // (1) — also the nil guard for every check below
		return nil
	}
	superseding := o.Decision == ingest.OpUpdate || o.Decision == ingest.OpInvalidate
	if !superseding || // (2)
		p.Subject == "" || p.Object == "" || // (3)
		o.Fact.ValidAt.Before(p.ValidAt) { // (4) late arrival
		return nil
	}

	mention := func(name string) Mention {
		return Mention{TenantID: p.TenantID, TeamID: p.TeamID, Scope: p.Scope, OwnerAgentID: p.OwnerAgentID,
			Name: name, Context: p.Statement}
	}
	fromID, ok, err := g.store.resolveEntityID(ctx, mention(p.Subject))
	if err != nil || !ok {
		return err
	}
	toID, ok, err := g.store.resolveEntityID(ctx, mention(p.Object))
	if err != nil || !ok {
		return err
	}

	edgeID := edgeFingerprint(p.TenantID, fromID, p.Predicate, toID)
	if edgeID == newEdgeID { // (5)
		return nil
	}
	return g.store.CloseEdge(ctx, p.TenantID, edgeID)
}

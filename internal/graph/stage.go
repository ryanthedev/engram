package graph

import (
	"context"
	"fmt"
	"log/slog"

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
func (g *Stage) Process(ctx context.Context, ev memory.Episodic, facts []memory.SemanticFact) error {
	for _, f := range facts {
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
		if f.Object == "" {
			continue // retraction intent: no object to connect (mirrors ParseExtraction)
		}
		objID, objDec, err := g.store.UpsertMention(ctx, Mention{
			TenantID: f.TenantID, TeamID: f.TeamID, Scope: f.Scope, OwnerAgentID: f.OwnerAgentID,
			Name: f.Object, Context: f.Statement, SourceID: ev.EventID,
		})
		if err != nil {
			return fmt.Errorf("graph: upserting object mention %q for %s: %w", f.Object, ev.EventID, err)
		}
		edgeID, err := g.store.UpsertEdge(ctx, EdgeSpec{
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
	return nil
}

package graph

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ryanthedev/engram/internal/ingest"
	"github.com/ryanthedev/engram/internal/memory"
)

// IndexDropper drops and recreates the graph's two indices from their
// templates — the "wipe" half of a rebuild (D2/D3: the graph is derived
// data, never episodic/semantic — see graph.go's package doc). Dropping an
// index that does not exist (an empty store, or a rebuild interrupted
// after the drop but before the recreate) is a no-op, not an error —
// idempotency matters here exactly as it does for CloseEdge and Apply.
// OpenSearchIndexDropper (rebuild_opensearch.go) is the production
// implementation; a fake backs unit tests so Rebuild's orchestration is
// testable without a live cluster.
type IndexDropper interface {
	DropAndRecreate(ctx context.Context) error
}

// FactCursor resumes a FactScanner page. Its meaning belongs entirely to
// the FactScanner implementation — store.OpenSearchStore's is (created_at,
// content_key); see store.FactCursor's doc comment for why. Rebuild never
// inspects or constructs a non-zero FactCursor itself, it only round-trips
// whatever value ScanLiveFacts hands back into the next call. The zero
// value starts a scan from the beginning.
type FactCursor struct {
	CreatedAtUnixMilli int64
	ContentKey         string
}

// FactScanner is the narrow read seam Rebuild needs from the semantic
// store: one page of LIVE facts for tenantID, resuming after cursor; next
// is the zero FactCursor when the scan is exhausted. It is deliberately
// NOT internal/store.OpenSearchStore's full interface, nor even its
// concrete type — this package's architecture boundary (graph.go's package
// doc) does not import internal/store at all, so the command's wiring
// layer (cmd/engram-graph-rebuild, which is free to import both) adapts
// store.OpenSearchStore.ScanLiveFacts to this shape. A single read-only
// method is also what makes DW-3.4 (never writes episodic/semantic) a
// structural guarantee rather than a runtime check: Rebuild has no
// writer-shaped dependency on the semantic store to misuse.
type FactScanner interface {
	ScanLiveFacts(ctx context.Context, tenantID string, cursor FactCursor) (facts []memory.SemanticFact, next FactCursor, err error)
}

// RebuildReport summarizes one Rebuild run.
type RebuildReport struct {
	// FactsReplayed counts every live fact scanned and handed to
	// Stage.Process, regardless of whether it produced an edge (a
	// retraction-only fact, or one with no Subject, still counts as
	// replayed — Stage.Process itself decides what to do with it).
	FactsReplayed int
}

// Rebuild wipes the graph's derived indices and replays every LIVE
// semantic fact for tenantID through stage.Process — the exact code path
// the live write path uses (Stage.Process), so a rebuilt graph is
// indistinguishable in shape from one the write path built incrementally.
// This exists because Phase 2's write-path fix (closing a superseded
// fact's edge) is not self-healing: a zombie edge written before that fix
// landed stays live forever unless the store it lives in is dropped and
// rebuilt from current truth.
//
// Every scanned fact is replayed as a fresh ingest.OpAdd with a nil
// Predecessor. This is deliberate, not a simplification that loses
// information: FactScanner's contract (mirroring store.ScanLiveFacts) only
// ever returns LIVE facts — invalid_at and expired_at both unset — so a
// superseded fact is never in the scanned set and therefore never
// replayed. Its edge is not merely closed by the rebuild, it is never
// written at all, which is what gives DW-3.2 its guarantee. Every entity
// and edge in the rebuilt graph is freshly resolved through the real
// UpsertMention/UpsertEdge dedup path — nothing is copied or assumed
// forward from the pre-rebuild graph, matching the plan's "entity merges
// must be re-derived, not assumed" edge case.
//
// The drop runs BEFORE any replay: a rebuild that scanned first and
// dropped after would briefly serve a mix of old and new data, and a
// crash between scan and drop would leave stale + fresh edges coexisting.
// Dropping first means a crash mid-rebuild leaves the graph empty or
// partially rebuilt, never corrupted — safe to simply re-run from
// scratch, which is this command's only supported recovery (see the
// plan's "must be re-runnable from scratch, not resume-corrupted" edge
// case).
func Rebuild(ctx context.Context, dropper IndexDropper, scanner FactScanner, stage *Stage, tenantID string, logger *slog.Logger) (RebuildReport, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if tenantID == "" {
		return RebuildReport{}, fmt.Errorf("graph: rebuild requires a non-empty tenant id")
	}
	if dropper == nil || scanner == nil || stage == nil {
		return RebuildReport{}, fmt.Errorf("graph: rebuild requires a dropper, scanner, and stage")
	}

	if err := dropper.DropAndRecreate(ctx); err != nil {
		return RebuildReport{}, fmt.Errorf("graph: dropping/recreating graph indices: %w", err)
	}
	logger.InfoContext(ctx, "graph rebuild: indices dropped and recreated", "tenant_id", tenantID)

	var report RebuildReport
	var cursor FactCursor
	for {
		facts, next, err := scanner.ScanLiveFacts(ctx, tenantID, cursor)
		if err != nil {
			return report, fmt.Errorf("graph: scanning live facts (replayed %d so far): %w", report.FactsReplayed, err)
		}
		for _, f := range facts {
			ev := memory.Episodic{EventID: replaySourceID(f), TenantID: f.TenantID}
			outcome := ingest.FactOutcome{Fact: f, Decision: ingest.OpAdd}
			if err := stage.Process(ctx, ev, []ingest.FactOutcome{outcome}); err != nil {
				return report, fmt.Errorf("graph: replaying fact (content_key=%s, replayed %d so far): %w", f.ContentKey, report.FactsReplayed, err)
			}
			report.FactsReplayed++
		}
		if next == (FactCursor{}) {
			break
		}
		cursor = next
	}
	logger.InfoContext(ctx, "graph rebuild complete", "tenant_id", tenantID, "facts_replayed", report.FactsReplayed)
	return report, nil
}

// replaySourceID picks the provenance id Stage.Process records against the
// mentions/edge it derives from f during a replay: the fact's own first
// recorded source event (real provenance, preferred), falling back to its
// content key when a fact somehow carries none (defensive — every landed
// fact's SourceIDs is populated by the write path, but Rebuild must not
// panic on an empty slice from a malformed or hand-inserted doc).
func replaySourceID(f memory.SemanticFact) string {
	if len(f.SourceIDs) > 0 && f.SourceIDs[0] != "" {
		return f.SourceIDs[0]
	}
	return "rebuild:" + f.ContentKey
}

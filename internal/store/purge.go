package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// PurgeCounts reports what one PurgeEvent call removed — or, on a dry run,
// what it WOULD remove. The three tiers are counted separately because they
// are erased by different mechanisms (see PurgeEvent) and an operator reading
// the numbers needs to know which kind of removal each one describes.
type PurgeCounts struct {
	// Episodic is the number of raw episodic docs hard-deleted. It can exceed
	// one per event id: event_id does NOT deduplicate the raw episodic log
	// (D13's idempotency lives on the extraction LEDGER, keyed
	// {tenant_id, event_id, extractor_version}), so a retried Ingest leaves a
	// second doc carrying the same event_id and the purge takes both.
	Episodic int64
	// Ledger is the number of extraction-ledger rows hard-deleted. Removing
	// the row is what makes a purge undo-able-and-redo-able: with it gone, a
	// re-ingest of the same event_id re-extracts from scratch instead of
	// short-circuiting on a LedgerComplete entry.
	Ledger int64
	// Semantic is the number of semantic facts soft-deleted by stamping
	// expired_at. Facts already carrying expired_at are not re-stamped and do
	// not count — the tombstone is written once, so the original transaction-
	// time close survives a repeated purge.
	Semantic int64
}

// purgeRefreshQuery is the query string every MUTATING purge request carries.
// conflicts=proceed: a concurrent worker write bumping one doc's version must
// skip that doc, not abort the whole purge (the same reasoning as the
// knowledge sweep's). refresh=true: the operator's very next read — the
// verification that the bad data is gone — must see the result, and a purge is
// a rare, human-initiated call, so the refresh cost is irrelevant.
const purgeRefreshQuery = "refresh=true&conflicts=proceed"

// PurgeEvent erases one ingested event from the memory tier, addressed by
// (tenantID, eventID), and reports the per-tier counts. It is the deliberate,
// documented exception to the tier's append-only write discipline
// (docs/code-standards.md's "never hard-delete or blind-overwrite" rule): the
// operator escape hatch for undoing a bad bulk ingest, which is otherwise
// permanent. It is NOT the retraction path — `retract:` (internal/ingest's
// reconciler) ADDS a retracting fact and closes the predecessor's valid time;
// it never removes anything.
//
// The three tiers are erased differently, and the asymmetry is the design:
//
//   - Episodic (engram-episodic-*): HARD delete by query. The episodic tier
//     has no liveness filter anywhere — the retriever registers it with
//     supportsValidity=false, and ClaimBatch/ScanEpisodic/Read/Export all read
//     it raw — so a tombstone would simply be invisible to every reader. A
//     soft delete here would mean adding a new filter to five read paths.
//   - Ledger (engram-ledger-*): HARD delete by query. The row is pure
//     bookkeeping (D13); leaving a LedgerComplete row behind after erasing its
//     event would make a corrected re-ingest silently no-op.
//   - Semantic (engram-semantic-*): SOFT delete, stamping expired_at (the
//     bi-temporal transaction-time close, D3) on every fact whose source_ids
//     contains eventID. Every read path already excludes expired_at (facts.go's
//     liveFilterClauses, the retriever's semantic tier, the repair sweep), so
//     the tombstone takes effect immediately, while AuditFact still shows the
//     fact and its version chain. Hard-deleting instead would dangle the
//     supersedes links Audit walks. This works on already-ingested data because
//     the worker stamps exactly one source event per fact
//     (worker.stampFacts: f.SourceIDs = []string{ev.EventID}) and source_ids is
//     mapped `keyword`, hence term-queryable.
//
// KNOWN, DELIBERATE LIMITATIONS — neither is an oversight:
//
//   - The GRAPH tier (entities/edges) is not touched. Graph rows accumulate
//     source_ids from many events, so removing one event's contribution is a
//     recompute, not a delete. Rebuild it instead:
//     `go run ./cmd/engram-graph-rebuild -tenant <id> -confirm`.
//
//   - The EXPERIENCE tier is not touched. Production wires
//     experience.RuleDistiller (cmd/engram-server/stages_experience.go), which
//     admits only events carrying a literal `experience:` line; a prose bulk
//     ingest produces none. An event that DOES distill an experience will leave
//     that experience behind after a purge — use the quarantine/soft-expire
//     admin surface for those.
//
//   - Purging an event whose extraction is STILL IN FLIGHT does not stop that
//     extraction, so it can resurrect rows. A worker that claimed the episodic
//     doc before the purge writes its ledger row and semantic facts AFTER the
//     purge deleted them, leaving orphans whose source event no longer exists.
//     Observed live 2026-07-24: a purge one second post-ingest reported
//     episodic 1 / ledger 1 / semantic 0, and moments later the tier held
//     ledger 1 / semantic 1 again, the fact's created_at stamped after the
//     purge. This is inherent to purging by query against an async pipeline —
//     closing it needs the worker to re-check episodic liveness at commit
//     time, which is a worker change, not a purge change.
//
//     THE OPERATIONAL RULE: purge is idempotent and repeatable, so let
//     extraction settle, then re-run until a dry run reports zeros across all
//     three tiers. A second pass is what actually removes a raced orphan.
//
// dryRun counts the same matches with `_count` and mutates nothing. A tier
// whose index does not exist yet counts as zero, not an error (the house
// isIndexNotFound-as-empty rule). Every mutating request runs with
// conflicts=proceed so a concurrent worker write on one doc cannot abort the
// whole purge, and refresh=true so the caller's next read sees the result.
func (s *OpenSearchStore) PurgeEvent(ctx context.Context, tenantID, eventID string, dryRun bool) (PurgeCounts, error) {
	if tenantID == "" || eventID == "" {
		// An empty tenant or event id would widen the term filter to "any
		// value absent" semantics on a keyword field and, worse, invites a
		// caller to purge by an unset variable. Refuse rather than run a
		// destructive query nobody meant.
		return PurgeCounts{}, fmt.Errorf("store: purging event: tenantID and eventID must both be non-empty")
	}
	// Barricade: these index names are option-settable (WithEpisodicIndex &
	// friends, used by tests and by any future per-deployment override), so
	// they are validated before being interpolated into a REST path — the
	// Phase-3 path-traversal lesson, applied here too.
	for _, index := range []string{s.episodicIndex, s.ledgerIndex, s.semanticIndex} {
		if err := validateIndexName(index); err != nil {
			return PurgeCounts{}, err
		}
	}

	var counts PurgeCounts
	var err error
	if counts.Episodic, err = s.purgeHardDelete(ctx, s.episodicIndex, "episodic", purgeEventQuery(tenantID, eventID), dryRun); err != nil {
		return PurgeCounts{}, err
	}
	if counts.Ledger, err = s.purgeHardDelete(ctx, s.ledgerIndex, "ledger", purgeEventQuery(tenantID, eventID), dryRun); err != nil {
		return counts, err
	}
	if counts.Semantic, err = s.purgeSoftDeleteFacts(ctx, tenantID, eventID, dryRun); err != nil {
		return counts, err
	}
	return counts, nil
}

// purgeEventQuery is the (tenant_id AND event_id) term filter shared by the
// episodic and ledger tiers — both index event_id as a top-level keyword
// (templates/episodic.json, templates/ledger.json).
func purgeEventQuery(tenantID, eventID string) map[string]any {
	return map[string]any{
		"bool": map[string]any{"filter": []any{
			map[string]any{"term": map[string]any{"tenant_id": tenantID}},
			map[string]any{"term": map[string]any{"event_id": eventID}},
		}},
	}
}

// purgeFactQuery matches LIVE semantic facts (expired_at unset) extracted
// from eventID, scoped to tenantID. The must_not on expired_at is what makes
// the tombstone write-once: a repeat purge matches nothing, reports 0, and
// leaves the original transaction-time close intact.
func purgeFactQuery(tenantID, eventID string) map[string]any {
	return map[string]any{
		"bool": map[string]any{
			"filter": []any{
				map[string]any{"term": map[string]any{"tenant_id": tenantID}},
				map[string]any{"term": map[string]any{"source_ids": eventID}},
			},
			"must_not": []any{map[string]any{"exists": map[string]any{"field": "expired_at"}}},
		},
	}
}

// purgeHardDelete runs one `_delete_by_query` (or, on a dry run, one
// `_count`) against index and returns the affected/matching document count.
func (s *OpenSearchStore) purgeHardDelete(ctx context.Context, index, tier string, query map[string]any, dryRun bool) (int64, error) {
	if dryRun {
		return s.purgeCount(ctx, index, tier, query)
	}
	body, err := json.Marshal(map[string]any{"query": query})
	if err != nil {
		return 0, fmt.Errorf("store: encoding %s purge query: %w", tier, err)
	}
	url := s.baseURL + "/" + index + "/_delete_by_query?" + purgeRefreshQuery
	status, decoded, err := doJSON(ctx, s.client, http.MethodPost, url, body)
	if err != nil {
		return 0, fmt.Errorf("store: purging %s tier: %w", tier, err)
	}
	if isIndexNotFound(status, decoded) {
		return 0, nil // tier index not created yet: nothing to purge
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("store: purging %s tier: unexpected status %d: %v", tier, status, decoded)
	}
	if err := purgeFailures(tier, decoded); err != nil {
		return 0, err
	}
	deleted, _ := decoded["deleted"].(float64)
	return int64(deleted), nil
}

// purgeSoftDeleteFacts stamps expired_at on every live fact sourced from
// eventID via `_update_by_query`. The timestamp is computed here (not by
// painless's own clock) so every fact in one purge shares an identical
// transaction-time close — an operator reading the audit trail sees one
// event, not a smear.
func (s *OpenSearchStore) purgeSoftDeleteFacts(ctx context.Context, tenantID, eventID string, dryRun bool) (int64, error) {
	query := purgeFactQuery(tenantID, eventID)
	if dryRun {
		return s.purgeCount(ctx, s.semanticIndex, "semantic", query)
	}
	body, err := json.Marshal(map[string]any{
		"script": map[string]any{
			"source": "ctx._source.expired_at = params.ts;",
			"lang":   "painless",
			"params": map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano)},
		},
		"query": query,
	})
	if err != nil {
		return 0, fmt.Errorf("store: encoding semantic purge query: %w", err)
	}
	url := s.baseURL + "/" + s.semanticIndex + "/_update_by_query?" + purgeRefreshQuery
	status, decoded, err := doJSON(ctx, s.client, http.MethodPost, url, body)
	if err != nil {
		return 0, fmt.Errorf("store: purging semantic tier: %w", err)
	}
	if isIndexNotFound(status, decoded) {
		return 0, nil
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("store: purging semantic tier: unexpected status %d: %v", status, decoded)
	}
	if err := purgeFailures("semantic", decoded); err != nil {
		return 0, err
	}
	updated, _ := decoded["updated"].(float64)
	return int64(updated), nil
}

// purgeCount answers the dry run: how many docs the same query matches, with
// no mutation at all. `_count` (not `_search` size=0) because it is the
// cheapest exact answer and cannot be mistaken for a read of the documents.
func (s *OpenSearchStore) purgeCount(ctx context.Context, index, tier string, query map[string]any) (int64, error) {
	body, err := json.Marshal(map[string]any{"query": query})
	if err != nil {
		return 0, fmt.Errorf("store: encoding %s purge count: %w", tier, err)
	}
	status, decoded, err := doJSON(ctx, s.client, http.MethodPost, s.baseURL+"/"+index+"/_count", body)
	if err != nil {
		return 0, fmt.Errorf("store: counting %s purge candidates: %w", tier, err)
	}
	if isIndexNotFound(status, decoded) {
		return 0, nil
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("store: counting %s purge candidates: unexpected status %d: %v", tier, status, decoded)
	}
	count, _ := decoded["count"].(float64)
	return int64(count), nil
}

// purgeFailures surfaces a by-query response's per-document failures. A purge
// that partially failed must never read as success: the operator's whole
// reason for calling is to know the bad data is gone. Version conflicts are
// NOT in here — conflicts=proceed reports those under "version_conflicts"
// (skipped, retriable by re-running), not "failures".
func purgeFailures(tier string, decoded map[string]any) error {
	failures, _ := decoded["failures"].([]any)
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("store: purging %s tier: %d doc(s) failed (first: %v)", tier, len(failures), failures[0])
}

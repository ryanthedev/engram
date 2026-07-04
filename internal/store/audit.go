package store

import (
	"context"
	"sort"
)

// AuditFact returns the fact identified by id together with the FULL version
// history of its bi-temporal chain — every record for the same
// (tenant, subject, predicate), including valid-time-closed and record-
// retracted (expired_at set) versions, which the live-only queries exclude.
// This is the as-of contract behind `engram audit <id>` (DW-4.6): the target
// carries the provenance (who wrote it, from what — owner_agent_id, source_ids,
// extractor_version), and the ordered history shows how the fact evolved.
// ok=false means no such fact. Callers must still authorize the read (the
// server checks the caller may read the target before returning anything).
func (s *OpenSearchStore) AuditFact(ctx context.Context, id string) (target VersionedFact, versions []VersionedFact, ok bool, err error) {
	target, ok, err = s.GetFact(ctx, id)
	if err != nil || !ok {
		return VersionedFact{}, nil, ok, err
	}
	f := target.Fact
	query := map[string]any{
		"size":                1000,
		"seq_no_primary_term": true,
		"query": map[string]any{"bool": map[string]any{"filter": []any{
			map[string]any{"term": map[string]any{"tenant_id": f.TenantID}},
			map[string]any{"term": map[string]any{"subject": f.Subject}},
			map[string]any{"term": map[string]any{"predicate": f.Predicate}},
		}}},
	}
	versions, err = s.searchFacts(ctx, "reading fact history", query)
	if err != nil {
		return VersionedFact{}, nil, false, err
	}
	// Deterministic chronology: transaction time first (created_at), then valid
	// time, then doc id — the order a reader walks the fact's evolution.
	sort.SliceStable(versions, func(i, j int) bool {
		a, b := versions[i].Fact, versions[j].Fact
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		if !a.ValidAt.Equal(b.ValidAt) {
			return a.ValidAt.Before(b.ValidAt)
		}
		return versions[i].ID < versions[j].ID
	})
	return target, versions, true, nil
}

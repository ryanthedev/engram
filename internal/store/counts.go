package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Counts returns coarse per-tier document counts for tenantID plus the
// cluster's pinned version line — the probe behind the Status RPC. A blank
// tenantID counts across all tenants (operator view). Counts are best-effort
// observability, not a consistency contract, so they use the refresh-visible
// _count endpoint.
func (s *OpenSearchStore) Counts(ctx context.Context, tenantID string) (episodic, semantic int64, version string, err error) {
	version, err = clusterVersion(ctx, s.client, s.baseURL)
	if err != nil {
		return 0, 0, "", err
	}
	episodic, err = s.countTenant(ctx, s.episodicIndex, tenantID)
	if err != nil {
		return 0, 0, version, err
	}
	semantic, err = s.countTenant(ctx, s.semanticIndex, tenantID)
	if err != nil {
		return 0, 0, version, err
	}
	return episodic, semantic, version, nil
}

// countTenant runs a filtered _count on index. An empty tenantID matches all.
func (s *OpenSearchStore) countTenant(ctx context.Context, index, tenantID string) (int64, error) {
	var body []byte
	if tenantID != "" {
		b, err := json.Marshal(map[string]any{
			"query": map[string]any{"term": map[string]any{"tenant_id": tenantID}},
		})
		if err != nil {
			return 0, fmt.Errorf("store: encoding count query: %w", err)
		}
		body = b
	}
	status, decoded, err := doJSON(ctx, s.client, http.MethodPost, s.baseURL+"/"+index+"/_count", body)
	if err != nil {
		return 0, fmt.Errorf("store: counting %s: %w", index, err)
	}
	if isIndexNotFound(status, decoded) {
		return 0, nil // index not created yet: count 0
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("store: counting %s: unexpected status %d: %v", index, status, decoded)
	}
	count, _ := decoded["count"].(float64)
	return int64(count), nil
}

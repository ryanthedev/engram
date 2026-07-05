package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// PendingBacklog is the Phase-7 outbox/worker-lag telemetry source
// (DW-7.8): it counts unprocessed, non-dead-lettered episodic docs and
// returns the age of the oldest one (created_at of the earliest match). Zero
// pending docs reports a zero age. This is a read-only probe — unlike
// ClaimBatch it never claims anything, so it is safe to poll continuously
// from a telemetry loop without perturbing the outbox.
func (s *OpenSearchStore) PendingBacklog(ctx context.Context) (count int64, oldestAge time.Duration, err error) {
	query := map[string]any{
		"query": pendingQuery(),
	}
	body, err := json.Marshal(query)
	if err != nil {
		return 0, 0, fmt.Errorf("store: encoding pending-backlog count query: %w", err)
	}
	status, decoded, err := doJSON(ctx, s.client, http.MethodPost, s.baseURL+"/"+s.episodicIndex+"/_count", body)
	if err != nil {
		return 0, 0, fmt.Errorf("store: counting pending backlog: %w", err)
	}
	if isIndexNotFound(status, decoded) {
		return 0, 0, nil // episodic index not created yet: empty backlog, zero lag
	}
	if status != http.StatusOK {
		return 0, 0, fmt.Errorf("store: counting pending backlog: unexpected status %d: %v", status, decoded)
	}
	countF, _ := decoded["count"].(float64)
	count = int64(countF)
	if count == 0 {
		return 0, 0, nil
	}

	oldest := map[string]any{
		"size":    1,
		"sort":    []any{map[string]any{"created_at": "asc"}},
		"query":   pendingQuery(),
		"_source": []string{"created_at"},
	}
	body, err = json.Marshal(oldest)
	if err != nil {
		return count, 0, fmt.Errorf("store: encoding oldest-pending query: %w", err)
	}
	status, decoded, err = doJSON(ctx, s.client, http.MethodPost, s.baseURL+"/"+s.episodicIndex+"/_search", body)
	if err != nil {
		return count, 0, fmt.Errorf("store: reading oldest pending: %w", err)
	}
	if isIndexNotFound(status, decoded) {
		return count, 0, nil // index vanished between count and search: no oldest doc
	}
	if status != http.StatusOK {
		return count, 0, fmt.Errorf("store: reading oldest pending: unexpected status %d: %v", status, decoded)
	}
	hits := searchHits(decoded)
	if len(hits) == 0 {
		return count, 0, nil
	}
	var rec struct {
		CreatedAt time.Time `json:"created_at"`
	}
	if err := decodeSource(hits[0], &rec); err != nil {
		return count, 0, fmt.Errorf("store: decoding oldest pending doc: %w", err)
	}
	return count, time.Since(rec.CreatedAt), nil
}

// pendingQuery matches the same "in the outbox, not dead-lettered" set
// ClaimBatch scans, minus the lease-expiry filter (a leased-but-in-flight
// event still counts toward the backlog and its lag).
func pendingQuery() map[string]any {
	return map[string]any{
		"bool": map[string]any{
			"must_not": []any{
				map[string]any{"exists": map[string]any{"field": "processed_at"}},
				map[string]any{"term": map[string]any{"dead_lettered": true}},
			},
		},
	}
}

// DeadLetteredCount is the Phase-7 DLQ-depth telemetry source (DW-7.8): the
// current count of permanently parked episodic docs.
func (s *OpenSearchStore) DeadLetteredCount(ctx context.Context) (int64, error) {
	body, err := json.Marshal(map[string]any{
		"query": map[string]any{"term": map[string]any{"dead_lettered": true}},
	})
	if err != nil {
		return 0, fmt.Errorf("store: encoding dead-lettered count query: %w", err)
	}
	status, decoded, err := doJSON(ctx, s.client, http.MethodPost, s.baseURL+"/"+s.episodicIndex+"/_count", body)
	if err != nil {
		return 0, fmt.Errorf("store: counting dead-lettered docs: %w", err)
	}
	if isIndexNotFound(status, decoded) {
		return 0, nil // episodic index not created yet: no dead-lettered docs
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("store: counting dead-lettered docs: unexpected status %d: %v", status, decoded)
	}
	count, _ := decoded["count"].(float64)
	return int64(count), nil
}

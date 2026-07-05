package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ryanthedev/engram/internal/memory"
)

// ClaimBatch implements Store: scan-and-claim on the episodic outbox (D12).
// The scan finds unprocessed, non-dead-lettered events whose lease is absent
// or expired; each is then claimed individually with a guarded partial update
// (if_seq_no/if_primary_term), so two workers scanning the same candidates
// never both win one — the loser's 409 skips the event. Claimed events return
// with Attempts already incremented.
func (s *OpenSearchStore) ClaimBatch(ctx context.Context, n int, lease time.Duration) ([]memory.Episodic, error) {
	if n <= 0 {
		return nil, nil
	}
	query := map[string]any{
		"size":                n,
		"seq_no_primary_term": true,
		"sort":                []any{map[string]any{"created_at": "asc"}},
		"query": map[string]any{
			"bool": map[string]any{
				"must_not": []any{
					map[string]any{"exists": map[string]any{"field": "processed_at"}},
					map[string]any{"term": map[string]any{"dead_lettered": true}},
				},
				"should": []any{
					map[string]any{"bool": map[string]any{"must_not": []any{map[string]any{"exists": map[string]any{"field": "claim_lease_until"}}}}},
					map[string]any{"range": map[string]any{"claim_lease_until": map[string]any{"lt": "now"}}},
				},
				"minimum_should_match": 1,
			},
		},
	}
	body, err := json.Marshal(query)
	if err != nil {
		return nil, fmt.Errorf("store: encoding outbox scan: %w", err)
	}
	status, decoded, err := doJSON(ctx, s.client, http.MethodPost, s.baseURL+"/"+s.episodicIndex+"/_search", body)
	if err != nil {
		return nil, fmt.Errorf("store: scanning outbox: %w", err)
	}
	if isIndexNotFound(status, decoded) {
		return nil, nil // episodic index not created yet: nothing to claim
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("store: scanning outbox: unexpected status %d: %v", status, decoded)
	}

	until := time.Now().UTC().Add(lease)
	var claimed []memory.Episodic
	for _, hit := range searchHits(decoded) {
		id, _ := hit["_id"].(string)
		seqNo, _ := hit["_seq_no"].(float64)
		primaryTerm, _ := hit["_primary_term"].(float64)
		var rec memory.Episodic
		if err := decodeSource(hit, &rec); err != nil {
			return claimed, fmt.Errorf("store: decoding episodic candidate %s: %w", id, err)
		}
		won, err := s.claimOne(ctx, id, int64(seqNo), int64(primaryTerm), until, rec.Attempts+1)
		if err != nil {
			return claimed, err
		}
		if !won {
			continue // another worker claimed it first
		}
		rec.Attempts++
		rec.ClaimLeaseUntil = &until
		claimed = append(claimed, rec)
	}
	return claimed, nil
}

// claimOne attempts the guarded lease write for one episodic doc. A stale
// guard (409) reports won=false — losing a claim race is expected, not an
// error.
func (s *OpenSearchStore) claimOne(ctx context.Context, id string, seqNo, primaryTerm int64, until time.Time, attempts int) (won bool, err error) {
	body, err := json.Marshal(map[string]any{
		"doc": map[string]any{"claim_lease_until": until, "attempts": attempts},
	})
	if err != nil {
		return false, fmt.Errorf("store: encoding claim update: %w", err)
	}
	url := fmt.Sprintf("%s/%s/_update/%s?if_seq_no=%d&if_primary_term=%d&refresh=true", s.baseURL, s.episodicIndex, id, seqNo, primaryTerm)
	status, decoded, err := doJSON(ctx, s.client, http.MethodPost, url, body)
	if err != nil {
		return false, fmt.Errorf("store: claiming episodic %s: %w", id, err)
	}
	switch status {
	case http.StatusOK, http.StatusCreated:
		return true, nil
	case http.StatusConflict:
		return false, nil
	default:
		return false, fmt.Errorf("store: claiming episodic %s: unexpected status %d: %v", id, status, decoded)
	}
}

// Complete implements Store: it stamps processed_at (and clears the lease) on
// every episodic doc carrying eventID. Matching by event_id rather than doc
// _id deliberately drains replayed appends too — a client retry of Ingest
// creates a second doc with the same event_id, and both must leave the
// outbox once the ledger short-circuits the duplicate.
func (s *OpenSearchStore) Complete(ctx context.Context, eventID string) error {
	script := "ctx._source.processed_at = params.ts; ctx._source.claim_lease_until = null;"
	params := map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano)}
	return s.updateByEventID(ctx, "completing", eventID, script, params)
}

// DeadLetter implements Store: it permanently parks every episodic doc
// carrying eventID with the reason; dead-lettered docs are excluded from
// ClaimBatch and surfaced for operator review.
func (s *OpenSearchStore) DeadLetter(ctx context.Context, eventID string, reason string) error {
	script := "ctx._source.dead_lettered = true; ctx._source.dead_letter_reason = params.reason; ctx._source.claim_lease_until = null;"
	params := map[string]any{"reason": reason}
	return s.updateByEventID(ctx, "dead-lettering", eventID, script, params)
}

// updateByEventID runs one _update_by_query against every episodic doc with
// eventID. conflicts=proceed tolerates concurrent version bumps: a doc whose
// update was skipped stays in the outbox and the retry path converges it.
func (s *OpenSearchStore) updateByEventID(ctx context.Context, verb, eventID, script string, params map[string]any) error {
	if eventID == "" {
		return fmt.Errorf("store: %s event: event_id is required", verb)
	}
	body, err := json.Marshal(map[string]any{
		"script": map[string]any{"source": script, "lang": "painless", "params": params},
		"query":  map[string]any{"term": map[string]any{"event_id": eventID}},
	})
	if err != nil {
		return fmt.Errorf("store: encoding %s update: %w", verb, err)
	}
	url := s.baseURL + "/" + s.episodicIndex + "/_update_by_query?refresh=true&conflicts=proceed"
	status, decoded, err := doJSON(ctx, s.client, http.MethodPost, url, body)
	if err != nil {
		return fmt.Errorf("store: %s event %s: %w", verb, eventID, err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("store: %s event %s: unexpected status %d: %v", verb, eventID, status, decoded)
	}
	if total, ok := decoded["total"].(float64); ok && total == 0 {
		return fmt.Errorf("store: %s event %s: no episodic docs matched: %w", verb, eventID, ErrNotFound)
	}
	return nil
}

// ErrNotFound is returned when an operation targets an event or fact that
// does not exist in the store.
var ErrNotFound = errors.New("store: not found")

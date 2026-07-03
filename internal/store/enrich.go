package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/ryanthedev/engram/internal/memory"
)

// Unembedded pairs an episodic doc's OpenSearch id with its record, as
// returned by FindUnembedded for the embedding-enrichment job to fill.
type Unembedded struct {
	DocID string
	Rec   memory.Episodic
}

// FindUnembedded returns up to limit episodic docs whose text_embedding is
// still unset — append is text-first (D15); BM25 serves these immediately
// and the background enrichment job fills the vector. Oldest first, so a
// backlog drains in order.
func (s *OpenSearchStore) FindUnembedded(ctx context.Context, limit int) ([]Unembedded, error) {
	query := map[string]any{
		"size": limit,
		"sort": []any{map[string]any{"created_at": "asc"}},
		"query": map[string]any{
			"bool": map[string]any{
				"must_not": []any{
					map[string]any{"exists": map[string]any{"field": "text_embedding"}},
				},
			},
		},
	}
	body, err := json.Marshal(query)
	if err != nil {
		return nil, fmt.Errorf("store: encoding unembedded scan: %w", err)
	}
	status, decoded, err := doJSON(ctx, s.client, http.MethodPost, s.baseURL+"/"+s.episodicIndex+"/_search", body)
	if err != nil {
		return nil, fmt.Errorf("store: scanning for unembedded episodic docs: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("store: scanning for unembedded episodic docs: unexpected status %d: %v", status, decoded)
	}
	hitsField, _ := decoded["hits"].(map[string]any)
	rawHits, _ := hitsField["hits"].([]any)
	out := make([]Unembedded, 0, len(rawHits))
	for _, rh := range rawHits {
		hit, ok := rh.(map[string]any)
		if !ok {
			continue
		}
		id, _ := hit["_id"].(string)
		src, _ := hit["_source"]
		srcBytes, err := json.Marshal(src)
		if err != nil {
			return nil, fmt.Errorf("store: re-encoding _source for %s: %w", id, err)
		}
		var rec memory.Episodic
		if err := json.Unmarshal(srcBytes, &rec); err != nil {
			return nil, fmt.Errorf("store: decoding episodic record %s: %w", id, err)
		}
		out = append(out, Unembedded{DocID: id, Rec: rec})
	}
	return out, nil
}

// SetTextEmbedding partially updates the episodic doc at docID, filling
// text_embedding without touching any other field.
func (s *OpenSearchStore) SetTextEmbedding(ctx context.Context, docID string, vec []float32) error {
	body, err := json.Marshal(map[string]any{
		"doc": map[string]any{"text_embedding": vec},
	})
	if err != nil {
		return fmt.Errorf("store: encoding text_embedding update: %w", err)
	}
	url := fmt.Sprintf("%s/%s/_update/%s", s.baseURL, s.episodicIndex, docID)
	status, decoded, err := doJSON(ctx, s.client, http.MethodPost, url, body)
	if err != nil {
		return fmt.Errorf("store: setting text_embedding for %s: %w", docID, err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("store: setting text_embedding for %s: unexpected status %d: %v", docID, status, decoded)
	}
	return nil
}

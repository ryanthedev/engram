package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ryanthedev/engram/internal/auth"
)

// AuthTokenStore is the OpenSearch-backed auth.TokenStore: one doc per issued
// token, keyed by the token hash (the raw token is never stored). It reuses
// the store package's HTTP primitive and lives on the engram-auth-tokens
// index (applied by Apply). Revocation is a durable field flip; verification
// reads are point GETs by hash, so revocation is effective on the next call.
type AuthTokenStore struct {
	client  *http.Client
	baseURL string
	index   string
}

var _ auth.TokenStore = (*AuthTokenStore)(nil)

// AuthOption configures an AuthTokenStore.
type AuthOption func(*AuthTokenStore)

// WithAuthTokenIndex overrides the token index (tests use a scratch index).
func WithAuthTokenIndex(name string) AuthOption {
	return func(s *AuthTokenStore) { s.index = name }
}

// NewAuthTokenStore returns an AuthTokenStore over the cluster at baseURL.
// client must not be nil.
func NewAuthTokenStore(client *http.Client, baseURL string, opts ...AuthOption) *AuthTokenStore {
	s := &AuthTokenStore{client: client, baseURL: strings.TrimRight(baseURL, "/"), index: AuthTokenIndex}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Put stores rec keyed by its hash. Re-issuing the same hash (astronomically
// unlikely) overwrites — issuance is the only writer of a given hash.
func (s *AuthTokenStore) Put(ctx context.Context, rec auth.TokenRecord) error {
	body, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("store: encoding token record: %w", err)
	}
	url := fmt.Sprintf("%s/%s/_doc/%s?refresh=true", s.baseURL, s.index, rec.Hash)
	status, decoded, err := doJSON(ctx, s.client, http.MethodPut, url, body)
	if err != nil {
		return fmt.Errorf("store: storing token: %w", err)
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return fmt.Errorf("store: storing token: unexpected status %d: %v", status, decoded)
	}
	return nil
}

// GetByHash reads the token record whose _id is hash.
func (s *AuthTokenStore) GetByHash(ctx context.Context, hash string) (auth.TokenRecord, bool, error) {
	status, decoded, err := doJSON(ctx, s.client, http.MethodGet, s.baseURL+"/"+s.index+"/_doc/"+hash, nil)
	if err != nil {
		return auth.TokenRecord{}, false, fmt.Errorf("store: getting token: %w", err)
	}
	switch status {
	case http.StatusOK:
		rec, err := tokenFromSource(decoded)
		if err != nil {
			return auth.TokenRecord{}, false, err
		}
		return rec, true, nil
	case http.StatusNotFound:
		return auth.TokenRecord{}, false, nil
	default:
		return auth.TokenRecord{}, false, fmt.Errorf("store: getting token: unexpected status %d: %v", status, decoded)
	}
}

// Revoke flips revoked=true on the token whose public handle is id, found via
// the id field. Returns found=false when no such token exists.
func (s *AuthTokenStore) Revoke(ctx context.Context, id string) (bool, error) {
	rec, hash, ok, err := s.findByHandle(ctx, id)
	if err != nil || !ok {
		return false, err
	}
	if rec.Revoked {
		return true, nil // already revoked — idempotent
	}
	rec.Revoked = true
	now := time.Now().UTC()
	rec.RevokedAt = &now
	body, err := json.Marshal(rec)
	if err != nil {
		return false, fmt.Errorf("store: encoding revoked token: %w", err)
	}
	url := fmt.Sprintf("%s/%s/_doc/%s?refresh=true", s.baseURL, s.index, hash)
	status, decoded, err := doJSON(ctx, s.client, http.MethodPut, url, body)
	if err != nil {
		return false, fmt.Errorf("store: revoking token: %w", err)
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return false, fmt.Errorf("store: revoking token: unexpected status %d: %v", status, decoded)
	}
	return true, nil
}

// ListByIdentity returns tokens bound to id, newest first.
func (s *AuthTokenStore) ListByIdentity(ctx context.Context, id auth.Identity) ([]auth.TokenRecord, error) {
	filter := []any{
		map[string]any{"term": map[string]any{"tenant_id": id.TenantID}},
		map[string]any{"term": map[string]any{"user_id": id.UserID}},
	}
	if id.AgentID != "" {
		filter = append(filter, map[string]any{"term": map[string]any{"agent_id": id.AgentID}})
	}
	query := map[string]any{
		"size":  1000,
		"sort":  []any{map[string]any{"issued_at": "desc"}},
		"query": map[string]any{"bool": map[string]any{"filter": filter}},
	}
	recs, err := s.searchTokens(ctx, query)
	if err != nil {
		return nil, err
	}
	return recs, nil
}

// findByHandle searches the id field and returns the record + its hash (_id).
func (s *AuthTokenStore) findByHandle(ctx context.Context, handle string) (auth.TokenRecord, string, bool, error) {
	query := map[string]any{
		"size":  1,
		"query": map[string]any{"term": map[string]any{"id": handle}},
	}
	body, err := json.Marshal(query)
	if err != nil {
		return auth.TokenRecord{}, "", false, fmt.Errorf("store: encoding token lookup: %w", err)
	}
	status, decoded, err := doJSON(ctx, s.client, http.MethodPost, s.baseURL+"/"+s.index+"/_search", body)
	if err != nil {
		return auth.TokenRecord{}, "", false, fmt.Errorf("store: looking up token %s: %w", handle, err)
	}
	if status != http.StatusOK {
		return auth.TokenRecord{}, "", false, fmt.Errorf("store: looking up token %s: unexpected status %d: %v", handle, status, decoded)
	}
	hits := searchHits(decoded)
	if len(hits) == 0 {
		return auth.TokenRecord{}, "", false, nil
	}
	rec, err := tokenFromSource(hits[0])
	if err != nil {
		return auth.TokenRecord{}, "", false, err
	}
	return rec, hitID(hits[0]), true, nil
}

// searchTokens runs one _search on the token index and decodes the hits.
func (s *AuthTokenStore) searchTokens(ctx context.Context, query map[string]any) ([]auth.TokenRecord, error) {
	body, err := json.Marshal(query)
	if err != nil {
		return nil, fmt.Errorf("store: encoding token search: %w", err)
	}
	status, decoded, err := doJSON(ctx, s.client, http.MethodPost, s.baseURL+"/"+s.index+"/_search", body)
	if err != nil {
		return nil, fmt.Errorf("store: searching tokens: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("store: searching tokens: unexpected status %d: %v", status, decoded)
	}
	hits := searchHits(decoded)
	out := make([]auth.TokenRecord, 0, len(hits))
	for _, hit := range hits {
		rec, err := tokenFromSource(hit)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	// Server sort keys on issued_at; stable client sort finalizes ties.
	sort.SliceStable(out, func(i, j int) bool { return out[i].IssuedAt.After(out[j].IssuedAt) })
	return out, nil
}

// tokenFromSource decodes the _source of a GET/_search doc into a TokenRecord.
func tokenFromSource(doc map[string]any) (auth.TokenRecord, error) {
	src, ok := doc["_source"].(map[string]any)
	if !ok {
		return auth.TokenRecord{}, fmt.Errorf("store: token doc has no _source")
	}
	raw, err := json.Marshal(src)
	if err != nil {
		return auth.TokenRecord{}, fmt.Errorf("store: re-encoding token source: %w", err)
	}
	var rec auth.TokenRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return auth.TokenRecord{}, fmt.Errorf("store: decoding token record: %w", err)
	}
	return rec, nil
}

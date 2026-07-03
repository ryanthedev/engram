package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ryanthedev/engram/internal/memory"
)

// ErrNotImplemented is returned by the OpenSearchStore outbox and ledger
// methods (ClaimBatch, Complete, DeadLetter, ClaimLedger, UpdateLedger,
// ScanIncomplete). Their storage design (D12/D13 — the ledger index shape,
// scan-and-claim query, lease semantics) is Phase 2 scope ("the outbox
// worker pool" and "the extraction ledger" are Phase 2 Produces items in the
// plan); Phase 1 needs only Append/Create/Update for the read/write paths it
// implements. The method set stays complete so OpenSearchStore satisfies
// Store today, and Phase 2 fills these bodies in without touching the
// signatures.
var ErrNotImplemented = errors.New("store: not implemented until Phase 2 (D12/D13 outbox + ledger design)")

// Option configures an OpenSearchStore constructed by NewOpenSearchStore.
type Option func(*OpenSearchStore)

// WithEpisodicIndex overrides the episodic index OpenSearchStore writes to.
// Defaults to EpisodicIndex; tests use a scratch index matching the
// engram-episodic* template pattern so they can't collide with other tests
// or with production data sharing the same cluster.
func WithEpisodicIndex(name string) Option {
	return func(s *OpenSearchStore) { s.episodicIndex = name }
}

// WithSemanticIndex overrides the semantic index OpenSearchStore writes to.
// Defaults to SemanticIndex; see WithEpisodicIndex.
func WithSemanticIndex(name string) Option {
	return func(s *OpenSearchStore) { s.semanticIndex = name }
}

// OpenSearchStore is the Store implementation over OpenSearch (Phase 1). The
// write protocol primitives it uses — op_type=create for Create, guarded
// if_seq_no/if_primary_term for Update — were proven against the live pinned
// cluster by the Phase-0 spikes.
type OpenSearchStore struct {
	client        *http.Client
	baseURL       string
	episodicIndex string
	semanticIndex string
}

var _ Store = (*OpenSearchStore)(nil)

// NewOpenSearchStore returns a Store backed by the OpenSearch cluster at
// baseURL, writing to the production episodic/semantic indices unless
// overridden by options. client must not be nil.
func NewOpenSearchStore(client *http.Client, baseURL string, opts ...Option) *OpenSearchStore {
	s := &OpenSearchStore{
		client:        client,
		baseURL:       strings.TrimRight(baseURL, "/"),
		episodicIndex: EpisodicIndex,
		semanticIndex: SemanticIndex,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Append implements Store: it POSTs rec to the episodic index and lets
// OpenSearch assign the doc id (episodic append has no content-addressing
// contract — D11 only pins the semantic fact id scheme). The append is the
// enqueue (D12): once this returns, rec exists in the durable log Phase 2's
// worker scans.
func (s *OpenSearchStore) Append(ctx context.Context, rec memory.Episodic) (string, error) {
	body, err := json.Marshal(rec)
	if err != nil {
		return "", fmt.Errorf("store: encoding episodic record: %w", err)
	}
	status, decoded, err := doJSON(ctx, s.client, http.MethodPost, s.baseURL+"/"+s.episodicIndex+"/_doc", body)
	if err != nil {
		return "", fmt.Errorf("store: appending episodic record: %w", err)
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return "", fmt.Errorf("store: appending episodic record: unexpected status %d: %v", status, decoded)
	}
	id, _ := decoded["_id"].(string)
	if id == "" {
		return "", fmt.Errorf("store: append response carried no _id: %v", decoded)
	}
	return id, nil
}

// Create implements Store: op_type=create under the given doc id. An
// existing id collides with 409, mapped to ErrConflict (D11's ADD-vs-ADD
// duplicate-extraction race).
func (s *OpenSearchStore) Create(ctx context.Context, id string, f memory.SemanticFact) error {
	body, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("store: encoding semantic fact: %w", err)
	}
	url := fmt.Sprintf("%s/%s/_create/%s", s.baseURL, s.semanticIndex, id)
	status, decoded, err := doJSON(ctx, s.client, http.MethodPut, url, body)
	if err != nil {
		return fmt.Errorf("store: creating semantic fact %s: %w", id, err)
	}
	switch status {
	case http.StatusCreated, http.StatusOK:
		return nil
	case http.StatusConflict:
		return fmt.Errorf("store: creating semantic fact %s: %w", id, ErrConflict)
	default:
		return fmt.Errorf("store: creating semantic fact %s: unexpected status %d: %v", id, status, decoded)
	}
}

// Update implements Store: a guarded rewrite of the fact at id. A stale
// if_seq_no/if_primary_term guard loses with 409, mapped to ErrConflict
// (D7); callers re-read and retry with bounded attempts.
func (s *OpenSearchStore) Update(ctx context.Context, id string, f memory.SemanticFact, ifSeqNo, ifPrimaryTerm int64) error {
	body, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("store: encoding semantic fact: %w", err)
	}
	url := fmt.Sprintf("%s/%s/_doc/%s?if_seq_no=%d&if_primary_term=%d", s.baseURL, s.semanticIndex, id, ifSeqNo, ifPrimaryTerm)
	status, decoded, err := doJSON(ctx, s.client, http.MethodPut, url, body)
	if err != nil {
		return fmt.Errorf("store: updating semantic fact %s: %w", id, err)
	}
	switch status {
	case http.StatusOK, http.StatusCreated:
		return nil
	case http.StatusConflict:
		return fmt.Errorf("store: updating semantic fact %s: %w", id, ErrConflict)
	default:
		return fmt.Errorf("store: updating semantic fact %s: unexpected status %d: %v", id, status, decoded)
	}
}

// ClaimBatch is not implemented in Phase 1; see ErrNotImplemented.
func (s *OpenSearchStore) ClaimBatch(context.Context, int, time.Duration) ([]memory.Episodic, error) {
	return nil, ErrNotImplemented
}

// Complete is not implemented in Phase 1; see ErrNotImplemented.
func (s *OpenSearchStore) Complete(context.Context, string) error {
	return ErrNotImplemented
}

// DeadLetter is not implemented in Phase 1; see ErrNotImplemented.
func (s *OpenSearchStore) DeadLetter(context.Context, string, string) error {
	return ErrNotImplemented
}

// ClaimLedger is not implemented in Phase 1; see ErrNotImplemented.
func (s *OpenSearchStore) ClaimLedger(context.Context, memory.LedgerKey) (LedgerEntry, error) {
	return LedgerEntry{}, ErrNotImplemented
}

// UpdateLedger is not implemented in Phase 1; see ErrNotImplemented.
func (s *OpenSearchStore) UpdateLedger(context.Context, memory.LedgerKey, LedgerState) error {
	return ErrNotImplemented
}

// ScanIncomplete is not implemented in Phase 1; see ErrNotImplemented.
func (s *OpenSearchStore) ScanIncomplete(context.Context) ([]LedgerEntry, error) {
	return nil, ErrNotImplemented
}

// doJSON issues one JSON request and returns the status code and decoded
// body. It is the Store package's shared HTTP primitive, alongside apply.go's
// do (which discards the body — Apply never needs it).
func doJSON(ctx context.Context, client *http.Client, method, url string, body []byte) (int, map[string]any, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	var decoded map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &decoded)
	}
	return resp.StatusCode, decoded, nil
}

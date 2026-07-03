package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/ryanthedev/engram/internal/memory"
)

// DefaultLedgerLease bounds how long a claim holder may work on one ledger
// entry before ScanIncomplete considers the run crashed (D13). UpdateLedger
// renews it on every state transition, so a live run never expires mid-work.
const DefaultLedgerLease = 2 * time.Minute

// WithLedgerIndex overrides the extraction-ledger index. Defaults to
// LedgerIndex; see WithEpisodicIndex.
func WithLedgerIndex(name string) Option {
	return func(s *OpenSearchStore) { s.ledgerIndex = name }
}

// WithLedgerLease overrides the ledger lease duration. Defaults to
// DefaultLedgerLease; tests shrink it to exercise crash-resume paths quickly.
func WithLedgerLease(d time.Duration) Option {
	return func(s *OpenSearchStore) { s.ledgerLease = d }
}

// ledgerDoc is the flat OpenSearch document shape of one ledger entry
// (templates/ledger.json). Extraction marshals to base64, matching the
// mapping's binary type.
type ledgerDoc struct {
	TenantID         string      `json:"tenant_id"`
	EventID          string      `json:"event_id"`
	ExtractorVersion string      `json:"extractor_version"`
	Phase            LedgerPhase `json:"phase"`
	Extraction       []byte      `json:"extraction,omitempty"`
	CompletedActions []string    `json:"completed_actions,omitempty"`
	LeaseUntil       time.Time   `json:"lease_until"`
}

func (d ledgerDoc) entry() LedgerEntry {
	return LedgerEntry{
		Key:        memory.LedgerKey{TenantID: d.TenantID, EventID: d.EventID, ExtractorVersion: d.ExtractorVersion},
		State:      LedgerState{Phase: d.Phase, Extraction: d.Extraction, CompletedActions: d.CompletedActions},
		LeaseUntil: d.LeaseUntil,
	}
}

// ClaimLedger implements Store: it claims the entry for key with
// op_type=create (claim-first, D13). A 409 means the entry already exists —
// a replay or a crashed run — so the existing entry is fetched (realtime GET,
// not refresh-dependent) and returned with Claimed=false for the caller to
// resume from.
func (s *OpenSearchStore) ClaimLedger(ctx context.Context, key memory.LedgerKey) (LedgerEntry, error) {
	id := key.DocID()
	lease := time.Now().UTC().Add(s.ledgerLease)
	doc := ledgerDoc{
		TenantID:         key.TenantID,
		EventID:          key.EventID,
		ExtractorVersion: key.ExtractorVersion,
		Phase:            LedgerClaimed,
		LeaseUntil:       lease,
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return LedgerEntry{}, fmt.Errorf("store: encoding ledger claim: %w", err)
	}
	url := fmt.Sprintf("%s/%s/_create/%s?refresh=true", s.baseURL, s.ledgerIndex, id)
	status, decoded, err := doJSON(ctx, s.client, http.MethodPut, url, body)
	if err != nil {
		return LedgerEntry{}, fmt.Errorf("store: claiming ledger entry %s: %w", id, err)
	}
	switch status {
	case http.StatusCreated, http.StatusOK:
		e := doc.entry()
		e.Claimed = true
		return e, nil
	case http.StatusConflict:
		e, err := s.getLedger(ctx, id)
		if err != nil {
			return LedgerEntry{}, fmt.Errorf("store: reading existing ledger entry %s after claim conflict: %w", id, err)
		}
		return e, nil
	default:
		return LedgerEntry{}, fmt.Errorf("store: claiming ledger entry %s: unexpected status %d: %v", id, status, decoded)
	}
}

// UpdateLedger implements Store: it persists the entry's state transition and
// renews the lease. The write is unguarded — the outbox lease serializes
// per-event workers, and the write protocol itself (content-addressed create
// + guarded close) converges the bounded double-work a lease-expired race can
// cause; it never corrupts.
func (s *OpenSearchStore) UpdateLedger(ctx context.Context, key memory.LedgerKey, state LedgerState) error {
	id := key.DocID()
	doc := ledgerDoc{
		TenantID:         key.TenantID,
		EventID:          key.EventID,
		ExtractorVersion: key.ExtractorVersion,
		Phase:            state.Phase,
		Extraction:       state.Extraction,
		CompletedActions: state.CompletedActions,
		LeaseUntil:       time.Now().UTC().Add(s.ledgerLease),
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("store: encoding ledger state: %w", err)
	}
	url := fmt.Sprintf("%s/%s/_doc/%s?refresh=true", s.baseURL, s.ledgerIndex, id)
	status, decoded, err := doJSON(ctx, s.client, http.MethodPut, url, body)
	if err != nil {
		return fmt.Errorf("store: updating ledger entry %s: %w", id, err)
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return fmt.Errorf("store: updating ledger entry %s: unexpected status %d: %v", id, status, decoded)
	}
	return nil
}

// ScanIncomplete implements Store: ledger entries past their lease and not
// LedgerComplete — crashed runs the repair sweep resumes (D10).
func (s *OpenSearchStore) ScanIncomplete(ctx context.Context) ([]LedgerEntry, error) {
	query := map[string]any{
		"size": 100,
		"sort": []any{map[string]any{"lease_until": "asc"}},
		"query": map[string]any{
			"bool": map[string]any{
				"must_not": []any{map[string]any{"term": map[string]any{"phase": string(LedgerComplete)}}},
				"filter":   []any{map[string]any{"range": map[string]any{"lease_until": map[string]any{"lt": "now"}}}},
			},
		},
	}
	body, err := json.Marshal(query)
	if err != nil {
		return nil, fmt.Errorf("store: encoding incomplete-ledger scan: %w", err)
	}
	status, decoded, err := doJSON(ctx, s.client, http.MethodPost, s.baseURL+"/"+s.ledgerIndex+"/_search", body)
	if err != nil {
		return nil, fmt.Errorf("store: scanning incomplete ledger entries: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("store: scanning incomplete ledger entries: unexpected status %d: %v", status, decoded)
	}
	var out []LedgerEntry
	for _, hit := range searchHits(decoded) {
		var doc ledgerDoc
		if err := decodeSource(hit, &doc); err != nil {
			return nil, fmt.Errorf("store: decoding ledger entry: %w", err)
		}
		out = append(out, doc.entry())
	}
	return out, nil
}

// getLedger fetches one ledger entry by doc id via realtime GET.
func (s *OpenSearchStore) getLedger(ctx context.Context, id string) (LedgerEntry, error) {
	status, decoded, err := doJSON(ctx, s.client, http.MethodGet, s.baseURL+"/"+s.ledgerIndex+"/_doc/"+id, nil)
	if err != nil {
		return LedgerEntry{}, err
	}
	if status != http.StatusOK {
		return LedgerEntry{}, fmt.Errorf("unexpected status %d: %v", status, decoded)
	}
	var doc ledgerDoc
	if err := decodeSource(decoded, &doc); err != nil {
		return LedgerEntry{}, err
	}
	return doc.entry(), nil
}

// searchHits extracts the hit maps from a decoded _search response.
func searchHits(decoded map[string]any) []map[string]any {
	hitsField, _ := decoded["hits"].(map[string]any)
	rawHits, _ := hitsField["hits"].([]any)
	out := make([]map[string]any, 0, len(rawHits))
	for _, rh := range rawHits {
		if hm, ok := rh.(map[string]any); ok {
			out = append(out, hm)
		}
	}
	return out
}

// decodeSource re-decodes a hit's (or GET response's) _source into dst.
func decodeSource(hit map[string]any, dst any) error {
	src, ok := hit["_source"]
	if !ok {
		return fmt.Errorf("response carries no _source: %v", hit)
	}
	raw, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, dst)
}

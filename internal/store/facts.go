package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/ryanthedev/engram/internal/memory"
)

// VersionedFact pairs a semantic fact with its doc _id and the
// optimistic-concurrency tokens (D7) a guarded close needs.
type VersionedFact struct {
	ID          string
	Fact        memory.SemanticFact
	SeqNo       int64
	PrimaryTerm int64
}

// GetFact fetches one semantic fact by doc id via realtime GET — visibility
// does NOT depend on index refresh, which is what makes 409 re-reads and
// in-batch candidate merging deterministic. ok=false means no such doc.
func (s *OpenSearchStore) GetFact(ctx context.Context, id string) (vf VersionedFact, ok bool, err error) {
	status, decoded, err := doJSON(ctx, s.client, http.MethodGet, s.baseURL+"/"+s.semanticIndex+"/_doc/"+id, nil)
	if err != nil {
		return VersionedFact{}, false, fmt.Errorf("store: getting fact %s: %w", id, err)
	}
	switch status {
	case http.StatusOK:
		vf, err := versionedFromGet(id, decoded)
		if err != nil {
			return VersionedFact{}, false, fmt.Errorf("store: decoding fact %s: %w", id, err)
		}
		return vf, true, nil
	case http.StatusNotFound:
		return VersionedFact{}, false, nil
	default:
		return VersionedFact{}, false, fmt.Errorf("store: getting fact %s: unexpected status %d: %v", id, status, decoded)
	}
}

// GetEpisodic fetches one episodic record by doc _id via realtime GET —
// mirrors GetFact on the episodic index. It returns the FULL stored record
// including its ACL/provenance fields: the server's Read handler authorizes
// against those fields BEFORE projecting them away (fetch -> authorize ->
// project), exactly as AuditFact's caller does. ok=false means no such doc
// (a semantic id asked for here lands in this branch — the getter never
// probes another index).
func (s *OpenSearchStore) GetEpisodic(ctx context.Context, id string) (rec memory.Episodic, ok bool, err error) {
	status, decoded, err := doJSON(ctx, s.client, http.MethodGet, s.baseURL+"/"+s.episodicIndex+"/_doc/"+id, nil)
	if err != nil {
		return memory.Episodic{}, false, fmt.Errorf("store: getting episodic %s: %w", id, err)
	}
	switch status {
	case http.StatusOK:
		if err := decodeSource(decoded, &rec); err != nil {
			return memory.Episodic{}, false, fmt.Errorf("store: decoding episodic %s: %w", id, err)
		}
		return rec, true, nil
	case http.StatusNotFound:
		return memory.Episodic{}, false, nil
	default:
		return memory.Episodic{}, false, fmt.Errorf("store: getting episodic %s: unexpected status %d: %v", id, status, decoded)
	}
}

// Candidates returns up to k live semantic facts relevant to f for
// reconciliation: exact content-key matches rank first, then same
// subject+predicate assertions, then statement text matches — all
// tenant-scoped and live-only (invalid_at and expired_at unset). Results
// carry seq_no/primary_term for the guarded close.
func (s *OpenSearchStore) Candidates(ctx context.Context, f memory.SemanticFact, k int) ([]VersionedFact, error) {
	if k <= 0 {
		k = 10
	}
	should := []any{
		map[string]any{"term": map[string]any{"content_key": map[string]any{"value": f.ContentKey, "boost": 3.0}}},
		map[string]any{"bool": map[string]any{
			"must": []any{
				map[string]any{"term": map[string]any{"subject": f.Subject}},
				map[string]any{"term": map[string]any{"predicate": f.Predicate}},
			},
			"boost": 2.0,
		}},
	}
	if f.Statement != "" {
		should = append(should, map[string]any{"match": map[string]any{"statement": f.Statement}})
	}
	query := map[string]any{
		"size":                k,
		"seq_no_primary_term": true,
		"query": map[string]any{
			"bool": map[string]any{
				"filter":               append(liveFilterClauses(), map[string]any{"term": map[string]any{"tenant_id": f.TenantID}}),
				"should":               should,
				"minimum_should_match": 1,
			},
		},
	}
	return s.searchFacts(ctx, "retrieving candidates", query)
}

// ValidTimeNeighbors returns f's nearest chain versions by valid time for
// (tenant, subject, predicate), INCLUDING valid-time-closed history — the
// read D10's neighbor-aware insertion needs (bounding a late arrival against
// only the live head overlaps closed intervals once a chain has ≥2 prior
// versions). pred is the latest version with valid_at ≤ f.ValidAt, succ the
// earliest with valid_at > f.ValidAt; either may be nil. Record-retracted
// rows (expired_at set) and f's own doc (selfID) are excluded. Ties break
// deterministically by (valid_at, doc id).
func (s *OpenSearchStore) ValidTimeNeighbors(ctx context.Context, f memory.SemanticFact, selfID string) (pred, succ *VersionedFact, err error) {
	chainFilter := []any{
		map[string]any{"term": map[string]any{"tenant_id": f.TenantID}},
		map[string]any{"term": map[string]any{"subject": f.Subject}},
		map[string]any{"term": map[string]any{"predicate": f.Predicate}},
		map[string]any{"bool": map[string]any{"must_not": []any{map[string]any{"exists": map[string]any{"field": "expired_at"}}}}},
	}
	at := f.ValidAt.UTC().Format(time.RFC3339Nano)
	query := func(rangeClause map[string]any, order string) map[string]any {
		return map[string]any{
			// Overfetch a few so equal-valid_at ties resolve client-side by
			// (valid_at, doc id) — the _id field is not sortable server-side.
			"size":                4,
			"seq_no_primary_term": true,
			"sort":                []any{map[string]any{"valid_at": order}},
			"query": map[string]any{
				"bool": map[string]any{
					"filter":   append(append([]any{}, chainFilter...), map[string]any{"range": map[string]any{"valid_at": rangeClause}}),
					"must_not": []any{map[string]any{"ids": map[string]any{"values": []string{selfID}}}},
				},
			},
		}
	}

	preds, err := s.searchFacts(ctx, "reading valid-time predecessor", query(map[string]any{"lte": at}, "desc"))
	if err != nil {
		return nil, nil, err
	}
	succs, err := s.searchFacts(ctx, "reading valid-time successor", query(map[string]any{"gt": at}, "asc"))
	if err != nil {
		return nil, nil, err
	}
	for i := range preds {
		if pred == nil || laterByValidThenID(preds[i], *pred) {
			pred = &preds[i]
		}
	}
	for i := range succs {
		if succ == nil || laterByValidThenID(*succ, succs[i]) {
			succ = &succs[i]
		}
	}
	return pred, succ, nil
}

// laterByValidThenID reports whether a sorts after b in the deterministic
// (valid_at, doc id) total order.
func laterByValidThenID(a, b VersionedFact) bool {
	if !a.Fact.ValidAt.Equal(b.Fact.ValidAt) {
		return a.Fact.ValidAt.After(b.Fact.ValidAt)
	}
	return a.ID > b.ID
}

// LiveSuperseders returns up to limit live facts whose supersedes link is
// set — the repair sweep's rule (a)/(a') input: chains whose predecessor
// close may still be pending (D10).
func (s *OpenSearchStore) LiveSuperseders(ctx context.Context, limit int) ([]VersionedFact, error) {
	if limit <= 0 {
		limit = 500
	}
	query := map[string]any{
		"size":                limit,
		"seq_no_primary_term": true,
		"query": map[string]any{
			"bool": map[string]any{
				"filter": append(liveFilterClauses(), map[string]any{"exists": map[string]any{"field": "supersedes"}}),
			},
		},
	}
	return s.searchFacts(ctx, "scanning live superseders", query)
}

// DuplicateLiveContentKeys returns content keys carried by more than one live
// fact — the repair sweep's rule (b) input (D10: keep the earliest, close the
// rest).
func (s *OpenSearchStore) DuplicateLiveContentKeys(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 100
	}
	query := map[string]any{
		"size":  0,
		"query": map[string]any{"bool": map[string]any{"filter": liveFilterClauses()}},
		"aggs": map[string]any{
			"dups": map[string]any{
				"terms": map[string]any{"field": "content_key", "min_doc_count": 2, "size": limit},
			},
		},
	}
	body, err := json.Marshal(query)
	if err != nil {
		return nil, fmt.Errorf("store: encoding duplicate-key scan: %w", err)
	}
	status, decoded, err := doJSON(ctx, s.client, http.MethodPost, s.baseURL+"/"+s.semanticIndex+"/_search", body)
	if err != nil {
		return nil, fmt.Errorf("store: scanning duplicate content keys: %w", err)
	}
	if isIndexNotFound(status, decoded) {
		return nil, nil // semantic index not created yet: no duplicates
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("store: scanning duplicate content keys: unexpected status %d: %v", status, decoded)
	}
	aggs, _ := decoded["aggregations"].(map[string]any)
	dups, _ := aggs["dups"].(map[string]any)
	buckets, _ := dups["buckets"].([]any)
	var keys []string
	for _, b := range buckets {
		bm, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if key, _ := bm["key"].(string); key != "" {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

// LiveByContentKey returns every live fact carrying key.
func (s *OpenSearchStore) LiveByContentKey(ctx context.Context, key string) ([]VersionedFact, error) {
	query := map[string]any{
		"size":                100,
		"seq_no_primary_term": true,
		"query": map[string]any{
			"bool": map[string]any{
				"filter": append(liveFilterClauses(), map[string]any{"term": map[string]any{"content_key": key}}),
			},
		},
	}
	return s.searchFacts(ctx, "scanning live facts by content key", query)
}

// FindByEventID returns every episodic doc carrying eventID (a replayed
// Ingest appends duplicates) — the repair sweep's rule (c) input for
// resuming a crashed run whose extraction was never cached.
func (s *OpenSearchStore) FindByEventID(ctx context.Context, eventID string) ([]memory.Episodic, error) {
	query := map[string]any{
		"size":  10,
		"query": map[string]any{"term": map[string]any{"event_id": eventID}},
	}
	body, err := json.Marshal(query)
	if err != nil {
		return nil, fmt.Errorf("store: encoding event lookup: %w", err)
	}
	status, decoded, err := doJSON(ctx, s.client, http.MethodPost, s.baseURL+"/"+s.episodicIndex+"/_search", body)
	if err != nil {
		return nil, fmt.Errorf("store: finding event %s: %w", eventID, err)
	}
	if isIndexNotFound(status, decoded) {
		return nil, nil // episodic index not created yet: no matching events
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("store: finding event %s: unexpected status %d: %v", eventID, status, decoded)
	}
	var out []memory.Episodic
	for _, hit := range searchHits(decoded) {
		var rec memory.Episodic
		if err := decodeSource(hit, &rec); err != nil {
			return nil, fmt.Errorf("store: decoding episodic for event %s: %w", eventID, err)
		}
		out = append(out, rec)
	}
	return out, nil
}

// ChainKey identifies one bi-temporal chain: the (tenant, subject, predicate)
// group whose versions share a valid-time timeline. It is the unit repair
// sweep rule (d) converges — closed-closed valid-time overlaps within a chain.
type ChainKey struct {
	TenantID  string
	Subject   string
	Predicate string
}

// ClosedOverlapChainKeys returns up to limit (tenant, subject, predicate)
// chains that carry ≥2 non-expired records — the chains where a closed-closed
// valid-time overlap (sweep rule d) is possible. It over-returns disjoint
// chains (any chain with a live head plus one closed version qualifies); the
// sweep loads each chain's versions and no-ops when already disjoint, so this
// scan only bounds work, it does not decide the repair.
func (s *OpenSearchStore) ClosedOverlapChainKeys(ctx context.Context, limit int) ([]ChainKey, error) {
	if limit <= 0 {
		limit = 500
	}
	query := map[string]any{
		"size": 0,
		"query": map[string]any{"bool": map[string]any{
			"must_not": []any{map[string]any{"exists": map[string]any{"field": "expired_at"}}},
		}},
		"aggs": map[string]any{
			"chains": map[string]any{
				"composite": map[string]any{
					"size": limit,
					"sources": []any{
						map[string]any{"tenant_id": map[string]any{"terms": map[string]any{"field": "tenant_id"}}},
						map[string]any{"subject": map[string]any{"terms": map[string]any{"field": "subject"}}},
						map[string]any{"predicate": map[string]any{"terms": map[string]any{"field": "predicate"}}},
					},
				},
				"aggs": map[string]any{
					"versions": map[string]any{"bucket_selector": map[string]any{
						"buckets_path": map[string]any{"c": "_count"},
						"script":       "params.c > 1",
					}},
				},
			},
		},
	}
	body, err := json.Marshal(query)
	if err != nil {
		return nil, fmt.Errorf("store: encoding chain scan: %w", err)
	}
	status, decoded, err := doJSON(ctx, s.client, http.MethodPost, s.baseURL+"/"+s.semanticIndex+"/_search", body)
	if err != nil {
		return nil, fmt.Errorf("store: scanning overlap chains: %w", err)
	}
	if isIndexNotFound(status, decoded) {
		return nil, nil // semantic index not created yet: no overlap chains
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("store: scanning overlap chains: unexpected status %d: %v", status, decoded)
	}
	aggs, _ := decoded["aggregations"].(map[string]any)
	chains, _ := aggs["chains"].(map[string]any)
	buckets, _ := chains["buckets"].([]any)
	var keys []ChainKey
	for _, b := range buckets {
		bm, ok := b.(map[string]any)
		if !ok {
			continue
		}
		km, _ := bm["key"].(map[string]any)
		tenant, _ := km["tenant_id"].(string)
		subject, _ := km["subject"].(string)
		predicate, _ := km["predicate"].(string)
		if tenant != "" && subject != "" && predicate != "" {
			keys = append(keys, ChainKey{TenantID: tenant, Subject: subject, Predicate: predicate})
		}
	}
	return keys, nil
}

// ChainVersions returns every non-expired record for key (live and
// valid-time-closed), ascending by (valid_at, doc id) — the input sweep rule
// (d) walks to detect and trim closed-closed overlaps. Record-retracted rows
// (expired_at set) are excluded: they are not truth boundaries.
func (s *OpenSearchStore) ChainVersions(ctx context.Context, key ChainKey) ([]VersionedFact, error) {
	query := map[string]any{
		"size":                1000,
		"seq_no_primary_term": true,
		"sort":                []any{map[string]any{"valid_at": "asc"}},
		"query": map[string]any{"bool": map[string]any{
			"filter": []any{
				map[string]any{"term": map[string]any{"tenant_id": key.TenantID}},
				map[string]any{"term": map[string]any{"subject": key.Subject}},
				map[string]any{"term": map[string]any{"predicate": key.Predicate}},
			},
			"must_not": []any{map[string]any{"exists": map[string]any{"field": "expired_at"}}},
		}},
	}
	facts, err := s.searchFacts(ctx, "reading chain versions", query)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(facts, func(i, j int) bool {
		if !facts[i].Fact.ValidAt.Equal(facts[j].Fact.ValidAt) {
			return facts[i].Fact.ValidAt.Before(facts[j].Fact.ValidAt)
		}
		return facts[i].ID < facts[j].ID
	})
	return facts, nil
}

// liveFilterClauses is the live-fact filter: invalid_at and expired_at both
// unset (the current-state query's open-interval case).
func liveFilterClauses() []any {
	return []any{
		map[string]any{"bool": map[string]any{"must_not": []any{map[string]any{"exists": map[string]any{"field": "invalid_at"}}}}},
		map[string]any{"bool": map[string]any{"must_not": []any{map[string]any{"exists": map[string]any{"field": "expired_at"}}}}},
	}
}

// searchFacts runs one _search on the semantic index and decodes the hits as
// VersionedFacts.
func (s *OpenSearchStore) searchFacts(ctx context.Context, verb string, query map[string]any) ([]VersionedFact, error) {
	body, err := json.Marshal(query)
	if err != nil {
		return nil, fmt.Errorf("store: encoding %s query: %w", verb, err)
	}
	status, decoded, err := doJSON(ctx, s.client, http.MethodPost, s.baseURL+"/"+s.semanticIndex+"/_search", body)
	if err != nil {
		return nil, fmt.Errorf("store: %s: %w", verb, err)
	}
	if isIndexNotFound(status, decoded) {
		return nil, nil // the semantic index does not exist yet: no facts, not an error
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("store: %s: unexpected status %d: %v", verb, status, decoded)
	}
	var out []VersionedFact
	for _, hit := range searchHits(decoded) {
		vf, err := versionedFromGet(hitID(hit), hit)
		if err != nil {
			return nil, fmt.Errorf("store: %s: decoding hit: %w", verb, err)
		}
		out = append(out, vf)
	}
	return out, nil
}

func hitID(hit map[string]any) string {
	id, _ := hit["_id"].(string)
	return id
}

// versionedFromGet decodes one GET/_search doc map into a VersionedFact.
func versionedFromGet(id string, doc map[string]any) (VersionedFact, error) {
	var f memory.SemanticFact
	if err := decodeSource(doc, &f); err != nil {
		return VersionedFact{}, err
	}
	seqNo, _ := doc["_seq_no"].(float64)
	primaryTerm, _ := doc["_primary_term"].(float64)
	return VersionedFact{ID: id, Fact: f, SeqNo: int64(seqNo), PrimaryTerm: int64(primaryTerm)}, nil
}

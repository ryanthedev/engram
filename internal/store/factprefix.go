package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// ResolveFactIDPrefix returns the semantic doc _ids that begin with prefix and
// match every clause in filters, in ascending _id order, at most limit of them.
// It is the by-prefix half of memory_read: callers see a semantic id as its
// first few hex characters, and this maps that back to the full sha256 _id
// (memory.FactDocID) so the ordinary by-id read can run.
//
// WHY A SEEK, NOT A QUERY. OpenSearch 3.1 refuses every text-shaped query on
// _id — prefix, wildcard, regexp, match_phrase_prefix and range all 400 with
// "Can only use ... queries on keyword and text fields" / "does not support
// range queries", and query_string `_id:abc*` silently matches nothing — and
// the semantic mapping is dynamic:strict with no keyword copy of the id. What
// _id DOES support is sorting, so this sorts on _id ascending and seeks with
// search_after:[prefix]: every id carrying the prefix sorts at or after it and
// before the first id that does not, so the matches are exactly the leading
// run of the page. (search_after is exclusive, so a FULL id would seek past
// itself — callers keep full ids on the realtime GET path.) Verified against
// 2976 live ids: the seek's run equals a brute-force prefix scan for 600
// random prefixes, and _id sort order equals byte order.
//
// COSTS AND LIMITS. Sorting on _id loads its fielddata, which depends on the
// cluster setting indices.id_field_data.enabled (true by default on 3.1, and
// emits no deprecation warning there) and holds every _id of each searched
// shard on the heap — ~64 bytes per fact, negligible at this store's scale,
// but a cost that grows with the index. Visibility is SEARCH visibility: a fact
// written within the last refresh_interval may not resolve by prefix yet, even
// though its full id reads at once via the realtime GET.
//
// filters are ANDed as bool filter clauses and are the caller's authorization
// (tenant pin, compiled ACL clause): this method is policy-free, and because
// they run INSIDE the query, a document the caller may not read is never
// returned, never counted, and cannot make an otherwise-unique prefix look
// ambiguous. An empty filters list is refused rather than run unscoped
// (fail-closed: no caller should resolve across tenants by accident). A missing
// semantic index resolves to no ids, not an error.
func (s *OpenSearchStore) ResolveFactIDPrefix(ctx context.Context, prefix string, filters []any, limit int) ([]string, error) {
	if prefix == "" || limit <= 0 {
		return nil, fmt.Errorf("store: resolving fact id prefix: prefix and a positive limit are required")
	}
	if len(filters) == 0 {
		return nil, fmt.Errorf("store: resolving fact id prefix: refusing an unscoped resolution (no filters)")
	}
	query := map[string]any{
		"size":             limit,
		"_source":          false,
		"track_total_hits": false,
		"sort":             []any{map[string]any{"_id": "asc"}},
		"search_after":     []any{prefix},
		"query":            map[string]any{"bool": map[string]any{"filter": filters}},
	}
	body, err := json.Marshal(query)
	if err != nil {
		return nil, fmt.Errorf("store: encoding fact id prefix query: %w", err)
	}
	status, decoded, err := doJSON(ctx, s.client, http.MethodPost, s.baseURL+"/"+s.semanticIndex+"/_search", body)
	if err != nil {
		return nil, fmt.Errorf("store: resolving fact id prefix: %w", err)
	}
	if isIndexNotFound(status, decoded) {
		return nil, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("store: resolving fact id prefix: unexpected status %d: %v", status, decoded)
	}
	var ids []string
	for _, hit := range searchHits(decoded) {
		id := hitID(hit)
		if !strings.HasPrefix(id, prefix) {
			break // past the run: the page is sorted, nothing later can match
		}
		ids = append(ids, id)
	}
	return ids, nil
}

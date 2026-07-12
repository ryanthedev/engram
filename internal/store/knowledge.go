package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/ryanthedev/engram/internal/knowledge"
)

// KnowledgeStore is the OpenSearch-backed batch document-write path for the
// knowledge platform: upsert-by-id bulk writes and mark-and-sweep deletes.
// This is Engram's ONE intentional, documented deviation from append-only
// memory writes (docs/code-standards.md's "never hard-delete or blind-
// overwrite" rule governs memory.SemanticFact only — see store.go). Knowledge
// documents are mutable harvested content, not reconciled facts, so upsert +
// real delete are correct here. Do not "fix" this back to op_type=create /
// invalid_at.
//
// KnowledgeStore holds no embedder dependency: BulkIndex issues zero
// embedding calls by construction, matching the plan's BM25-only v1 scope.
//
// No interface is declared for KnowledgeStore here: per the plan's
// Assumption Verification, the narrow seam Phase 6 will consume is Phase
// 6's to define (Go's structural typing lets it declare a 2-method
// interface later that this type satisfies for free) — declaring one now
// would also reach outside this phase's File scope (internal/knowledge/**
// is not listed for Phase 4).
type KnowledgeStore struct {
	client  *http.Client
	baseURL string
}

// NewKnowledgeStore returns a KnowledgeStore over the OpenSearch cluster at
// baseURL. client must not be nil.
func NewKnowledgeStore(client *http.Client, baseURL string) *KnowledgeStore {
	return &KnowledgeStore{client: client, baseURL: strings.TrimRight(baseURL, "/")}
}

// indexNameRE is the safe-path-segment grammar for an OpenSearch index/alias
// name embedded in a REST path: it must start with a lowercase letter or
// digit and contain only lowercase letters, digits, underscore, hyphen, or
// dot thereafter — no '/', no whitespace, no control characters. Unlike
// registry.go's collectionNameRE (collection names, no hyphens),
// index/alias names DO contain hyphens (knowledge-<name>-v<N>).
var indexNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,254}$`)

// validateIndexName is the barricade every code path that embeds a
// caller-supplied index name into an OpenSearch request path MUST pass the
// name through first — the same discipline as registry.go's
// validateCollectionName (Phase 3 SECURITY LESSON: a missed gate there
// allowed path traversal). The grammar admits no '/', and an explicit ".."
// substring check closes the one gap dots-in-the-charset would otherwise
// leave open (e.g. "a..%2Fb" style tricks are irrelevant since '/' is
// already excluded, but ".." alone must never validate as a legal segment).
func validateIndexName(index string) error {
	if !indexNameRE.MatchString(index) || strings.Contains(index, "..") {
		return fmt.Errorf("store: invalid index name %q: must match %s and not contain \"..\"", index, indexNameRE)
	}
	return nil
}

// validateTextField gates the caller-supplied BM25 field name (the
// collection's CollectionSpec.TextField) before it is used as a JSON key in
// the bulk row. It mirrors registry.go's normalizeSpec discipline — the same
// collectionNameRE grammar the registry validated the field against at
// creation time, plus a reserved-field check so a caller can never redirect
// Document.Text onto a server-owned provenance field (title/collection/
// source/source_version/harvest_id/harvested_at). This is the Phase-3
// path-safety lesson applied one level in: validate any caller-supplied
// name before it steers where data lands, not just names that hit the URL.
func validateTextField(textField string) error {
	if !collectionNameRE.MatchString(textField) {
		return fmt.Errorf("store: invalid text field %q: must match %s", textField, collectionNameRE)
	}
	if _, reserved := baseDocProperties[textField]; reserved {
		return fmt.Errorf("store: text field %q collides with a reserved document field", textField)
	}
	return nil
}

// BulkIndex upserts docs into index via the OpenSearch `_bulk` API (action
// "index": create-or-overwrite by _id — deliberately NOT op_type=create,
// deliberately no if_seq_no guard; see the type doc). Every row is stamped
// with harvestID and a server-assigned harvested_at, stamped LAST so a
// caller-supplied Fields entry can never spoof them (harvested_at is never
// client-trusted). Document.Fields is merged in as-is: BulkIndex neither
// requires nor synthesizes "collection"/"source" — those are batch-level
// concerns the Phase 6 caller injects into each doc's Fields before calling
// here (see the design note on Document's frozen shape).
//
// Document.Text is written under textField — the collection's
// CollectionSpec.TextField, which the registry provisioned the BM25 mapping
// under (arXiv, for example, uses "abstract", not "text"). The caller
// (Phase 6) passes spec.TextField; it is validated before use so it can
// never redirect the body onto a reserved provenance field.
//
// An empty docs surfaces as a no-op (0, nil). A partial `_bulk` failure
// returns the count that DID succeed alongside a non-nil error — it never
// reports full success when any item failed.
func (s *KnowledgeStore) BulkIndex(ctx context.Context, index, textField string, docs []knowledge.Document, harvestID string) (int, error) {
	if len(docs) == 0 {
		return 0, nil
	}
	if err := validateIndexName(index); err != nil {
		return 0, err
	}
	if err := validateTextField(textField); err != nil {
		return 0, err
	}
	if harvestID == "" {
		return 0, fmt.Errorf("store: bulk-indexing into %s: harvestID must not be empty", index)
	}
	for i, d := range docs {
		if d.ID == "" {
			return 0, fmt.Errorf("store: bulk-indexing into %s: doc at position %d has an empty ID", index, i)
		}
	}
	harvestedAt := time.Now().UTC().Format(time.RFC3339Nano)
	body, err := buildBulkBody(index, textField, docs, harvestID, harvestedAt)
	if err != nil {
		return 0, fmt.Errorf("store: encoding bulk request for %s: %w", index, err)
	}
	status, decoded, err := doNDJSON(ctx, s.client, http.MethodPost, s.baseURL+"/_bulk", body)
	if err != nil {
		return 0, fmt.Errorf("store: bulk-indexing into %s: %w", index, err)
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("store: bulk-indexing into %s: unexpected status %d: %v", index, status, decoded)
	}
	succeeded, failures := bulkItemResults(decoded)
	if len(failures) > 0 {
		return succeeded, fmt.Errorf("store: bulk-indexing into %s: %d/%d docs failed (first: %s)", index, len(failures), len(docs), failures[0])
	}
	return succeeded, nil
}

// buildBulkBody assembles the NDJSON `_bulk` request body: one action line
// + one source line per doc, every line (including the last) `\n`-terminated
// as the `_bulk` API requires.
func buildBulkBody(index, textField string, docs []knowledge.Document, harvestID, harvestedAt string) ([]byte, error) {
	var buf bytes.Buffer
	for _, d := range docs {
		action, err := json.Marshal(map[string]any{"index": map[string]any{"_index": index, "_id": d.ID}})
		if err != nil {
			return nil, err
		}
		buf.Write(action)
		buf.WriteByte('\n')

		row := make(map[string]any, len(d.Fields)+5)
		for k, v := range d.Fields {
			row[k] = v
		}
		row["title"] = d.Title
		row[textField] = d.Text
		row["source_version"] = d.SourceVersion
		// Stamped LAST: a Fields entry can never override server-assigned
		// provenance, even if it happens to collide with a reserved key.
		row["harvest_id"] = harvestID
		row["harvested_at"] = harvestedAt

		src, err := json.Marshal(row)
		if err != nil {
			return nil, err
		}
		buf.Write(src)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// bulkItemResults walks a `_bulk` response's items, counting successes and
// collecting per-item error messages. Every item is checked individually —
// DW-4.4: a partial failure must never be mistaken for full success, so this
// never trusts a coarse "no error" inference from the top-level status alone.
func bulkItemResults(decoded map[string]any) (succeeded int, failures []string) {
	items, _ := decoded["items"].([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		for _, action := range item { // exactly one key per item: "index"
			a, _ := action.(map[string]any)
			if errObj, ok := a["error"]; ok {
				id, _ := a["_id"].(string)
				failures = append(failures, fmt.Sprintf("id=%s: %v", id, errObj))
				continue
			}
			if st, _ := a["status"].(float64); st == http.StatusOK || st == http.StatusCreated {
				succeeded++
			}
		}
	}
	return succeeded, failures
}

// DeleteByQuery sweeps index for rows matching collection AND source whose
// harvest_id differs from currentHarvestID (the mark-and-sweep predicate:
// rows the latest harvest run did not touch) via OpenSearch's
// `_delete_by_query`, a genuine hard delete — see the type doc. A
// not-yet-created index (nothing has ever been harvested there) reads as
// nothing to sweep: deleted=0, not an error, matching the house
// isIndexNotFound-as-empty rule.
func (s *KnowledgeStore) DeleteByQuery(ctx context.Context, index, collection, source, currentHarvestID string) (int, error) {
	if err := validateIndexName(index); err != nil {
		return 0, err
	}
	if collection == "" || source == "" {
		return 0, fmt.Errorf("store: sweeping %s: collection and source must not be empty", index)
	}
	if currentHarvestID == "" {
		// An empty currentHarvestID would match harvest_id != "" — i.e.
		// every harvested row — turning a routine sweep into a full wipe of
		// the collection+source. Refuse rather than silently doing the
		// wrong destructive thing.
		return 0, fmt.Errorf("store: sweeping %s (collection=%s source=%s): currentHarvestID must not be empty", index, collection, source)
	}
	body := deleteByQueryBody(collection, source, currentHarvestID)
	// conflicts=proceed: a sweep runs immediately after the run's bulk upsert,
	// so _delete_by_query's point-in-time snapshot can still see a just-
	// re-upserted row under its OLD harvest_id and match it for deletion; by
	// the time the delete executes the row carries the CURRENT harvest_id and
	// a bumped version, yielding a version conflict. Skipping that conflict is
	// exactly correct — the row was touched by this run and must survive — so
	// proceed rather than aborting the whole sweep. Genuine orphans (untouched,
	// no version change) still delete.
	status, decoded, err := doJSON(ctx, s.client, http.MethodPost, s.baseURL+"/"+index+"/_delete_by_query?conflicts=proceed", body)
	if err != nil {
		return 0, fmt.Errorf("store: sweeping %s (collection=%s source=%s): %w", index, collection, source, err)
	}
	if isIndexNotFound(status, decoded) {
		return 0, nil
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("store: sweeping %s (collection=%s source=%s): unexpected status %d: %v", index, collection, source, status, decoded)
	}
	if failures, _ := decoded["failures"].([]any); len(failures) > 0 {
		return 0, fmt.Errorf("store: sweeping %s (collection=%s source=%s): %d docs failed to delete (first: %v)", index, collection, source, len(failures), failures[0])
	}
	deleted, _ := decoded["deleted"].(float64)
	return int(deleted), nil
}

// deleteByQueryBody builds the bool query matching collection AND source AND
// NOT harvest_id=currentHarvestID.
func deleteByQueryBody(collection, source, currentHarvestID string) []byte {
	body := map[string]any{
		"query": map[string]any{
			"bool": map[string]any{
				"must": []any{
					map[string]any{"term": map[string]any{"collection": collection}},
					map[string]any{"term": map[string]any{"source": source}},
				},
				"must_not": []any{
					map[string]any{"term": map[string]any{"harvest_id": currentHarvestID}},
				},
			},
		},
	}
	raw, _ := json.Marshal(body) // static shape built from strings: cannot fail
	return raw
}

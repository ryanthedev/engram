//go:build integration

package store

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/knowledge"
)

// knowledgeSpec is a collection spec for this file's tests: default
// TextField ("text") plus one declared, filterable field so Fields merging
// is exercised. textField, when non-empty, overrides the BM25 field so a
// test can provision a collection whose text lives under a non-default name
// (e.g. arXiv's "abstract").
func knowledgeSpec(name, textField string) knowledge.CollectionSpec {
	return knowledge.CollectionSpec{
		Name:      name,
		TextField: textField,
		Mappings: map[string]knowledge.FieldSpec{
			"year": {Type: "keyword", Filterable: true},
		},
		Access: knowledge.AccessPolicy{Public: true},
	}
}

// provisionedCollection creates a real, live-provisioned collection via the
// Phase 3 registry (reusing registry_integration_test.go's liveRegistry /
// scratchName / deleteIndices helpers, same package) and returns its spec —
// so BulkIndex/DeleteByQuery are exercised against the actual physical
// mapping a real collection produces, not a hand-rolled scratch index. An
// empty textField takes the registry default ("text").
func provisionedCollection(t *testing.T, textField string) (spec knowledge.CollectionSpec, client *http.Client, url string) {
	t.Helper()
	reg, client, url, _ := liveRegistry(t)
	name := scratchName(t, client, url)
	ctx := context.Background()
	s := knowledgeSpec(name, textField)
	if err := reg.Create(ctx, s); err != nil {
		t.Fatalf("provisioning collection %s: %v", name, err)
	}
	got, err := reg.Get(ctx, name)
	if err != nil {
		t.Fatalf("reading back provisioned collection %s: %v", name, err)
	}
	return got, client, url
}

// getDoc reads one document directly (bypassing KnowledgeStore) so tests can
// assert on the raw stored shape.
func getDoc(t *testing.T, client *http.Client, url, index, id string) (map[string]any, bool) {
	t.Helper()
	status, decoded, err := doJSON(context.Background(), client, http.MethodGet, url+"/"+index+"/_doc/"+id, nil)
	if err != nil {
		t.Fatalf("reading doc %s/%s: %v", index, id, err)
	}
	if status == http.StatusNotFound {
		return nil, false
	}
	if status != http.StatusOK {
		t.Fatalf("reading doc %s/%s: unexpected status %d: %v", index, id, status, decoded)
	}
	src, _ := decoded["_source"].(map[string]any)
	return src, true
}

// countDocs refreshes then counts index (refresh so a just-completed write
// is visible — OpenSearch is near-real-time, not immediately consistent).
func countDocs(t *testing.T, client *http.Client, url, index string) int {
	t.Helper()
	if _, _, err := doJSON(context.Background(), client, http.MethodPost, url+"/"+index+"/_refresh", nil); err != nil {
		t.Fatalf("refreshing %s: %v", index, err)
	}
	status, decoded, err := doJSON(context.Background(), client, http.MethodGet, url+"/"+index+"/_count", nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("counting %s: status=%d err=%v", index, status, err)
	}
	count, _ := decoded["count"].(float64)
	return int(count)
}

// TestDW_4_1_BulkIndexStampsFieldsNoEmbedCalls pins BulkIndex's core write
// contract: N docs land by _id, every row carries harvest_id/source_version/
// harvested_at, and — since KnowledgeStore has no embedder dependency at all
// — no *_embedding field is ever written (the "zero embedding calls" DW,
// verified structurally: there is nothing in this path that could call one).
func TestDW_4_1_BulkIndexStampsFieldsNoEmbedCalls(t *testing.T) {
	spec, client, url := provisionedCollection(t, "")
	ks := NewKnowledgeStore(client, url)
	ctx := context.Background()

	docs := []knowledge.Document{
		{ID: "d1", Title: "Doc One", Text: "hello world", SourceVersion: "v1", Fields: map[string]any{"collection": spec.Name, "source": "kaggle", "year": "2021"}},
		{ID: "d2", Title: "Doc Two", Text: "goodbye world", SourceVersion: "v1", Fields: map[string]any{"collection": spec.Name, "source": "kaggle", "year": "2022"}},
		{ID: "d3", Title: "Doc Three", Text: "third", SourceVersion: "v2", Fields: map[string]any{"collection": spec.Name, "source": "kaggle"}},
	}
	indexed, err := ks.BulkIndex(ctx, spec.Index, spec.TextField, docs, "harvest-1")
	if err != nil {
		t.Fatalf("BulkIndex: %v", err)
	}
	if indexed != 3 {
		t.Errorf("indexed = %d, want 3", indexed)
	}

	before := time.Now().UTC()
	src, ok := getDoc(t, client, url, spec.Index, "d1")
	if !ok {
		t.Fatalf("doc d1 not found")
	}
	if src["title"] != "Doc One" || src[spec.TextField] != "hello world" {
		t.Errorf("doc d1 title/%s = %v/%v", spec.TextField, src["title"], src[spec.TextField])
	}
	if src["source_version"] != "v1" || src["harvest_id"] != "harvest-1" {
		t.Errorf("doc d1 source_version/harvest_id = %v/%v", src["source_version"], src["harvest_id"])
	}
	if src["year"] != "2021" {
		t.Errorf("doc d1 year (from Fields) = %v, want 2021", src["year"])
	}
	harvestedAtStr, _ := src["harvested_at"].(string)
	harvestedAt, err := time.Parse(time.RFC3339Nano, harvestedAtStr)
	if err != nil {
		t.Fatalf("harvested_at %q did not parse as a timestamp: %v", harvestedAtStr, err)
	}
	if harvestedAt.After(before.Add(time.Minute)) || harvestedAt.Before(before.Add(-time.Minute)) {
		t.Errorf("harvested_at %v not close to write time %v", harvestedAt, before)
	}
	for k := range src {
		if strings.HasSuffix(k, "_embedding") {
			t.Errorf("doc d1 unexpectedly carries embedding field %q — BulkIndex must never call an embedder", k)
		}
	}
}

// matchQueryHits runs a BM25 match query against field on index and returns
// the number of hits — the reader's-eye proof that BulkIndex wrote the text
// under the field the collection's BM25 mapping actually indexes.
func matchQueryHits(t *testing.T, client *http.Client, url, index, field, term string) int {
	t.Helper()
	if _, _, err := doJSON(context.Background(), client, http.MethodPost, url+"/"+index+"/_refresh", nil); err != nil {
		t.Fatalf("refreshing %s: %v", index, err)
	}
	body := []byte(fmt.Sprintf(`{"query":{"match":{%q:%q}}}`, field, term))
	status, decoded, err := doJSON(context.Background(), client, http.MethodPost, url+"/"+index+"/_search", body)
	if err != nil || status != http.StatusOK {
		t.Fatalf("match query on %s.%s: status=%d err=%v", index, field, status, err)
	}
	outer, _ := decoded["hits"].(map[string]any)
	hits, _ := outer["hits"].([]any)
	return len(hits)
}

// TestDW_4_1_BulkIndexRoundTripsCustomTextField is the regression guard for
// the un-ingestability defect: a collection whose CollectionSpec.TextField is
// "abstract" (arXiv's shape — NOT "text") must accept a BulkIndex whose
// Document.Text lands under "abstract", strict-mapping and all, and be
// retrievable by a BM25 match on "abstract". Before the fix, BulkIndex wrote
// Document.Text under the hardcoded key "text", which the strict mapping
// rejected — making every such collection un-writable.
func TestDW_4_1_BulkIndexRoundTripsCustomTextField(t *testing.T) {
	spec, client, url := provisionedCollection(t, "abstract")
	if spec.TextField != "abstract" {
		t.Fatalf("provisioned TextField = %q, want abstract", spec.TextField)
	}
	ks := NewKnowledgeStore(client, url)
	ctx := context.Background()

	doc := knowledge.Document{ID: "p1", Title: "A Paper", Text: "quantum entanglement in superconductors", SourceVersion: "v1", Fields: map[string]any{"collection": spec.Name, "source": "kaggle", "year": "2021"}}
	indexed, err := ks.BulkIndex(ctx, spec.Index, spec.TextField, []knowledge.Document{doc}, "h1")
	if err != nil {
		t.Fatalf("BulkIndex into custom-text-field collection: %v", err)
	}
	if indexed != 1 {
		t.Fatalf("indexed = %d, want 1", indexed)
	}

	src, ok := getDoc(t, client, url, spec.Index, "p1")
	if !ok {
		t.Fatalf("doc p1 not found")
	}
	if src["abstract"] != "quantum entanglement in superconductors" {
		t.Errorf("text landed under %v, want under \"abstract\"; row = %v", missingText(src), src)
	}
	if _, leaked := src["text"]; leaked {
		t.Errorf("text unexpectedly written under the default \"text\" key: %v", src["text"])
	}
	if hits := matchQueryHits(t, client, url, spec.Index, "abstract", "entanglement"); hits != 1 {
		t.Errorf("BM25 match on abstract:entanglement returned %d hits, want 1 (text is not searchable under abstract)", hits)
	}
}

// missingText reports which text-bearing key (if any) a row carries, for a
// clearer failure message when the custom-field round-trip breaks.
func missingText(src map[string]any) string {
	for _, k := range []string{"abstract", "text"} {
		if v, ok := src[k]; ok {
			return fmt.Sprintf("%s=%v", k, v)
		}
	}
	return "no text field present"
}

// TestBulkIndexRejectsReservedTextField: a textField that would redirect
// Document.Text onto a server-owned provenance field (here "harvest_id") is
// rejected before any request is built — the fake handler fails the test if
// the network is ever reached. Guards the validateTextField barricade added
// with the custom-text-field fix (Phase-3 path-safety discipline).
func TestBulkIndexRejectsReservedTextField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request reached the network: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	ks := NewKnowledgeStore(srv.Client(), srv.URL)
	docs := []knowledge.Document{{ID: "x", Text: "y"}}
	if _, err := ks.BulkIndex(context.Background(), "knowledge-papers", "harvest_id", docs, "h1"); err == nil {
		t.Errorf("BulkIndex with a reserved textField returned nil error")
	}
	if _, err := ks.BulkIndex(context.Background(), "knowledge-papers", "bad-field!", docs, "h1"); err == nil {
		t.Errorf("BulkIndex with an invalid textField returned nil error")
	}
}

// TestDW_4_2_ReBulkIndexOverwritesInPlace: re-indexing the same _id upserts
// in place — no duplicate row, and the fields reflect the newer write.
func TestDW_4_2_ReBulkIndexOverwritesInPlace(t *testing.T) {
	spec, client, url := provisionedCollection(t, "")
	ks := NewKnowledgeStore(client, url)
	ctx := context.Background()

	doc := knowledge.Document{ID: "dup", Title: "First", Text: "v1 text", Fields: map[string]any{"collection": spec.Name, "source": "s"}}
	if _, err := ks.BulkIndex(ctx, spec.Index, spec.TextField, []knowledge.Document{doc}, "h1"); err != nil {
		t.Fatalf("first BulkIndex: %v", err)
	}
	doc.Title, doc.Text = "Second", "v2 text"
	if _, err := ks.BulkIndex(ctx, spec.Index, spec.TextField, []knowledge.Document{doc}, "h2"); err != nil {
		t.Fatalf("second BulkIndex: %v", err)
	}

	if n := countDocs(t, client, url, spec.Index); n != 1 {
		t.Errorf("doc count = %d, want 1 (overwrite, not duplicate)", n)
	}
	src, ok := getDoc(t, client, url, spec.Index, "dup")
	if !ok {
		t.Fatalf("doc dup not found")
	}
	if src["title"] != "Second" || src["text"] != "v2 text" || src["harvest_id"] != "h2" {
		t.Errorf("doc dup after overwrite = %v, want title=Second text=\"v2 text\" harvest_id=h2", src)
	}
}

// TestDW_4_3_DeleteByQuerySweepsStaleRows pins the mark-and-sweep predicate:
// collection AND source AND harvest_id != currentHarvestID. A same-harvest
// row and a same-collection-different-source row must both survive — this
// proves the AND, not just a harvest-id mismatch.
func TestDW_4_3_DeleteByQuerySweepsStaleRows(t *testing.T) {
	spec, client, url := provisionedCollection(t, "")
	ks := NewKnowledgeStore(client, url)
	ctx := context.Background()

	stale := knowledge.Document{ID: "stale", Text: "old", Fields: map[string]any{"collection": spec.Name, "source": "kaggle"}}
	current := knowledge.Document{ID: "current", Text: "new", Fields: map[string]any{"collection": spec.Name, "source": "kaggle"}}
	otherSource := knowledge.Document{ID: "other-source", Text: "unrelated", Fields: map[string]any{"collection": spec.Name, "source": "oai-pmh"}}

	if _, err := ks.BulkIndex(ctx, spec.Index, spec.TextField, []knowledge.Document{stale, otherSource}, "h1"); err != nil {
		t.Fatalf("seeding h1 docs: %v", err)
	}
	if _, err := ks.BulkIndex(ctx, spec.Index, spec.TextField, []knowledge.Document{current}, "h2"); err != nil {
		t.Fatalf("seeding h2 doc: %v", err)
	}
	if _, _, err := doJSON(ctx, client, http.MethodPost, url+"/"+spec.Index+"/_refresh", nil); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	deleted, err := ks.DeleteByQuery(ctx, spec.Index, spec.Name, "kaggle", "h2")
	if err != nil {
		t.Fatalf("DeleteByQuery: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1 (only the stale kaggle row)", deleted)
	}

	if _, ok := getDoc(t, client, url, spec.Index, "stale"); ok {
		t.Errorf("stale doc still present after sweep")
	}
	if _, ok := getDoc(t, client, url, spec.Index, "current"); !ok {
		t.Errorf("current doc was swept away, want it kept")
	}
	if _, ok := getDoc(t, client, url, spec.Index, "other-source"); !ok {
		t.Errorf("other-source doc was swept away (different source), want it kept")
	}
}

// TestDW_4_4_BulkIndexSurfacesPerItemErrors: one doc in the batch carries a
// field the strict-mapped collection never declared (rejected per-item by
// OpenSearch); the other is valid. BulkIndex must report the failure loudly
// AND still land the valid doc — a _bulk call is not atomic, and hiding the
// partial failure would silently under-report what actually happened.
func TestDW_4_4_BulkIndexSurfacesPerItemErrors(t *testing.T) {
	spec, client, url := provisionedCollection(t, "")
	ks := NewKnowledgeStore(client, url)
	ctx := context.Background()

	good := knowledge.Document{ID: "good", Text: "fine", Fields: map[string]any{"collection": spec.Name, "source": "s"}}
	bad := knowledge.Document{ID: "bad", Text: "broken", Fields: map[string]any{"collection": spec.Name, "source": "s", "undeclared_field": "x"}}

	indexed, err := ks.BulkIndex(ctx, spec.Index, spec.TextField, []knowledge.Document{good, bad}, "h1")
	if err == nil {
		t.Fatalf("BulkIndex with a per-item failure returned nil error — must not report full success")
	}
	if indexed != 1 {
		t.Errorf("indexed = %d, want 1 (only the valid doc)", indexed)
	}
	if _, ok := getDoc(t, client, url, spec.Index, "good"); !ok {
		t.Errorf("valid doc was not indexed despite the batch partially succeeding")
	}
	if _, ok := getDoc(t, client, url, spec.Index, "bad"); ok {
		t.Errorf("invalid doc unexpectedly landed")
	}
}

// TestBulkIndexEmptyDocsIsNoOp: an empty batch is a no-op, not an error.
func TestBulkIndexEmptyDocsIsNoOp(t *testing.T) {
	ks := NewKnowledgeStore(http.DefaultClient, "http://localhost:9200")
	indexed, err := ks.BulkIndex(context.Background(), "knowledge-whatever", "text", nil, "h1")
	if err != nil || indexed != 0 {
		t.Errorf("BulkIndex(nil docs) = (%d, %v), want (0, nil)", indexed, err)
	}
}

// TestBulkIndexRejectsEmptyID: a doc with an empty ID would break upsert-by-
// id semantics (OpenSearch would auto-generate an _id instead) — rejected
// before any network call.
func TestBulkIndexRejectsEmptyID(t *testing.T) {
	ks := NewKnowledgeStore(http.DefaultClient, "http://localhost:9200")
	docs := []knowledge.Document{{ID: "", Text: "x"}}
	if _, err := ks.BulkIndex(context.Background(), "knowledge-whatever", "text", docs, "h1"); err == nil {
		t.Errorf("BulkIndex with an empty doc ID returned nil error")
	}
}

// TestDeleteByQueryZeroMatchesIsNotError: a sweep with nothing stale to
// remove reports deleted=0, not an error.
func TestDeleteByQueryZeroMatchesIsNotError(t *testing.T) {
	spec, client, url := provisionedCollection(t, "")
	ks := NewKnowledgeStore(client, url)
	ctx := context.Background()

	doc := knowledge.Document{ID: "only", Text: "x", Fields: map[string]any{"collection": spec.Name, "source": "s"}}
	if _, err := ks.BulkIndex(ctx, spec.Index, spec.TextField, []knowledge.Document{doc}, "h1"); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, _, err := doJSON(ctx, client, http.MethodPost, url+"/"+spec.Index+"/_refresh", nil); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	deleted, err := ks.DeleteByQuery(ctx, spec.Index, spec.Name, "s", "h1")
	if err != nil {
		t.Fatalf("DeleteByQuery: %v", err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0 (the only row is current-harvest)", deleted)
	}
}

// TestDeleteByQueryOnMissingIndexIsNotError: sweeping an index that was
// never provisioned (nothing has ever been harvested there) reads as
// "nothing to sweep", matching the house 404-as-empty rule.
func TestDeleteByQueryOnMissingIndexIsNotError(t *testing.T) {
	_, client, url := provisionedCollection(t, "")
	ks := NewKnowledgeStore(client, url)
	deleted, err := ks.DeleteByQuery(context.Background(), "knowledge-never-provisioned-xyz", "c", "s", "h1")
	if err != nil {
		t.Fatalf("DeleteByQuery on a missing index: %v", err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0", deleted)
	}
}

// TestDeleteByQueryRejectsEmptyCurrentHarvestID: an empty currentHarvestID
// would match every harvested row (harvest_id != ""), turning a routine
// sweep into a full wipe — refused up front, and nothing is touched.
func TestDeleteByQueryRejectsEmptyCurrentHarvestID(t *testing.T) {
	spec, client, url := provisionedCollection(t, "")
	ks := NewKnowledgeStore(client, url)
	ctx := context.Background()

	doc := knowledge.Document{ID: "keep-me", Text: "x", Fields: map[string]any{"collection": spec.Name, "source": "s"}}
	if _, err := ks.BulkIndex(ctx, spec.Index, spec.TextField, []knowledge.Document{doc}, "h1"); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	deleted, err := ks.DeleteByQuery(ctx, spec.Index, spec.Name, "s", "")
	if err == nil {
		t.Fatalf("DeleteByQuery with an empty currentHarvestID returned nil error")
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0", deleted)
	}
	if _, ok := getDoc(t, client, url, spec.Index, "keep-me"); !ok {
		t.Errorf("doc was removed despite the rejected empty-currentHarvestID call")
	}
}

// TestBulkIndexRejectsPathTraversalIndex / TestDeleteByQueryRejectsPathTraversalIndex
// lock in the SECURITY LESSON carried over from Phase 3: any caller-supplied
// name interpolated into an OpenSearch REST path is validated first. Both
// use a fake handler that fails the test if the network is ever reached, so
// these prove the barricade runs BEFORE any request is built, not just that
// an eventual error surfaces.
func TestBulkIndexRejectsPathTraversalIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request reached the network: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	ks := NewKnowledgeStore(srv.Client(), srv.URL)
	docs := []knowledge.Document{{ID: "x", Text: "y"}}
	if _, err := ks.BulkIndex(context.Background(), "../other-index", "text", docs, "h1"); err == nil {
		t.Errorf("BulkIndex with a path-traversal index name returned nil error")
	}
}

func TestDeleteByQueryRejectsPathTraversalIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request reached the network: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	ks := NewKnowledgeStore(srv.Client(), srv.URL)
	if _, err := ks.DeleteByQuery(context.Background(), "../other-index", "c", "s", "h1"); err == nil {
		t.Errorf("DeleteByQuery with a path-traversal index name returned nil error")
	}
}

// TestBuildBulkBodyEndsWithTrailingNewline pins the `_bulk` wire-format
// requirement directly: the body must end with '\n' after the last line.
func TestBuildBulkBodyEndsWithTrailingNewline(t *testing.T) {
	docs := []knowledge.Document{{ID: "a", Text: "x"}, {ID: "b", Text: "y"}}
	body, err := buildBulkBody("idx", "text", docs, "h1", "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("buildBulkBody: %v", err)
	}
	if len(body) == 0 || body[len(body)-1] != '\n' {
		t.Errorf("bulk body does not end with a trailing newline: %q", string(body))
	}
}

// TestFieldsCannotOverrideStampedProvenance: a caller-supplied Fields entry
// literally named "harvest_id" or "harvested_at" must never survive into the
// stored row — the stamped values always win.
func TestFieldsCannotOverrideStampedProvenance(t *testing.T) {
	spec, client, url := provisionedCollection(t, "")
	ks := NewKnowledgeStore(client, url)
	ctx := context.Background()

	doc := knowledge.Document{
		ID:   "spoof",
		Text: "x",
		Fields: map[string]any{
			"collection":   spec.Name,
			"source":       "s",
			"harvest_id":   "attacker-supplied",
			"harvested_at": "1999-01-01T00:00:00Z",
		},
	}
	if _, err := ks.BulkIndex(ctx, spec.Index, spec.TextField, []knowledge.Document{doc}, "real-harvest"); err != nil {
		t.Fatalf("BulkIndex: %v", err)
	}
	src, ok := getDoc(t, client, url, spec.Index, "spoof")
	if !ok {
		t.Fatalf("doc not found")
	}
	if src["harvest_id"] != "real-harvest" {
		t.Errorf("harvest_id = %v, want real-harvest (Fields entry must not override it)", src["harvest_id"])
	}
	if src["harvested_at"] == "1999-01-01T00:00:00Z" {
		t.Errorf("harvested_at was overridden by a Fields entry — must always be server-assigned")
	}
}

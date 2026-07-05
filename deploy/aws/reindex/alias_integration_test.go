//go:build integration

package reindex_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/ryanthedev/engram/deploy/aws/reindex"
	"github.com/ryanthedev/engram/internal/testutil"
)

// fixtureTemplate is a minimal, self-contained template (not the real
// semantic/episodic templates — this test proves the reindex MECHANISM in
// isolation, on a throwaway alias name, so it never risks the shared dev
// cluster's real indices).
var fixtureTemplateV1 = []byte(`{
  "index_patterns": ["engram-reindex-fixture-*"],
  "template": {
    "settings": {"number_of_shards": 1, "number_of_replicas": 0},
    "mappings": {"properties": {"text": {"type": "keyword"}}}
  }
}`)

// TestDW_7_Rollback_VersionedIndexAliasFlip proves the Phase-7 rollback
// mitigation end to end against the real cluster: creating v2 and flipping
// the alias never touches v1's mapping, the alias always resolves to
// exactly one index, and the flip is atomic (readers see v1's docs, then
// v2's — never both, never neither).
func TestDW_7_Rollback_VersionedIndexAliasFlip(t *testing.T) {
	base := testutil.OpenSearchURL()
	ctx := context.Background()
	alias := fmt.Sprintf("engram-reindex-fixture-%d", time.Now().UnixNano())

	v1, created, err := reindex.EnsureAliasedIndex(ctx, testutil.HTTPClient, base, alias, 1, fixtureTemplateV1)
	if err != nil {
		t.Fatalf("EnsureAliasedIndex v1: %v", err)
	}
	if !created {
		t.Fatal("v1 should have been created (fresh alias)")
	}
	t.Cleanup(func() { testutil.DeleteIndex(t, base, v1) })

	target, err := reindex.CurrentAliasTarget(ctx, testutil.HTTPClient, base, alias)
	if err != nil {
		t.Fatalf("CurrentAliasTarget after create: %v", err)
	}
	if target != v1 {
		t.Fatalf("alias target = %s, want %s", target, v1)
	}

	// Index one doc through v1 via the alias, capture v1's mapping bytes.
	indexDoc(t, base, alias, "doc-1", map[string]any{"text": "before the flip"})
	testutil.RefreshIndex(t, base, v1)
	mappingBefore := getMapping(t, base, v1)

	// A second call to EnsureAliasedIndex with the SAME version is a no-op:
	// it does not recreate v1 or re-point the alias.
	again, createdAgain, err := reindex.EnsureAliasedIndex(ctx, testutil.HTTPClient, base, alias, 1, fixtureTemplateV1)
	if err != nil {
		t.Fatalf("EnsureAliasedIndex v1 (repeat): %v", err)
	}
	if again != v1 || createdAgain {
		t.Fatalf("repeat EnsureAliasedIndex = (%s, created=%v), want (%s, created=false)", again, createdAgain, v1)
	}

	// Create v2 under a DIFFERENT (breaking) mapping — this is the "breaking
	// index-template change" scenario. v1 must be completely untouched.
	fixtureTemplateV2 := []byte(`{
	  "index_patterns": ["engram-reindex-fixture-*"],
	  "template": {
	    "settings": {"number_of_shards": 1, "number_of_replicas": 0},
	    "mappings": {"properties": {"text": {"type": "text"}, "new_field": {"type": "keyword"}}}
	  }
	}`)
	v2, created, err := reindex.EnsureAliasedIndex(ctx, testutil.HTTPClient, base, alias, 2, fixtureTemplateV2)
	if err != nil {
		t.Fatalf("EnsureAliasedIndex v2: %v", err)
	}
	if !created {
		t.Fatal("v2 should have been created")
	}
	t.Cleanup(func() { testutil.DeleteIndex(t, base, v2) })
	// Creating v2 must not have re-pointed the alias — it still resolves to
	// v1 until FlipAlias is called explicitly.
	if stillV1, err := reindex.CurrentAliasTarget(ctx, testutil.HTTPClient, base, alias); err != nil || stillV1 != v1 {
		t.Fatalf("alias after creating v2 = (%s, %v), want (%s, nil) — v2 must not auto-attach", stillV1, err, v1)
	}

	mappingAfterV2Create := getMapping(t, base, v1)
	if mappingBefore != mappingAfterV2Create {
		t.Fatalf("v1's mapping changed just from creating v2 — in-place mutation detected:\nbefore: %s\nafter:  %s", mappingBefore, mappingAfterV2Create)
	}

	// Backfill v2 with the same doc (a real migration would reindex; this
	// test only needs one doc to prove atomic visibility across the flip).
	indexDocDirect(t, base, v2, "doc-1", map[string]any{"text": "after the flip", "new_field": "x"})
	testutil.RefreshIndex(t, base, v2)

	// Flip the alias — atomic remove-v1/add-v2 in one call.
	if err := reindex.FlipAlias(ctx, testutil.HTTPClient, base, alias, v1, v2); err != nil {
		t.Fatalf("FlipAlias: %v", err)
	}

	target, err = reindex.CurrentAliasTarget(ctx, testutil.HTTPClient, base, alias)
	if err != nil {
		t.Fatalf("CurrentAliasTarget after flip: %v", err)
	}
	if target != v2 {
		t.Fatalf("alias target after flip = %s, want %s", target, v2)
	}

	mappingAfterFlip := getMapping(t, base, v1)
	if mappingBefore != mappingAfterFlip {
		t.Fatalf("v1's mapping changed by FlipAlias — in-place mutation detected:\nbefore: %s\nafter:  %s", mappingBefore, mappingAfterFlip)
	}

	// A search through the alias now sees ONLY v2's data.
	hits := searchAlias(t, base, alias)
	if len(hits) != 1 {
		t.Fatalf("search through alias after flip returned %d hits, want 1 (only v2's doc)", len(hits))
	}
}

func indexDoc(t *testing.T, base, alias, id string, doc map[string]any) {
	t.Helper()
	indexDocDirect(t, base, alias, id, doc)
}

func indexDocDirect(t *testing.T, base, target, id string, doc map[string]any) {
	t.Helper()
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encoding doc: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, base+"/"+target+"/_doc/"+id, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("building index request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := testutil.HTTPClient.Do(req)
	if err != nil {
		t.Fatalf("indexing doc into %s: %v", target, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("indexing doc into %s: status %d", target, resp.StatusCode)
	}
}

func getMapping(t *testing.T, base, index string) string {
	t.Helper()
	resp, err := testutil.HTTPClient.Get(base + "/" + index + "/_mapping")
	if err != nil {
		t.Fatalf("getting mapping for %s: %v", index, err)
	}
	defer resp.Body.Close()
	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decoding mapping for %s: %v", index, err)
	}
	b, _ := json.Marshal(decoded[index])
	return string(b)
}

func searchAlias(t *testing.T, base, alias string) []any {
	t.Helper()
	resp, err := testutil.HTTPClient.Get(base + "/" + alias + "/_search")
	if err != nil {
		t.Fatalf("searching alias %s: %v", alias, err)
	}
	defer resp.Body.Close()
	var decoded struct {
		Hits struct {
			Hits []any `json:"hits"`
		} `json:"hits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decoding search response for %s: %v", alias, err)
	}
	return decoded.Hits.Hits
}

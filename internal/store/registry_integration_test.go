//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/knowledge"
)

// liveRegistry returns a CollectionRegistry against the live pinned cluster,
// on a scratch meta-index (matching the knowledge-collections-* template
// pattern) so parallel runs on the shared cluster can't collide. Apply runs
// first (idempotent) so the template exists; everything created is cleaned up.
func liveRegistry(t *testing.T) (*CollectionRegistry, *http.Client, string, string) {
	t.Helper()
	url := os.Getenv("ENGRAM_OPENSEARCH_URL")
	if url == "" {
		url = "http://localhost:9200"
	}
	client := &http.Client{Timeout: DefaultTimeout}
	if _, err := Apply(context.Background(), client, url); err != nil {
		t.Fatalf("apply: %v", err)
	}
	metaIndex := fmt.Sprintf("knowledge-collections-it%d", time.Now().UnixNano())
	t.Cleanup(func() { deleteIndices(t, client, url, metaIndex) })
	return NewCollectionRegistry(client, url, WithRegistryMetaIndex(metaIndex)), client, url, metaIndex
}

// scratchName returns a unique legal collection name and registers cleanup of
// its physical indices (v1..v4 generously; the alias dies with its index).
func scratchName(t *testing.T, client *http.Client, url string) string {
	t.Helper()
	name := fmt.Sprintf("it%d", time.Now().UnixNano())
	t.Cleanup(func() {
		for v := 1; v <= 4; v++ {
			deleteIndices(t, client, url, physicalFor(name, v))
		}
	})
	return name
}

// deleteIndices best-effort deletes one index, tolerating absence.
func deleteIndices(t *testing.T, client *http.Client, url, index string) {
	t.Helper()
	status, decoded, err := doJSON(context.Background(), client, http.MethodDelete, url+"/"+index, nil)
	if err == nil && status != http.StatusOK && status != http.StatusNotFound {
		t.Logf("cleanup of %s returned %d: %v", index, status, decoded)
	}
}

func liveSpec(name string) knowledge.CollectionSpec {
	return knowledge.CollectionSpec{
		Name:      name,
		TextField: "abstract",
		Mappings: map[string]knowledge.FieldSpec{
			"year":       {Type: "keyword", Filterable: true},
			"categories": {Type: "keyword", Filterable: true},
		},
		Access: knowledge.AccessPolicy{Public: true},
	}
}

// TestDW_3_1_CreateProvisionsAndGetReturnsSpec_Live: one Create writes the
// durable meta row AND stands up the live index/alias; a SECOND registry
// instance (cold cache — the "no restart needed" proof: state lives on the
// cluster, not in the process) Gets the full spec back.
func TestDW_3_1_CreateProvisionsAndGetReturnsSpec_Live(t *testing.T) {
	reg, client, url, metaIndex := liveRegistry(t)
	name := scratchName(t, client, url)
	ctx := context.Background()

	if err := reg.Create(ctx, liveSpec(name)); err != nil {
		t.Fatalf("create: %v", err)
	}
	// The alias answers live queries immediately.
	status, decoded, err := doJSON(ctx, client, http.MethodGet, url+"/"+aliasFor(name)+"/_count", nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("alias not live: status=%d err=%v body=%v", status, err, decoded)
	}
	// A fresh registry over the same meta-index sees the collection.
	fresh := NewCollectionRegistry(client, url, WithRegistryMetaIndex(metaIndex))
	got, err := fresh.Get(ctx, name)
	if err != nil {
		t.Fatalf("get from fresh registry: %v", err)
	}
	if got.Index != aliasFor(name) || got.TextField != "abstract" || !got.Access.Public {
		t.Errorf("spec = %+v, want index=%s text=abstract public", got, aliasFor(name))
	}
	if got.Mappings["year"] != (knowledge.FieldSpec{Type: "keyword", Filterable: true}) {
		t.Errorf("year field = %+v", got.Mappings["year"])
	}
}

// TestDW_3_2_UpdateAddsMappingField_Live: Update lands a new field on the
// LIVE index via PUT _mapping — visible in the cluster mapping, no restart.
func TestDW_3_2_UpdateAddsMappingField_Live(t *testing.T) {
	reg, client, url, _ := liveRegistry(t)
	name := scratchName(t, client, url)
	ctx := context.Background()

	if err := reg.Create(ctx, liveSpec(name)); err != nil {
		t.Fatalf("create: %v", err)
	}
	spec := liveSpec(name)
	spec.Mappings["venue"] = knowledge.FieldSpec{Type: "keyword", Filterable: true}
	if err := reg.Update(ctx, spec); err != nil {
		t.Fatalf("update: %v", err)
	}
	status, decoded, err := doJSON(ctx, client, http.MethodGet, url+"/"+aliasFor(name)+"/_mapping", nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("reading live mapping: status=%d err=%v", status, err)
	}
	if fmt.Sprint(decoded) == "" || !mappingHasField(decoded, "venue", "keyword") {
		t.Errorf("live mapping lacks venue:keyword: %v", decoded)
	}
}

// mappingHasField walks a GET _mapping response for field with type typ.
func mappingHasField(mapping map[string]any, field, typ string) bool {
	for _, idx := range mapping {
		m, _ := idx.(map[string]any)
		mm, _ := m["mappings"].(map[string]any)
		props, _ := mm["properties"].(map[string]any)
		f, _ := props[field].(map[string]any)
		if f != nil && f["type"] == typ {
			return true
		}
	}
	return false
}

// TestDW_3_2_TypeChangeReindexesAndSwapsAlias_Live: a field-type change
// provisions -v2 with the new mapping, copies the data, and atomically swaps
// the alias — the document survives and the collection name never changes.
func TestDW_3_2_TypeChangeReindexesAndSwapsAlias_Live(t *testing.T) {
	reg, client, url, _ := liveRegistry(t)
	name := scratchName(t, client, url)
	ctx := context.Background()

	if err := reg.Create(ctx, liveSpec(name)); err != nil {
		t.Fatalf("create: %v", err)
	}
	doc := []byte(fmt.Sprintf(`{"collection":%q,"source":"s","abstract":"hello","year":"2021"}`, name))
	status, decoded, err := doJSON(ctx, client, http.MethodPut, url+"/"+aliasFor(name)+"/_doc/1?refresh=true", doc)
	if err != nil || (status != http.StatusCreated && status != http.StatusOK) {
		t.Fatalf("seeding doc through alias: status=%d err=%v body=%v", status, err, decoded)
	}

	spec := liveSpec(name)
	spec.Mappings["year"] = knowledge.FieldSpec{Type: "integer", Filterable: true, Sortable: true} // keyword -> integer
	if err := reg.Update(ctx, spec); err != nil {
		t.Fatalf("type-change update: %v", err)
	}

	// Alias now resolves to -v2.
	status, decoded, err = doJSON(ctx, client, http.MethodGet, url+"/_alias/"+aliasFor(name), nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("resolving alias: status=%d err=%v", status, err)
	}
	if _, ok := decoded[physicalFor(name, 2)]; !ok {
		t.Errorf("alias resolves to %v, want %s", decoded, physicalFor(name, 2))
	}
	// The document survived the reindex and is reachable through the alias.
	status, decoded, err = doJSON(ctx, client, http.MethodGet, url+"/"+aliasFor(name)+"/_count", nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("counting through alias: status=%d err=%v", status, err)
	}
	if count, _ := decoded["count"].(float64); count != 1 {
		t.Errorf("post-swap count = %v, want 1", decoded["count"])
	}
	// The new physical index carries the new type.
	status, mapping, err := doJSON(ctx, client, http.MethodGet, url+"/"+physicalFor(name, 2)+"/_mapping", nil)
	if err != nil || status != http.StatusOK || !mappingHasField(mapping, "year", "integer") {
		t.Errorf("v2 mapping lacks year:integer (status=%d err=%v): %v", status, err, mapping)
	}
	if got, _ := reg.Get(ctx, name); got.Mappings["year"].Type != "integer" || got.Index != aliasFor(name) {
		t.Errorf("registry spec after type change = %+v", got)
	}
}

// TestDW_3_3_CreateReflectsInListWithoutDirectRead_Live: the caller only ever
// talks to the registry — create, then List (served through the registry's
// cache machinery) reflects the new collection.
func TestDW_3_3_CreateReflectsInListWithoutDirectRead_Live(t *testing.T) {
	reg, client, url, _ := liveRegistry(t)
	name := scratchName(t, client, url)
	ctx := context.Background()

	if list, err := reg.List(ctx); err != nil || len(list) != 0 {
		t.Fatalf("pre-create list = %v, %v; want empty", list, err)
	}
	if err := reg.Create(ctx, liveSpec(name)); err != nil {
		t.Fatalf("create: %v", err)
	}
	list, err := reg.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Name != name || !list[0].Access.Public {
		t.Errorf("list after create = %+v, want [%s public]", list, name)
	}
}

// TestCreateDuplicateAndConcurrentRace_Live: a duplicate name is ErrConflict;
// two racing Creates resolve to exactly one winner (op_type=create on the
// meta row decides) and one live index.
func TestCreateDuplicateAndConcurrentRace_Live(t *testing.T) {
	reg, client, url, _ := liveRegistry(t)
	name := scratchName(t, client, url)
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = reg.Create(ctx, liveSpec(name))
		}(i)
	}
	wg.Wait()
	winners, conflicts := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, knowledge.ErrConflict):
			conflicts++
		default:
			t.Errorf("racing create: %v", err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Errorf("race resolved to %d winners / %d conflicts, want 1/1", winners, conflicts)
	}
	// And a plain duplicate afterwards is still ErrConflict.
	if err := reg.Create(ctx, liveSpec(name)); !errors.Is(err, knowledge.ErrConflict) {
		t.Errorf("duplicate create = %v, want ErrConflict", err)
	}
	if _, err := reg.Get(ctx, name); err != nil {
		t.Errorf("get after race: %v", err)
	}
}

// TestGetUnknown_Live: an unknown collection is the not-found sentinel.
func TestGetUnknown_Live(t *testing.T) {
	reg, _, _, _ := liveRegistry(t)
	if _, err := reg.Get(context.Background(), "neverexists"); !errors.Is(err, knowledge.ErrNotFound) {
		t.Errorf("get unknown = %v, want ErrNotFound", err)
	}
}

// TestDW_3_4_SeedIdempotent_Live: the YAML boot seed applied twice — the
// second pass creates nothing and rewrites nothing (meta row _seq_no stable).
func TestDW_3_4_SeedIdempotent_Live(t *testing.T) {
	reg, client, url, metaIndex := liveRegistry(t)
	nameA := scratchName(t, client, url)
	nameB := scratchName(t, client, url)
	ctx := context.Background()
	seed := []byte(fmt.Sprintf(`
collections:
  - name: %s
    text_field: abstract
    public: true
    fields:
      year: {type: keyword, filterable: true}
  - name: %s
    roles: [ops]
`, nameA, nameB))

	created, err := knowledge.Seed(ctx, reg, seed)
	if err != nil {
		t.Fatalf("first seed: %v", err)
	}
	if len(created) != 2 {
		t.Fatalf("first seed created %v, want both", created)
	}
	seqBefore := metaSeqNo(t, client, url, metaIndex, nameA)

	created, err = knowledge.Seed(ctx, reg, seed)
	if err != nil {
		t.Fatalf("second seed: %v", err)
	}
	if len(created) != 0 {
		t.Errorf("second seed created %v, want none", created)
	}
	if seqAfter := metaSeqNo(t, client, url, metaIndex, nameA); seqAfter != seqBefore {
		t.Errorf("second seed rewrote the meta row (_seq_no %d -> %d)", seqBefore, seqAfter)
	}
}

// metaSeqNo reads a meta row's _seq_no directly (write detector).
func metaSeqNo(t *testing.T, client *http.Client, url, metaIndex, name string) int64 {
	t.Helper()
	status, decoded, err := doJSON(context.Background(), client, http.MethodGet, url+"/"+metaIndex+"/_doc/"+name, nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("reading meta row %s: status=%d err=%v", name, status, err)
	}
	seq, _ := decoded["_seq_no"].(float64)
	return int64(seq)
}

// TestProvisionRepairsLostIndex_Live: delete a collection's live index out
// from under the registry, then Provision restores index + alias per the
// durable meta row.
func TestProvisionRepairsLostIndex_Live(t *testing.T) {
	reg, client, url, _ := liveRegistry(t)
	name := scratchName(t, client, url)
	ctx := context.Background()

	if err := reg.Create(ctx, liveSpec(name)); err != nil {
		t.Fatalf("create: %v", err)
	}
	deleteIndices(t, client, url, physicalFor(name, 1)) // alias dies with it

	if err := reg.Provision(ctx, name); err != nil {
		t.Fatalf("provision: %v", err)
	}
	status, _, err := doJSON(ctx, client, http.MethodGet, url+"/"+aliasFor(name)+"/_count", nil)
	if err != nil || status != http.StatusOK {
		t.Errorf("alias not restored: status=%d err=%v", status, err)
	}
	if err := reg.Provision(ctx, name); err != nil {
		t.Errorf("second provision must be a no-op, got: %v", err)
	}
}

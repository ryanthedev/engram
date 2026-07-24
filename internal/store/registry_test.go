package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ryanthedev/engram/internal/knowledge"
)

// fakeKnowledgeCluster simulates the OpenSearch surface CollectionRegistry
// touches: meta-doc CRUD (with op_type=create and if_seq_no semantics), index
// create with body, PUT _mapping, alias resolution, atomic _aliases actions,
// and _reindex — plus counters the cache/mechanics tests assert on.
type fakeKnowledgeCluster struct {
	mu          sync.Mutex
	docs        map[string]*fakeMetaDoc
	indices     map[string]map[string]any // physical -> create body
	aliases     map[string]string         // alias -> physical
	searches    int
	mappingPuts []string    // resolved physical targets of PUT _mapping
	reindexes   [][2]string // {source, dest}

	forceDocPutConflict bool // next guarded _doc PUT loses with 409
}

type fakeMetaDoc struct {
	src   json.RawMessage
	seqNo int
}

func newFakeKnowledgeCluster() *fakeKnowledgeCluster {
	return &fakeKnowledgeCluster{
		docs:    map[string]*fakeMetaDoc{},
		indices: map[string]map[string]any{},
		aliases: map[string]string{},
	}
}

func (f *fakeKnowledgeCluster) resolve(target string) string {
	if physical, ok := f.aliases[target]; ok {
		return physical
	}
	return target
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeKnowledgeCluster) handler() http.Handler {
	meta := KnowledgeCollectionsIndex
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		switch {
		case parts[0] == "_aliases" && r.Method == http.MethodPost:
			var req struct {
				Actions []map[string]struct{ Index, Alias string } `json:"actions"`
			}
			_ = json.Unmarshal(body, &req)
			for _, action := range req.Actions {
				if a, ok := action["remove"]; ok {
					if f.aliases[a.Alias] != a.Index {
						writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"type": "aliases_not_found_exception"}})
						return
					}
					delete(f.aliases, a.Alias)
				}
				if a, ok := action["add"]; ok {
					f.aliases[a.Alias] = a.Index
				}
			}
			writeJSON(w, http.StatusOK, map[string]any{"acknowledged": true})
		case parts[0] == "_reindex" && r.Method == http.MethodPost:
			var req struct {
				Source, Dest struct {
					Index string `json:"index"`
				}
			}
			_ = json.Unmarshal(body, &req)
			f.reindexes = append(f.reindexes, [2]string{req.Source.Index, req.Dest.Index})
			writeJSON(w, http.StatusOK, map[string]any{"took": 1, "failures": []any{}})
		case parts[0] == "_alias" && r.Method == http.MethodGet:
			alias := parts[1]
			if physical, ok := f.aliases[alias]; ok {
				writeJSON(w, http.StatusOK, map[string]any{physical: map[string]any{"aliases": map[string]any{alias: map[string]any{}}}})
				return
			}
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "alias missing"})
		case parts[0] == meta && len(parts) > 1 && parts[1] == "_search":
			f.searches++
			hits := []any{}
			for _, doc := range f.docs {
				hits = append(hits, map[string]any{"_source": json.RawMessage(doc.src)})
			}
			writeJSON(w, http.StatusOK, map[string]any{"hits": map[string]any{"hits": hits}})
		case parts[0] == meta && len(parts) == 3 && parts[1] == "_create" && r.Method == http.MethodPut:
			id := parts[2]
			if _, exists := f.docs[id]; exists {
				writeJSON(w, http.StatusConflict, map[string]any{"error": map[string]any{"type": "version_conflict_engine_exception"}})
				return
			}
			f.docs[id] = &fakeMetaDoc{src: body}
			writeJSON(w, http.StatusCreated, map[string]any{"_id": id, "result": "created"})
		case parts[0] == meta && len(parts) == 3 && parts[1] == "_doc" && r.Method == http.MethodGet:
			doc, ok := f.docs[parts[2]]
			if !ok {
				writeJSON(w, http.StatusNotFound, map[string]any{"found": false})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"_source": doc.src, "_seq_no": doc.seqNo, "_primary_term": 1, "found": true})
		case parts[0] == meta && len(parts) == 3 && parts[1] == "_doc" && r.Method == http.MethodPut:
			doc, ok := f.docs[parts[2]]
			seq, _ := strconv.Atoi(r.URL.Query().Get("if_seq_no"))
			if !ok || seq != doc.seqNo || f.forceDocPutConflict {
				writeJSON(w, http.StatusConflict, map[string]any{"error": map[string]any{"type": "version_conflict_engine_exception"}})
				return
			}
			doc.src, doc.seqNo = body, doc.seqNo+1
			writeJSON(w, http.StatusOK, map[string]any{"result": "updated"})
		case len(parts) == 2 && parts[1] == "_mapping" && r.Method == http.MethodPut:
			physical := f.resolve(parts[0])
			idx, ok := f.indices[physical]
			if !ok {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"type": "index_not_found_exception"}})
				return
			}
			var req struct {
				Properties map[string]any `json:"properties"`
			}
			_ = json.Unmarshal(body, &req)
			props, _ := idx["properties"].(map[string]any)
			for k, v := range req.Properties {
				props[k] = v
			}
			f.mappingPuts = append(f.mappingPuts, physical)
			writeJSON(w, http.StatusOK, map[string]any{"acknowledged": true})
		case len(parts) == 1 && r.Method == http.MethodHead:
			if _, ok := f.indices[parts[0]]; ok {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		case len(parts) == 1 && r.Method == http.MethodPut:
			index := parts[0]
			if _, exists := f.indices[index]; exists {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"type": "resource_already_exists_exception"}})
				return
			}
			var req struct {
				Mappings struct {
					Dynamic    string         `json:"dynamic"`
					Properties map[string]any `json:"properties"`
				} `json:"mappings"`
				Aliases map[string]any `json:"aliases"`
			}
			_ = json.Unmarshal(body, &req)
			f.indices[index] = map[string]any{"dynamic": req.Mappings.Dynamic, "properties": req.Mappings.Properties}
			for alias := range req.Aliases {
				f.aliases[alias] = index
			}
			writeJSON(w, http.StatusOK, map[string]any{"acknowledged": true})
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unhandled: " + r.Method + " " + r.URL.Path})
		}
	})
}

// newFakeRegistry returns a registry over a fresh fake cluster.
func newFakeRegistry(t *testing.T) (*CollectionRegistry, *fakeKnowledgeCluster) {
	t.Helper()
	fake := newFakeKnowledgeCluster()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	return NewCollectionRegistry(srv.Client(), srv.URL), fake
}

func paperSpec() knowledge.CollectionSpec {
	return knowledge.CollectionSpec{
		Name:      "papers",
		TextField: "abstract",
		Mappings: map[string]knowledge.FieldSpec{
			"year":       {Type: "integer", Filterable: true, Sortable: true},
			"categories": {Type: "keyword", Filterable: true},
		},
		Access: knowledge.AccessPolicy{Public: true},
	}
}

// TestDW_3_1_CreateWritesMetaAndProvisionsInOneCall pins Create's contract:
// one call writes the meta-doc AND creates the live physical index with its
// alias (mappings included), and a follow-up Get returns the spec — no
// restart, no second step.
func TestDW_3_1_CreateWritesMetaAndProvisionsInOneCall(t *testing.T) {
	reg, fake := newFakeRegistry(t)
	ctx := context.Background()
	if err := reg.Create(ctx, paperSpec()); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, ok := fake.docs["papers"]; !ok {
		t.Error("meta-doc not written")
	}
	idx, ok := fake.indices["knowledge-papers-v1"]
	if !ok {
		t.Fatal("physical index knowledge-papers-v1 not provisioned")
	}
	if idx["dynamic"] != "strict" {
		t.Errorf("data index dynamic = %v, want strict", idx["dynamic"])
	}
	props := idx["properties"].(map[string]any)
	for field, typ := range map[string]string{"abstract": "text", "year": "integer", "categories": "keyword", "harvest_id": "keyword", "harvested_at": "date", "source": "keyword", "source_version": "keyword", "title": "text", "collection": "keyword"} {
		p, _ := props[field].(map[string]any)
		if p == nil || p["type"] != typ {
			t.Errorf("index property %s = %v, want type %s", field, props[field], typ)
		}
	}
	if fake.aliases["knowledge-papers"] != "knowledge-papers-v1" {
		t.Errorf("alias knowledge-papers -> %q, want knowledge-papers-v1", fake.aliases["knowledge-papers"])
	}

	got, err := reg.Get(ctx, "papers")
	if err != nil {
		t.Fatalf("get after create: %v", err)
	}
	if got.Index != "knowledge-papers" || got.TextField != "abstract" || !got.Access.Public {
		t.Errorf("get returned %+v, want index=knowledge-papers text=abstract public", got)
	}
	if got.Mappings["year"] != (knowledge.FieldSpec{Type: "integer", Filterable: true, Sortable: true}) {
		t.Errorf("year field = %+v", got.Mappings["year"])
	}
}

// TestDW_3_3_ReadsHitCacheWritesInvalidate pins cache coherence: repeated
// reads issue ONE meta-index search; a write invalidates so the next read
// reloads and reflects the change.
func TestDW_3_3_ReadsHitCacheWritesInvalidate(t *testing.T) {
	reg, fake := newFakeRegistry(t)
	ctx := context.Background()

	if _, err := reg.List(ctx); err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, err := reg.List(ctx); err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, err := reg.Get(ctx, "nope"); !errors.Is(err, knowledge.ErrNotFound) {
		t.Fatalf("get unknown = %v, want ErrNotFound", err)
	}
	if fake.searches != 1 {
		t.Fatalf("three reads issued %d searches, want 1 (cache)", fake.searches)
	}

	if err := reg.Create(ctx, paperSpec()); err != nil {
		t.Fatalf("create: %v", err)
	}
	list, err := reg.List(ctx)
	if err != nil {
		t.Fatalf("list after create: %v", err)
	}
	if len(list) != 1 || list[0].Name != "papers" {
		t.Errorf("list after create = %+v, want [papers]", list)
	}
	if fake.searches != 2 {
		t.Errorf("post-write read issued %d total searches, want 2 (invalidated once)", fake.searches)
	}
	if _, err := reg.Get(ctx, "papers"); err != nil || fake.searches != 2 {
		t.Errorf("get after reload: err=%v searches=%d, want nil/2", err, fake.searches)
	}
}

// TestCreateDuplicateNameIsErrConflict pins the duplicate edge — including
// the concurrent-create race, which the meta-doc op_type=create decides.
func TestCreateDuplicateNameIsErrConflict(t *testing.T) {
	reg, _ := newFakeRegistry(t)
	ctx := context.Background()
	if err := reg.Create(ctx, paperSpec()); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := reg.Create(ctx, paperSpec()); !errors.Is(err, knowledge.ErrConflict) {
		t.Errorf("duplicate create = %v, want ErrConflict", err)
	}
}

// TestCreateToleratesExistingPhysicalIndex pins the provisioning half of the
// create race: the loser's index PUT hits resource_already_exists and reads
// as provisioned, not as an error (the ensureIndex idempotency pattern).
func TestCreateToleratesExistingPhysicalIndex(t *testing.T) {
	reg, fake := newFakeRegistry(t)
	fake.indices["knowledge-papers-v1"] = map[string]any{"properties": map[string]any{}}
	fake.aliases["knowledge-papers"] = "knowledge-papers-v1"
	if err := reg.Create(context.Background(), paperSpec()); err != nil {
		t.Fatalf("create over an existing index must succeed, got: %v", err)
	}
}

// TestUpdateUnknownCollectionIsErrNotFound.
func TestUpdateUnknownCollectionIsErrNotFound(t *testing.T) {
	reg, _ := newFakeRegistry(t)
	if err := reg.Update(context.Background(), paperSpec()); !errors.Is(err, knowledge.ErrNotFound) {
		t.Errorf("update unknown = %v, want ErrNotFound", err)
	}
}

// TestDW_3_2_UpdateAddsFieldViaLiveMapping pins the additive-update path: a
// new field lands via PUT _mapping on the live index (no reindex, no new
// version) and the meta spec reflects it.
func TestDW_3_2_UpdateAddsFieldViaLiveMapping(t *testing.T) {
	reg, fake := newFakeRegistry(t)
	ctx := context.Background()
	if err := reg.Create(ctx, paperSpec()); err != nil {
		t.Fatalf("create: %v", err)
	}
	spec := paperSpec()
	spec.Mappings["venue"] = knowledge.FieldSpec{Type: "keyword", Filterable: true}
	if err := reg.Update(ctx, spec); err != nil {
		t.Fatalf("update: %v", err)
	}

	if len(fake.mappingPuts) != 1 || fake.mappingPuts[0] != "knowledge-papers-v1" {
		t.Errorf("mapping puts = %v, want one on knowledge-papers-v1", fake.mappingPuts)
	}
	props := fake.indices["knowledge-papers-v1"]["properties"].(map[string]any)
	if p, _ := props["venue"].(map[string]any); p == nil || p["type"] != "keyword" {
		t.Errorf("live mapping venue = %v, want keyword", props["venue"])
	}
	if len(fake.reindexes) != 0 {
		t.Errorf("additive update reindexed: %v", fake.reindexes)
	}
	got, err := reg.Get(ctx, "papers")
	if err != nil || got.Mappings["venue"].Type != "keyword" {
		t.Errorf("get after update: %+v, %v", got, err)
	}
}

// TestDW_3_2_TypeChangeProvisionsV2AndSwapsAlias pins the reindex path: a
// field-type change creates knowledge-<name>-v2 with the full new mapping,
// copies the data, and atomically moves the alias — callers keep using the
// same collection name throughout.
func TestDW_3_2_TypeChangeProvisionsV2AndSwapsAlias(t *testing.T) {
	reg, fake := newFakeRegistry(t)
	ctx := context.Background()
	if err := reg.Create(ctx, paperSpec()); err != nil {
		t.Fatalf("create: %v", err)
	}
	spec := paperSpec()
	spec.Mappings["year"] = knowledge.FieldSpec{Type: "keyword", Filterable: true} // integer -> keyword
	if err := reg.Update(ctx, spec); err != nil {
		t.Fatalf("update: %v", err)
	}

	v2, ok := fake.indices["knowledge-papers-v2"]
	if !ok {
		t.Fatal("knowledge-papers-v2 not provisioned")
	}
	props := v2["properties"].(map[string]any)
	if p, _ := props["year"].(map[string]any); p == nil || p["type"] != "keyword" {
		t.Errorf("v2 year mapping = %v, want keyword", props["year"])
	}
	if len(fake.reindexes) != 1 || fake.reindexes[0] != [2]string{"knowledge-papers-v1", "knowledge-papers-v2"} {
		t.Errorf("reindexes = %v, want v1->v2", fake.reindexes)
	}
	if fake.aliases["knowledge-papers"] != "knowledge-papers-v2" {
		t.Errorf("alias -> %q, want knowledge-papers-v2", fake.aliases["knowledge-papers"])
	}
	if _, stillThere := fake.indices["knowledge-papers-v1"]; !stillThere {
		t.Error("old physical index deleted; it must be left as a safety net")
	}
	got, err := reg.Get(ctx, "papers")
	if err != nil || got.Mappings["year"].Type != "keyword" || got.Index != "knowledge-papers" {
		t.Errorf("get after type change: %+v, %v", got, err)
	}
}

// TestUpdateConcurrentWriteIsErrConflict pins the guarded meta write: losing
// the if_seq_no race surfaces ErrConflict for the caller to retry.
func TestUpdateConcurrentWriteIsErrConflict(t *testing.T) {
	reg, fake := newFakeRegistry(t)
	ctx := context.Background()
	if err := reg.Create(ctx, paperSpec()); err != nil {
		t.Fatalf("create: %v", err)
	}
	fake.forceDocPutConflict = true
	spec := paperSpec()
	spec.Access.Public = false
	if err := reg.Update(ctx, spec); !errors.Is(err, knowledge.ErrConflict) {
		t.Errorf("raced update = %v, want ErrConflict", err)
	}
}

// TestProvisionRepairsMissingIndex pins Provision as the Create-repair /
// boot-reconcile path: registered but index-less collections get their index
// and alias back; an already-live alias is a no-op.
func TestProvisionRepairsMissingIndex(t *testing.T) {
	reg, fake := newFakeRegistry(t)
	ctx := context.Background()
	if err := reg.Create(ctx, paperSpec()); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Simulate the index lost after registration (partial create / restored meta).
	delete(fake.indices, "knowledge-papers-v1")
	delete(fake.aliases, "knowledge-papers")

	if err := reg.Provision(ctx, "papers"); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if fake.aliases["knowledge-papers"] != "knowledge-papers-v1" {
		t.Errorf("provision did not restore the alias: %v", fake.aliases)
	}
	if err := reg.Provision(ctx, "papers"); err != nil {
		t.Fatalf("second provision must be a no-op, got: %v", err)
	}
	if err := reg.Provision(ctx, "ghost"); !errors.Is(err, knowledge.ErrNotFound) {
		t.Errorf("provision unknown = %v, want ErrNotFound", err)
	}
}

// TestSpecValidationRejectsBadInput: invalid specs fail loudly BEFORE any
// cluster write, with errors naming the rule.
func TestSpecValidationRejectsBadInput(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*knowledge.CollectionSpec)
	}{
		{"uppercase name", func(s *knowledge.CollectionSpec) { s.Name = "Papers" }},
		{"hyphenated name", func(s *knowledge.CollectionSpec) { s.Name = "a-b" }},
		{"empty name", func(s *knowledge.CollectionSpec) { s.Name = "" }},
		{"reserved name", func(s *knowledge.CollectionSpec) { s.Name = "collections" }},
		{"reserved field", func(s *knowledge.CollectionSpec) { s.Mappings["harvest_id"] = knowledge.FieldSpec{Type: "keyword"} }},
		{"field equals text field", func(s *knowledge.CollectionSpec) { s.Mappings["abstract"] = knowledge.FieldSpec{Type: "keyword"} }},
		{"unsupported type", func(s *knowledge.CollectionSpec) { s.Mappings["v"] = knowledge.FieldSpec{Type: "knn_vector"} }},
		{"reserved text field", func(s *knowledge.CollectionSpec) { s.TextField = "harvested_at" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg, fake := newFakeRegistry(t)
			spec := paperSpec()
			tt.mutate(&spec)
			if err := reg.Create(context.Background(), spec); err == nil {
				t.Fatal("want validation error, got nil")
			}
			if len(fake.docs) != 0 || len(fake.indices) != 0 {
				t.Error("invalid spec reached the cluster")
			}
		})
	}
}

// TestCreateDefaultsTextField: an empty TextField becomes "text".
func TestCreateDefaultsTextField(t *testing.T) {
	reg, fake := newFakeRegistry(t)
	spec := paperSpec()
	spec.TextField = ""
	if err := reg.Create(context.Background(), spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	props := fake.indices["knowledge-papers-v1"]["properties"].(map[string]any)
	if p, _ := props["text"].(map[string]any); p == nil || p["type"] != "text" {
		t.Errorf("default text field mapping = %v, want text:text", props["text"])
	}
	got, _ := reg.Get(context.Background(), "papers")
	if got.TextField != "text" {
		t.Errorf("TextField = %q, want text", got.TextField)
	}
}

// TestGetReturnsACopy: mutating a Get result must not poison the cache.
func TestGetReturnsACopy(t *testing.T) {
	reg, _ := newFakeRegistry(t)
	ctx := context.Background()
	if err := reg.Create(ctx, paperSpec()); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, _ := reg.Get(ctx, "papers")
	got.Mappings["year"] = knowledge.FieldSpec{Type: "boolean"}
	again, _ := reg.Get(ctx, "papers")
	if again.Mappings["year"].Type != "integer" {
		t.Errorf("cache poisoned through a Get result: year = %+v", again.Mappings["year"])
	}
}

// TestKnowledgeCollectionsTemplateContract asserts the meta-index template:
// the knowledge-collections-* pattern, strict mapping, and the spec row
// fields the registry round-trips.
func TestKnowledgeCollectionsTemplateContract(t *testing.T) {
	var tpl struct {
		IndexPatterns []string `json:"index_patterns"`
		Template      struct {
			Mappings struct {
				Dynamic    string         `json:"dynamic"`
				Properties map[string]any `json:"properties"`
			} `json:"mappings"`
		} `json:"template"`
	}
	if err := json.Unmarshal(KnowledgeCollectionsTemplateJSON, &tpl); err != nil {
		t.Fatalf("knowledge-collections template JSON invalid: %v", err)
	}
	if len(tpl.IndexPatterns) != 1 || tpl.IndexPatterns[0] != "knowledge-collections-*" {
		t.Errorf("index_patterns = %v, want [knowledge-collections-*]", tpl.IndexPatterns)
	}
	if tpl.Template.Mappings.Dynamic != "strict" {
		t.Errorf("dynamic = %q, want strict", tpl.Template.Mappings.Dynamic)
	}
	requireFieldTypes(t, tpl.Template.Mappings.Properties, map[string]string{
		"name":       "keyword",
		"text_field": "keyword",
		"index":      "keyword",
		"version":    "integer",
		"public":     "boolean",
		"roles":      "keyword",
		"updated_at": "date",
	})
	fields, _ := tpl.Template.Mappings.Properties["fields"].(map[string]any)
	if fields == nil {
		t.Fatal("missing fields row mapping")
	}
	sub, _ := fields["properties"].(map[string]any)
	requireFieldTypes(t, sub, map[string]string{"name": "keyword", "type": "keyword", "filterable": "boolean", "sortable": "boolean"})
}

// TestApplyProvisionsKnowledgeCollectionsMetaIndex: Apply PUTs the template
// and creates the meta-index alongside the memory-path resources.
func TestApplyProvisionsKnowledgeCollectionsMetaIndex(t *testing.T) {
	fake := newFakeCluster("3.1.0")
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	res, err := Apply(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, ok := fake.templates[KnowledgeCollectionsTemplateName]; !ok {
		t.Error("knowledge-collections template not applied")
	}
	if res.Actions[KnowledgeCollectionsIndex] != "created" {
		t.Errorf("meta-index action = %q, want created", res.Actions[KnowledgeCollectionsIndex])
	}
}

// TestProvisionRejectsPathTraversalName is the security regression for the
// Phase-3 review finding: Provision embeds its name argument in a request
// path, so a crafted name like "../../engram-episodic-000001/_search" must be
// rejected by the collection-name gate BEFORE any HTTP request is built — it
// must never reach the cluster and never touch another index. The same gate
// is asserted for the traversal shapes a caller might try. A server that fails
// the test the instant it is contacted proves "no HTTP request that could
// reach another index" was issued.
func TestProvisionRejectsPathTraversalName(t *testing.T) {
	var contacted int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		contacted++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	reg := NewCollectionRegistry(srv.Client(), srv.URL)
	ctx := context.Background()

	for _, name := range []string{
		"../../engram-episodic-000001/_search",
		"..%2f..%2fengram-episodic-000001",
		"papers/_doc/../../secret",
		"papers ",
		"Papers",
		"",
	} {
		t.Run(name, func(t *testing.T) {
			if err := reg.Provision(ctx, name); err == nil {
				t.Fatalf("Provision(%q) = nil, want a validation error", name)
			} else if !strings.Contains(err.Error(), "invalid collection name") && !strings.Contains(err.Error(), "reserved") {
				t.Errorf("Provision(%q) error = %v, want an invalid-name validation error", name, err)
			}
		})
	}
	if contacted != 0 {
		t.Errorf("a traversal name reached the cluster (%d HTTP requests); it must be rejected before any path is built", contacted)
	}
}

// TestGetMetaDocRejectsPathTraversalName pins the defense-in-depth barricade
// on the one helper that interpolates a name into a request path — even a
// future caller that skips the top-level gate cannot traverse.
func TestGetMetaDocRejectsPathTraversalName(t *testing.T) {
	var contacted int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		contacted++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	reg := NewCollectionRegistry(srv.Client(), srv.URL)

	if _, _, _, err := reg.getMetaDoc(context.Background(), "../../engram-episodic-000001/_search"); err == nil {
		t.Fatal("getMetaDoc accepted a traversal name")
	}
	if contacted != 0 {
		t.Errorf("getMetaDoc issued %d HTTP requests for a traversal name, want 0", contacted)
	}
}

// TestReservedNameCanNeverShadowMetaIndex documents WHY the grammar bans
// hyphens and reserves "collections": no legal collection's physical index
// can match the meta template pattern.
func TestReservedNameCanNeverShadowMetaIndex(t *testing.T) {
	for _, name := range []string{"collections", "collections-000001", "collections_x"} {
		physical := fmt.Sprintf("knowledge-%s-v1", name)
		matches := strings.HasPrefix(physical, "knowledge-collections-")
		legal := collectionNameRE.MatchString(name) && !reservedCollectionNames[name]
		if legal && matches {
			t.Errorf("legal name %q produces physical %q shadowing the meta pattern", name, physical)
		}
	}
}

// TestMetaDocRoundTripsFragmentSizing (Phase 2): the registry's persisted
// form carries the four sizing/tag knobs, so a collection's fragment
// configuration survives Create/Update -> Get through the meta-index — the
// third translation beside the proto and MCP ones (DW-2.2's persistence
// leg; this was the gap the Phase-2 integration test surfaced).
func TestMetaDocRoundTripsFragmentSizing(t *testing.T) {
	in := knowledge.CollectionSpec{
		Name: "docs", Index: "knowledge-docs-v1", TextField: "text",
		FragmentSize: 100, NumberOfFragments: 2,
		HighlightPreTag: "«", HighlightPostTag: "»",
	}
	doc := metaDocFrom(in, 1)
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var reloaded collectionMetaDoc
	if err := json.Unmarshal(raw, &reloaded); err != nil {
		t.Fatal(err)
	}
	out := reloaded.spec()
	if out.FragmentSize != 100 || out.NumberOfFragments != 2 ||
		out.HighlightPreTag != "«" || out.HighlightPostTag != "»" {
		t.Errorf("sizing/tags lost through the meta doc: %+v", out)
	}

	// Unset knobs stay unset (zero) on the wire so FragmentSizing's fallback
	// still owns the default, and the JSON omits them (pre-fragment rows are
	// byte-stable).
	plain := metaDocFrom(knowledge.CollectionSpec{Name: "d", Index: "i", TextField: "t"}, 1)
	rawPlain, _ := json.Marshal(plain)
	for _, key := range []string{"fragment_size", "number_of_fragments", "highlight_pre_tag", "highlight_post_tag"} {
		if strings.Contains(string(rawPlain), key) {
			t.Errorf("unset knob %q serialized: %s", key, rawPlain)
		}
	}
}

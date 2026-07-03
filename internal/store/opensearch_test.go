package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/store"
)

// fakeDoc is one document in the fake cluster below.
type fakeDoc struct {
	source      map[string]any
	seqNo       int64
	primaryTerm int64
}

// fakeOS simulates just enough of OpenSearch's document API — auto-id
// append, op_type=create, guarded update, and the two enrichment-job
// endpoints (a text_embedding-missing search, a partial update) — to unit
// test OpenSearchStore without a live cluster. The write-protocol primitives
// themselves (op_type=create 409, guarded update 409) were proven against
// the real pinned cluster by the Phase-0 spikes; this fake exercises
// OpenSearchStore's request/response mapping on top of them.
type fakeOS struct {
	mu     sync.Mutex
	docs   map[string]map[string]*fakeDoc
	autoID int
}

func newFakeOS() *fakeOS { return &fakeOS{docs: map[string]map[string]*fakeDoc{}} }

func (f *fakeOS) index(name string) map[string]*fakeDoc {
	if f.docs[name] == nil {
		f.docs[name] = map[string]*fakeDoc{}
	}
	return f.docs[name]
}

func (f *fakeOS) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/_doc"):
			f.handleAppend(w, r, strings.TrimSuffix(path, "/_doc"))
		case r.Method == http.MethodPut && strings.Contains(path, "/_create/"):
			parts := strings.SplitN(path, "/_create/", 2)
			f.handleCreate(w, r, parts[0], parts[1])
		case r.Method == http.MethodPut && strings.Contains(path, "/_doc/"):
			parts := strings.SplitN(path, "/_doc/", 2)
			f.handleUpdate(w, r, parts[0], parts[1])
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/_search"):
			f.handleSearch(w, r, strings.TrimSuffix(path, "/_search"))
		case r.Method == http.MethodPost && strings.Contains(path, "/_update/"):
			parts := strings.SplitN(path, "/_update/", 2)
			f.handlePartialUpdate(w, r, parts[0], parts[1])
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func (f *fakeOS) handleAppend(w http.ResponseWriter, r *http.Request, idx string) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.autoID++
	id := fmt.Sprintf("auto-%d", f.autoID)
	f.index(idx)[id] = &fakeDoc{source: body, seqNo: 1, primaryTerm: 1}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"_id": id, "result": "created"})
}

func (f *fakeOS) handleCreate(w http.ResponseWriter, r *http.Request, idx, id string) {
	if _, exists := f.index(idx)[id]; exists {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "version_conflict_engine_exception"}})
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.index(idx)[id] = &fakeDoc{source: body, seqNo: 1, primaryTerm: 1}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"_id": id, "result": "created", "_seq_no": 1.0, "_primary_term": 1.0})
}

func (f *fakeOS) handleUpdate(w http.ResponseWriter, r *http.Request, idx, id string) {
	doc, exists := f.index(idx)[id]
	if seqNoStr := r.URL.Query().Get("if_seq_no"); seqNoStr != "" {
		wantSeq, _ := strconv.ParseInt(seqNoStr, 10, 64)
		wantPT, _ := strconv.ParseInt(r.URL.Query().Get("if_primary_term"), 10, 64)
		if !exists || doc.seqNo != wantSeq || doc.primaryTerm != wantPT {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "version_conflict_engine_exception"}})
			return
		}
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	newSeq := int64(1)
	if exists {
		newSeq = doc.seqNo + 1
	}
	f.index(idx)[id] = &fakeDoc{source: body, seqNo: newSeq, primaryTerm: 1}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"_id": id, "result": "updated", "_seq_no": float64(newSeq), "_primary_term": 1.0})
}

// handleSearch answers exactly the query FindUnembedded issues: docs whose
// source has no "text_embedding" key.
func (f *fakeOS) handleSearch(w http.ResponseWriter, _ *http.Request, idx string) {
	var hits []map[string]any
	for id, d := range f.index(idx) {
		if _, has := d.source["text_embedding"]; has {
			continue
		}
		hits = append(hits, map[string]any{"_id": id, "_source": d.source})
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"hits": map[string]any{"hits": hits}})
}

func (f *fakeOS) handlePartialUpdate(w http.ResponseWriter, r *http.Request, idx, id string) {
	doc, exists := f.index(idx)[id]
	if !exists {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var body struct {
		Doc map[string]any `json:"doc"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	for k, v := range body.Doc {
		doc.source[k] = v
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"_id": id, "result": "updated"})
}

func newTestStore(t *testing.T) (*store.OpenSearchStore, *fakeOS) {
	t.Helper()
	fake := newFakeOS()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	return store.NewOpenSearchStore(srv.Client(), srv.URL, store.WithEpisodicIndex("ep"), store.WithSemanticIndex("sem")), fake
}

// TestOpenSearchStoreAppendReturnsDurableID covers DW-1.1's write half: an
// episodic Append succeeds and returns a non-empty durable id.
func TestOpenSearchStoreAppendReturnsDurableID(t *testing.T) {
	s, _ := newTestStore(t)
	id, err := s.Append(context.Background(), memory.Episodic{EventID: "ev-1", Text: "hello", TenantID: "t1"})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if id == "" {
		t.Fatal("Append returned empty id")
	}
}

// TestOpenSearchStoreCreateThenDuplicateConflicts pins the D11 collision
// mapping: a second op_type=create under the same id surfaces ErrConflict.
func TestOpenSearchStoreCreateThenDuplicateConflicts(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	fact := memory.SemanticFact{Statement: "v1", ContentKey: "ck1", ValidAt: time.Now()}
	if err := s.Create(ctx, "fact-1", fact); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	err := s.Create(ctx, "fact-1", fact)
	if err == nil {
		t.Fatal("duplicate Create should conflict")
	}
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("duplicate Create error = %v, want wrapping ErrConflict", err)
	}
}

// TestOpenSearchStoreUpdateGuardedConflict pins D7's guarded-close contract:
// a fresh guard succeeds, the now-stale guard loses with ErrConflict.
func TestOpenSearchStoreUpdateGuardedConflict(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	fact := memory.SemanticFact{Statement: "v1", ContentKey: "ck1", ValidAt: time.Now()}
	if err := s.Create(ctx, "fact-1", fact); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Update(ctx, "fact-1", fact, 1, 1); err != nil {
		t.Fatalf("guarded Update with fresh tokens: %v", err)
	}
	err := s.Update(ctx, "fact-1", fact, 1, 1) // now-stale guard
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("stale-guard Update error = %v, want wrapping ErrConflict", err)
	}
}

// TestOpenSearchStoreOutboxLedgerMethodsNotImplemented asserts the Phase-2
// deferral: OpenSearchStore satisfies store.Store fully (compiles as a
// Store), but its outbox/ledger methods report ErrNotImplemented rather than
// silently doing nothing.
func TestOpenSearchStoreOutboxLedgerMethodsNotImplemented(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if _, err := s.ClaimBatch(ctx, 10, time.Minute); !errors.Is(err, store.ErrNotImplemented) {
		t.Errorf("ClaimBatch error = %v, want ErrNotImplemented", err)
	}
	if err := s.Complete(ctx, "ev-1"); !errors.Is(err, store.ErrNotImplemented) {
		t.Errorf("Complete error = %v, want ErrNotImplemented", err)
	}
	if err := s.DeadLetter(ctx, "ev-1", "boom"); !errors.Is(err, store.ErrNotImplemented) {
		t.Errorf("DeadLetter error = %v, want ErrNotImplemented", err)
	}
	if _, err := s.ClaimLedger(ctx, memory.LedgerKey{}); !errors.Is(err, store.ErrNotImplemented) {
		t.Errorf("ClaimLedger error = %v, want ErrNotImplemented", err)
	}
	if err := s.UpdateLedger(ctx, memory.LedgerKey{}, store.LedgerState{}); !errors.Is(err, store.ErrNotImplemented) {
		t.Errorf("UpdateLedger error = %v, want ErrNotImplemented", err)
	}
	if _, err := s.ScanIncomplete(ctx); !errors.Is(err, store.ErrNotImplemented) {
		t.Errorf("ScanIncomplete error = %v, want ErrNotImplemented", err)
	}
}

// TestOpenSearchStoreFindUnembeddedAndSetTextEmbedding covers the
// embedding-enrichment plumbing: a freshly appended episodic doc (text-only,
// D15) is found as unembedded; after SetTextEmbedding it is not.
func TestOpenSearchStoreFindUnembeddedAndSetTextEmbedding(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	id, err := s.Append(ctx, memory.Episodic{EventID: "ev-1", Text: "needs embedding"})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	pending, err := s.FindUnembedded(ctx, 10)
	if err != nil {
		t.Fatalf("FindUnembedded: %v", err)
	}
	if len(pending) != 1 || pending[0].DocID != id || pending[0].Rec.Text != "needs embedding" {
		t.Fatalf("FindUnembedded = %+v, want one entry for %s", pending, id)
	}

	if err := s.SetTextEmbedding(ctx, id, []float32{0.1, 0.2, 0.3}); err != nil {
		t.Fatalf("SetTextEmbedding: %v", err)
	}

	pending, err = s.FindUnembedded(ctx, 10)
	if err != nil {
		t.Fatalf("FindUnembedded after fill: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("FindUnembedded after fill = %+v, want empty", pending)
	}
}

package graph

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// missingIndexServer stands up an httptest server that answers every
// _count read with the exact index_not_found_exception shape OpenSearch
// returns for a not-yet-created index — the DW-3.4 dirty-test knob, mirroring
// internal/store/robustness_test.go's errorServer/indexNotFoundServer.
func missingIndexServer(t *testing.T) *OpenSearchBackend {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"type": "index_not_found_exception", "reason": "no such index"},
		})
	}))
	t.Cleanup(srv.Close)
	return NewOpenSearchBackend(srv.Client(), srv.URL,
		WithIndices("engram-graph-entities-missing", "engram-graph-edges-missing"))
}

// TestDW_3_4_MissingEntityIndexCountsZero is the dirty test: a gauge poll
// against an entity index that does not exist yet must read 0, not error or
// panic — both the per-tenant and all-tenant count methods.
func TestDW_3_4_MissingEntityIndexCountsZero(t *testing.T) {
	backend := missingIndexServer(t)
	ctx := context.Background()

	if count, err := backend.CountEntities(ctx, "t1"); err != nil || count != 0 {
		t.Fatalf("CountEntities against a missing index = (%d, %v), want (0, nil)", count, err)
	}
	if count, err := backend.CountAllEntities(ctx); err != nil || count != 0 {
		t.Fatalf("CountAllEntities against a missing index = (%d, %v), want (0, nil)", count, err)
	}
}

// TestDW_3_4_GenuineErrorStillPropagates proves the guard is narrow: a
// non-index_not_found error status is NOT swallowed into a false 0.
func TestDW_3_4_GenuineErrorStillPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"type": "some_other_exception", "reason": "cluster unavailable"},
		})
	}))
	t.Cleanup(srv.Close)
	backend := NewOpenSearchBackend(srv.Client(), srv.URL,
		WithIndices("engram-graph-entities-broken", "engram-graph-edges-broken"))
	ctx := context.Background()

	if _, err := backend.CountEntities(ctx, "t1"); err == nil {
		t.Error("CountEntities against a genuine 500 = nil error, want a propagated error")
	}
	if _, err := backend.CountAllEntities(ctx); err == nil {
		t.Error("CountAllEntities against a genuine 500 = nil error, want a propagated error")
	}
}

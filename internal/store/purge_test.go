package store_test

// Barricade tests for PurgeEvent that must run on every `go test ./...` — the
// refusals that keep a destructive by-query request from ever being built.
// Each uses a canary httptest server that FAILS the test the moment any
// request reaches it, so these prove the guard runs BEFORE the network, not
// merely that some error eventually surfaces (the same construction
// knowledge_test.go's path-traversal tests use).
//
// The live-cluster semantics — replay duplicates, the semantic tombstone,
// ledger removal, dry run, missing indices — live in purge_integration_test.go.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ryanthedev/engram/internal/store"
)

// canaryStore returns a store whose every request fails the test, plus the
// caller-chosen index-name overrides. Nothing here should ever reach the wire.
func canaryStore(t *testing.T, opts ...store.Option) *store.OpenSearchStore {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request reached the network: %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	return store.NewOpenSearchStore(srv.Client(), srv.URL, opts...)
}

// TestPurgeEventRejectsEmptyKeys: a blank tenant or event id is refused up
// front. Either one would turn a targeted purge into a query nobody wrote —
// and unlike a bad search, the result is unrecoverable on the episodic tier.
func TestPurgeEventRejectsEmptyKeys(t *testing.T) {
	s := canaryStore(t)
	tests := []struct {
		name              string
		tenantID, eventID string
	}{
		{"empty event id", "t1", ""},
		{"empty tenant id", "", "ev-1"},
		{"both empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			counts, err := s.PurgeEvent(context.Background(), tt.tenantID, tt.eventID, false)
			if err == nil {
				t.Fatalf("PurgeEvent(%q, %q) returned nil error", tt.tenantID, tt.eventID)
			}
			if counts != (store.PurgeCounts{}) {
				t.Errorf("counts = %+v, want the zero value", counts)
			}
		})
	}
}

// TestPurgeEventRejectsEmptyKeysOnDryRun: the same refusal applies to a dry
// run. A dry run mutates nothing, but it must not quietly report the count of
// a query the caller never meant either — that number would be the evidence a
// later --confirm run is based on.
func TestPurgeEventRejectsEmptyKeysOnDryRun(t *testing.T) {
	s := canaryStore(t)
	if _, err := s.PurgeEvent(context.Background(), "t1", "", true); err == nil {
		t.Errorf("PurgeEvent(dryRun, empty event id) returned nil error")
	}
}

// TestPurgeEventRejectsPathTraversalIndex locks in the Phase-3 SECURITY
// LESSON one level in: the tier index names are option-settable, so they are
// validated before being interpolated into a REST path. Each case poisons ONE
// tier, proving all three are gated rather than just the first one queried.
func TestPurgeEventRejectsPathTraversalIndex(t *testing.T) {
	tests := []struct {
		name string
		opt  store.Option
	}{
		{"episodic", store.WithEpisodicIndex("../other-index")},
		{"ledger", store.WithLedgerIndex("../other-index")},
		{"semantic", store.WithSemanticIndex("../other-index")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := canaryStore(t, tt.opt)
			if _, err := s.PurgeEvent(context.Background(), "t1", "ev-1", false); err == nil {
				t.Errorf("PurgeEvent with a path-traversal %s index returned nil error", tt.name)
			}
			// The dry-run path must be gated identically: it is the same URL
			// construction, just against _count.
			if _, err := s.PurgeEvent(context.Background(), "t1", "ev-1", true); err == nil {
				t.Errorf("PurgeEvent(dryRun) with a path-traversal %s index returned nil error", tt.name)
			}
		})
	}
}

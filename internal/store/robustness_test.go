package store_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/store"
)

// errorServer stands up an httptest cluster that answers the cluster-root
// version probe (so Counts' clusterVersion succeeds) and returns a fixed
// status+body for every _search/_count read — the knob the robustness tests
// turn to reproduce a missing index vs. a genuine fault.
func errorServer(t *testing.T, status int, errType string) *store.OpenSearchStore {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			_ = json.NewEncoder(w).Encode(map[string]any{"version": map[string]any{"number": "3.1.0"}})
			return
		}
		w.WriteHeader(status)
		body := map[string]any{"status": status}
		if errType != "" {
			body["error"] = map[string]any{"type": errType, "reason": "no such index"}
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return store.NewOpenSearchStore(srv.Client(), srv.URL,
		store.WithEpisodicIndex("engram-episodic-missing"),
		store.WithSemanticIndex("engram-semantic-missing"),
		store.WithLedgerIndex("engram-ledger-missing"))
}

// indexNotFound is the exact shape the live cluster returns for a read against
// a not-yet-created index (verified against OpenSearch 3.1.0).
func indexNotFoundServer(t *testing.T) *store.OpenSearchStore {
	return errorServer(t, http.StatusNotFound, "index_not_found_exception")
}

func sampleFact() memory.SemanticFact {
	return memory.SemanticFact{
		Subject: "svc", Predicate: "owner", Object: "ana",
		Statement: "svc owner ana", ContentKey: "ck-1", TenantID: "t1",
		ValidAt: time.Now().UTC(),
	}
}

// TestDW_1_1_CandidatesMissingIndexEmpty is the regression: the reconcile
// candidate search against a semantic index that does not exist returns empty
// candidates, not an error. Before the 404-as-empty guard the raw 404 fell
// through to the "unexpected status" branch and the worker retried the first
// fact forever. Reverting isIndexNotFound's use in searchFacts turns this red.
func TestDW_1_1_CandidatesMissingIndexEmpty(t *testing.T) {
	s := indexNotFoundServer(t)
	cands, err := s.Candidates(context.Background(), sampleFact(), 10)
	if err != nil {
		t.Fatalf("Candidates against a missing index = error %v, want empty + nil", err)
	}
	if len(cands) != 0 {
		t.Fatalf("Candidates against a missing index = %d candidates, want 0", len(cands))
	}
}

// TestDW_1_1_AllReadPathsEmptyOnMissingIndex proves every enumerated read path
// — the shared searchFacts helper AND each inline _search/_count site — treats
// a missing index as empty, so guarding one wrapper is demonstrably not what
// this relies on. A single erroring path fails the test.
func TestDW_1_1_AllReadPathsEmptyOnMissingIndex(t *testing.T) {
	s := indexNotFoundServer(t)
	ctx := context.Background()
	f := sampleFact()

	assertNoErr := func(name string, err error) {
		t.Helper()
		if err != nil {
			t.Errorf("%s against a missing index = error %v, want empty + nil", name, err)
		}
	}

	// searchFacts-backed reads.
	_, err := s.Candidates(ctx, f, 10)
	assertNoErr("Candidates", err)
	_, _, err = s.ValidTimeNeighbors(ctx, f, "self")
	assertNoErr("ValidTimeNeighbors", err)
	_, err = s.LiveSuperseders(ctx, 10)
	assertNoErr("LiveSuperseders", err)
	_, err = s.LiveByContentKey(ctx, "ck-1")
	assertNoErr("LiveByContentKey", err)
	_, err = s.ChainVersions(ctx, store.ChainKey{TenantID: "t1", Subject: "svc", Predicate: "owner"})
	assertNoErr("ChainVersions", err)

	// Inline _search sites.
	_, err = s.DuplicateLiveContentKeys(ctx, 10)
	assertNoErr("DuplicateLiveContentKeys", err)
	_, err = s.ClosedOverlapChainKeys(ctx, 10)
	assertNoErr("ClosedOverlapChainKeys", err)
	_, err = s.FindByEventID(ctx, "ev-1")
	assertNoErr("FindByEventID", err)
	_, err = s.ScanIncomplete(ctx)
	assertNoErr("ScanIncomplete", err)
	_, err = s.ClaimBatch(ctx, 5, time.Minute)
	assertNoErr("ClaimBatch", err)
	_, err = s.FindUnembedded(ctx, 5)
	assertNoErr("FindUnembedded", err)

	// Inline _count / telemetry sites.
	ep, sem, _, err := s.Counts(ctx, "t1")
	assertNoErr("Counts", err)
	if ep != 0 || sem != 0 {
		t.Errorf("Counts on missing indices = ep %d sem %d, want 0/0", ep, sem)
	}
	cnt, age, err := s.PendingBacklog(ctx)
	assertNoErr("PendingBacklog", err)
	if cnt != 0 || age != 0 {
		t.Errorf("PendingBacklog on missing index = count %d age %v, want 0/0", cnt, age)
	}
	dlq, err := s.DeadLetteredCount(ctx)
	assertNoErr("DeadLetteredCount", err)
	if dlq != 0 {
		t.Errorf("DeadLetteredCount on missing index = %d, want 0", dlq)
	}
}

// TestDW_1_4_NonIndexNotFoundErrorsPropagate is the dirty test: the guard
// matches ONLY index_not_found_exception. A 404 with a different error type, a
// non-404 status, and a transport failure each still surface as an error from
// a read path — never silently swallowed as "empty".
func TestDW_1_4_NonIndexNotFoundErrorsPropagate(t *testing.T) {
	ctx := context.Background()
	f := sampleFact()

	t.Run("404 with a different error type", func(t *testing.T) {
		s := errorServer(t, http.StatusNotFound, "some_other_exception")
		if _, err := s.Candidates(ctx, f, 10); err == nil {
			t.Fatal("Candidates on a non-index_not_found 404 = nil error, want an error")
		}
	})

	t.Run("500 internal error", func(t *testing.T) {
		s := errorServer(t, http.StatusInternalServerError, "search_phase_execution_exception")
		if _, err := s.Candidates(ctx, f, 10); err == nil {
			t.Fatal("Candidates on a 500 = nil error, want an error")
		}
	})

	t.Run("transport failure", func(t *testing.T) {
		// Stand a server up, capture its client+URL, then close it so the next
		// request fails at the transport layer (connection refused) — doJSON
		// returns a non-nil err the guard must not reach.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		s := store.NewOpenSearchStore(srv.Client(), srv.URL, store.WithSemanticIndex("engram-semantic-missing"))
		srv.Close()
		if _, err := s.Candidates(ctx, f, 10); err == nil {
			t.Fatal("Candidates on a transport failure = nil error, want an error")
		}
	})
}

// TestDW_1_3_EnsureIndicesRejectsOffPatternName proves the boot-ensure step
// validates configured names against their engram-<tier>* template pattern: an
// override that would inherit the WRONG template (a semantic index misnamed
// under the episodic pattern, first in the validation loop) is rejected loudly
// before any index is created — the correctness guard on the belt-and-suspender
// design. No cluster needed: validation precedes the HTTP call.
func TestDW_1_3_EnsureIndicesRejectsOffPatternName(t *testing.T) {
	s := store.NewOpenSearchStore(&http.Client{}, "http://unused.invalid",
		store.WithEpisodicIndex("engram-semantic-oops")) // off-pattern for the episodic slot
	if _, err := s.EnsureIndices(context.Background()); err == nil {
		t.Fatal("EnsureIndices accepted an off-pattern index name; want a loud error before any creation")
	}
}

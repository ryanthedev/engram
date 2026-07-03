//go:build integration

package spike

import (
	"fmt"
	"net/http"
	"testing"
)

// TestDW_0_9a_OpTypeCreate409Flow is spike 1: the write protocol's collision
// primitive (D11) and guarded close (D10 step 4) against the live 3.1.0
// cluster. It proves: (1) op_type=create returns 201 then 409 on the same
// _id; (2) a guarded update with fresh if_seq_no/if_primary_term succeeds;
// (3) a stale guard returns 409 without applying.
func TestDW_0_9a_OpTypeCreate409Flow(t *testing.T) {
	base := cluster(t)
	idx := "engram-spike-create"
	deleteIndex(t, base, idx)
	defer deleteIndex(t, base, idx)

	docURL := fmt.Sprintf("%s/%s/_create/%s", base, idx, "fact-1")

	// 1) First op_type=create wins.
	status, body := call(t, http.MethodPut, docURL, map[string]any{"statement": "v1"})
	if status != http.StatusCreated {
		t.Fatalf("first create: got %d %v, want 201", status, body)
	}
	t.Logf("finding: first _create → %d (result=%v, _seq_no=%v, _primary_term=%v)",
		status, body["result"], body["_seq_no"], body["_primary_term"])

	// 2) Identical concurrent extraction (same content-addressed _id) loses
	// with 409 — the D11 ADD-vs-ADD race resolution.
	status, body = call(t, http.MethodPut, docURL, map[string]any{"statement": "v1-duplicate"})
	if status != http.StatusConflict {
		t.Fatalf("duplicate create: got %d %v, want 409", status, body)
	}
	errType := dig(body, "error", "type")
	t.Logf("finding: duplicate _create → 409 (error.type=%v)", errType)
	if errType != "version_conflict_engine_exception" {
		t.Errorf("expected version_conflict_engine_exception, got %v — Store must map this to ErrConflict", errType)
	}

	// 3) Read the concurrency tokens, then close under a fresh guard.
	status, body = call(t, http.MethodGet, fmt.Sprintf("%s/%s/_doc/fact-1", base, idx), nil)
	if status != http.StatusOK {
		t.Fatalf("get: %d %v", status, body)
	}
	seqNo, primaryTerm := num(body["_seq_no"]), num(body["_primary_term"])

	guardedURL := fmt.Sprintf("%s/%s/_doc/fact-1?if_seq_no=%d&if_primary_term=%d", base, idx, seqNo, primaryTerm)
	status, body = call(t, http.MethodPut, guardedURL, map[string]any{"statement": "v1", "invalid_at": "2026-07-03T00:00:00Z"})
	if status != http.StatusOK {
		t.Fatalf("guarded update with fresh tokens: got %d %v, want 200", status, body)
	}
	t.Logf("finding: guarded update (if_seq_no=%d if_primary_term=%d) → %d, new _seq_no=%v",
		seqNo, primaryTerm, status, body["_seq_no"])

	// 4) The same (now stale) guard loses with 409 — a lost-update is
	// impossible; the caller must re-read and retry (bounded).
	status, body = call(t, http.MethodPut, guardedURL, map[string]any{"statement": "lost-update"})
	if status != http.StatusConflict {
		t.Fatalf("stale guarded update: got %d %v, want 409", status, body)
	}
	t.Logf("finding: stale guard → 409 (error.type=%v); D7/D10 guarded-close semantics verified", dig(body, "error", "type"))
}

// dig walks nested string-keyed maps.
func dig(m map[string]any, keys ...string) any {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

func num(v any) int64 {
	f, _ := v.(float64)
	return int64(f)
}

//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/store"
	"github.com/ryanthedev/engram/internal/testutil"
)

// TestDW_3_4_TokenStoreRoundTripHashedOnly proves the OpenSearch token store
// on the live pinned cluster: issue → verify → revoke → verify, and that the
// persisted _source carries no raw token (hashed-only at rest).
func TestDW_3_4_TokenStoreRoundTripHashedOnly(t *testing.T) {
	base := testutil.OpenSearchURL()
	if _, err := store.Apply(context.Background(), testutil.HTTPClient, base); err != nil {
		t.Fatalf("apply: %v", err)
	}
	idx := testutil.ScratchIndexName("auth-tokens", t.Name())
	testutil.DeleteIndex(t, base, idx)
	t.Cleanup(func() { testutil.DeleteIndex(t, base, idx) })
	testutil.CreateScratchIndex(t, base, idx)

	ts := store.NewAuthTokenStore(testutil.HTTPClient, base, store.WithAuthTokenIndex(idx))
	iss := auth.NewTokenIssuer(ts, nil)
	authn := auth.NewAuthenticator(ts, nil)
	id := auth.Identity{TenantID: "t1", UserID: "alice", AgentID: "claude"}
	ctx := context.Background()

	raw, handle, err := iss.Issue(ctx, id, time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	got, err := authn.Verify(ctx, raw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got != id {
		t.Fatalf("Verify identity = %+v, want %+v", got, id)
	}

	// The stored _source must not contain the raw token anywhere.
	status, doc := testutil.Call(t, http.MethodGet, base+"/"+idx+"/_search?q=*", nil)
	if status != http.StatusOK {
		t.Fatalf("search tokens: %d", status)
	}
	if strings.Contains(mustJSON(t, doc), raw) {
		t.Fatal("persisted token source contains the raw token — must be hashed-only")
	}

	list, err := iss.List(ctx, id)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != handle {
		t.Fatalf("List = %+v, want one handle %s", list, handle)
	}

	if err := iss.Revoke(ctx, handle); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := authn.Verify(ctx, raw); err != auth.ErrTokenRevoked {
		t.Fatalf("Verify after revoke = %v, want ErrTokenRevoked", err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

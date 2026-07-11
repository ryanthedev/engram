package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/memory"
)

// TestOpenSearchStoreGetEpisodic pins the realtime by-id read behind the
// Read RPC's episodic branch (Phase 2 drill-down): an existing doc returns
// its FULL stored record — text and the ACL fields the server authorizes
// against before projecting — and a missing id reports ok=false without
// error (the NOT_FOUND path, mirroring GetFact).
func TestOpenSearchStoreGetEpisodic(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	rec := memory.Episodic{
		EventID: "ev-1", TenantID: "t1", TeamID: "teamX", Scope: "team",
		OwnerAgentID: "a1", Kind: "tool_result", Text: "the full fat body 👻",
		OccurredAt: time.Unix(3000, 0).UTC(), CreatedAt: time.Unix(3001, 0).UTC(),
	}
	id, err := s.Append(ctx, rec)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, ok, err := s.GetEpisodic(ctx, id)
	if err != nil || !ok {
		t.Fatalf("GetEpisodic(%s) = ok=%v err=%v, want found", id, ok, err)
	}
	if got.Text != rec.Text || got.EventID != "ev-1" || got.Kind != "tool_result" {
		t.Errorf("GetEpisodic content = %+v", got)
	}
	// The getter must return the ACL fields intact: authorization happens
	// AFTER this fetch, on these fields (fetch -> authorize -> project).
	if got.TenantID != "t1" || got.TeamID != "teamX" || got.Scope != "team" || got.OwnerAgentID != "a1" {
		t.Errorf("GetEpisodic ACL fields = tenant=%s team=%s scope=%s owner=%s, want them intact for authorization",
			got.TenantID, got.TeamID, got.Scope, got.OwnerAgentID)
	}

	if _, ok, err := s.GetEpisodic(ctx, "missing"); err != nil || ok {
		t.Errorf("GetEpisodic(missing) = ok=%v err=%v, want ok=false err=nil", ok, err)
	}
}

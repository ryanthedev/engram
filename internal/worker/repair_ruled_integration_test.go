//go:build integration

package worker_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/store"
	"github.com/ryanthedev/engram/internal/testutil"
)

// TestRuleD_LiveClosedOverlapConverges proves sweep rule (d) on the real
// pinned cluster: two overlapping CLOSED records plus a live head are indexed
// directly (the write-skew end state the Phase-2 review adjudicated
// irreducible), and one sweep pass trims the earlier record to its nearest
// valid-time successor via the OpenSearch composite-agg scan + ChainVersions
// read, leaving the chain pairwise-disjoint with the live head untouched.
func TestRuleD_LiveClosedOverlapConverges(t *testing.T) {
	_, sw, s, base, _, semIdx := liveWorker(t, "ruled")
	ctx := context.Background()

	tv0 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	tv1 := tv0.Add(24 * time.Hour)
	tvMid := tv0.Add(6 * time.Hour)

	seedDoc := func(id, object string, validAt time.Time, invalidAt *time.Time) {
		f := memory.SemanticFact{
			Subject: "svc", Predicate: "owner", Object: object,
			Statement: "svc owner " + object, TenantID: "t1",
			ContentKey: memory.ContentKey("t1", "svc", "owner", object),
			ValidAt:    validAt, InvalidAt: invalidAt, CreatedAt: tv0,
		}
		status, body := testutil.Call(t, http.MethodPut, base+"/"+semIdx+"/_doc/"+id, f)
		if status != http.StatusCreated && status != http.StatusOK {
			t.Fatalf("seeding %s: status %d: %v", id, status, body)
		}
	}
	inv := tv1
	seedDoc("ana", "ana", tv0, &inv)   // [tv0, tv1) — overlaps eve
	seedDoc("eve", "eve", tvMid, &inv) // [tv0+6h, tv1) — overlaps ana's tail
	seedDoc("cid", "cid", tv1, nil)    // live head @tv1
	testutil.RefreshIndex(t, base, semIdx)

	rep, err := sw.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rep.OverlapsTrimmed != 1 {
		t.Errorf("OverlapsTrimmed = %d, want 1", rep.OverlapsTrimmed)
	}
	testutil.RefreshIndex(t, base, semIdx)

	versions, err := s.ChainVersions(ctx, store.ChainKey{TenantID: "t1", Subject: "svc", Predicate: "owner"})
	if err != nil {
		t.Fatalf("ChainVersions: %v", err)
	}
	byObj := map[string]memory.SemanticFact{}
	for _, v := range versions {
		byObj[v.Fact.Object] = v.Fact
	}
	if ana := byObj["ana"]; ana.InvalidAt == nil || !ana.InvalidAt.Equal(tvMid) {
		t.Fatalf("ana.invalid_at = %v, want trimmed to eve's valid_at %v", ana.InvalidAt, tvMid)
	}
	if cid := byObj["cid"]; cid.InvalidAt != nil {
		t.Errorf("live head cid must stay live, got invalid_at %v", cid.InvalidAt)
	}
	// Assert pairwise-disjoint closed intervals across the live chain.
	for i := range versions {
		for j := i + 1; j < len(versions); j++ {
			a, b := versions[i].Fact, versions[j].Fact
			if a.InvalidAt == nil || b.InvalidAt == nil {
				continue
			}
			lo, hi := a, b
			if b.ValidAt.Before(a.ValidAt) {
				lo, hi = b, a
			}
			if lo.InvalidAt.After(hi.ValidAt) {
				t.Errorf("overlap remains: [%v,%v) and [%v,%v)", lo.ValidAt, *lo.InvalidAt, hi.ValidAt, *hi.InvalidAt)
			}
		}
	}
}

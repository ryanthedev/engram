//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/acl"
	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/store"
	"github.com/ryanthedev/engram/internal/testutil"
)

func liveBase(t *testing.T) string {
	t.Helper()
	base := testutil.OpenSearchURL()
	if _, err := store.Apply(context.Background(), testutil.HTTPClient, base); err != nil {
		t.Fatalf("apply: %v", err)
	}
	return base
}

func scratchEdges(t *testing.T, base string) *store.ACLEdgeStore {
	t.Helper()
	idx := testutil.ScratchIndexName("acl-edges", t.Name())
	testutil.DeleteIndex(t, base, idx)
	t.Cleanup(func() { testutil.DeleteIndex(t, base, idx) })
	testutil.CreateScratchIndex(t, base, idx)
	return store.NewACLEdgeStore(testutil.HTTPClient, base, store.WithACLEdgesIndex(idx))
}

// TestDW_4_3_AppendDeniedByGuard proves write-time scope authorization through
// the real store: with a ScopeGuard registered, an agent appending a scope its
// identity does not reach gets a typed denial and nothing is written; scopes it
// reaches succeed; and a worker-style write (no client identity) is unguarded.
func TestDW_4_3_AppendDeniedByGuard(t *testing.T) {
	base := liveBase(t)
	epIdx := testutil.ScratchIndexName("episodic", t.Name())
	testutil.DeleteIndex(t, base, epIdx)
	t.Cleanup(func() { testutil.DeleteIndex(t, base, epIdx) })
	testutil.CreateScratchIndex(t, base, epIdx)

	edges := scratchEdges(t, base)
	if err := edges.PutEdge(context.Background(), acl.Edge{Type: acl.EdgeMember, TenantID: "t1", UserID: "u1", TeamID: "teamX"}); err != nil {
		t.Fatalf("PutEdge: %v", err)
	}

	st := store.NewOpenSearchStore(testutil.HTTPClient, base, store.WithEpisodicIndex(epIdx))
	st.RegisterWriteGuard(acl.NewScopeGuard(edges))

	u1 := auth.ContextWithIdentity(context.Background(), auth.Identity{TenantID: "t1", UserID: "u1", AgentID: "a1"})
	rec := func(scope, team string) memory.Episodic {
		return memory.Episodic{EventID: "e", TenantID: "t1", OwnerAgentID: "a1", Scope: scope, TeamID: team, Text: "x", CreatedAt: time.Now().UTC()}
	}

	// Allowed: private, and team for a team the identity is a member of.
	if _, err := st.Append(u1, rec(acl.ScopePrivate, "")); err != nil {
		t.Fatalf("private append denied: %v", err)
	}
	if _, err := st.Append(u1, rec(acl.ScopeTeam, "teamX")); err != nil {
		t.Fatalf("team (member) append denied: %v", err)
	}

	// Denied: team for a non-member team → typed ErrScopeDenied, nothing written.
	if id, err := st.Append(u1, rec(acl.ScopeTeam, "teamY")); !errors.Is(err, acl.ErrScopeDenied) || id != "" {
		t.Fatalf("unauthorized team append = (%q, %v), want ('', ErrScopeDenied)", id, err)
	}
	// Denied: unknown scope.
	if _, err := st.Append(u1, rec("galactic", "")); !errors.Is(err, acl.ErrUnknownScope) {
		t.Fatalf("unknown-scope append err = %v, want ErrUnknownScope", err)
	}

	// Worker-style write (no client identity in ctx) is trusted → unguarded.
	if _, err := st.Append(context.Background(), rec(acl.ScopeTeam, "teamZ")); err != nil {
		t.Fatalf("worker (no identity) append denied: %v", err)
	}
}

// TestMalformedEdgeRejectedAtWrite: a structurally invalid edge is rejected by
// the store with ErrMalformedEdge and never persisted (edge-case hardening).
func TestMalformedEdgeRejectedAtWrite(t *testing.T) {
	base := liveBase(t)
	edges := scratchEdges(t, base)
	err := edges.PutEdge(context.Background(), acl.Edge{Type: acl.EdgeUserAgent, TenantID: "t1", UserID: "u1"}) // missing agent
	if !errors.Is(err, acl.ErrMalformedEdge) {
		t.Fatalf("PutEdge(malformed) = %v, want ErrMalformedEdge", err)
	}
	got, err := edges.ListEdges(context.Background(), auth.Identity{TenantID: "t1", UserID: "u1"})
	if err != nil {
		t.Fatalf("ListEdges: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("malformed edge persisted: %v", got)
	}
}

// TestReachabilityRoundTrip: grant/list/revoke of edges reflects in Reachability
// (the read the ACL uses), and self is always reachable.
func TestReachabilityRoundTrip(t *testing.T) {
	base := liveBase(t)
	edges := scratchEdges(t, base)
	ctx := context.Background()
	for _, e := range []acl.Edge{
		{Type: acl.EdgeUserAgent, TenantID: "t1", UserID: "u1", AgentID: "a2"},
		{Type: acl.EdgeMember, TenantID: "t1", UserID: "u1", TeamID: "teamX"},
		{Type: acl.EdgeOrgGrant, TenantID: "t1", UserID: "u1"},
	} {
		if err := edges.PutEdge(ctx, e); err != nil {
			t.Fatalf("PutEdge: %v", err)
		}
	}
	reach, err := edges.Reachability(ctx, auth.Identity{TenantID: "t1", UserID: "u1", AgentID: "a1"})
	if err != nil {
		t.Fatalf("Reachability: %v", err)
	}
	if !contains(reach.Agents, "a1") || !contains(reach.Agents, "a2") {
		t.Errorf("agents = %v, want a1(self)+a2", reach.Agents)
	}
	if !contains(reach.Teams, "teamX") || !reach.OrgGrant {
		t.Errorf("teams=%v orgGrant=%v", reach.Teams, reach.OrgGrant)
	}
	// Revoke a2 → gone from reachability.
	if _, err := edges.DeleteEdge(ctx, acl.Edge{Type: acl.EdgeUserAgent, TenantID: "t1", UserID: "u1", AgentID: "a2"}); err != nil {
		t.Fatalf("DeleteEdge: %v", err)
	}
	reach, _ = edges.Reachability(ctx, auth.Identity{TenantID: "t1", UserID: "u1", AgentID: "a1"})
	if contains(reach.Agents, "a2") {
		t.Errorf("a2 still reachable after revoke: %v", reach.Agents)
	}
}

// TestDW_4_6_AuditHistory: AuditFact returns the target fact's provenance and
// the FULL bi-temporal chain — including a valid-time-closed predecessor — for
// a (tenant, subject, predicate) chain. DW-4.6 (store layer).
func TestDW_4_6_AuditHistory(t *testing.T) {
	base := liveBase(t)
	semIdx := testutil.ScratchIndexName("semantic", t.Name())
	testutil.DeleteIndex(t, base, semIdx)
	t.Cleanup(func() { testutil.DeleteIndex(t, base, semIdx) })
	testutil.CreateScratchIndex(t, base, semIdx)

	past := time.Now().UTC().Add(-2 * time.Hour)
	closed := past.Add(time.Hour)
	// v1: closed valid interval (superseded); v2: live head, supersedes v1.
	testutil.IndexDoc(t, base, semIdx, "v1", map[string]any{
		"subject": "svc", "predicate": "owner", "object": "team-a", "statement": "svc owner team-a",
		"tenant_id": "t1", "scope": "team", "team_id": "teamX", "owner_agent_id": "a1",
		"source_ids": []string{"ev-1"}, "extractor_version": "v1", "content_key": "ck1",
		"valid_at": past.Format(time.RFC3339Nano), "invalid_at": closed.Format(time.RFC3339Nano),
		"created_at": past.Format(time.RFC3339Nano),
	})
	testutil.IndexDoc(t, base, semIdx, "v2", map[string]any{
		"subject": "svc", "predicate": "owner", "object": "team-b", "statement": "svc owner team-b",
		"tenant_id": "t1", "scope": "team", "team_id": "teamX", "owner_agent_id": "a1",
		"source_ids": []string{"ev-2"}, "extractor_version": "v1", "content_key": "ck2", "supersedes": "v1",
		"valid_at": closed.Format(time.RFC3339Nano), "created_at": closed.Format(time.RFC3339Nano),
	})
	testutil.RefreshIndex(t, base, semIdx)

	st := store.NewOpenSearchStore(testutil.HTTPClient, base, store.WithSemanticIndex(semIdx))
	target, versions, ok, err := st.AuditFact(context.Background(), "v2")
	if err != nil || !ok {
		t.Fatalf("AuditFact = (ok=%v, err=%v)", ok, err)
	}
	if target.Fact.OwnerAgentID != "a1" || len(target.Fact.SourceIDs) != 1 || target.Fact.SourceIDs[0] != "ev-2" {
		t.Errorf("provenance = %+v", target.Fact)
	}
	if len(versions) != 2 {
		t.Fatalf("history len = %d, want 2 (v1 closed + v2 live)", len(versions))
	}
	if versions[0].ID != "v1" || versions[1].ID != "v2" {
		t.Errorf("history order = %s,%s, want v1,v2 (chronological)", versions[0].ID, versions[1].ID)
	}
	if versions[0].Fact.InvalidAt == nil {
		t.Error("v1 should carry a closed valid interval in the audit history")
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

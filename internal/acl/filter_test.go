package acl_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/ryanthedev/engram/internal/acl"
	"github.com/ryanthedev/engram/internal/auth"
)

// fakeEdges is an in-memory acl.EdgeSource keyed by user id. Tests set a user's
// Reach directly and mutate it to simulate grant/revoke — no cluster needed.
type fakeEdges struct {
	reach map[string]acl.Reach
	err   error
}

func (f *fakeEdges) Reachability(_ context.Context, id auth.Identity) (acl.Reach, error) {
	if f.err != nil {
		return acl.Reach{}, f.err
	}
	return f.reach[id.UserID], nil
}

func id(user, agent string) auth.Identity {
	return auth.Identity{TenantID: "t1", UserID: user, AgentID: agent}
}

func rec(owner, scope, team string) acl.Record {
	return acl.Record{TenantID: "t1", OwnerAgentID: owner, Scope: scope, TeamID: team}
}

// matrixEdges builds the reachability for the DW-4.1 read matrix:
//   - u1/a1 reaches a1(self)+a2 (a user_agent grant), team teamX
//   - u2/a2 reaches a2(self), team teamX
//   - u3/a3 reaches a3(self), team teamY
func matrixEdges() *fakeEdges {
	return &fakeEdges{reach: map[string]acl.Reach{
		"u1": {Agents: []string{"a1", "a2"}, Teams: []string{"teamX"}},
		"u2": {Agents: []string{"a2"}, Teams: []string{"teamX"}},
		"u3": {Agents: []string{"a3"}, Teams: []string{"teamY"}},
	}}
}

// TestDW_4_1_ScopeRuleMatrix proves the read rule across 3 identities × the 3
// scopes: private is self-only, team needs both agent-reachability and team
// membership, org needs agent-reachability. Every cell must match exactly.
func TestDW_4_1_ScopeRuleMatrix(t *testing.T) {
	f := acl.NewFilter(matrixEdges(), slog.Default())
	records := map[string]acl.Record{
		"a1_priv": rec("a1", acl.ScopePrivate, ""),
		"a1_team": rec("a1", acl.ScopeTeam, "teamX"),
		"a1_org":  rec("a1", acl.ScopeOrg, ""),
		"a2_team": rec("a2", acl.ScopeTeam, "teamX"),
		"a2_org":  rec("a2", acl.ScopeOrg, ""),
		"a3_team": rec("a3", acl.ScopeTeam, "teamY"),
	}
	// want[user][record] = authorized.
	want := map[string]map[string]bool{
		"u1": {"a1_priv": true, "a1_team": true, "a1_org": true, "a2_team": true, "a2_org": true, "a3_team": false},
		"u2": {"a1_priv": false, "a1_team": false, "a1_org": false, "a2_team": true, "a2_org": true, "a3_team": false},
		"u3": {"a1_priv": false, "a1_team": false, "a1_org": false, "a2_team": false, "a2_org": false, "a3_team": true},
	}
	users := map[string]auth.Identity{"u1": id("u1", "a1"), "u2": id("u2", "a2"), "u3": id("u3", "a3")}
	for user, cells := range want {
		enf, err := f.Enforce(context.Background(), users[user])
		if err != nil {
			t.Fatalf("Enforce(%s): %v", user, err)
		}
		for rname, exp := range cells {
			if got := enf.Authorize(records[rname]); got != exp {
				t.Errorf("Authorize(%s, %s) = %v, want %v", user, rname, got, exp)
			}
		}
	}
}

// TestDW_4_2_RevokeDropsReachableAgent: revoking u1's user_agent edge to a2
// (removing a2 from u1's reachable agents) hides a2's team AND org facts from
// u1 on the next Enforce, while u1's own private/team/org facts stay visible.
func TestDW_4_2_RevokeDropsReachableAgent(t *testing.T) {
	edges := matrixEdges()
	f := acl.NewFilter(edges, slog.Default())
	ctx, u1 := context.Background(), id("u1", "a1")

	before, _ := f.Enforce(ctx, u1)
	if !before.Authorize(rec("a2", acl.ScopeTeam, "teamX")) || !before.Authorize(rec("a2", acl.ScopeOrg, "")) {
		t.Fatal("precondition: u1 should see a2's team+org facts before revoke")
	}

	// Revoke: a2 no longer reachable from u1 (edge deleted).
	edges.reach["u1"] = acl.Reach{Agents: []string{"a1"}, Teams: []string{"teamX"}}

	after, _ := f.Enforce(ctx, u1)
	if after.Authorize(rec("a2", acl.ScopeTeam, "teamX")) {
		t.Error("after revoke: a2's team fact still visible to u1")
	}
	if after.Authorize(rec("a2", acl.ScopeOrg, "")) {
		t.Error("after revoke: a2's org fact still visible to u1")
	}
	// u1's own facts remain visible — revocation is targeted.
	if !after.Authorize(rec("a1", acl.ScopePrivate, "")) || !after.Authorize(rec("a1", acl.ScopeTeam, "teamX")) {
		t.Error("after revoke: u1 lost visibility of its own facts")
	}
}

// TestDW_4_4_EmptyReachabilityDenyAll: a valid identity that reaches nothing
// (blank agent, no edges) compiles to a match_none clause, authorizes nothing,
// and logs a denial (fail-closed).
func TestDW_4_4_EmptyReachabilityDenyAll(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	edges := &fakeEdges{reach: map[string]acl.Reach{"ghost": {}}} // no agents at all
	f := acl.NewFilter(edges, logger)

	enf, err := f.Enforce(context.Background(), auth.Identity{TenantID: "t1", UserID: "ghost"})
	if err != nil {
		t.Fatalf("Enforce: %v", err)
	}
	if _, isNone := enf.Clause()["match_none"]; !isNone {
		t.Errorf("empty reachability clause = %v, want match_none", enf.Clause())
	}
	if enf.Authorize(rec("a1", acl.ScopeOrg, "")) || enf.Authorize(rec("a1", acl.ScopePrivate, "")) {
		t.Error("empty-reachability enforcer authorized a record")
	}
	if !strings.Contains(buf.String(), "acl deny") {
		t.Errorf("no denial logged; log=%q", buf.String())
	}
}

// TestEnforceEdgeErrorFailsClosed: an edge-store error yields a deny-all
// enforcer AND the error, and logs a denial — the caller (retriever) short-
// circuits to zero results rather than running unfiltered.
func TestEnforceEdgeErrorFailsClosed(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	f := acl.NewFilter(&fakeEdges{err: errors.New("opensearch down")}, logger)

	enf, err := f.Enforce(context.Background(), id("u1", "a1"))
	if err == nil {
		t.Fatal("expected an error from the edge failure")
	}
	if _, isNone := enf.Clause()["match_none"]; !isNone {
		t.Error("edge-error enforcer did not deny-all")
	}
	if enf.Authorize(rec("a1", acl.ScopePrivate, "")) {
		t.Error("edge-error enforcer authorized a record")
	}
	if !strings.Contains(buf.String(), "acl deny") {
		t.Errorf("no denial logged; log=%q", buf.String())
	}
}

// TestZeroEnforcerDeniesAll: the Enforcer zero value denies everything — the
// fail-closed-by-construction property a caller relies on if it forgets to
// check an error.
func TestZeroEnforcerDeniesAll(t *testing.T) {
	var zero acl.Enforcer
	if _, isNone := zero.Clause()["match_none"]; !isNone {
		t.Error("zero Enforcer clause is not match_none")
	}
	if zero.Authorize(rec("a1", acl.ScopePrivate, "")) {
		t.Error("zero Enforcer authorized a record")
	}
}

// TestInvalidIdentityDeniesWithoutEdgeRead: an invalid identity denies all and
// never touches the edge store (no read to leak).
func TestInvalidIdentityDeniesWithoutEdgeRead(t *testing.T) {
	edges := &fakeEdges{err: errors.New("must not be called")}
	f := acl.NewFilter(edges, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	enf, err := f.Enforce(context.Background(), auth.Identity{TenantID: "t1"}) // no user
	if err != nil {
		t.Fatalf("invalid identity should not error, got %v", err)
	}
	if enf.Authorize(rec("a1", acl.ScopePrivate, "")) {
		t.Error("invalid identity authorized a record")
	}
}

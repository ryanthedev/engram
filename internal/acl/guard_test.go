package acl_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ryanthedev/engram/internal/acl"
	"github.com/ryanthedev/engram/internal/auth"
)

// TestDW_4_3_ScopeGuardDeniesUnauthorized is the write-rule dirty test: an
// identity writing a scope it does not reach gets a typed denial; scopes it
// reaches are allowed; an unknown scope is rejected.
func TestDW_4_3_ScopeGuardDeniesUnauthorized(t *testing.T) {
	edges := &fakeEdges{reach: map[string]acl.Reach{
		// u1 is a member of teamX and holds an org grant; u2 has neither.
		"u1": {Agents: []string{"a1"}, Teams: []string{"teamX"}, OrgGrant: true},
		"u2": {Agents: []string{"a2"}},
	}}
	g := acl.NewScopeGuard(edges)
	ctx := context.Background()
	u1, u2 := id("u1", "a1"), id("u2", "a2")

	cases := []struct {
		name    string
		who     auth.Identity
		rec     acl.Record
		wantErr error
	}{
		{"private always allowed", u2, rec("a2", acl.ScopePrivate, ""), nil},
		{"empty scope is private", u2, rec("a2", "", ""), nil},
		{"team member allowed", u1, rec("a1", acl.ScopeTeam, "teamX"), nil},
		{"team non-member denied", u2, rec("a2", acl.ScopeTeam, "teamX"), acl.ErrScopeDenied},
		{"team wrong team denied", u1, rec("a1", acl.ScopeTeam, "teamY"), acl.ErrScopeDenied},
		{"org with grant allowed", u1, rec("a1", acl.ScopeOrg, ""), nil},
		{"org without grant denied", u2, rec("a2", acl.ScopeOrg, ""), acl.ErrScopeDenied},
		{"unknown scope denied", u1, rec("a1", "galactic", ""), acl.ErrUnknownScope},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := g.Check(ctx, tc.who, tc.rec)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Check = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Check = %v, want errors.Is %v", err, tc.wantErr)
			}
		})
	}
}

// TestScopeGuardFailsClosedOnEdgeError: a reachability read failure denies the
// write (never admits) for a scope that needs an edge lookup.
func TestScopeGuardFailsClosedOnEdgeError(t *testing.T) {
	g := acl.NewScopeGuard(&fakeEdges{err: errors.New("opensearch down")})
	err := g.Check(context.Background(), id("u1", "a1"), rec("a1", acl.ScopeTeam, "teamX"))
	if err == nil {
		t.Fatal("team write admitted despite an edge-store failure")
	}
}

// TestScopeGuardPrivateNeedsNoEdgeRead: private writes are allowed without
// consulting the edge store (which here would error if consulted).
func TestScopeGuardPrivateNeedsNoEdgeRead(t *testing.T) {
	g := acl.NewScopeGuard(&fakeEdges{err: errors.New("must not be called")})
	if err := g.Check(context.Background(), id("u1", "a1"), rec("a1", acl.ScopePrivate, "")); err != nil {
		t.Fatalf("private write denied: %v", err)
	}
}

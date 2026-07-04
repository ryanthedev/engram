package acl_test

import (
	"errors"
	"testing"

	"github.com/ryanthedev/engram/internal/acl"
)

// TestEdgeValidateRejectsMalformed proves the acl_edge write barricade: every
// structurally invalid edge is rejected with ErrMalformedEdge (so the store
// never persists garbage that could widen access), and well-formed edges pass.
func TestEdgeValidateRejectsMalformed(t *testing.T) {
	valid := []acl.Edge{
		{Type: acl.EdgeUserAgent, TenantID: "t1", UserID: "u1", AgentID: "a1"},
		{Type: acl.EdgeMember, TenantID: "t1", UserID: "u1", TeamID: "teamX"},
		{Type: acl.EdgeOrgGrant, TenantID: "t1", UserID: "u1"},
	}
	for _, e := range valid {
		if err := e.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v, want nil", e, err)
		}
	}

	malformed := []acl.Edge{
		{Type: acl.EdgeUserAgent, TenantID: "t1", UserID: "u1"},               // missing agent
		{Type: acl.EdgeMember, TenantID: "t1", UserID: "u1"},                  // missing team
		{Type: acl.EdgeUserAgent, TenantID: "", UserID: "u1", AgentID: "a1"},  // missing tenant
		{Type: acl.EdgeUserAgent, TenantID: "t1", UserID: "", AgentID: "a1"},  // missing user
		{Type: "bogus", TenantID: "t1", UserID: "u1", AgentID: "a1"},          // unknown type
		{Type: acl.EdgeMember, TenantID: "  ", UserID: "u1", TeamID: "teamX"}, // blank tenant
	}
	for _, e := range malformed {
		if err := e.Validate(); !errors.Is(err, acl.ErrMalformedEdge) {
			t.Errorf("Validate(%+v) = %v, want ErrMalformedEdge", e, err)
		}
	}
}

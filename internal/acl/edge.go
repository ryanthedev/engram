package acl

import "strings"

// EdgeType names a reachability relation stored in the acl_edges index.
type EdgeType string

const (
	// EdgeUserAgent authorizes a user to consume an agent's shared (team/org)
	// contributions. Deleting it is revocation (DW-4.2): from=UserID, to=AgentID.
	EdgeUserAgent EdgeType = "user_agent"
	// EdgeMember records a user's membership in a team: from=UserID, to=TeamID.
	EdgeMember EdgeType = "member"
	// EdgeOrgGrant grants a user the right to WRITE org scope: from=UserID.
	EdgeOrgGrant EdgeType = "org_grant"
)

// Edge is one acl_edges document — a single reachability grant. It is the unit
// of ACL administration (grant/revoke). Which fields are required depends on
// Type; Validate is the barricade that rejects malformed edges before any
// write (fail-closed — an unvalidated edge could silently widen access).
type Edge struct {
	Type     EdgeType `json:"type"`
	TenantID string   `json:"tenant_id"`
	UserID   string   `json:"user_id"`
	AgentID  string   `json:"agent_id,omitempty"`
	TeamID   string   `json:"team_id,omitempty"`
}

// Validate reports whether the edge is well-formed for its type. Every edge
// needs a tenant and a user; a user_agent edge needs an agent; a member edge
// needs a team; an org_grant needs neither. An unknown type is rejected. A
// malformed edge yields ErrMalformedEdge so the store never persists it.
func (e Edge) Validate() error {
	if strings.TrimSpace(e.TenantID) == "" || strings.TrimSpace(e.UserID) == "" {
		return ErrMalformedEdge
	}
	switch e.Type {
	case EdgeUserAgent:
		if strings.TrimSpace(e.AgentID) == "" {
			return ErrMalformedEdge
		}
	case EdgeMember:
		if strings.TrimSpace(e.TeamID) == "" {
			return ErrMalformedEdge
		}
	case EdgeOrgGrant:
		// user + tenant suffice.
	default:
		return ErrMalformedEdge
	}
	return nil
}

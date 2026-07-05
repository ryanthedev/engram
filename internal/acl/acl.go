// Package acl is Engram's authorization policy: provenance-as-ACL enforced at
// QUERY TIME (D6). It translates a verified Identity plus reachability edges
// into (a) an OpenSearch bool filter applied inside the Retriever so callers
// cannot bypass it, and (b) a write-time scope guard so an agent writes only
// the scopes its identity reaches. It is defense-in-depth (scope checked at
// read AND write) and fail-closed everywhere: an unknown scope, empty
// reachability, an invalid identity, or an edge-store error all mean DENY.
//
// Architecture boundary (ca-architecture-boundaries): this is the innermost
// policy layer. It imports ONLY internal/auth (the Identity entity). It knows
// nothing of gRPC, the generated proto API, the Retriever, or OpenSearch's Go
// client — the depguard/importlint boundary is satisfied structurally. The
// OpenSearch-backed reachability store lives in internal/store and implements
// the EdgeSource interface DEFINED HERE (dependency inversion: the arrow points
// inward from infrastructure to policy).
package acl

import (
	"context"
	"errors"

	"github.com/ryanthedev/engram/internal/auth"
)

// Scope values (D23: three tiers — private, team, org; keyword field, additive
// so new tiers need no reindex). The empty string is treated as private (a
// record whose writer never set a scope is its owner's private memory).
const (
	ScopePrivate = "private"
	ScopeTeam    = "team"
	ScopeOrg     = "org"
)

// Typed authorization failures. Callers assert on these with errors.Is; the
// transport layer collapses them to an opaque status so no policy detail leaks
// to an unauthorized caller (no oracle).
var (
	// ErrScopeDenied means the identity may not write the requested scope
	// (e.g. team scope for a team it is not a member of). DW-4.3.
	ErrScopeDenied = errors.New("acl: identity may not write this scope")
	// ErrUnknownScope means the record's scope is not one of the known tiers —
	// rejected fail-closed rather than written with unenforceable semantics.
	ErrUnknownScope = errors.New("acl: unknown scope")
	// ErrMalformedEdge means an acl_edge document is structurally invalid and
	// must be rejected at write (never persisted).
	ErrMalformedEdge = errors.New("acl: malformed edge")
)

// Record is the neutral authorization subject: the tenancy/provenance fields a
// scope decision needs, extracted from any stored memory record (episodic or
// semantic) or search hit. Keeping it vendor-neutral lets the store, the
// retriever, and the audit path share one policy without leaking OpenSearch or
// memory types into acl.
type Record struct {
	TenantID     string
	TeamID       string
	Scope        string
	OwnerAgentID string
}

// scope normalizes the record's scope: an empty scope is private.
func (r Record) scope() string {
	if r.Scope == "" {
		return ScopePrivate
	}
	return r.Scope
}

// Reach is an identity's resolved reachability — everything the scope rules
// need to decide read and write. Agents always includes the identity's own
// agent (self is reachable); Teams are the user's team memberships; OrgGrant
// reports whether the user holds an org-scope grant.
type Reach struct {
	Agents []string
	Teams  []string
	// OrgGrant authorizes WRITING org scope. Reading org scope is governed by
	// agent reachability, not this flag.
	OrgGrant bool
}

// hasAgent reports whether agentID is in the reachable-agent set.
func (r Reach) hasAgent(agentID string) bool { return contains(r.Agents, agentID) }

// hasTeam reports whether teamID is in the user's team set.
func (r Reach) hasTeam(teamID string) bool { return contains(r.Teams, teamID) }

// empty reports whether the identity reaches no agents at all — a valid
// identity always reaches itself, so an empty set means a blank agent with no
// edges: deny-all.
func (r Reach) empty() bool { return len(r.Agents) == 0 }

// EdgeSource resolves an identity's reachability from the acl_edges store. It
// is a consumer-defined seam (Clean Architecture): acl declares what it needs;
// internal/store.ACLEdgeStore implements it over OpenSearch. Reachability MUST
// be a fresh read every call — no caching — so an edge deletion (revocation)
// takes effect on the very next query (D6, ≤5 s SLO with zero cache to bust).
type EdgeSource interface {
	Reachability(ctx context.Context, id auth.Identity) (Reach, error)
}

// WriteGuard authorizes one write against an identity. A non-nil return denies
// the write (fail-closed). It is the type internal/store.RegisterWriteGuard
// accepts; ScopeGuard is the Phase-4 implementation, and P5's experience gate
// registers through the same seam.
type WriteGuard interface {
	Check(ctx context.Context, id auth.Identity, r Record) error
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

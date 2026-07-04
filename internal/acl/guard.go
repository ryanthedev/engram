package acl

import (
	"context"
	"fmt"

	"github.com/ryanthedev/engram/internal/auth"
)

// ScopeGuard is the write-time half of the ACL (defense in depth — the read
// filter is not trusted to be the only gate; auth bugs happen). It authorizes
// an agent to write a record only at a scope its identity reaches, and is
// registered on the Store via RegisterWriteGuard so every write flows through
// it. Fail-closed: an unknown scope, an unreachable team/org, or an edge-store
// error all deny.
type ScopeGuard struct {
	edges EdgeSource
}

var _ WriteGuard = (*ScopeGuard)(nil)

// NewScopeGuard returns a ScopeGuard over the reachability source.
func NewScopeGuard(edges EdgeSource) *ScopeGuard {
	return &ScopeGuard{edges: edges}
}

// Check authorizes writing r as id. It returns nil to allow, or a typed error
// to deny (which the store surfaces without writing):
//   - private (or empty scope): always allowed — an agent may write its own
//     private memory;
//   - team: allowed only if the identity's user is a member of r.TeamID;
//   - org: allowed only if the identity holds an org grant;
//   - any other scope: ErrUnknownScope.
//
// An invalid identity or an edge-store failure denies (fail-closed).
func (g *ScopeGuard) Check(ctx context.Context, id auth.Identity, r Record) error {
	scope := r.scope()
	if scope == ScopePrivate {
		// Private needs no reachability read: any valid identity may write its
		// own private memory. An invalid identity should never reach a write
		// (the barricade precedes this), but deny it to be safe.
		if !id.Valid() {
			return fmt.Errorf("%w: invalid identity", ErrScopeDenied)
		}
		return nil
	}
	if scope != ScopeTeam && scope != ScopeOrg {
		return fmt.Errorf("%w: %q", ErrUnknownScope, r.Scope)
	}
	if !id.Valid() {
		return fmt.Errorf("%w: invalid identity", ErrScopeDenied)
	}
	reach, err := g.edges.Reachability(ctx, id)
	if err != nil {
		// Fail-closed: a reachability failure denies rather than admits.
		return fmt.Errorf("acl: write authorization failed: %w", err)
	}
	switch scope {
	case ScopeTeam:
		if !reach.hasTeam(r.TeamID) {
			return fmt.Errorf("%w: team %q", ErrScopeDenied, r.TeamID)
		}
	case ScopeOrg:
		if !reach.OrgGrant {
			return fmt.Errorf("%w: org", ErrScopeDenied)
		}
	}
	return nil
}

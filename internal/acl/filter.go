package acl

import (
	"context"
	"log/slog"

	"github.com/ryanthedev/engram/internal/auth"
)

// Filter is the query-time ACL compiler (the ACLFilter contract, D6). One
// Filter is shared by the Retriever (read filtering) and the audit path (read
// checks); both go through Enforce, which performs exactly one reachability
// read per call — so revocation is instant and there is no cache to invalidate.
type Filter struct {
	edges EdgeSource
	log   *slog.Logger
}

// NewFilter returns a Filter over the reachability source. logger may be nil
// (defaults to slog.Default); it records fail-closed denials for audit.
func NewFilter(edges EdgeSource, logger *slog.Logger) *Filter {
	if logger == nil {
		logger = slog.Default()
	}
	return &Filter{edges: edges, log: logger}
}

// Enforcer is a compiled, single-identity authorization decision: it carries
// the resolved reachability and produces both the OpenSearch read filter
// (Clause) and the in-Go post-hoc predicate (Authorize) from the SAME rule, so
// query-time and post-hoc enforcement can never diverge. Its ZERO VALUE denies
// everything — fail-closed by construction, so even a caller that ignores an
// error and uses the zero Enforcer leaks nothing.
type Enforcer struct {
	id    auth.Identity
	reach Reach
	ok    bool
}

// Enforce resolves id's reachability and returns an Enforcer. It is fail-closed:
//   - an invalid identity → deny-all Enforcer (no edge read), logged;
//   - an edge-store error → deny-all Enforcer AND the error (the retriever
//     logs and returns zero results; it never proceeds unfiltered);
//   - empty reachability (a blank agent with no edges) → deny-all, logged.
func (f *Filter) Enforce(ctx context.Context, id auth.Identity) (Enforcer, error) {
	if !id.Valid() {
		f.log.WarnContext(ctx, "acl deny: invalid identity (fail-closed)", "identity", id.String())
		return Enforcer{id: id}, nil
	}
	reach, err := f.edges.Reachability(ctx, id)
	if err != nil {
		f.log.WarnContext(ctx, "acl deny: reachability read failed (fail-closed)", "identity", id.String(), "err", err)
		return Enforcer{id: id}, err
	}
	if reach.empty() {
		f.log.WarnContext(ctx, "acl deny: empty reachability (fail-closed)", "identity", id.String())
		return Enforcer{id: id, reach: reach}, nil
	}
	return Enforcer{id: id, reach: reach, ok: true}, nil
}

// Compile is the named contract seam: it returns the OpenSearch bool filter
// clause for id (fail-closed — a deny-all clause on any denial). Equivalent to
// Enforce(...).Clause(); the error is propagated so the caller can log and
// short-circuit rather than run a query it must then discard.
func (f *Filter) Compile(ctx context.Context, id auth.Identity) (map[string]any, error) {
	enf, err := f.Enforce(ctx, id)
	return enf.Clause(), err
}

// CanRead reports whether id may read r — the audit path's read check. Fail-
// closed: an edge error denies.
func (f *Filter) CanRead(ctx context.Context, id auth.Identity, r Record) (bool, error) {
	enf, err := f.Enforce(ctx, id)
	if err != nil {
		return false, err
	}
	return enf.Authorize(r), nil
}

// Clause returns the OpenSearch bool-filter fragment enforcing this decision,
// to be ANDed into a search. A denied decision returns a match_none clause so
// the query returns zero documents. The clause mirrors Authorize exactly.
func (e Enforcer) Clause() map[string]any {
	if !e.ok {
		return map[string]any{"match_none": map[string]any{}}
	}
	// Non-nil slices: a nil slice marshals to JSON null, which OpenSearch's
	// terms query rejects; an empty array is valid and matches nothing.
	agents, teams := jsonStrings(e.reach.Agents), jsonStrings(e.reach.Teams)
	return map[string]any{"bool": map[string]any{
		"filter": []any{
			map[string]any{"term": map[string]any{"tenant_id": e.id.TenantID}},
			map[string]any{"bool": map[string]any{
				"minimum_should_match": 1,
				"should": []any{
					// private: the querying agent's own facts only.
					map[string]any{"bool": map[string]any{"filter": []any{
						map[string]any{"term": map[string]any{"scope": ScopePrivate}},
						map[string]any{"term": map[string]any{"owner_agent_id": e.id.AgentID}},
					}}},
					// team: a reachable agent's fact, on one of the user's teams.
					map[string]any{"bool": map[string]any{"filter": []any{
						map[string]any{"term": map[string]any{"scope": ScopeTeam}},
						map[string]any{"terms": map[string]any{"owner_agent_id": agents}},
						map[string]any{"terms": map[string]any{"team_id": teams}},
					}}},
					// org: any reachable agent's org-scoped fact in the tenant.
					map[string]any{"bool": map[string]any{"filter": []any{
						map[string]any{"term": map[string]any{"scope": ScopeOrg}},
						map[string]any{"terms": map[string]any{"owner_agent_id": agents}},
					}}},
				},
			}},
		},
	}}
}

// jsonStrings returns xs, or an empty (non-nil) slice if xs is nil, so it
// marshals to a JSON array rather than null.
func jsonStrings(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}

// Authorize reports whether r is readable under this decision — the in-Go twin
// of Clause, used to re-filter hits produced OUTSIDE the query-time clause
// (registered tier sources and graph-expanded hits) so no retrieval path can
// bypass the ACL. Fail-closed: a denied decision authorizes nothing.
func (e Enforcer) Authorize(r Record) bool {
	if !e.ok || r.TenantID != e.id.TenantID {
		return false
	}
	switch r.scope() {
	case ScopePrivate:
		return r.OwnerAgentID == e.id.AgentID
	case ScopeTeam:
		return e.reach.hasAgent(r.OwnerAgentID) && e.reach.hasTeam(r.TeamID)
	case ScopeOrg:
		return e.reach.hasAgent(r.OwnerAgentID)
	default:
		return false
	}
}

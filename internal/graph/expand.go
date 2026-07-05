package graph

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/retrieval"
)

// ErrDepthExceeded is returned when Expand (or NewExpander) is asked for a
// depth beyond MaxDepth. Refused loudly rather than silently clamped — a
// misconfigured depth should fail construction/the call, not quietly serve
// a shallower expansion than requested.
var ErrDepthExceeded = fmt.Errorf("graph: expansion depth exceeds the %d-hop ceiling (D8)", MaxDepth)

// Defaults bounding one expansion's blast radius, so latency stays
// predictable regardless of graph density (DW-6.5's ≤250ms budget). Not
// derived from any DW item — an engineering default, tunable via
// ExpanderOption.
const (
	DefaultMaxFanout = 5  // neighbor edges explored per entity per hop
	DefaultMaxAdded  = 20 // total new hits one Expand call may add
)

// Expander is the retrieval.PostHook that performs bounded (≤2-hop)
// "connect the dots" expansion (GeAR-style triple expansion, post-RRF):
// given the fused top-k hits, it resolves each semantic hit's
// subject/object into graph entities, walks their live edges up to depth
// hops, and returns the seed hits plus every entity/edge it reached as new
// hits. It satisfies retrieval.PostHook by delegating Apply to the exported
// Expand (the plan's literal contract, independently unit-testable for the
// depth-2-honored/depth-3-rejected boundary).
//
// Every added hit carries the traversed EDGE's own provenance fields
// (tenant_id, team_id, scope, owner_agent_id) — never the seed hit's — so
// MultiRetriever's existing post-hook re-authorization (unchanged by this
// phase) drops anything reached only through an edge the caller may not
// read. This is the ACL-inside-expansion guarantee (D6/DW-6.4): Expander
// adds no authorization logic of its own; it just never asserts hits are
// pre-authorized.
type Expander struct {
	store     *Store
	depth     int
	maxFanout int
	maxAdded  int
	logger    *slog.Logger
}

var _ retrieval.PostHook = (*Expander)(nil)

// ExpanderOption configures an Expander.
type ExpanderOption func(*Expander)

// WithFanoutLimit overrides the per-node, per-hop neighbor cap.
func WithFanoutLimit(n int) ExpanderOption { return func(e *Expander) { e.maxFanout = n } }

// WithAddedLimit overrides the total-new-hits cap for one Expand call.
func WithAddedLimit(n int) ExpanderOption { return func(e *Expander) { e.maxAdded = n } }

// NewExpander returns an Expander bounded to depth hops (must be 0..MaxDepth
// — construction fails on an out-of-range depth rather than clamping it).
// logger nil uses slog.Default().
func NewExpander(store *Store, depth int, logger *slog.Logger, opts ...ExpanderOption) (*Expander, error) {
	if depth < 0 || depth > MaxDepth {
		return nil, ErrDepthExceeded
	}
	if logger == nil {
		logger = slog.Default()
	}
	e := &Expander{store: store, depth: depth, maxFanout: DefaultMaxFanout, maxAdded: DefaultMaxAdded, logger: logger}
	for _, opt := range opts {
		opt(e)
	}
	return e, nil
}

// Apply implements retrieval.PostHook: it expands at this Expander's
// configured depth. The Identity is accepted (per the PostHook contract) but
// not itself consulted — authorization is entirely the retriever's
// re-verification of the provenance fields Expand stamps onto new hits.
func (e *Expander) Apply(ctx context.Context, _ auth.Identity, hits []retrieval.Hit) ([]retrieval.Hit, error) {
	return e.Expand(ctx, hits, e.depth)
}

// Expand performs the bounded traversal: zero seed hits is a no-op (nothing
// to anchor from); depth 0 is a no-op (expansion disabled); depth outside
// 0..MaxDepth is ErrDepthExceeded — the depth-2-honored/depth-3-rejected
// boundary. Anchors are every semantic-tier hit's Subject and Object fields,
// resolved to graph entities by name; a hit with no matching entity (never
// graphed, or its entity was soft-expired — "dangling edge" skip) is simply
// not used as an anchor.
func (e *Expander) Expand(ctx context.Context, hits []retrieval.Hit, depth int) ([]retrieval.Hit, error) {
	if depth < 0 || depth > MaxDepth {
		return nil, ErrDepthExceeded
	}
	if len(hits) == 0 || depth == 0 {
		return hits, nil
	}

	tenantID := seedTenantID(hits)
	if tenantID == "" {
		return hits, nil // no tenant-bearing seed hit to anchor from
	}

	visitedEntities := map[string]bool{}
	visitedEdges := map[string]bool{}
	for _, h := range hits {
		if id := h.ID; id != "" {
			visitedEdges[id] = true // never re-add a seed hit as an "expanded" one
		}
	}

	frontier := e.anchorEntities(ctx, tenantID, hits)
	for _, id := range frontier {
		visitedEntities[id] = true
	}
	names := map[string]string{}

	var added []retrieval.Hit
	for hop := 1; hop <= depth && len(added) < e.maxAdded && len(frontier) > 0; hop++ {
		var next []string
		for _, entityID := range frontier {
			edges, err := e.store.Neighbors(ctx, tenantID, entityID)
			if err != nil {
				return nil, fmt.Errorf("graph: expanding neighbors of %s: %w", entityID, err)
			}
			explored := 0
			for _, edge := range edges {
				if explored >= e.maxFanout || len(added) >= e.maxAdded {
					break
				}
				if visitedEdges[edge.ID] {
					continue
				}
				visitedEdges[edge.ID] = true
				explored++

				other := edge.ToEntityID
				if other == entityID {
					other = edge.FromEntityID
				}
				otherEntity, ok, err := e.store.GetEntity(ctx, tenantID, other)
				if err != nil {
					return nil, fmt.Errorf("graph: reading neighbor entity %s: %w", other, err)
				}
				if !ok || !otherEntity.Live() {
					// Dangling edge: its entity was soft-expired (or never
					// existed) — skip, never traversed further.
					continue
				}
				names[other] = otherEntity.Name
				added = append(added, edgeHit(edge, hop, e.entityName(ctx, tenantID, edge.FromEntityID, names), e.entityName(ctx, tenantID, edge.ToEntityID, names)))
				if !visitedEntities[other] {
					visitedEntities[other] = true
					next = append(next, other)
				}
			}
		}
		frontier = next
	}

	if len(added) == 0 {
		return hits, nil
	}
	e.logger.DebugContext(ctx, "graph expansion added hits", "seed_count", len(hits), "added_count", len(added), "depth", depth)
	return append(append([]retrieval.Hit{}, hits...), added...), nil
}

// anchorEntities resolves every seed hit's Subject/Object fields (semantic
// hits only — other tiers carry no such fields, and homonym candidates all
// fan out as anchors, deliberately: query-time we lack a hit-specific
// disambiguating context beyond what the fact itself already gave us, and
// the bounded fanout/added caps keep the blast radius small either way).
func (e *Expander) anchorEntities(ctx context.Context, tenantID string, hits []retrieval.Hit) []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if name == "" {
			return
		}
		cands, err := e.store.backend.CandidateEntities(ctx, tenantID, name)
		if err != nil {
			return
		}
		for _, c := range cands {
			if c.Live() && !seen[c.ID] {
				seen[c.ID] = true
				out = append(out, c.ID)
			}
		}
	}
	for _, h := range hits {
		subj, _ := h.Fields["subject"].(string)
		obj, _ := h.Fields["object"].(string)
		add(subj)
		add(obj)
	}
	return out
}

// seedTenantID returns the first non-empty tenant_id carried by hits (every
// hit in one Search call shares the same caller's tenant).
func seedTenantID(hits []retrieval.Hit) string {
	for _, h := range hits {
		if t, _ := h.Fields["tenant_id"].(string); t != "" {
			return t
		}
	}
	return ""
}

// entityName resolves id's display name, caching in names (populated as
// traversal discovers entities); a lookup miss falls back to the id itself
// rather than an empty string.
func (e *Expander) entityName(ctx context.Context, tenantID, id string, names map[string]string) string {
	if n, ok := names[id]; ok {
		return n
	}
	if ent, ok, err := e.store.GetEntity(ctx, tenantID, id); err == nil && ok {
		names[id] = ent.Name
		return ent.Name
	}
	return id
}

// edgeHit renders one traversed edge as a retrieval.Hit, with subject/object
// as human-readable entity NAMES (matching the semantic tier's hit shape,
// so a connect-the-dots query result reads naturally). Its provenance
// fields are the EDGE's own (never inherited from a seed hit or its
// endpoint), so the retriever's ACL re-verification is exact.
func edgeHit(edge Edge, hop int, fromName, toName string) retrieval.Hit {
	return retrieval.Hit{
		ID: edge.ID,
		// Hop-decayed score: expanded hits rank below directly-matched seed
		// hits if anything downstream re-sorts by score; the retriever
		// itself does not re-truncate post-hook additions (Phase 4 design).
		Score:  1.0 / float64(hop+1),
		Source: "graph",
		Fields: map[string]any{
			"tenant_id":      edge.TenantID,
			"team_id":        edge.TeamID,
			"scope":          edge.Scope,
			"owner_agent_id": edge.OwnerAgentID,
			"subject":        fromName,
			"predicate":      edge.Predicate,
			"object":         toName,
			"statement":      edge.Statement,
			"hop":            hop,
		},
	}
}

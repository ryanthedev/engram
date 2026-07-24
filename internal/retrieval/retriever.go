// Package retrieval defines the Retriever seam: hybrid BM25 + kNN search
// fused by RRF (D1), with validity and tenancy filters applied inside the
// query (never post-filtered). Phase 1 implements it over OpenSearch.
package retrieval

import (
	"context"

	"github.com/ryanthedev/engram/internal/acl"
	"github.com/ryanthedev/engram/internal/auth"
)

// Query is one retrieval request. Text is embedded at query time (D15) for
// the kNN clause and searched verbatim for the BM25 clause; K bounds the
// fused result count.
type Query struct {
	Text string
	K    int
}

// Filter scopes a search. TenantID and UserID narrow by tenancy (D16);
// ValidOnly restricts to currently-valid facts (invalid_at == null AND
// expired_at == null — the plan's current-state query). Identity carries the
// verified principal (Phase 4): when the Retriever is built WithACL, the ACL
// filter is compiled from Identity and applied inside every query — callers
// cannot bypass it. A zero Identity through an ACL-enabled Retriever denies
// all (fail-closed).
//
// Predicates and Sources are the caller-facing filter surface. A zero Filter
// (both nil) searches every source with no field constraints — byte-for-byte
// the query the Retriever issued before they existed.
type Filter struct {
	TenantID string
	UserID   string
	// ValidOnly applies the bi-temporal current-state filter.
	ValidOnly bool
	// Identity is the verified caller; drives ACL enforcement (Phase 4).
	Identity auth.Identity

	// Predicates narrow the search by field. Each is ROUTED to the tiers that
	// declare its field (see FilterableFields) and compiled into their query;
	// tiers that do not declare the field are left unconstrained rather than
	// zeroed. A predicate naming a field no selected tier declares is a
	// validation error naming the valid fields — never a silent no-op.
	Predicates []Predicate

	// Sources restricts the search to the named sources — built-in tiers
	// ("episodic", "semantic"), registered tier sources, and registered
	// post-hooks ("graph"). nil means every source. An EMPTY (non-nil) slice
	// is a validation error, not a silent "all": "search nothing" and "search
	// everything" must not be the same request. An unknown name is an error
	// naming the valid sources.
	Sources []string
}

// ACLFilter compiles a verified Identity into an authorization decision applied
// inside retrieval so no caller can bypass scope enforcement (D6). It is a
// consumer-defined seam (internal/acl.Filter implements it); the Retriever
// depends on this abstraction, not the concrete policy. Fail-closed: an error
// means deny (the Retriever returns zero results and logs a denial).
type ACLFilter interface {
	Enforce(ctx context.Context, id auth.Identity) (acl.Enforcer, error)
}

// TierSource is an additional retrieval tier registered at wiring time
// (Phase 4 seam; Phase 5 plugs the experience tier in here). It receives the
// verified Identity so it can enforce ACL on its own index; its hits are still
// re-verified through the ACL predicate before return (defense in depth).
type TierSource interface {
	Search(ctx context.Context, id auth.Identity, q Query) ([]Hit, error)
}

// PostHook post-processes the fused hit list with the Identity in hand
// (Phase 4 seam; Phase 6 registers graph expansion here). Hits it adds are
// re-verified through the ACL predicate before return, so an expansion cannot
// leak a fact the caller may not read.
type PostHook interface {
	Apply(ctx context.Context, id auth.Identity, hits []Hit) ([]Hit, error)
}

// Hit is one fused result. Source names the index tier the hit came from
// (e.g. "episodic", "semantic") or, on the knowledge path, the collection
// name; Fields carries the stored document fields.
type Hit struct {
	ID     string
	Score  float64
	Source string
	Fields map[string]any
	// Fragments are the highlight-extracted excerpts that replace the
	// document body on the knowledge search path (fragments default, body
	// suppressed). Always nil on the memory path — memory queries never ask
	// for highlighting, so parseHits finds no highlight key to read.
	Fragments []string
}

// Retriever is the read seam Phases 1–2 and the eval harness consume: one
// hybrid query (BM25 clause + kNN clause → RRF pipeline) returning a single
// fused, ranked list. An empty query or zero matches returns an empty slice,
// not an error.
type Retriever interface {
	// Search runs the hybrid query under the filter and returns fused hits
	// in descending score order, at most q.K of them.
	Search(ctx context.Context, q Query, f Filter) ([]Hit, error)
}

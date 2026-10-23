package server

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/authgrpc"
)

// FactPrefixResolver maps a hex prefix of a semantic doc _id to the full ids
// that carry it and satisfy every filter clause, ascending, at most limit
// (consumer-defined seam; *store.OpenSearchStore satisfies it). It is
// policy-free: the filters ARE the authorization, and the server builds them.
type FactPrefixResolver interface {
	ResolveFactIDPrefix(ctx context.Context, prefix string, filters []any, limit int) ([]string, error)
}

// ReadClauseCompiler compiles an identity into the OpenSearch read-filter
// clause search enforces (consumer-defined seam; *acl.Filter satisfies it via
// Compile). The prefix resolver needs the CLAUSE, not ReadAuthorizer's
// per-record verdict: authorization has to run inside the resolving query so
// an unreadable fact is never even counted (see resolveFactPrefix).
type ReadClauseCompiler interface {
	Compile(ctx context.Context, id auth.Identity) (map[string]any, error)
}

const (
	// factIDLen is the length of a semantic doc _id: hex sha256
	// (memory.FactDocID). An id this long is a full id and keeps the exact,
	// realtime-GET read path.
	factIDLen = 64
	// minFactIDPrefix is the shortest prefix resolved. 8 hex chars is 32 bits:
	// at this store's scale (thousands of facts) a collision is rare enough
	// that an ambiguity error is an edge case, not the common answer.
	minFactIDPrefix = 8
	// prefixMatchLimit bounds the resolving page. Any count above 1 is already
	// an ambiguity; the bound only caps what the error can report exactly.
	prefixMatchLimit = 10
)

// isFactIDPrefix reports whether id is shaped like a resolvable prefix of a
// semantic id: lowercase hex (the alphabet FactDocID emits), at least
// minFactIDPrefix and fewer than factIDLen characters. Anything else —
// including a full id — keeps today's exact path, byte for byte.
func isFactIDPrefix(id string) bool {
	return len(id) >= minFactIDPrefix && len(id) < factIDLen && isHex(id)
}

// isHex reports whether s is non-empty lowercase hex.
func isHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// resolveFactPrefix resolves a semantic id prefix to the single full id the
// caller may read. It is a by-id trust boundary with the same fail-closed
// contract as the exact read, enforced the same way search enforces it:
//
//   - The resolving query is scoped by the caller's tenant pin AND the compiled
//     ACL clause, both INSIDE the query. So a fact the caller cannot read never
//     enters the result: it cannot be returned, cannot be counted into "matches
//     N records", and cannot turn a prefix that is unique among the caller's
//     readable facts into an ambiguity. No existence leaks, in any branch.
//   - No match is the same opaque NOT_FOUND as any other miss.
//   - More than one readable match is a self-correcting INVALID_ARGUMENT —
//     safe to be specific about, because every record it counts is one the
//     caller could already read.
//   - An ACL compile error fails closed (INTERNAL, mirroring Audit's CanRead
//     error) — the resolution never runs unfiltered.
//   - A server wired with an ACL but no clause compiler cannot scope the query
//     and refuses prefixes outright rather than resolving with the tenant pin
//     alone.
//
// The id it returns is still read through Audit, which re-fetches by realtime
// GET and re-checks the tenant pin and CanRead: defense in depth, and the
// guarantee that a prefix read and a full-id read can never answer
// differently.
func (s *Server) resolveFactPrefix(ctx context.Context, prefix string) (string, error) {
	if s.FactPrefix == nil || (s.ACL != nil && s.ACLClause == nil) {
		return "", status.Errorf(codes.Unimplemented,
			"semantic id prefixes are not supported by this server; pass the full %d-character id", factIDLen)
	}
	id, _ := authgrpc.IdentityFrom(ctx)
	filters := []any{map[string]any{"term": map[string]any{"tenant_id": id.TenantID}}}
	if s.ACL != nil {
		clause, err := s.ACLClause.Compile(ctx, id)
		if err != nil {
			return "", status.Errorf(codes.Internal, "read authorization: %v", err)
		}
		filters = append(filters, clause)
	}
	ids, err := s.FactPrefix.ResolveFactIDPrefix(ctx, prefix, filters, prefixMatchLimit)
	if err != nil {
		return "", status.Errorf(codes.Internal, "read: %v", err)
	}
	switch n := len(ids); {
	case n == 0:
		return "", errReadNotFound
	case n == 1:
		return ids[0], nil
	case n >= prefixMatchLimit:
		return "", status.Errorf(codes.InvalidArgument,
			"semantic id prefix %q matches %d or more records; use more characters", prefix, n)
	default:
		return "", status.Errorf(codes.InvalidArgument,
			"semantic id prefix %q matches %d records; use more characters", prefix, n)
	}
}

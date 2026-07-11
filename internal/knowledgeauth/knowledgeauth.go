// Package knowledgeauth decides per-collection read/write access for the
// knowledge platform from a caller's verified role claims (Phase 2). It is
// deliberately primitive-typed — an Authorizer sees `public bool` and required
// role names, never a knowledge collection type — so auth does not couple to
// the knowledge package. Policy storage (Phase 3) and enforcement sites
// (Phase 6, the request barricade) live elsewhere; this package only answers
// "may this identity do that?".
//
// Security posture (cc-defensive-programming): every branch fails closed. An
// unauthenticated identity is denied even for public collections; an empty or
// unknown role in a policy denies rather than errors; a misconfigured empty
// write role grants nobody, not everybody. All denials collapse to the single
// ErrForbidden sentinel — callers get no oracle for which role would have
// sufficed — and it is returned UNWRAPPED so errors.Is survives to the gRPC
// edge (docs/code-standards.md §3).
package knowledgeauth

import (
	"errors"
	"slices"
	"strings"

	"github.com/ryanthedev/engram/internal/auth"
)

// ErrForbidden is the single denial sentinel for every authorization failure
// (missing role, unauthenticated caller, misconfigured policy). Phase 6 maps
// it to codes.PermissionDenied at the gRPC edge via errors.Is, so it must
// never be fmt.Errorf-wrapped on the return path.
var ErrForbidden = errors.New("knowledgeauth: forbidden")

// Authorizer makes role-based access decisions for knowledge collections. The
// zero value is ready to use; it carries no state today and exists (rather
// than package-level funcs) so Phase 6 injects it as a dependency and future
// policy state (audit hooks, role hierarchies) lands without call-site churn.
type Authorizer struct{}

// AuthorizeRead reports whether id may read a collection. A public collection
// admits any authenticated caller — authentication is still required (an
// invalid identity is denied even when public). A role-gated collection
// (public=false) requires id to hold at least one of requiredRoles; an empty
// or unknown-role policy therefore denies everyone, by design, rather than
// erroring. Matching is exact and case-sensitive; empty-string roles grant
// nothing on either side. Every denial is ErrForbidden.
func (Authorizer) AuthorizeRead(id auth.Identity, public bool, requiredRoles []string) error {
	if !id.Valid() {
		return ErrForbidden
	}
	if public {
		return nil
	}
	for _, want := range requiredRoles {
		if want = strings.TrimSpace(want); want != "" && slices.Contains(id.Roles, want) {
			return nil
		}
	}
	return ErrForbidden
}

// AuthorizeWrite reports whether id may perform a write-side operation
// (ingest, delete, collection create/update) gated by requiredRole. The
// caller must be authenticated and hold exactly that role. An empty
// requiredRole is a misconfigured policy and fails closed — it grants
// nobody write access, never everybody. Every denial is ErrForbidden.
func (Authorizer) AuthorizeWrite(id auth.Identity, requiredRole string) error {
	requiredRole = strings.TrimSpace(requiredRole)
	if !id.Valid() || requiredRole == "" || !slices.Contains(id.Roles, requiredRole) {
		return ErrForbidden
	}
	return nil
}

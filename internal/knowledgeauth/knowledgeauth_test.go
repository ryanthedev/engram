package knowledgeauth_test

import (
	"errors"
	"testing"

	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/knowledgeauth"
)

// caller builds an authenticated identity holding roles; no roles means an
// authenticated caller with an empty claim set.
func caller(roles ...string) auth.Identity {
	return auth.Identity{TenantID: "t1", UserID: "alice", Roles: roles}
}

// TestDW_2_2_AuthorizeRead: public admits any authenticated caller, a
// required-role holder is admitted, and everything else — including every
// misconfiguration — denies with ErrForbidden (never a different error).
func TestDW_2_2_AuthorizeRead(t *testing.T) {
	var az knowledgeauth.Authorizer
	tests := []struct {
		name     string
		id       auth.Identity
		public   bool
		required []string
		allow    bool
	}{
		{"public allows authenticated caller with no roles", caller(), true, nil, true},
		{"public allows even when required roles are not held", caller(), true, []string{"curator"}, true},
		{"gated allows a caller holding a required role", caller("curator"), false, []string{"admin", "curator"}, true},
		{"gated denies a caller lacking every required role", caller("reader"), false, []string{"admin", "curator"}, false},
		{"gated denies a caller with empty roles", caller(), false, []string{"curator"}, false},
		{"gated with empty policy denies everyone (fail closed)", caller("admin"), false, nil, false},
		{"unknown role in policy denies, not errors", caller("reader"), false, []string{"no-such-role"}, false},
		{"public still requires authentication", auth.Identity{}, true, nil, false},
		{"gated denies unauthenticated caller even with matching roles", auth.Identity{Roles: []string{"curator"}}, false, []string{"curator"}, false},
		{"empty-string role grants nothing on either side", caller(""), false, []string{""}, false},
		{"blank policy entry grants nothing to a blank claim", caller("  "), false, []string{"   "}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := az.AuthorizeRead(tt.id, tt.public, tt.required)
			if tt.allow && err != nil {
				t.Errorf("AuthorizeRead = %v, want allow", err)
			}
			if !tt.allow && !errors.Is(err, knowledgeauth.ErrForbidden) {
				t.Errorf("AuthorizeRead = %v, want ErrForbidden", err)
			}
		})
	}
}

// TestDW_2_3_AuthorizeWrite: only an authenticated caller holding exactly the
// required role may write; a missing admin/harvester role — or a
// misconfigured empty policy — denies with ErrForbidden.
func TestDW_2_3_AuthorizeWrite(t *testing.T) {
	var az knowledgeauth.Authorizer
	tests := []struct {
		name     string
		id       auth.Identity
		required string
		allow    bool
	}{
		{"caller holding the role is allowed", caller("harvester"), "harvester", true},
		{"caller holding it among others is allowed", caller("reader", "admin"), "admin", true},
		{"caller lacking the harvester role is denied", caller("reader", "curator"), "harvester", false},
		{"caller with empty roles is denied", caller(), "admin", false},
		{"empty required role fails closed, even for empty-role claim", caller(""), "", false},
		{"blank required role fails closed", caller("admin"), "   ", false},
		{"unauthenticated caller is denied even with the role", auth.Identity{Roles: []string{"admin"}}, "admin", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := az.AuthorizeWrite(tt.id, tt.required)
			if tt.allow && err != nil {
				t.Errorf("AuthorizeWrite = %v, want allow", err)
			}
			if !tt.allow && !errors.Is(err, knowledgeauth.ErrForbidden) {
				t.Errorf("AuthorizeWrite = %v, want ErrForbidden", err)
			}
		})
	}
}

// TestErrForbiddenIsUnwrappedSentinel: the denial the caller receives IS the
// sentinel (not a wrapped derivative), so errors.Is keeps matching at the
// Phase 6 gRPC edge and no denial reason leaks role details.
func TestErrForbiddenIsUnwrappedSentinel(t *testing.T) {
	var az knowledgeauth.Authorizer
	if err := az.AuthorizeRead(caller("reader"), false, []string{"admin"}); err != knowledgeauth.ErrForbidden {
		t.Errorf("AuthorizeRead denial = %#v, want the unwrapped ErrForbidden sentinel", err)
	}
	if err := az.AuthorizeWrite(caller("reader"), "admin"); err != knowledgeauth.ErrForbidden {
		t.Errorf("AuthorizeWrite denial = %#v, want the unwrapped ErrForbidden sentinel", err)
	}
}

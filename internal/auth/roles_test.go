package auth

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// TestDW_2_1_RolesFromVerifiedToken (clean): roles bound at issuance are the
// roles Verify returns — the token record is the claim set, end to end.
func TestDW_2_1_RolesFromVerifiedToken(t *testing.T) {
	s := newMemStore()
	iss := NewTokenIssuer(s, nil)
	authn := NewAuthenticator(s, nil)

	id := Identity{TenantID: "t1", UserID: "alice", AgentID: "claude", Roles: []string{"curator", "harvester"}}
	raw, _, err := iss.Issue(context.Background(), id, time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	got, err := authn.Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if want := []string{"curator", "harvester"}; !reflect.DeepEqual(got.Roles, want) {
		t.Errorf("Verify roles = %v, want %v", got.Roles, want)
	}
}

// TestDW_2_1_ClientSuppliedRolesIgnored (dirty): a client that smuggles an
// elevated identity into the request context cannot influence the verified
// roles. This mirrors the real barricade flow (authgrpc/interceptor.go:102):
// Verify reads claims ONLY from the stored token record, then the barricade
// overwrites the context identity wholesale — the forged roles vanish.
func TestDW_2_1_ClientSuppliedRolesIgnored(t *testing.T) {
	s := newMemStore()
	iss := NewTokenIssuer(s, nil)
	authn := NewAuthenticator(s, nil)

	bound := Identity{TenantID: "t1", UserID: "alice", Roles: []string{"curator"}}
	raw, _, err := iss.Issue(context.Background(), bound, time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// The client forges an admin identity into the incoming context.
	forged := Identity{TenantID: "t1", UserID: "alice", Roles: []string{"admin"}}
	ctx := ContextWithIdentity(context.Background(), forged)

	// Verify ignores the context entirely: claims come from the token record.
	verified, err := authn.Verify(ctx, raw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if want := []string{"curator"}; !reflect.DeepEqual(verified.Roles, want) {
		t.Fatalf("Verify roles = %v, want %v (token claims only)", verified.Roles, want)
	}

	// The barricade then overwrites the forged identity; what business code
	// reads downstream carries no trace of the client-supplied roles.
	ctx = ContextWithIdentity(ctx, verified)
	got, ok := IdentityFromContext(ctx)
	if !ok {
		t.Fatal("IdentityFromContext: no identity after barricade injection")
	}
	for _, r := range got.Roles {
		if r == "admin" {
			t.Fatalf("client-supplied role %q leaked into the verified identity: %v", r, got.Roles)
		}
	}
	if want := []string{"curator"}; !reflect.DeepEqual(got.Roles, want) {
		t.Errorf("post-barricade roles = %v, want %v", got.Roles, want)
	}
}

// TestIssueNormalizesRoles: the mint-time half of the claim barricade —
// whitespace is trimmed, empties dropped, duplicates removed, order kept.
func TestIssueNormalizesRoles(t *testing.T) {
	s := newMemStore()
	iss := NewTokenIssuer(s, nil)
	id := Identity{TenantID: "t1", UserID: "alice", Roles: []string{"  curator ", "", "curator", "harvester", "   "}}
	if _, _, err := iss.Issue(context.Background(), id, time.Hour); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	recs := s.records()
	if len(recs) != 1 {
		t.Fatalf("stored %d records, want 1", len(recs))
	}
	if want := []string{"curator", "harvester"}; !reflect.DeepEqual(recs[0].Roles, want) {
		t.Errorf("stored roles = %v, want normalized %v", recs[0].Roles, want)
	}
}

// TestTokenRecordIdentityNormalizesRoles: the read-side half — a record
// written by some other path (dirty claims) is still sanitized before its
// roles reach authorization, and the returned slice never aliases the record.
func TestTokenRecordIdentityNormalizesRoles(t *testing.T) {
	rec := TokenRecord{TenantID: "t1", UserID: "alice", Roles: []string{" admin", "admin", ""}}
	id := rec.Identity()
	if want := []string{"admin"}; !reflect.DeepEqual(id.Roles, want) {
		t.Fatalf("Identity roles = %v, want %v", id.Roles, want)
	}
	id.Roles[0] = "mutated"
	if rec.Roles[0] != " admin" {
		t.Error("Identity().Roles aliases the record's backing array")
	}
}

// TestTokenRecordRolesJSONRoundTrip: roles survive the store's JSON
// round-trip under the snake_case "roles" key, and a role-less record
// marshals with NO roles key at all (omitempty) — the property that keeps
// pre-roles strict-mapped indices writable.
func TestTokenRecordRolesJSONRoundTrip(t *testing.T) {
	rec := TokenRecord{ID: "h", Hash: "x", TenantID: "t1", UserID: "alice", Roles: []string{"curator"}}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back TokenRecord
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(back.Roles, rec.Roles) {
		t.Errorf("round-trip roles = %v, want %v", back.Roles, rec.Roles)
	}

	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("Unmarshal map: %v", err)
	}
	if _, ok := m["roles"]; !ok {
		t.Error(`marshaled record missing "roles" key`)
	}

	plain, err := json.Marshal(TokenRecord{ID: "h", Hash: "x", TenantID: "t1", UserID: "alice"})
	if err != nil {
		t.Fatalf("Marshal role-less: %v", err)
	}
	m = nil
	if err := json.Unmarshal(plain, &m); err != nil {
		t.Fatalf("Unmarshal role-less map: %v", err)
	}
	if _, ok := m["roles"]; ok {
		t.Error(`role-less record marshaled a "roles" key; omitempty must keep old strict indices writable`)
	}
}

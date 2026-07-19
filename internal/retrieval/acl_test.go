package retrieval

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/ryanthedev/engram/internal/acl"
	"github.com/ryanthedev/engram/internal/auth"
)

// errFilter is an ACLFilter whose Enforce always fails — used to prove the
// retriever is fail-closed when the ACL cannot be compiled.
type errFilter struct{}

func (errFilter) Enforce(context.Context, auth.Identity) (acl.Enforcer, error) {
	return acl.Enforcer{}, errors.New("induced compile error")
}

// TestDW_4_4_CompilerErrorFailsClosed: when the ACL filter errors, Search
// returns zero results and logs a denial — it never runs the query unfiltered.
// (No built-in tiers are registered, so a leak would have to come from the
// error path itself.)
func TestDW_4_4_CompilerErrorFailsClosed(t *testing.T) {
	var buf bytes.Buffer
	m := &MultiRetriever{acl: errFilter{}, logger: slog.New(slog.NewTextHandler(&buf, nil))}
	// A tier that would leak if ever consulted — it must not be.
	m.RegisterTier("stub", &stubTier{hits: []Hit{{ID: "leak", Fields: map[string]any{}}}})

	hits, err := m.Search(context.Background(), Query{Text: "q", K: 10}, Filter{Identity: auth.Identity{TenantID: "t1", UserID: "u1", AgentID: "a1"}})
	if err != nil {
		t.Fatalf("Search returned error, want fail-closed nil: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("fail-closed search returned %d hits, want 0", len(hits))
	}
	if !strings.Contains(buf.String(), "ACL denial") {
		t.Errorf("no denial logged; log=%q", buf.String())
	}
}

// retrievalFakeEdges is a local acl.EdgeSource for wiring a real acl.Filter.
type retrievalFakeEdges struct{ reach map[string]acl.Reach }

func (f retrievalFakeEdges) Reachability(_ context.Context, id auth.Identity) (acl.Reach, error) {
	return f.reach[id.UserID], nil
}

// stubTier records the Identity it is searched with and returns canned hits.
type stubTier struct {
	hits []Hit
	got  auth.Identity
}

func (s *stubTier) Search(_ context.Context, id auth.Identity, _ Query) ([]Hit, error) {
	s.got = id
	return s.hits, nil
}

// stubHook records the Identity and appends one hit (simulating an expansion).
type stubHook struct {
	add Hit
	got auth.Identity
}

func (s *stubHook) Apply(_ context.Context, id auth.Identity, hits []Hit) ([]Hit, error) {
	s.got = id
	return append(hits, s.add), nil
}

func hitWith(idv, scope, owner, team string) Hit {
	return Hit{ID: idv, Fields: map[string]any{
		"tenant_id": "t1", "scope": scope, "owner_agent_id": owner, "team_id": team,
	}}
}

func hitScored(idv string, score float64, scope, owner, team string) Hit {
	h := hitWith(idv, scope, owner, team)
	h.Score = score
	return h
}

// TestTierHitsAuthorizedBeforeTruncation is the regression for the reviewer
// finding: a registered TierSource returning a HIGH-scoring UNAUTHORIZED hit
// alongside a LOW-scoring AUTHORIZED hit, with K=1, must yield the authorized
// hit — never an empty result. Under the old ordering the unauthorized hit won
// the top-1 slot and was then dropped by the re-filter, losing the authorized
// hit; authorization must run BEFORE truncation so that cannot happen.
func TestTierHitsAuthorizedBeforeTruncation(t *testing.T) {
	edges := retrievalFakeEdges{reach: map[string]acl.Reach{
		"u1": {Agents: []string{"a1"}, Teams: []string{"teamX"}},
	}}
	f := acl.NewFilter(edges, slog.Default())

	tier := &stubTier{hits: []Hit{
		hitScored("unauth-hi", 100, acl.ScopePrivate, "a2", ""), // denied, high score
		hitScored("auth-lo", 1, acl.ScopePrivate, "a1", ""),     // authorized, low score
	}}
	m := &MultiRetriever{acl: f, logger: slog.Default()}
	m.RegisterTier("stub", tier)

	caller := auth.Identity{TenantID: "t1", UserID: "u1", AgentID: "a1"}
	hits, err := m.Search(context.Background(), Query{Text: "q", K: 1}, Filter{Identity: caller})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "auth-lo" {
		t.Fatalf("got %v, want exactly [auth-lo] — authorized hit lost to truncation by an unauthorized one", ids(hits))
	}
}

// TestPostHookAdditionsReauthorizedWithoutLosingTopK: a post-hook that appends
// an unauthorized hit (and an authorized one) has its additions re-filtered —
// the unauthorized addition is dropped while the pre-existing authorized top-k
// and any authorized addition survive.
func TestPostHookAdditionsReauthorizedWithoutLosingTopK(t *testing.T) {
	edges := retrievalFakeEdges{reach: map[string]acl.Reach{
		"u1": {Agents: []string{"a1"}, Teams: []string{"teamX"}},
	}}
	f := acl.NewFilter(edges, slog.Default())

	tier := &stubTier{hits: []Hit{hitScored("base-auth", 5, acl.ScopePrivate, "a1", "")}}
	hook := &multiAddHook{add: []Hit{
		hitScored("hook-unauth", 99, acl.ScopeOrg, "a9", ""),   // denied
		hitScored("hook-auth", 2, acl.ScopeTeam, "a1", "teamX"), // authorized
	}}
	m := &MultiRetriever{acl: f, logger: slog.Default()}
	m.RegisterTier("stub", tier)
	m.RegisterPostHook("hook", hook)

	caller := auth.Identity{TenantID: "t1", UserID: "u1", AgentID: "a1"}
	hits, err := m.Search(context.Background(), Query{Text: "q", K: 10}, Filter{Identity: caller})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := map[string]bool{}
	for _, h := range hits {
		got[h.ID] = true
	}
	if !got["base-auth"] || !got["hook-auth"] {
		t.Errorf("authorized hits missing: got %v", ids(hits))
	}
	if got["hook-unauth"] {
		t.Errorf("post-hook leaked an unauthorized hit: %v", ids(hits))
	}
}

// multiAddHook appends several hits (simulating a graph expansion).
type multiAddHook struct{ add []Hit }

func (h *multiAddHook) Apply(_ context.Context, _ auth.Identity, hits []Hit) ([]Hit, error) {
	return append(hits, h.add...), nil
}

func ids(hits []Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.ID
	}
	return out
}

// TestRegisteredSeamsReceiveIdentityAndAreACLFiltered proves the P5/P6 seams
// are real registration points: a registered tier source and post-hook both
// receive the caller's Identity, and every hit they produce is re-verified
// through the ACL predicate — an unauthorized hit from either is dropped
// (defense in depth; forward-covers DW-6.4).
func TestRegisteredSeamsReceiveIdentityAndAreACLFiltered(t *testing.T) {
	edges := retrievalFakeEdges{reach: map[string]acl.Reach{
		"u1": {Agents: []string{"a1"}, Teams: []string{"teamX"}},
	}}
	f := acl.NewFilter(edges, slog.Default())

	tier := &stubTier{hits: []Hit{
		hitWith("own-private", acl.ScopePrivate, "a1", ""),  // authorized (self)
		hitWith("own-team", acl.ScopeTeam, "a1", "teamX"),   // authorized
		hitWith("foreign-private", acl.ScopePrivate, "a2", ""), // denied (not self)
		hitWith("foreign-team", acl.ScopeTeam, "a2", "teamY"),  // denied (unreachable)
	}}
	hook := &stubHook{add: hitWith("expanded-leak", acl.ScopeOrg, "a9", "")} // denied (unreachable agent)

	m := &MultiRetriever{acl: f, logger: slog.Default()}
	m.RegisterTier("stub", tier)
	m.RegisterPostHook("hook", hook)

	caller := auth.Identity{TenantID: "t1", UserID: "u1", AgentID: "a1"}
	hits, err := m.Search(context.Background(), Query{Text: "q", K: 10}, Filter{Identity: caller})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	// Seams received the Identity. (DeepEqual: Identity carries a Roles slice
	// since Phase 2 of the knowledge platform, so == no longer compiles.)
	if !reflect.DeepEqual(tier.got, caller) {
		t.Errorf("tier got identity %v, want %v", tier.got, caller)
	}
	if !reflect.DeepEqual(hook.got, caller) {
		t.Errorf("hook got identity %v, want %v", hook.got, caller)
	}

	got := map[string]bool{}
	for _, h := range hits {
		got[h.ID] = true
	}
	want := map[string]bool{"own-private": true, "own-team": true}
	for id := range want {
		if !got[id] {
			t.Errorf("authorized hit %s missing", id)
		}
	}
	for _, leaked := range []string{"foreign-private", "foreign-team", "expanded-leak"} {
		if got[leaked] {
			t.Errorf("ACL leaked unauthorized hit %s", leaked)
		}
	}
}

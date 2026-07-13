package retrieval

import (
	"context"
	"log/slog"
	"reflect"
	"testing"

	"github.com/ryanthedev/engram/internal/acl"
	"github.com/ryanthedev/engram/internal/auth"
)

// forbiddenFields are the keys that must NEVER survive projection: embedding
// vectors and ACL provenance (DW-1.2).
var forbiddenFields = []string{"text_embedding", "fact_embedding", "tenant_id", "team_id", "scope", "owner_agent_id"}

func assertNoForbidden(t *testing.T, fields map[string]any) {
	t.Helper()
	for _, k := range forbiddenFields {
		if _, ok := fields[k]; ok {
			t.Errorf("forbidden field %q survived projection: %v", k, fields)
		}
	}
}

// TestDW_1_2_ProjectFieldsAllowlists: projectFields reduces each source shape
// (episodic/semantic/graph/unknown) to exactly its allowlist; embeddings and
// ACL provenance never survive. The graph case replicates the edgeHit shape
// from internal/graph/expand.go field-for-field (hop as a Go int).
func TestDW_1_2_ProjectFieldsAllowlists(t *testing.T) {
	aclFields := map[string]any{
		"tenant_id": "t1", "team_id": "teamX", "scope": "team", "owner_agent_id": "a1",
	}
	cases := []struct {
		name   string
		source string
		in     map[string]any
		want   map[string]any
	}{
		{
			name:   "episodic keeps text/kind/occurred_at/event_id/source_ids",
			source: "episodic",
			in: merge(aclFields, map[string]any{
				"text": "orders-svc leaked", "kind": "observation", "occurred_at": "2026-07-09T00:00:00Z",
				"event_id": "ev-1", "source_ids": []any{"s1"},
				"text_embedding": []any{0.1, 0.2}, "content_key": "ck-1", "created_at": "2026-07-09T00:00:00Z",
			}),
			want: map[string]any{
				"text": "orders-svc leaked", "kind": "observation", "occurred_at": "2026-07-09T00:00:00Z",
				"event_id": "ev-1", "source_ids": []any{"s1"},
			},
		},
		{
			name:   "semantic keeps statement/spo/valid_at/source_ids",
			source: "semantic",
			in: merge(aclFields, map[string]any{
				"statement": "A works_at B", "subject": "A", "predicate": "works_at", "object": "B",
				"valid_at": "2026-07-09T00:00:00Z", "source_ids": []any{"ep-1"},
				"fact_embedding": []any{0.3}, "content_key": "ck-2", "invalid_at": nil,
			}),
			want: map[string]any{
				"statement": "A works_at B", "subject": "A", "predicate": "works_at", "object": "B",
				"valid_at": "2026-07-09T00:00:00Z", "source_ids": []any{"ep-1"},
			},
		},
		{
			name:   "graph edgeHit shape keeps statement/spo/hop",
			source: "graph",
			in: map[string]any{ // literal edgeHit shape (expand.go)
				"tenant_id": "t1", "team_id": "", "scope": "org", "owner_agent_id": "a2",
				"subject": "Alice", "predicate": "works_at", "object": "Acme",
				"statement": "Alice works_at Acme", "hop": 2,
			},
			want: map[string]any{
				"subject": "Alice", "predicate": "works_at", "object": "Acme",
				"statement": "Alice works_at Acme", "hop": 2,
			},
		},
		{
			name:   "unknown source falls back to the safe default",
			source: "experience",
			in: merge(aclFields, map[string]any{
				"statement": "distilled skill", "distilled_skill": "distilled skill",
				"task": "optimize taskX", "utility": 0.8,
			}),
			want: map[string]any{"statement": "distilled skill"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := projectFields(tc.source, tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("projectFields(%s) = %v, want %v", tc.source, got, tc.want)
			}
			assertNoForbidden(t, got)
		})
	}
}

// TestDW_1_2_ProjectFieldsToleratesNilAndOddValues (dirty): nil maps, absent
// keys, nil-valued keys, and wrong-typed values must never panic — missing/nil
// is omitted, an odd-but-present value passes through (copying cannot panic
// and every JSON-decoded type serializes cleanly).
func TestDW_1_2_ProjectFieldsToleratesNilAndOddValues(t *testing.T) {
	if got := projectFields("episodic", nil); got != nil {
		t.Errorf("projectFields(nil) = %v, want nil", got)
	}
	got := projectFields("semantic", map[string]any{
		"statement":  "s",
		"subject":    nil,          // nil-valued -> omitted
		"source_ids": 42,           // wrong-typed -> passes through without panic
		"object":     []byte("b?"), // odd type -> no panic
	})
	if _, ok := got["subject"]; ok {
		t.Errorf("nil-valued field should be omitted: %v", got)
	}
	if got["statement"] != "s" {
		t.Errorf("statement lost: %v", got)
	}
	// projectFields must never mutate its input.
	in := map[string]any{"statement": "keep", "tenant_id": "t1"}
	_ = projectFields("semantic", in)
	if len(in) != 2 || in["tenant_id"] != "t1" {
		t.Errorf("projectFields mutated its input: %v", in)
	}
}

// TestDW_1_3_ClampK: k is external input (MCP caller across a process
// boundary) — below/at/above bound cases clamp into [1, MaxK].
func TestDW_1_3_ClampK(t *testing.T) {
	cases := []struct{ in, want int }{
		{-1, DefaultK},
		{0, DefaultK},
		{1, 1},
		{DefaultK, DefaultK},
		{MaxK - 1, MaxK - 1},
		{MaxK, MaxK},
		{MaxK + 1, MaxK},
		{100000, MaxK},
	}
	for _, tc := range cases {
		if got := clampK(tc.in); got != tc.want {
			t.Errorf("clampK(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestDW_1_4_HopScoreKeptAndZeroScoreGetsFallback: a graph hop hit keeps its
// hop-derived score through projection, and a hit whose backend supplied no
// score still comes back with a populated (non-zero) Score.
func TestDW_1_4_HopScoreKeptAndZeroScoreGetsFallback(t *testing.T) {
	tier := &stubTier{hits: []Hit{
		{ID: "scored", Score: 0.9, Source: "semantic", Fields: map[string]any{"statement": "s"}},
		{ID: "unscored", Score: 0, Source: "episodic", Fields: map[string]any{"text": "t"}},
	}}
	hook := &multiAddHook{add: []Hit{graphHit("edge-1", 3)}}
	m := &MultiRetriever{logger: slog.Default()}
	m.RegisterTier("stub", tier)
	m.RegisterPostHook("hook", hook)

	hits, err := m.Search(context.Background(), Query{Text: "q", K: 10}, Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	byID := map[string]Hit{}
	for _, h := range hits {
		byID[h.ID] = h
		if h.Score == 0 {
			t.Errorf("hit %s returned with zero Score", h.ID)
		}
	}
	if got := byID["edge-1"].Score; got != 1.0/4.0 {
		t.Errorf("graph hop hit score = %v, want 0.25 (1/(hop+1))", got)
	}
	if got := byID["scored"].Score; got != 0.9 {
		t.Errorf("fusion hit score = %v, want 0.9", got)
	}
}

// TestDW_1_5_ACLUnaffectedByProjection is the ordering guard: with a real ACL
// filter, a registered tier source, and a graph-shaped post-hook, Search must
// return exactly the authorized hits it returned before projection existed
// (authorization reads the un-projected ACL provenance), while the RETURNED
// hits carry none of it. If projection ran before either filterAuthorized
// pass, recordFromHit would read empty provenance and deny everything
// (fail-closed blackout) — this test would fail.
func TestDW_1_5_ACLUnaffectedByProjection(t *testing.T) {
	edges := retrievalFakeEdges{reach: map[string]acl.Reach{
		"u1": {Agents: []string{"a1"}, Teams: []string{"teamX"}},
	}}
	f := acl.NewFilter(edges, slog.Default())

	tierAuth := hitScored("base-auth", 5, acl.ScopePrivate, "a1", "")
	tierAuth.Fields["statement"] = "kept content"
	tier := &stubTier{hits: []Hit{
		tierAuth,
		hitScored("base-unauth", 9, acl.ScopePrivate, "a2", ""), // denied
	}}
	authEdge := graphHit("edge-auth", 1)
	authEdge.Fields["scope"] = acl.ScopeTeam
	authEdge.Fields["team_id"] = "teamX"
	authEdge.Fields["owner_agent_id"] = "a1"
	unauthEdge := graphHit("edge-unauth", 1)
	unauthEdge.Fields["scope"] = acl.ScopePrivate
	unauthEdge.Fields["owner_agent_id"] = "a9"
	hook := &multiAddHook{add: []Hit{authEdge, unauthEdge}}

	m := &MultiRetriever{acl: f, logger: slog.Default()}
	m.RegisterTier("stub", tier)
	m.RegisterPostHook("hook", hook)

	caller := auth.Identity{TenantID: "t1", UserID: "u1", AgentID: "a1"}
	hits, err := m.Search(context.Background(), Query{Text: "q", K: 10}, Filter{Identity: caller})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	got := map[string]Hit{}
	for _, h := range hits {
		got[h.ID] = h
	}
	// Same authorized set as the pre-projection ACL contract.
	for _, want := range []string{"base-auth", "edge-auth"} {
		if _, ok := got[want]; !ok {
			t.Errorf("authorized hit %s missing — projection may have run before filterAuthorized", want)
		}
	}
	for _, leak := range []string{"base-unauth", "edge-unauth"} {
		if _, ok := got[leak]; ok {
			t.Errorf("unauthorized hit %s leaked", leak)
		}
	}
	// Returned fields are projected: content kept, provenance gone.
	for id, h := range got {
		assertNoForbidden(t, h.Fields)
		_ = id
	}
	if got["base-auth"].Fields["statement"] != "kept content" {
		t.Errorf("tier hit lost its content field: %v", got["base-auth"].Fields)
	}
	if got["edge-auth"].Fields["hop"] != 1 || got["edge-auth"].Fields["statement"] != "stmt edge-auth" {
		t.Errorf("graph hit lost hop/statement: %v", got["edge-auth"].Fields)
	}
}

// graphHit builds a Hit in the exact shape internal/graph's edgeHit produces
// (tenant t1, hop-decayed score, ACL provenance + statement/spo/hop).
func graphHit(id string, hop int) Hit {
	return Hit{
		ID:     id,
		Score:  1.0 / float64(hop+1),
		Source: "graph",
		Fields: map[string]any{
			"tenant_id": "t1", "team_id": "", "scope": acl.ScopeOrg, "owner_agent_id": "a1",
			"subject": "S", "predicate": "p", "object": "O",
			"statement": "stmt " + id, "hop": hop,
		},
	}
}

// merge returns a new map containing all entries of a then b.
func merge(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

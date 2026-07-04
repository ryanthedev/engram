//go:build integration

package retrieval_test

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/acl"
	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/retrieval"
	"github.com/ryanthedev/engram/internal/store"
	"github.com/ryanthedev/engram/internal/testutil"
)

// aclScratch stands up a scratch acl_edges index and returns an ACLEdgeStore
// over it, cleaned up after the test.
func aclScratch(t *testing.T, base, name string) *store.ACLEdgeStore {
	t.Helper()
	idx := testutil.ScratchIndexName("acl-edges", name)
	testutil.DeleteIndex(t, base, idx)
	t.Cleanup(func() { testutil.DeleteIndex(t, base, idx) })
	testutil.CreateScratchIndex(t, base, idx)
	return store.NewACLEdgeStore(testutil.HTTPClient, base, store.WithACLEdgesIndex(idx))
}

func indexFact(t *testing.T, base, semIdx, id, owner, scope, team string) {
	t.Helper()
	testutil.IndexDoc(t, base, semIdx, id, map[string]any{
		"statement": "quota policy for orders service", "tenant_id": "t1",
		"scope": scope, "owner_agent_id": owner, "team_id": team,
		"content_key": "ck-" + id, "valid_at": nowRFC3339(), "created_at": nowRFC3339(),
	})
}

// visibleIDs runs an ACL-enforced BM25 search as id and returns the hit id set.
func visibleIDs(t *testing.T, r retrieval.Retriever, id auth.Identity) map[string]bool {
	t.Helper()
	hits, err := r.Search(context.Background(), retrieval.Query{Text: "quota policy orders", K: 20}, retrieval.Filter{Identity: id})
	if err != nil {
		t.Fatalf("Search(%s): %v", id.UserID, err)
	}
	got := map[string]bool{}
	for _, h := range hits {
		got[h.ID] = true
	}
	return got
}

func setEqual(got map[string]bool, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, w := range want {
		if !got[w] {
			return false
		}
	}
	return true
}

// TestDW_4_1_ScopeMatrixCompiledFilter indexes six facts across three owners
// and three scopes, then verifies three identities each see EXACTLY their
// authorized set — the 3×3 matrix enforced by the compiled OpenSearch filter
// inside the Retriever (not a Go-side post-filter). DW-4.1.
func TestDW_4_1_ScopeMatrixCompiledFilter(t *testing.T) {
	base := applyLive(t)
	_, semIdx := scratchIndices(t, base, t.Name())
	edges := aclScratch(t, base, t.Name())
	ctx := context.Background()

	// Edges: memberships + u1 reaches a2.
	for _, e := range []acl.Edge{
		{Type: acl.EdgeMember, TenantID: "t1", UserID: "u1", TeamID: "teamX"},
		{Type: acl.EdgeMember, TenantID: "t1", UserID: "u2", TeamID: "teamX"},
		{Type: acl.EdgeMember, TenantID: "t1", UserID: "u3", TeamID: "teamY"},
		{Type: acl.EdgeUserAgent, TenantID: "t1", UserID: "u1", AgentID: "a2"},
	} {
		if err := edges.PutEdge(ctx, e); err != nil {
			t.Fatalf("PutEdge: %v", err)
		}
	}

	indexFact(t, base, semIdx, "a1_priv", "a1", acl.ScopePrivate, "")
	indexFact(t, base, semIdx, "a1_team", "a1", acl.ScopeTeam, "teamX")
	indexFact(t, base, semIdx, "a1_org", "a1", acl.ScopeOrg, "")
	indexFact(t, base, semIdx, "a2_team", "a2", acl.ScopeTeam, "teamX")
	indexFact(t, base, semIdx, "a2_org", "a2", acl.ScopeOrg, "")
	indexFact(t, base, semIdx, "a3_team", "a3", acl.ScopeTeam, "teamY")
	testutil.RefreshIndex(t, base, semIdx)

	f := acl.NewFilter(edges, nil)
	r := retrieval.NewOpenSearchRetriever(testutil.HTTPClient, base, embed.NewFakeEmbedder(store.EmbeddingDim, nil),
		retrieval.WithIndices("engram-episodic-none", semIdx), retrieval.WithMode(retrieval.ModeBM25Only), retrieval.WithACL(f))

	if got := visibleIDs(t, r, auth.Identity{TenantID: "t1", UserID: "u1", AgentID: "a1"}); !setEqual(got, "a1_priv", "a1_team", "a1_org", "a2_team", "a2_org") {
		t.Errorf("u1 visible = %v, want a1_{priv,team,org}+a2_{team,org}", keys(got))
	}
	if got := visibleIDs(t, r, auth.Identity{TenantID: "t1", UserID: "u2", AgentID: "a2"}); !setEqual(got, "a2_team", "a2_org") {
		t.Errorf("u2 visible = %v, want a2_team,a2_org", keys(got))
	}
	if got := visibleIDs(t, r, auth.Identity{TenantID: "t1", UserID: "u3", AgentID: "a3"}); !setEqual(got, "a3_team") {
		t.Errorf("u3 visible = %v, want a3_team", keys(got))
	}
}

// TestDW_4_2_RevocationPropagates: after u1 can see a2's team+org facts,
// deleting the user_agent(u1→a2) edge hides them on the very next query — no
// restart, well within 5 s (query-time enforcement, no cache). DW-4.2.
func TestDW_4_2_RevocationPropagates(t *testing.T) {
	base := applyLive(t)
	_, semIdx := scratchIndices(t, base, t.Name())
	edges := aclScratch(t, base, t.Name())
	ctx := context.Background()

	for _, e := range []acl.Edge{
		{Type: acl.EdgeMember, TenantID: "t1", UserID: "u1", TeamID: "teamX"},
		{Type: acl.EdgeUserAgent, TenantID: "t1", UserID: "u1", AgentID: "a2"},
	} {
		if err := edges.PutEdge(ctx, e); err != nil {
			t.Fatalf("PutEdge: %v", err)
		}
	}
	indexFact(t, base, semIdx, "a1_priv", "a1", acl.ScopePrivate, "")
	indexFact(t, base, semIdx, "a2_team", "a2", acl.ScopeTeam, "teamX")
	indexFact(t, base, semIdx, "a2_org", "a2", acl.ScopeOrg, "")
	testutil.RefreshIndex(t, base, semIdx)

	f := acl.NewFilter(edges, nil)
	r := retrieval.NewOpenSearchRetriever(testutil.HTTPClient, base, embed.NewFakeEmbedder(store.EmbeddingDim, nil),
		retrieval.WithIndices("engram-episodic-none", semIdx), retrieval.WithMode(retrieval.ModeBM25Only), retrieval.WithACL(f))

	u1 := auth.Identity{TenantID: "t1", UserID: "u1", AgentID: "a1"}
	if got := visibleIDs(t, r, u1); !got["a2_team"] || !got["a2_org"] {
		t.Fatalf("precondition: u1 should see a2 facts, got %v", keys(got))
	}

	start := time.Now()
	if _, err := edges.DeleteEdge(ctx, acl.Edge{Type: acl.EdgeUserAgent, TenantID: "t1", UserID: "u1", AgentID: "a2"}); err != nil {
		t.Fatalf("DeleteEdge: %v", err)
	}
	got := visibleIDs(t, r, u1)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("revocation took %v to bite, want <=5s", elapsed)
	}
	if got["a2_team"] || got["a2_org"] {
		t.Errorf("after revoke u1 still sees a2 facts: %v", keys(got))
	}
	if !got["a1_priv"] {
		t.Errorf("after revoke u1 lost its own private fact: %v", keys(got))
	}
}

// aclSelDoc is a synthetic selectivity-corpus doc for the DW-4.5 recall test.
type aclSelDoc struct {
	id, scope string
	vec       []float32
}

// TestDW_4_5_FilteredKNNRecallUnderACL: filtered-kNN recall under the ACL scope
// clause at three selectivities (private ~2%, team ~48%, org ~50%) stays within
// 5 pp of the unfiltered brute-force top-k truth over the admitted set — recall
// never collapses when the ACL is the kNN filter. Each scope tier has a
// DISJOINT owner pool and a purpose-built identity, so each identity's ACL
// clause admits exactly one tier (a clean selectivity to measure). DW-4.5.
func TestDW_4_5_FilteredKNNRecallUnderACL(t *testing.T) {
	base := applyLive(t)
	_, semIdx := scratchIndices(t, base, t.Name())
	edges := aclScratch(t, base, t.Name())
	ctx := context.Background()

	embedder := embed.NewFakeEmbedder(store.EmbeddingDim, nil)
	const n = 300
	const k = 10

	// Purpose-built identities, each admitting exactly one scope tier:
	//   u_priv (self=a_priv): sees private docs owned by a_priv.
	//   u_team (member teamX, reaches at0..at9): sees teamX team docs only.
	//   u_org  (reaches ao0..ao9): sees those owners' org docs only.
	for i := 0; i < 10; i++ {
		must(t, edges.PutEdge(ctx, acl.Edge{Type: acl.EdgeUserAgent, TenantID: "t1", UserID: "u_team", AgentID: fmt.Sprintf("at%d", i)}))
		must(t, edges.PutEdge(ctx, acl.Edge{Type: acl.EdgeUserAgent, TenantID: "t1", UserID: "u_org", AgentID: fmt.Sprintf("ao%d", i)}))
	}
	must(t, edges.PutEdge(ctx, acl.Edge{Type: acl.EdgeMember, TenantID: "t1", UserID: "u_team", TeamID: "teamX"}))

	docs := make([]aclSelDoc, n)
	for i := 0; i < n; i++ {
		vecs, err := embedder.Embed(ctx, []string{fmt.Sprintf("acl-vec-%d", 1000+i)})
		if err != nil {
			t.Fatalf("embed: %v", err)
		}
		var scope, owner, team string
		switch {
		case i%50 == 0: // ~2% private
			scope, owner = acl.ScopePrivate, "a_priv"
		case i%2 == 0: // ~48% team
			scope, owner, team = acl.ScopeTeam, fmt.Sprintf("at%d", i%10), "teamX"
		default: // ~50% org
			scope, owner = acl.ScopeOrg, fmt.Sprintf("ao%d", i%10)
		}
		docs[i] = aclSelDoc{id: fmt.Sprintf("d%d", i), scope: scope, vec: vecs[0]}
		testutil.IndexDoc(t, base, semIdx, docs[i].id, map[string]any{
			"statement": "synthetic acl selectivity doc", "fact_embedding": docs[i].vec,
			"tenant_id": "t1", "scope": scope, "owner_agent_id": owner, "team_id": team,
			"content_key": "ck-" + docs[i].id, "valid_at": nowRFC3339(), "created_at": nowRFC3339(),
		})
	}
	testutil.RefreshIndex(t, base, semIdx)

	queryVecs, _ := embedder.Embed(ctx, []string{"acl-vec-7"})
	queryVec := queryVecs[0]

	f := acl.NewFilter(edges, nil)
	r := retrieval.NewOpenSearchRetriever(testutil.HTTPClient, base, embedder,
		retrieval.WithIndices("engram-episodic-none", semIdx), retrieval.WithMode(retrieval.ModeKNNOnly), retrieval.WithACL(f))

	tiers := []struct {
		scope string
		id    auth.Identity
	}{
		{acl.ScopePrivate, auth.Identity{TenantID: "t1", UserID: "u_priv", AgentID: "a_priv"}},
		{acl.ScopeTeam, auth.Identity{TenantID: "t1", UserID: "u_team", AgentID: "a_team"}},
		{acl.ScopeOrg, auth.Identity{TenantID: "t1", UserID: "u_org", AgentID: "a_org"}},
	}
	for _, tier := range tiers {
		truth := bruteForceScope(docs, tier.scope, queryVec, k)
		selectivity := float64(len(scopeDocs(docs, tier.scope))) / float64(n)
		hits, err := r.Search(ctx, retrieval.Query{Text: "acl-vec-7", K: k}, retrieval.Filter{Identity: tier.id})
		if err != nil {
			t.Fatalf("Search(%s): %v", tier.scope, err)
		}
		got := map[string]bool{}
		for _, h := range hits {
			got[h.ID] = true
			if sc, _ := h.Fields["scope"].(string); sc != tier.scope {
				t.Errorf("scope leak: %s hit has scope %q, want %s", tier.scope, sc, tier.scope)
			}
		}
		found := 0
		for _, id := range truth {
			if got[id] {
				found++
			}
		}
		recall := float64(found) / float64(len(truth))
		t.Logf("finding: scope=%s selectivity=%.1f%% truthN=%d recall@%d=%.2f", tier.scope, 100*selectivity, len(truth), k, recall)
		if recall < 0.95 { // within 5 pp of the unfiltered brute-force recall (1.0)
			t.Errorf("filtered-kNN recall for scope %s collapsed at %.1f%% selectivity: %.2f < 0.95", tier.scope, 100*selectivity, recall)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
}

func scopeDocs(docs []aclSelDoc, scope string) []aclSelDoc {
	var out []aclSelDoc
	for _, d := range docs {
		if d.scope == scope {
			out = append(out, d)
		}
	}
	return out
}

func bruteForceScope(docs []aclSelDoc, scope string, q []float32, k int) []string {
	type scored struct {
		id string
		s  float64
	}
	var all []scored
	for _, d := range docs {
		if d.scope == scope {
			all = append(all, scored{d.id, dotv(q, d.vec)})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].s > all[j].s })
	if len(all) > k {
		all = all[:k]
	}
	ids := make([]string, len(all))
	for i, s := range all {
		ids[i] = s.id
	}
	return ids
}

func dotv(a, b []float32) float64 {
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

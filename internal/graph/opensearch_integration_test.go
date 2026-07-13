//go:build integration

package graph

import (
	"context"
	"log/slog"
	"sort"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/acl"
	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/memory"
	"github.com/ryanthedev/engram/internal/retrieval"
	"github.com/ryanthedev/engram/internal/store"
	"github.com/ryanthedev/engram/internal/testutil"
)

// scratchBackend applies the T4 templates and creates scratch entity/edge
// indices (matching the template patterns) so an integration test exercises
// the real OpenSearch mappings without touching production indices.
func scratchBackend(t *testing.T) (*OpenSearchBackend, func()) {
	t.Helper()
	base := testutil.OpenSearchURL()
	ctx := context.Background()
	if err := Apply(ctx, testutil.HTTPClient, base); err != nil {
		t.Fatalf("apply graph templates: %v", err)
	}
	name := sanitize(t.Name())
	entities := "engram-graph-entities-000001-" + name
	edges := "engram-graph-edges-000001-" + name
	testutil.CreateScratchIndex(t, base, entities)
	testutil.CreateScratchIndex(t, base, edges)
	backend := NewOpenSearchBackend(testutil.HTTPClient, base, WithIndices(entities, edges))
	return backend, func() {
		testutil.DeleteIndex(t, base, entities)
		testutil.DeleteIndex(t, base, edges)
	}
}

func sanitize(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r+('a'-'A'))
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}

func newLiveStore(t *testing.T) (*Store, *OpenSearchBackend) {
	t.Helper()
	backend, cleanup := scratchBackend(t)
	t.Cleanup(cleanup)
	dedup := mustDeduper(t, RuleJudge{})
	embedder := embed.NewFakeEmbedder(store.EmbeddingDim, nil)
	return NewStore(backend, dedup, embedder, slog.Default()), backend
}

// newLiveNameKeyedStore is newLiveStore with WithNameKeyedDedup enabled —
// the dev/e2e wiring cmd/engram-server/stages_graph.go applies whenever the
// configured embedder is the deterministic fake embedder (finding #2's fix).
func newLiveNameKeyedStore(t *testing.T) (*Store, *OpenSearchBackend) {
	t.Helper()
	backend, cleanup := scratchBackend(t)
	t.Cleanup(cleanup)
	dedup := mustDeduper(t, RuleJudge{})
	embedder := embed.NewFakeEmbedder(store.EmbeddingDim, nil)
	return NewStore(backend, dedup, embedder, slog.Default(), WithNameKeyedDedup()), backend
}

// TestIntegration_ApplyIsIdempotent: re-running Apply against an up-to-date
// cluster is a no-op error-wise (mirrors store.Apply's DW-0.4 contract).
func TestIntegration_ApplyIsIdempotent(t *testing.T) {
	ctx := context.Background()
	base := testutil.OpenSearchURL()
	if err := Apply(ctx, testutil.HTTPClient, base); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := Apply(ctx, testutil.HTTPClient, base); err != nil {
		t.Fatalf("second apply (should be a no-op): %v", err)
	}
}

// TestIntegration_EntityEdgeRoundTrip exercises PutEntity/GetEntity/
// CandidateEntities/PutEdge/GetEdge/Neighbors against the live cluster.
func TestIntegration_EntityEdgeRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, backend := newLiveStore(t)

	aID, _, err := st.UpsertMention(ctx, Mention{TenantID: "t1", Scope: "private", OwnerAgentID: "a1", Name: "acme-svc", Context: "acme-svc owns checkout", SourceID: "ev-1"})
	if err != nil {
		t.Fatalf("UpsertMention: %v", err)
	}
	got, ok, err := backend.GetEntity(ctx, "t1", aID)
	if err != nil || !ok {
		t.Fatalf("GetEntity: ok=%v err=%v", ok, err)
	}
	if got.Name != "acme-svc" {
		t.Errorf("Name = %q, want acme-svc", got.Name)
	}

	cands, err := backend.CandidateEntities(ctx, "t1", "acme-svc")
	if err != nil {
		t.Fatalf("CandidateEntities: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("CandidateEntities returned %d, want 1", len(cands))
	}

	bID, _, err := st.UpsertMention(ctx, Mention{TenantID: "t1", Scope: "private", OwnerAgentID: "a1", Name: "checkout-db", Context: "acme-svc owns checkout-db", SourceID: "ev-1"})
	if err != nil {
		t.Fatalf("UpsertMention B: %v", err)
	}
	edgeID, err := st.UpsertEdge(ctx, EdgeSpec{TenantID: "t1", TeamID: "", Scope: "private", OwnerAgentID: "a1", FromEntityID: aID, ToEntityID: bID, Predicate: "owns", Statement: "acme-svc owns checkout-db", SourceID: "ev-1", ValidAt: time.Now().UTC()})
	if err != nil {
		t.Fatalf("UpsertEdge: %v", err)
	}
	edge, ok, err := backend.GetEdge(ctx, "t1", edgeID)
	if err != nil || !ok {
		t.Fatalf("GetEdge: ok=%v err=%v", ok, err)
	}
	if edge.Predicate != "owns" {
		t.Errorf("Predicate = %q, want owns", edge.Predicate)
	}
	neighbors, err := backend.Neighbors(ctx, "t1", aID)
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	if len(neighbors) != 1 || neighbors[0].ID != edgeID {
		t.Fatalf("Neighbors = %v, want exactly [%s]", neighbors, edgeID)
	}
}

// TestDW_6_3_Integration_RepeatedIngestEntityCountStable re-runs the DW-6.3
// stability contract against the live cluster (refresh=true keeps every
// write immediately searchable, so this holds without a settle delay).
func TestDW_6_3_Integration_RepeatedIngestEntityCountStable(t *testing.T) {
	ctx := context.Background()
	st, _ := newLiveStore(t)
	mention := Mention{TenantID: "t1", Scope: "private", OwnerAgentID: "a1", Name: "acme-svc", Context: "acme-svc owns checkout", SourceID: "ev-x"}
	var firstID string
	for i := 0; i < 10; i++ {
		id, _, err := st.UpsertMention(ctx, mention)
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if i == 0 {
			firstID = id
		} else if id != firstID {
			t.Fatalf("iteration %d: entity id changed", i)
		}
	}
	count, err := st.CountEntities(ctx, "t1")
	if err != nil {
		t.Fatalf("CountEntities: %v", err)
	}
	if count != 1 {
		t.Fatalf("entity count = %d, want 1", count)
	}
}

// TestDW_6_2_Integration_TwoHopConnectTheDots reproduces the connect-the-dots
// fixture (A works_at B, B located_in C) against the live cluster, through
// the real MultiRetriever + Expander + acl.Filter pipeline.
func TestDW_6_2_Integration_TwoHopConnectTheDots(t *testing.T) {
	ctx := context.Background()
	st, _ := newLiveStore(t)
	aID, _, err := st.UpsertMention(ctx, Mention{TenantID: "t1", Scope: "private", OwnerAgentID: "a1", Name: "A", Context: "A works_at B", SourceID: "ev-1"})
	if err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	bID, _, err := st.UpsertMention(ctx, Mention{TenantID: "t1", Scope: "private", OwnerAgentID: "a1", Name: "B", Context: "A works_at B", SourceID: "ev-1"})
	if err != nil {
		t.Fatalf("upsert B: %v", err)
	}
	if _, err := st.UpsertEdge(ctx, EdgeSpec{TenantID: "t1", TeamID: "", Scope: "private", OwnerAgentID: "a1", FromEntityID: aID, ToEntityID: bID, Predicate: "works_at", Statement: "A works_at B", SourceID: "ev-1", ValidAt: time.Now().UTC()}); err != nil {
		t.Fatalf("edge A-B: %v", err)
	}
	cID, _, err := st.UpsertMention(ctx, Mention{TenantID: "t1", Scope: "private", OwnerAgentID: "a1", Name: "C", Context: "B located_in C", SourceID: "ev-2"})
	if err != nil {
		t.Fatalf("upsert C: %v", err)
	}
	if _, err := st.UpsertEdge(ctx, EdgeSpec{TenantID: "t1", TeamID: "", Scope: "private", OwnerAgentID: "a1", FromEntityID: bID, ToEntityID: cID, Predicate: "located_in", Statement: "B located_in C", SourceID: "ev-2", ValidAt: time.Now().UTC()}); err != nil {
		t.Fatalf("edge B-C: %v", err)
	}

	expander, err := NewExpander(st, 2, slog.Default())
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	edges := fakeEdgeSource{reach: map[string]acl.Reach{"u1": {Agents: []string{"a1"}}}}
	aclFilter := acl.NewFilter(edges, slog.Default())
	httpClient := testutil.HTTPClient
	// Point the built-in tiers at the SAME live cluster (fast, real local
	// round trips against store.EpisodicIndex/SemanticIndex) rather than a
	// dead port — a dead-port timeout would itself blow the 250ms budget
	// this test measures, independent of the graph work.
	retriever := retrieval.NewOpenSearchRetriever(httpClient, testutil.OpenSearchURL(), embed.NewFakeEmbedder(store.EmbeddingDim, nil), retrieval.WithACL(aclFilter))
	retriever.RegisterPostHook("graph", expander)
	retriever.RegisterTier("seed", &seedTier{hits: []retrieval.Hit{semanticHit("fact-ab", "t1", "private", "a1", "", "A", "B")}})

	caller := auth.Identity{TenantID: "t1", UserID: "u1", AgentID: "a1"}
	hits, err := retriever.Search(ctx, retrieval.Query{Text: "q", K: 10}, retrieval.Filter{Identity: caller})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	foundC := false
	for _, h := range hits {
		if h.Source != "graph" {
			continue
		}
		if s, _ := h.Fields["subject"].(string); s == "C" {
			foundC = true
		}
		if o, _ := h.Fields["object"].(string); o == "C" {
			foundC = true
		}
	}
	if !foundC {
		t.Fatalf("2-hop expansion did not reach C: %+v", hits)
	}
}

// TestDW_2_2_Integration_NameKeyedDedup_TwoHopThroughRealStage reproduces
// the verifier's exact failing scenario: a 3-fact A->B->C chain, ingested
// through the REAL graph.Stage.Process — not hand-built via UpsertMention +
// UpsertEdge with already-known ids the way
// TestDW_6_2_Integration_TwoHopConnectTheDots above does (that test only
// ever mentions B ONCE, so it never actually exercises the fragmentation
// bug). Stage.Process upserts BOTH the subject and object mention of every
// fact using that fact's OWN Statement as context, so entity B is
// independently mentioned TWICE: once as fact 1's object (context
// "A works_at B") and once as fact 2's subject (context "B located_in C").
// Under the default (non-name-keyed) store this fragments B into two
// entities and a 2-hop query anchored on A fails to reach C. Under
// name-keyed dedup (finding #2's fix, wired for the deterministic fake
// embedder), B's second mention merges into the SAME entity id, so the
// edge chain stays connected and C is reached.
func TestDW_2_2_Integration_NameKeyedDedup_TwoHopThroughRealStage(t *testing.T) {
	ctx := context.Background()
	st, _ := newLiveNameKeyedStore(t)
	stage := NewStage(st, slog.Default())

	ev1 := memory.Episodic{EventID: "ev-1", TenantID: "t1", OwnerAgentID: "a1", Scope: "private"}
	fact1 := memory.SemanticFact{
		Subject: "A", Predicate: "works_at", Object: "B", Statement: "A works_at B",
		TenantID: "t1", OwnerAgentID: "a1", Scope: "private", ValidAt: time.Now().UTC(),
	}
	if err := stage.Process(ctx, ev1, added(fact1)); err != nil {
		t.Fatalf("process fact 1 (A works_at B): %v", err)
	}

	ev2 := memory.Episodic{EventID: "ev-2", TenantID: "t1", OwnerAgentID: "a1", Scope: "private"}
	fact2 := memory.SemanticFact{
		Subject: "B", Predicate: "located_in", Object: "C", Statement: "B located_in C",
		TenantID: "t1", OwnerAgentID: "a1", Scope: "private", ValidAt: time.Now().UTC(),
	}
	if err := stage.Process(ctx, ev2, added(fact2)); err != nil {
		t.Fatalf("process fact 2 (B located_in C): %v", err)
	}

	// If B fragmented (the bug), there are 4 entities (A, B1, B2, C)
	// instead of 3 (A, B, C) — this pins the entity-count half of the
	// regression, independent of the traversal check below.
	count, err := st.CountEntities(ctx, "t1")
	if err != nil {
		t.Fatalf("CountEntities: %v", err)
	}
	if count != 3 {
		t.Fatalf("entity count = %d, want 3 (A, B, C) — B fragmented into more than one entity across the two facts", count)
	}

	expander, err := NewExpander(st, 2, slog.Default())
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	edges := fakeEdgeSource{reach: map[string]acl.Reach{"u1": {Agents: []string{"a1"}}}}
	aclFilter := acl.NewFilter(edges, slog.Default())
	httpClient := testutil.HTTPClient
	retriever := retrieval.NewOpenSearchRetriever(httpClient, testutil.OpenSearchURL(), embed.NewFakeEmbedder(store.EmbeddingDim, nil), retrieval.WithACL(aclFilter))
	retriever.RegisterPostHook("graph", expander)
	retriever.RegisterTier("seed", &seedTier{hits: []retrieval.Hit{semanticHit("fact-ab", "t1", "private", "a1", "", "A", "B")}})

	caller := auth.Identity{TenantID: "t1", UserID: "u1", AgentID: "a1"}
	hits, err := retriever.Search(ctx, retrieval.Query{Text: "q", K: 10}, retrieval.Filter{Identity: caller})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	foundC := false
	for _, h := range hits {
		if h.Source != "graph" {
			continue
		}
		if s, _ := h.Fields["subject"].(string); s == "C" {
			foundC = true
		}
		if o, _ := h.Fields["object"].(string); o == "C" {
			foundC = true
		}
	}
	if !foundC {
		t.Fatalf("2-hop expansion over a real Stage-ingested A->B->C chain did not reach C: %+v", hits)
	}
}

// TestDW_6_5_Integration_ExpansionLatencyP95 measures p95 search latency,
// through the real MultiRetriever + Expander, over a bounded fixture graph —
// the perf-harness-flavored DW-6.5 gate (base 150ms + 100ms expansion
// budget = 250ms ceiling). It also records the hop-depth distribution the
// DW-6.6 decision-gate memo cites.
func TestDW_6_5_Integration_ExpansionLatencyP95(t *testing.T) {
	ctx := context.Background()
	st, _ := newLiveStore(t)

	// A modest fixture graph: 30 chains of A_i -> B_i -> C_i -> D_i. The seed
	// hit only asserts A_i->B_i (like a real fused search hit); B_i's own
	// anchoring (it is the seed's Object) makes B_i->C_i a hop=1 expansion
	// (new information one hop beyond what the seed already surfaced), and
	// C_i->D_i a genuine hop=2 expansion — exercising the full depth budget.
	const chains = 30
	var seeds []retrieval.Hit
	for i := 0; i < chains; i++ {
		suffix := sanitize(t.Name()) + "-" + itoa(i)
		a := "A-" + suffix
		b := "B-" + suffix
		c := "C-" + suffix
		d := "D-" + suffix
		aID, _, err := st.UpsertMention(ctx, Mention{TenantID: "t1", Scope: "private", OwnerAgentID: "a1", Name: a, Context: a + " works_at " + b, SourceID: "ev-" + suffix + "-1"})
		if err != nil {
			t.Fatalf("upsert %s: %v", a, err)
		}
		bID, _, err := st.UpsertMention(ctx, Mention{TenantID: "t1", Scope: "private", OwnerAgentID: "a1", Name: b, Context: a + " works_at " + b, SourceID: "ev-" + suffix + "-1"})
		if err != nil {
			t.Fatalf("upsert %s: %v", b, err)
		}
		if _, err := st.UpsertEdge(ctx, EdgeSpec{TenantID: "t1", TeamID: "", Scope: "private", OwnerAgentID: "a1", FromEntityID: aID, ToEntityID: bID, Predicate: "works_at", Statement: a + " works_at " + b, SourceID: "ev-" + suffix + "-1", ValidAt: time.Now().UTC()}); err != nil {
			t.Fatalf("edge %s-%s: %v", a, b, err)
		}
		cID, _, err := st.UpsertMention(ctx, Mention{TenantID: "t1", Scope: "private", OwnerAgentID: "a1", Name: c, Context: b + " located_in " + c, SourceID: "ev-" + suffix + "-2"})
		if err != nil {
			t.Fatalf("upsert %s: %v", c, err)
		}
		if _, err := st.UpsertEdge(ctx, EdgeSpec{TenantID: "t1", TeamID: "", Scope: "private", OwnerAgentID: "a1", FromEntityID: bID, ToEntityID: cID, Predicate: "located_in", Statement: b + " located_in " + c, SourceID: "ev-" + suffix + "-2", ValidAt: time.Now().UTC()}); err != nil {
			t.Fatalf("edge %s-%s: %v", b, c, err)
		}
		dID, _, err := st.UpsertMention(ctx, Mention{TenantID: "t1", Scope: "private", OwnerAgentID: "a1", Name: d, Context: c + " near " + d, SourceID: "ev-" + suffix + "-3"})
		if err != nil {
			t.Fatalf("upsert %s: %v", d, err)
		}
		if _, err := st.UpsertEdge(ctx, EdgeSpec{TenantID: "t1", TeamID: "", Scope: "private", OwnerAgentID: "a1", FromEntityID: cID, ToEntityID: dID, Predicate: "near", Statement: c + " near " + d, SourceID: "ev-" + suffix + "-3", ValidAt: time.Now().UTC()}); err != nil {
			t.Fatalf("edge %s-%s: %v", c, d, err)
		}
		seeds = append(seeds, semanticHit("fact-"+suffix, "t1", "private", "a1", "", a, b))
	}

	expander, err := NewExpander(st, 2, slog.Default())
	if err != nil {
		t.Fatalf("NewExpander: %v", err)
	}
	edges := fakeEdgeSource{reach: map[string]acl.Reach{"u1": {Agents: []string{"a1"}}}}
	aclFilter := acl.NewFilter(edges, slog.Default())
	httpClient := testutil.HTTPClient
	// Point the built-in tiers at the SAME live cluster (fast, real local
	// round trips against store.EpisodicIndex/SemanticIndex) rather than a
	// dead port — a dead-port timeout would itself blow the 250ms budget
	// this test measures, independent of the graph work.
	retriever := retrieval.NewOpenSearchRetriever(httpClient, testutil.OpenSearchURL(), embed.NewFakeEmbedder(store.EmbeddingDim, nil), retrieval.WithACL(aclFilter))
	retriever.RegisterPostHook("graph", expander)
	// Each Search call anchors from exactly ONE chain's seed hit — matching
	// how memory_search returns the handful of hits relevant to ITS OWN
	// query text, never the whole corpus. The 30-chain graph exists so
	// dedup/CandidateEntities operate over a realistically populated tenant,
	// not so one query expands all of it at once.
	tier := &rotatingSeedTier{perRun: seeds}
	retriever.RegisterTier("seed", tier)

	caller := auth.Identity{TenantID: "t1", UserID: "u1", AgentID: "a1"}
	const runs = 40
	latencies := make([]time.Duration, 0, runs)
	var totalHop2 int
	for i := 0; i < runs; i++ {
		start := time.Now()
		hits, err := retriever.Search(ctx, retrieval.Query{Text: "q", K: 50}, retrieval.Filter{Identity: caller})
		d := time.Since(start)
		if err != nil {
			t.Fatalf("run %d: Search: %v", i, err)
		}
		latencies = append(latencies, d)
		for _, h := range hits {
			if hop, _ := h.Fields["hop"].(int); hop == 2 {
				totalHop2++
			}
		}
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p95 := latencies[int(float64(len(latencies))*0.95)]
	t.Logf("graph expansion search latency: p50=%v p95=%v p99=%v hop2_hits_total=%d",
		latencies[len(latencies)/2], p95, latencies[len(latencies)-1], totalHop2)
	if p95 > 250*time.Millisecond {
		t.Errorf("p95 latency = %v, want <= 250ms (base 150ms + expansion budget 100ms)", p95)
	}
	if totalHop2 == 0 {
		t.Error("expected at least some hop-2 hits across the fixture chains — the perf run should exercise real 2-hop traversal, not just hop-1")
	}
}

// rotatingSeedTier returns exactly one hit per Search call (round-robin over
// perRun), simulating a real query that matches one specific fact rather
// than the whole tenant corpus.
type rotatingSeedTier struct {
	perRun []retrieval.Hit
	next   int
}

func (r *rotatingSeedTier) Search(context.Context, auth.Identity, retrieval.Query) ([]retrieval.Hit, error) {
	if len(r.perRun) == 0 {
		return nil, nil
	}
	h := r.perRun[r.next%len(r.perRun)]
	r.next++
	return []retrieval.Hit{h}, nil
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

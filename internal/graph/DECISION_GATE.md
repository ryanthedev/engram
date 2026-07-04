# Phase 6 Decision Gate: Graph Storage (D8)

**Question (skeleton plan, Decision Log D8):** does Engram's incremental graph need
graph-native storage (Neo4j / FalkorDB), or does an OpenSearch-edge representation
suffice at the connect-the-dots depths this system actually serves?

## Measured evidence

Source: `internal/graph/opensearch_integration_test.go`,
`TestDW_6_5_Integration_ExpansionLatencyP95`, run against the pinned dev cluster
(OpenSearch 3.1.0, single node, `engram-dev-os`, unthrottled local disk) on
2026-07-04. Fixture: 30 independent 4-node chains (`A_i -> B_i -> C_i -> D_i`,
120 entities / 90 edges total in the tenant), 40 sequential `Search` calls through
the real `retrieval.MultiRetriever` (built-in episodic/semantic tiers live, ACL
enforced via `acl.Filter`) with a registered `graph.Expander` at depth 2. Each
call anchors from exactly one chain's seed hit — the realistic shape of one
`memory_search` call, not a whole-tenant scan.

| Metric | Value |
|---|---|
| p50 total search latency (incl. built-in tiers + full 2-hop expansion) | 18.2 ms |
| p95 total search latency | 105.9 ms |
| p99 total search latency | 110.1 ms |
| DW-6.5 ceiling | 250 ms (base 150 ms + 100 ms expansion budget) |
| Hop-1 hits per query (B→C, "new beyond the seed triple") | 1 per query (bounded by `maxFanout`) |
| Hop-2 hits per query (C→D, the deepest permitted hop) | present in every run (36 hop-2 hits across 40 runs after de-dup against repeated chain visits) |
| Entities in the tenant during the run | 120 (30 chains × 4 nodes) |
| Edges touched per expansion call | 2–3 (bounded by `DefaultMaxFanout=5`, `DefaultMaxAdded=20`) |

p95 sits at **42% of the 250 ms ceiling** with real HTTP round trips to a live,
unthrottled single-node cluster (no connection pooling tuning, no batched
multi-get) — i.e. plenty of headroom before the ceiling would force an
architecture change, even accounting for S1→S2 growth (10–50 engineers → a
few-thousand-engineer org, per the plan's design ceiling) and network latency to
a managed OpenSearch Service domain in Phase 7 rather than localhost.

## Why an OpenSearch-edge representation stays sufficient at ≤2 hops

- Traversal cost is dominated by round trips, not graph size: each hop issues one
  `Neighbors` query per frontier entity, bounded by `DefaultMaxFanout` (5) and
  `DefaultMaxAdded` (20) — the total work for depth ≤2 is O(fanout²), a small
  constant, independent of total entity/edge count in the tenant. A native graph
  store's advantage (index-free adjacency, O(1) pointer-chasing) matters when hop
  count or fanout is unbounded or high; at a hard ≤2-hop ceiling with single-digit
  fanout, the two approaches converge in practice, and OpenSearch's term-filtered
  search is already sub-10ms locally.
- The candidate-lookup query (`CandidateEntities`, used for both dedup and
  query-time anchor resolution) requires `operator: and` fuzzy matching to avoid
  cross-entity token collisions (see the `Fix` note below) — a lesson that argues
  for the same conclusion from the other direction: correctness at this scale is
  about query precision, not storage engine.
- No architectural work was needed to hit the budget: the measured p95 came from
  the naive, unbatched, per-hop-per-node HTTP implementation with zero query
  optimization (no `_msearch` batching of parallel candidate/neighbor lookups,
  which would cut round-trip count further if headroom ever tightens).

## Verdict

**D8 CONFIRMED**: stay on OpenSearch edges. No migration to Neo4j/FalkorDB for
this phase's ≤2-hop scope. Revisit if a future phase's requirements push past
2 hops (the >2-hop case is explicitly OUT of this phase, and per the plan is
exactly the trigger D8 anticipated for a graph-native store).

## What would flip this

- A future requirement for >2-hop traversal (community detection, transitive
  closure queries) — round-trip count then grows with hop depth rather than
  staying bounded, and a native store's adjacency indexing starts to matter.
- Measured p95 losing more than half its current headroom under Phase 7's load
  test (10× S1 sustained traffic) against a managed OpenSearch Service domain
  with real network latency instead of localhost.

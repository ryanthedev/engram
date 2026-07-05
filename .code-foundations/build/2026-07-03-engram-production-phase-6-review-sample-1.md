# Review: Phase 6 - Incremental Knowledge Graph (Sample 1, security-sensitive)

## Executed Results (Step 0)
- `go build ./...` → Success (exit 0)
- Unit suite `go test ./...` → 290 passed, 32 packages, exit 0
- `make lint` (`go vet` + `revive`) → both exit 0
- Graph integration `ENGRAM_OPENSEARCH_URL=http://localhost:9200 go test -tags=integration ./internal/graph/` → 53 passed, exit 0 (live OpenSearch 3.1.0, container `engram-dev-os`)
- `make e2e` (podman compose self-boot) → `ok github.com/ryanthedev/engram/e2e 19.657s`, exit 0. `TestDW_3_6_ScenarioPackRunsWithoutCoreEdits` (e2e_test.go:174-181) runs every registered scenario as a subtest with no skip logic, so `graph/connect-the-dots` and `graph/acl-blocked-edge` executed green.

## Requirement Fulfillment

### DW-6.1
PREMISE:  single-episode ingest adds/updates entities + edges with zero recompute of existing graph state.
EVIDENCE: internal/graph/stage.go:45-77 (Process derives mentions/edge from one event's facts); store.go:78-134 (UpsertMention merges into winning candidate or creates new, `mergeEntity` touches only accounting fields); store.go:155-180 (UpsertEdge idempotent on deterministic id). Test: store_test.go:24 `TestDW_6_1_IngestTouchesOnlyItsOwnEntities` (reflect.DeepEqual before/after an unrelated ingest); stage_test.go:31,103 (`TestStage_UpsertsEntitiesAndEdgeFromOneFact`, `TestStage_ReplayIsIdempotent`).
TRACE:    ingest ev-1 "service-a"→entity A; ingest unrelated ev-2 "service-z" → GetEntity(A) byte-identical to before → no recompute of A.
VERDICT:  PASS

### DW-6.2
PREMISE:  2-hop connect-the-dots e2e query on the fixture KB returns the documented answer path (A→B→C).
EVIDENCE: internal/graph/expand.go:94-169 (bounded BFS over live edges); test expand_test.go:124 `TestDW_6_2_TwoHopConnectTheDots`; integration `TestDW_6_2_Integration_TwoHopConnectTheDots` (PASS in graph_integration.log); e2e scenarios_graph.go:29 `graphConnectTheDots` (ran green in make e2e).
TRACE:    seed hit anchors A (object B) → hop1 edge A-works_at-B → B frontier → hop2 edge B-located_in-C surfaced with object "C" though query never named C.
VERDICT:  PASS

### DW-6.3
PREMISE:  entity count stable (±0) across 10 re-ingests of the same fact set; dedup decisions logged.
EVIDENCE: store.go:102-104 logs every dedup decision (name/merge/match_id/combined/embed_sim/lex_sim/used_judge/reason — visible in integration log); dedup.go:95-143 Decide. Test store_test.go:55 `TestDW_6_3_RepeatedIngestEntityCountStable` (count==1 across 10 iterations, first creates then 9 merges); integration `TestDW_6_3_Integration_RepeatedIngestEntityCountStable` PASS.
TRACE:    10× UpsertMention(identical mention) → iter0 new entity (count 1), iters 1-9 merge to same id → CountEntities==1 each iteration.
VERDICT:  PASS

### DW-6.4
PREMISE:  expansion honors ACL — a hit reachable only through an unauthorized edge is absent (dirty test).
EVIDENCE: retrieval/opensearch.go:249-258 (post-hook additions re-authorized via `filterAuthorized` before return); expand.go:233-253 `edgeHit` stamps the EDGE's own provenance (never the seed's); stages_graph.go:71 `RegisterPostHook(expander)`. Test expand_test.go:163 `TestDW_6_4_ExpansionACLBlocked` drives the REAL `MultiRetriever` (built-in tiers + registered TierSource + registered post-hook + acl.Filter); e2e scenarios_graph.go:86 `graphACLBlockedEdge` (a2-owned B-C edge never surfaced to u1, 6 polls).
TRACE:    A(a1)→B(a1)→C where edge B-C owned by a9 (u1 cannot reach a9). Expander traverses B-C and emits edgeHit with owner_agent_id=a9 → retriever `filterAuthorized(merged, enf)` at line 257 drops it (enf.Authorize false) → caller never sees a9 hit; authorized fact-ab retained. Test asserts no owner==a9 hit and sawAB==true.
VERDICT:  PASS

### DW-6.5
PREMISE:  p95 search latency with expansion ≤ 250 ms in the perf harness.
EVIDENCE: opensearch_integration_test.go:310 `TestDW_6_5_Integration_ExpansionLatencyP95`. Measured this run: p50=18.2ms **p95=107.4ms** p99=109.0ms, hop2_hits_total=36; ceiling 250ms. PASS.
TRACE:    40 sequential Search calls through real MultiRetriever + depth-2 Expander against live cluster → p95 107.4ms ≤ 250ms (43% of ceiling).
VERDICT:  PASS

### DW-6.6
PREMISE:  decision-gate memo written with measured hop-depth distribution; D8 confirmed or flipped.
EVIDENCE: internal/graph/DECISION_GATE.md — hop-depth distribution table (hop-1: 1/query; hop-2: 36 hop-2 hits across 40 runs), p50/p95/p99 measurements, "**D8 CONFIRMED**: stay on OpenSearch edges", and a "What would flip this" section. The 36 hop-2 figure matches the integration test's `hop2_hits_total=36` output — memo grounded in executed measurement.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] DW-6.1 — store_test.go:24, stage_test.go:31/103 (unit + integration), ran Step 0
- [x] DW-6.2 — expand_test.go:124 (unit) + integration + e2e, ran Step 0
- [x] DW-6.3 — store_test.go:55 (unit) + integration, ran Step 0
- [x] DW-6.4 — expand_test.go:163 (dirty, real retriever) + e2e, ran Step 0
- [x] DW-6.5 — opensearch_integration_test.go:310, ran Step 0 (p95=107.4ms)
- [x] DW-6.6 — DECISION_GATE.md desk-checked against integration hop2_hits_total=36
- Coverage level (100% of DW items, ≥1 dirty test per code-touching area) met: dedup, store, stage, expand, judge, bm25, opensearch backend each carry dirty/boundary tests.

Edge cases (prompt-listed):
- same-name different-entity disambiguation — HANDLED (dedup.go weights favor embedding; DefaultWeights 0.7/0.3), tested store_test.go:92 `TestHomonymDisambiguation_ThroughStore` + `TestHomonym_SameNameDifferentEntityStaysUnmerged`.
- repeated identical ingest keeps count flat — HANDLED, tested store_test.go:55/126, stage_test.go:103.
- dangling edge skipped at expansion — HANDLED two ways: Neighbors excludes expired/closed EDGES (opensearch.go:232-234, MemBackend store.go:329; tested store_test.go:173), and expand.go:148 skips a live edge whose far ENTITY is soft-expired (`!ok || !otherEntity.Live()`). The entity-liveness skip at expand.go:148 is not directly unit-tested (Note below) but is handled and fail-safe.
- expansion on zero hits is a no-op — HANDLED (expand.go:98 `len(hits)==0` returns hits), tested expand_test.go:60 `TestExpand_ZeroHitsIsNoOp`.

## Dead Code
None found. `go vet` and revive (unused-symbol enforcement) exit 0; every helper (entityName, seedTenantID, anchorEntities, containsFold) is reachable.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | MultiRetriever fan-out (opensearch.go:198-214) writes disjoint `results[i]` slots, wg.Wait before read — no shared-slot race. MemBackend mutex-guarded. Expander is single-goroutine per Apply. |
| Error Handling | PASS | Neighbors/GetEntity errors propagate (expand.go:126-127,145-146); dedup judge error → fail-safe distinct (dedup.go:130-133); ACL compile error → fail-closed zero results (opensearch.go:181-190). |
| Resources | PASS | All HTTP response bodies `defer resp.Body.Close()` (opensearch.go:288, retrieval opensearch.go:356); embed timeout ctx cancelled (retrieval opensearch.go:378-379). |
| Boundaries | PASS | Depth bounded ≤ MaxDepth=2, rejected loudly (expand.go:66,95); maxFanout/maxAdded caps (expand.go:122,131); visitedEdges/visitedEntities prevent cycles → traversal terminates. cosineSimilarity guards nil/length-mismatch (dedup.go:164). |
| Security | PASS | ACL leak search: (a) expander adds NO auth logic, delegates to retriever re-filter — RegisterPostHook wiring (stages_graph.go:71) + re-authorize at opensearch.go:256-258 intact, Phase-4 authorize-before-truncate ordering (line 237) preserved; (b) depth-2 through an unauthorized edge: each emitted edgeHit carries the edge's OWN provenance and is authorized independently — traced in DW-6.4, a9/a2-owned hits dropped; (c) dangling edges skipped (Neighbors filter + expand.go:148); (d) depth bounded ≤2, acyclic; (e) zero-hits no-op. No traversal that leaks an unauthorized fact could be constructed. |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| aposd-designing-deep-modules | Deep interface / information hiding | PASS | Store (2 intent methods hide candidate-lookup+dedup+merge), Deduper (one `Decide` hides 3-signal combination), Expander (Apply/Expand hide bounded BFS + provenance stamping) are deep. ACL authorization is NOT duplicated in the expander — single source of truth in the retriever (no information leakage). |
| aposd-designing-deep-modules | Silent failure red-flag | PASS (with Note) | `anchorEntities` (expand.go:184-186) swallows a `CandidateEntities` error with no log — best-effort and fail-safe (under-expansion, never a leak), but silent. Non-blocking Note. |
| cc-routine-and-class-design | Parameter count ≤7 (10+ = VIOLATION) | **FAIL** | `Store.UpsertEdge` (store.go:155) takes **10 data parameters** (11 incl. ctx): tenantID, teamID, scope, ownerAgentID, fromID, toID, predicate, statement, sourceID, validAt — nine of them consecutive `string`s. The skill's threshold table classifies 10+ as VIOLATION ("Redesign — parameter object or split responsibilities"). Every other touched routine is ≤7 except `wireGraph` (8, WARNING). |
| cc-routine-and-class-design | Functional cohesion | PASS | UpsertMention/UpsertEdge/Decide/Expand each name one operation at their abstraction level; no logical/coincidental cohesion. |
| cc-routine-and-class-design | Inheritance/LSP | N/A | No inheritance introduced (interfaces + containment only: Backend, Judge, PostHook, TierSource). |

## Notes (non-blocking)
1. `wireGraph` (stages_graph.go:33) has 8 parameters — WARNING band (8-9) per cc-routine skill; an orchestration/construction routine, tolerable, but a `graphDeps` struct would clear it.
2. `anchorEntities` silently drops a `CandidateEntities` error (expand.go:184-186) with no log — fail-safe (fewer hits, never a leak) but a `DebugContext` log would aid observability, matching the expander's other logging.
3. The expand.go:148 entity-liveness "dangling edge" skip (live edge → soft-expired far entity) is handled and correct but has no direct unit test; the store-level expired-EDGE exclusion is tested (store_test.go:173). A one-test addition would close the gap.

## Issues (FAIL)
1. `Store.UpsertEdge` violates the loaded cc-routine-and-class-design parameter-count criterion (10 data parameters, VIOLATION tier).
   - File: internal/graph/store.go:155 (caller: stage.go:67)
   - Demonstrated by: parameter count = 10 (tenantID, teamID, scope, ownerAgentID, fromID, toID, predicate, statement, sourceID, validAt), 9 consecutive same-type strings — the transposition hazard the ≤7 threshold exists to prevent (Selby 1991: high coupling → ~7x errors). The skill explicitly forbids downgrading a demonstrated criterion violation to a Note.
   - Fix: pass a parameter object. The `Edge` struct (graph.go:94) or a small provenance/relation struct already models these fields; `UpsertMention` already uses this pattern (`Mention`). e.g. `UpsertEdge(ctx, e Edge)` (validAt/statement/sourceID set on the value), collapsing the signature to 2 parameters and eliminating the positional-string hazard.

**Verdict: FAIL — sole blocker: DW-code passes all 6 requirements, edge cases, correctness dimensions, and the ACL/security review with unit+integration+e2e evidence; the failure is a demonstrated VIOLATION-tier breach of the loaded cc-routine-and-class-design parameter-count criterion at store.go:155 (UpsertEdge, 10 params), which the protocol treats as an acceptance criterion on equal footing with the Done-When items. Fixable by a parameter object without touching behavior or tests' assertions.**

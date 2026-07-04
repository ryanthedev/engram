# Review: Phase 6 - Incremental Knowledge Graph (T4) — Security Sample 3

## Executed Results (Step 0)
- Build: `go build ./...` → Success (exit 0)
- Unit test suite: `go test ./...` → 290 passed in 32 packages (exit 0)
- Lint: `make lint` (`go vet ./...` + revive) → exit 0
- Integration (live dev cluster `engram-dev-os`, OpenSearch 3.1.0 @ :9200): `ENGRAM_OPENSEARCH_URL=http://localhost:9200 go test -tags=integration -run TestDW_6 ./internal/graph/` → 9 passed
  - `TestDW_6_2_Integration_TwoHopConnectTheDots` PASS (0.12s)
  - `TestDW_6_5_Integration_ExpansionLatencyP95` PASS (3.03s) — logged `p50=18.78ms p95=108.32ms p99=113.17ms hop2_hits_total=36`
- e2e (`make e2e`): NOT run. Requires building the compose stack (`podman compose up --build`); every Phase-6 DW item already has executed unit + live-integration evidence, so e2e is a redundant third layer here, not the sole evidence for any item. Scenarios `graph/connect-the-dots` (DW-6.2) and `graph/acl-blocked-edge` (DW-6.4) exist in `e2e/scenarios_graph.go`.

## Requirement Fulfillment

### DW-6.1
PREMISE:  single-episode ingest adds/updates entities + edges with zero recompute.
EVIDENCE: internal/graph/stage.go:45-80 (Process derives mentions+edge per fact); internal/graph/store.go:78-181 (UpsertMention / UpsertEdge touch only the resolved entity/edge)
TRACE:    one fact (service-a owns billing-db) → 2 UpsertMention + 1 UpsertEdge → CountEntities==2, one edge; an unrelated later ingest leaves entity A byte-identical (reflect.DeepEqual before==after).
VERDICT:  PASS — `TestStage_UpsertsEntitiesAndEdgeFromOneFact`, `TestDW_6_1_IngestTouchesOnlyItsOwnEntities` ran green.

### DW-6.2
PREMISE:  2-hop connect-the-dots e2e query returns the documented A→B→C path.
EVIDENCE: internal/graph/expand.go:94-169 (BFS to depth 2); internal/graph/opensearch_integration_test.go:160-217
TRACE:    seed hit fact-ab (subject A, object B) anchors A,B → hop1 surfaces B→C edge → object "C" present in a source=="graph" hit, though the seed never named C.
VERDICT:  PASS — unit `TestDW_6_2_TwoHopConnectTheDots` + live-cluster `TestDW_6_2_Integration_TwoHopConnectTheDots` ran green through the real MultiRetriever+Expander+acl.Filter.

### DW-6.3
PREMISE:  entity count stable (±0) across 10 re-ingests; dedup decisions logged.
EVIDENCE: internal/graph/store.go:98-133 (dedup Decide + create/merge), store.go:102-104 (unconditional InfoContext dedup log)
TRACE:    10× identical Mention → iteration 0 creates, iterations 1-9 merge into the same id, CountEntities==1 throughout; every call emits `graph dedup decision …` (observed in the integration run output).
VERDICT:  PASS — `TestDW_6_3_RepeatedIngestEntityCountStable`, `TestStage_ReplayIsIdempotent`, `TestUpsertEdge_RepeatedIdenticalFactStaysOneDoc` ran green; log line observed live.

### DW-6.4
PREMISE:  expansion honors ACL — unauthorized-edge hit absent (dirty test).
EVIDENCE: internal/graph/expand.go:233-253 (edgeHit stamps the EDGE's own provenance); internal/retrieval/opensearch.go:249-258 (post-hook additions re-run through filterAuthorized); cmd/engram-server/{main.go:108 WithACL, stages_graph.go RegisterPostHook}
TRACE:    A(a1)→B(a1)→C where the B→C edge and C are owned by a9 (caller u1/a1 reaches only {a1}). Expand adds edgeHit(A-B,a1) and edgeHit(B-C,a9); filterAuthorized authorizes each added hit by its own provenance → B-C(a9) dropped. Result = [fact-ab, A-B], no a9 field, "C" never surfaces; authorized seed retained.
VERDICT:  PASS — `TestDW_6_4_ExpansionACLBlocked` (real MultiRetriever + acl.Filter + registered post-hook) and live `TestDW_6_4`-family integration ran green. See adversarial analysis below.

### DW-6.5
PREMISE:  p95 search latency with expansion ≤ 250 ms.
EVIDENCE: internal/graph/opensearch_integration_test.go:224-318
TRACE:    30 A→B→C→D chains, 40 sequential Search calls through the real ACL-enforced MultiRetriever + depth-2 Expander → measured p95.
VERDICT:  PASS — my run: `p95=108.32ms` (43% of the 250 ms ceiling), `hop2_hits_total=36` (real 2-hop traversal exercised). Assertion `p95 > 250ms → Error` did not fire.

### DW-6.6
PREMISE:  decision-gate memo with measured hop-depth distribution; D8 confirmed/flipped.
EVIDENCE: internal/graph/DECISION_GATE.md (hop-1/hop-2 distribution table, verdict)
TRACE:    Memo cites the `TestDW_6_5_Integration_ExpansionLatencyP95` run (hop-1: 1/query, hop-2: 36 across 40 runs — matches my reproduced `hop2_hits_total=36`); concludes **D8 CONFIRMED** (stay on OpenSearch edges at ≤2 hops), with explicit flip conditions (>2-hop requirement, or losing half the headroom under Phase-7 load).
VERDICT:  PASS — memo present, numbers reproduced within run-to-run variance (memo p95 105.9ms vs my 108.3ms).

**All requirements met:** YES

## Test-DW Coverage
- [x] DW-6.1 — `TestStage_UpsertsEntitiesAndEdgeFromOneFact`, `TestDW_6_1_IngestTouchesOnlyItsOwnEntities`
- [x] DW-6.2 — `TestDW_6_2_TwoHopConnectTheDots` (unit) + `TestDW_6_2_Integration_TwoHopConnectTheDots` (live)
- [x] DW-6.3 — `TestDW_6_3_RepeatedIngestEntityCountStable`, `TestUpsertEdge_RepeatedIdenticalFactStaysOneDoc`; dedup log observed
- [x] DW-6.4 — `TestDW_6_4_ExpansionACLBlocked` (real retriever + ACL, ≥1 dirty test) + live integration variant
- [x] DW-6.5 — `TestDW_6_5_Integration_ExpansionLatencyP95` (executed, p95=108ms)
- [x] DW-6.6 — DECISION_GATE.md desk-checked against the reproduced hop-depth numbers
- Edge cases: same-name/different-entity (`TestHomonymDisambiguation_ThroughStore`), repeated-identical-ingest-flat (`TestDW_6_3`, `TestStage_ReplayIsIdempotent`), dangling-edge-skipped (`TestNeighbors_ExcludesSoftExpiredAndClosedEdges` + expand.go:148 endpoint-liveness skip), zero-hit-no-op (`TestExpand_ZeroHitsIsNoOp`, `TestExpand_DepthZeroIsNoOp`) — all covered, all ran green.
- Coverage matches the stated 100%-of-DW / ≥1-dirty-test level.

## Dead Code
None found. `go vet` and revive (unused-parameter/unreachable rules) pass clean; every exported symbol in expand.go/store.go/stage.go/graph.go has a caller or a compile-time interface assertion.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | MemBackend guarded by mutex (store.go:236-339); MultiRetriever fans tiers out on goroutines writing disjoint `results[i]` slots (opensearch.go:196-214) then joins — no shared write. Expander holds no cross-call state (maps are per-Expand locals). |
| Error Handling | PASS | Neighbors/GetEntity errors propagate with %w (expand.go:127,146); post-hook error aborts Search (opensearch.go:251-253); embed/ACL failures degrade-and-log or fail-closed. |
| Resources | PASS | tier HTTP responses `defer resp.Body.Close()` (opensearch.go:356); expansion bounded by maxFanout/maxAdded + visited-sets, so depth-≤2 traversal is O(fanout²), terminating even on a cyclic graph (visitedEdges/visitedEntities guard re-entry, expand.go:107-137). |
| Boundaries | PASS | zero-hit and depth-0 no-ops (expand.go:98); empty tenant seed → return unchanged (102-104); depth outside 0..2 → ErrDepthExceeded at both construction and call. |
| Security | PASS | See adversarial ACL analysis. No unauthorized-content leak demonstrable; every post-hook addition re-authorized by its own edge provenance before return. |

### Adversarial ACL analysis (the security-sensitive claim)
Question: does BFS traverse THROUGH edges the caller cannot see to reach deeper nodes, and if so is any unauthorized content returned?

- **BFS does traverse through unauthorized edges.** `Store.Neighbors` returns every LIVE edge regardless of ACL (store.go:325-338); `Expand` enqueues the far endpoint of any live edge to the next frontier (expand.go:140-158). There is no ACL gate inside the traversal — by design (expand.go:38-42).
- **But no unauthorized content is ever returned.** Each traversed edge is emitted as an `edgeHit` stamped with the EDGE's *own* provenance (`edge.TenantID/TeamID/Scope/OwnerAgentID`, expand.go:241-246), never the seed's or an endpoint's. After the post-hook runs, `MultiRetriever.Search` unconditionally re-runs `filterAuthorized(merged, enf)` over the whole list when `acl != nil && len(postHooks) > 0` (opensearch.go:256-258), dropping any hit whose provenance the Enforcer rejects (fail-closed on unreadable fields, recordFromHit opensearch.go:277-285).

Concrete trace (DW-6.4 fixture): A(a1)→B(a1) --secretly_owns(a9)--> C(a9), caller reaches {a1}. Expand adds edgeHit(A-B,a1) and edgeHit(B-C,a9); the a9 edge — the ONLY path to C, carrying "C" only inside its dropped object field — fails Authorize and is removed. Output = [fact-ab, A-B]; "C", the a9 owner, and the statement "B secretly_owns C" never reach the caller. `TestDW_6_4_ExpansionACLBlocked` asserts exactly this and passes.

Second trace (authorized edge behind an unauthorized edge): A --e1(a9)--> B --e2(a1)--> C, caller reaches {a1}. Expansion walks through unauthorized e1 to reach e2; e1(a9) is dropped, **e2(a1) is returned**. e2 is a1-owned content the caller is entitled to read under the per-record ACL model, so this returns only authorized content. What it reveals is that some authorized fact e2 is topologically near the query — not the content, provenance, or specific existence of the hidden e1.

**Classification: (ii) a benign relevance side-channel consistent with the per-record ACL model, shading into (iii) a weak inference channel** — expansion changes *which authorized* records surface, never *whether an unauthorized* one does. It is **not (i) a content leak**: no field of any record the caller cannot read ever appears in the result. The critical claim ("a hit whose content is reachable only through an unauthorized edge is never returned") holds for content. **No FAIL.**

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-routine-and-class-design | Parameter count — UpsertEdge is now a parameter object | PASS | `UpsertEdge(ctx, spec EdgeSpec)` = 2 params (store.go:156); EdgeSpec (graph.go:146-160) is a named DTO, the intended fix — not a 10-field positional signature. |
| cc-routine-and-class-design | Parameter count — other routines ≤7 | WARNING (non-blocking) | `wireGraph(...)` = 8 params (cmd/engram-server/stages_graph.go). 8 is WARNING per the 7±2 table, not the 10+ VIOLATION. It is a composition-root wiring function (borderline-exempt); collapsing httpClient/osURL/embedder/logger into a deps struct would clear it. Note, not FAIL. Every other routine (UpsertMention 2, NewExpander 4 incl. variadic, NewStore 4, edgeHit 4, Expand 3) is PASS. |
| cc-routine-and-class-design | Functional cohesion | PASS | UpsertMention (resolve mention→entity), Expand (bounded traversal), Stage.Process (derive graph deltas from facts) each name one operation at their abstraction level. |
| cc-routine-and-class-design | LSP / inheritance | N/A | No implementation inheritance; Backend/Deduper/Judge/PostHook/TierSource are interfaces used via containment + DI (store.go:24-46, expand.go:51). |
| aposd-designing-deep-modules | Deep module / narrow interface | PASS | Store exposes 5 intent methods hiding candidate-lookup + dedup-decide + upsert-or-merge choreography; Expander satisfies the single-method PostHook and delegates Apply→Expand. |
| aposd-designing-deep-modules | No silent failure | PASS | dedup embedding failure logs+degrades (store.go:207); ACL denial logs and fail-closes (opensearch.go:184); expander propagates Neighbors/GetEntity errors; no error swallowed without a return or a log. |
| aposd-designing-deep-modules | Information hiding at the ACL boundary | PASS (with Note) | edgeHit resolves endpoint entity NAMES regardless of entity-level ACL (expand.go:154,217), but those names are only ever attached to an edge hit that must itself survive filterAuthorized — so a name surfaces only as part of an authorized fact that inherently names its endpoints. Consistent with the fact-level ACL model. |

## Notes (non-blocking)
1. `wireGraph` has 8 parameters (WARNING, cc-routine-and-class-design). Consider a `graphDeps` struct for httpClient/osURL/embedder/logger. Not a blocker (threshold VIOLATION is 10+; this is a wiring routine).
2. Weak relevance/inference side-channel (analysis above): an authorized deeper edge can surface even when the only path to it runs through an unauthorized edge. This is consistent with the documented per-record ACL model and leaks no unauthorized content, but is worth a one-line acknowledgement in the D6 security notes if graph topology is ever considered sensitive on its own.
3. Post-hook additions are intentionally not re-truncated to K (opensearch.go:238 comment) — expansion hits augment the authorized top-k. All are ACL-filtered; a functional choice, not a security gap.

## Issues (if FAIL)
None.

**Verdict: PASS**

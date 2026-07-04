# Review: Phase 6 - Incremental Knowledge Graph (T4) + Connect-the-Dots Expansion (Sample 2, security-sensitive)

## Executed Results (Step 0)
- Build: `go build ./...` → Success (exit 0)
- Unit test suite: `make test` (`go test ./...`) → all packages ok (graph + retrieval: 68 tests pass)
- Typecheck: covered by `go build ./...` + `go vet ./...` (inside `make lint`) → clean
- Lint: `make lint` (`go vet` + `revive -set_exit_status`) → **exit 0**
- Graph integration: `go test -tags integration ./internal/graph/` → PASS; `TestDW_6_5_Integration_ExpansionLatencyP95` measured **p50=19.4ms p95=111.9ms p99=113.1ms hop2_hits_total=36** (ceiling 250ms)
- Retrieval integration: `go test -tags integration ./internal/retrieval/` → ok (2.46s)
- e2e: `make e2e` (full compose stack: OpenSearch 3.1 + embed + stub-llm + engramd) → `ok e2e 17.752s`, **E2E_EXIT=0**. Ran without compose port conflict (external mode). The harness (e2e_test.go:174-181) executes every registered scenario as a failing subtest, so a green package means `graph/connect-the-dots` (DW-6.2 e2e) and `graph/acl-blocked-edge` (DW-6.4 e2e) both passed.

## Requirement Fulfillment

### DW-6.1
PREMISE:  "single-episode ingest adds/updates entities + edges with zero recompute."
EVIDENCE: internal/graph/store.go:78-181 (UpsertMention/UpsertEdge, candidate-scoped merge, never touches unrelated entities); stage.go:45-80; store_test.go:24-50 (`TestDW_6_1_IngestTouchesOnlyItsOwnEntities`).
TRACE:    Upsert entity A → ingest unrelated entity Z (different name-key) → `reflect.DeepEqual(before,after)` on A holds; Z's ingest only fetched Z's candidates and wrote Z's doc. No batch pass over existing docs anywhere in the path.
VERDICT:  PASS

### DW-6.2
PREMISE:  "2-hop connect-the-dots e2e query returns the documented A→B→C path."
EVIDENCE: internal/graph/expand.go:94-169; expand_test.go:124-156 (`TestDW_6_2_TwoHopConnectTheDots`); opensearch_integration_test.go:160 (`TestDW_6_2_Integration_TwoHopConnectTheDots`, PASS); e2e/scenarios_graph.go:29-78 (`graph/connect-the-dots`, passed under `make e2e`).
TRACE:    Seed hit "A works_at B" (subject A, object B) → anchors {A,B} → hop1 Neighbors(B) yields B located_in C → C surfaced as a source="graph" hit though the query never named C. Verified at unit, integration, and full-stack e2e levels.
VERDICT:  PASS

### DW-6.3
PREMISE:  "entity count stable (±0) across 10 re-ingests; dedup decisions logged."
EVIDENCE: store_test.go:55-87 (`TestDW_6_3_RepeatedIngestEntityCountStable`, loops 10×, asserts count==1 and Merge on every replay); dedup logged at store.go:102-104 (`InfoContext "graph dedup decision"` with merge/match_id/reason/scores — observed in the live integration run output); opensearch_integration_test.go:132 (`TestDW_6_3_Integration_...`, PASS).
TRACE:    10 identical `UpsertMention` → iter0 creates, iters1-9 re-find same NameKey candidate, dedup Combined≥MergeThreshold → merge → CountEntities stays 1. Each decision emitted a structured log line (seen in integration stdout).
VERDICT:  PASS

### DW-6.4
PREMISE:  "expansion honors ACL — unauthorized-edge hit absent (dirty test)."
EVIDENCE: expand.go:233-253 (edgeHit stamps the EDGE's OWN tenant/team/scope/owner, never the seed's or an endpoint's); retrieval/opensearch.go:249-258 (post-hooks run, then `filterAuthorized(merged, enf)` re-authorizes ALL hits when acl!=nil && postHooks>0); expand_test.go:163-229 (`TestDW_6_4_ExpansionACLBlocked`, drives the REAL MultiRetriever + acl.Filter + registered post-hook); e2e/scenarios_graph.go:86-153 (`graph/acl-blocked-edge`, passed under `make e2e`); production wiring main.go:108 (retriever built `WithACL`) + stages_graph.go:67-71 (expander RegisterPostHook on that retriever).
TRACE:    Caller u1 reaches only a1. Seed "A works_at B" (a1). Expand walks A-B (a1, kept) and B→C via edge owned by a9 (unreachable). edgeHit(B-C) carries owner_agent_id=a9 → recordFromHit → enf.Authorize=false → dropped before return. Test asserts no a9-owned hit AND the authorized a1 seed survives (not an over-broad deny). PASS.
VERDICT:  PASS

### DW-6.5
PREMISE:  "p95 search latency with expansion ≤ 250 ms."
EVIDENCE: opensearch_integration_test.go:224-317; live run against pinned dev cluster (engram-dev-os, OpenSearch 3.1.0) on 2026-07-04.
TRACE:    30 four-node chains, 40 sequential real Searches through MultiRetriever (ACL on, depth-2 expander) → measured **p95=111.9ms** (44.7% of the 250ms ceiling), hop2_hits_total=36 (real 2-hop traversal exercised). Test asserts p95≤250ms and totalHop2>0.
VERDICT:  PASS

### DW-6.6
PREMISE:  "decision-gate memo with measured hop-depth distribution; D8 confirmed/flipped."
EVIDENCE: internal/graph/DECISION_GATE.md (measured table: p95 105.9ms, hop-1 = 1/query, hop-2 = 36 across 40 runs, 120 entities; **D8 CONFIRMED — stay on OpenSearch edges**; explicit "what would flip this" section). Backed by re-running `TestDW_6_5_Integration_ExpansionLatencyP95`, which reproduced hop2=36 and p95≈112ms — the memo's numbers are real and reproducible, not asserted.
TRACE:    Memo cites the named integration test as its source; independent re-run matches the hop distribution exactly and the p95 to the same band. Verdict D8 CONFIRMED with reproducible evidence.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] DW-6.1 — `TestDW_6_1_IngestTouchesOnlyItsOwnEntities` (ran)
- [x] DW-6.2 — unit `TestDW_6_2_TwoHopConnectTheDots`, integration `TestDW_6_2_Integration_...`, e2e `graph/connect-the-dots` (all ran)
- [x] DW-6.3 — `TestDW_6_3_RepeatedIngestEntityCountStable` + integration variant (ran); dedup logging observed live
- [x] DW-6.4 — dirty `TestDW_6_4_ExpansionACLBlocked` (real retriever+ACL+post-hook) + e2e `graph/acl-blocked-edge` (ran)
- [x] DW-6.5 — `TestDW_6_5_Integration_ExpansionLatencyP95` (ran, measured p95=111.9ms)
- [x] DW-6.6 — DECISION_GATE.md backed by the reproduced DW-6.5 integration run
- [x] ≥1 dirty test per code-touching area: DW-6.4 ACL leak test, `TestUpsertMention_RequiresTenantAndName`, `TestNeighbors_ExcludesSoftExpiredAndClosedEdges`, depth-boundary rejection tests, homonym disambiguation.
- Coverage matches the stated 100%-of-DW-items level.

## Dead Code
None found. `go vet` (in `make lint`) passed — no unreachable code, no unused locals. Expansion/dedup use slog, not debug prints. (staticcheck could not build under the installed Go toolchain — tool incompatibility, not a code finding; go vet + revive both clean.)

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | MemBackend guards maps with a mutex (store.go:236-339); retriever fans out tiers into a pre-sized `results` slice with per-index writes + WaitGroup (opensearch.go:196-214) — no shared-slice race. Expander is stateless per call (local visited maps). |
| Error Handling | PASS | Neighbors/GetEntity errors abort the whole expansion (expand.go:127,146) rather than returning partial; ACL compile error is fail-closed (opensearch.go:181-187, returns zero results); post-hook error aborts Search (opensearch.go:250-253). |
| Resources | PASS | No leaked handles; response bodies closed (opensearch.go:356); blast radius bounded by maxFanout(5)/maxAdded(20) and MaxDepth(2), so traversal is O(fanout²) constant regardless of graph size. |
| Boundaries | PASS | Zero-hit and depth-0 no-ops (expand.go:98, tested); empty tenant seed → no-op (line 102-105); dangling/soft-expired endpoint skipped (line 148, tested); visited sets make traversal acyclic; depth hard-bounded at construction AND call time. |
| Security (ACL) | PASS | See adversarial analysis below — every returned hit is re-authorized on its OWN edge provenance; the edgeHit stamps edge-owned scope fields, never inherited. No construction disclosed content the caller isn't authorized to read. |

### Adversarial ACL analysis (the critical claim)
Attempted to construct a leaking traversal per the dispatch's five probes:
- (a) Expander adds hits WITHOUT its own auth logic and relies on the retriever's re-authorization — verified wiring: `RegisterPostHook` (opensearch.go:157) + Search re-runs `filterAuthorized(merged, enf)` on the FULL post-hook output when `acl!=nil && len(postHooks)>0` (opensearch.go:256-258), AFTER additions. edgeHit provenance is the edge's own (expand.go:241-250). Result: an unauthorized edge-hit is dropped by its own provenance. **No leak.**
- (b) Depth-2 through an unseen edge: the BFS frontier does continue through an unauthorized edge at the graph level (Expander has no ACL), so a deeper AUTHORIZED edge can be surfaced. But that deeper hit is authorized on its own provenance — content the caller is entitled to read — and the unauthorized connecting edge itself is always dropped. This is consistent with the system's per-record ACL model ("every returned hit authorized on its own provenance"); no protected content is disclosed. Recorded as a non-blocking note (relevance side-channel at most), not a leak.
- (c) Dangling edge (soft-expired entity): skipped at expand.go:148 (`!ok || !otherEntity.Live()`) and at the backend Neighbors layer (only Live edges) — tested.
- (d) Depth hard-bounded ≤2 (MaxDepth const, enforced in NewExpander AND Expand), acyclic via visitedEntities/visitedEdges — tested (`TestNewExpander_RejectsDepthAbove2`, `TestExpand_RejectsDepthAbove2AtCallTime`).
- (e) Zero-hit expansion no-ops (expand.go:98) — tested.
Cross-tenant: seedTenantID scopes all Neighbors/GetEntity lookups, and filterAuthorized independently checks tenant — double-guarded. The DW-6.4 dirty test and the e2e acl-blocked-edge scenario both exercise the realistic leak shape (unreachable-owner connecting edge) and confirm the target entity never surfaces.

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-routine-and-class-design | Parameter count ≤7 (called-out: UpsertEdge previously 10) | PASS | `UpsertEdge(ctx, EdgeSpec)` = 2 params; EdgeSpec (graph.go:146-160) is the parameter object. Max param count in the graph package is 5 (osJSON/osDo HTTP helpers). No 10+ (VIOLATION) function. |
| cc-routine-and-class-design | Functional cohesion | PASS | Deduper.Decide = "decide same-entity" (one operation, dedup.go:64-69); UpsertMention/UpsertEdge named at their abstraction level; Expand is one bounded-traversal operation. |
| cc-routine-and-class-design | Containment over inheritance / LSP | PASS | No inheritance; Backend/Judge/PostHook/TierSource are interfaces (ports) with containment (Store contains backend+dedup+embedder). No empty overrides, no protected base data. |
| aposd-designing-deep-modules | Deep module / information hiding | PASS | Store hides candidate-lookup + dedup-decide + upsert-or-merge behind 2 intent methods; Expander hides BFS/fanout/decay behind Apply/Expand; edge-provenance ACL exactness is encapsulated (callers never re-derive scope). No information leakage across the graph/retrieval boundary. |
| aposd-designing-deep-modules | Silent-failure guard | PASS | Failures surfaced: dedup logs every decision; embedding degradation logged ("degraded to lexical-only"); ACL denial logged and fail-closed; depth misconfig returns ErrDepthExceeded loudly rather than clamping. |

## Notes (non-blocking)
1. `wireGraph` (cmd/engram-server/stages_graph.go:33-34) has 8 parameters — a WARNING band per cc-routine-and-class-design (8-9 = justify; VIOLATION is 10+), not a defect. It is a one-shot wiring/orchestration function; acceptable but could take a config struct if it grows.
2. ACL relevance side-channel (probe b above): graph BFS continues through edges the caller can't read, so an authorized deeper fact can be surfaced that the caller would only have reached via a hidden edge. No unauthorized *content* is disclosed (every returned hit is independently authorized), so this is not a leak of protected data — but if "which of my authorized facts are graph-adjacent to a hidden relationship" is itself considered sensitive in a future threat model, the frontier could be pruned to authorized edges before continuing. Out of scope for the stated per-record ACL guarantee.
3. e2e scenario PASS is inferred from the green package + the harness's run-every-scenario-as-a-failing-subtest loop (e2e_test.go:174-181), since `make e2e` runs without `-v`. Direct subtest PASS lines were not captured (would require re-booting the compose stack with `-v`).

**Verdict: PASS**

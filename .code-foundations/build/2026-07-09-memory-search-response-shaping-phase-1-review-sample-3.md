# Review: Phase 1 - Shape hits at retrieval boundary (sample 3)

## Executed Results (Step 0)
- Test suite (package): `go test ./internal/retrieval/...` → 38 passed, 0 failed
- Test suite (full): `go test ./...` → 607 passed in 41 packages, 0 failed
- Race check: `go test -race ./internal/retrieval/...` → 38 passed, no races
- Typecheck/Lint: `go vet ./...` → no issues (exit 0)

## Requirement Fulfillment

### DW-1.1
PREMISE:  `buildQuery` sets `_source` to exclude `text_embedding` and `fact_embedding`; a query-shape test asserts the exclusion is present.
EVIDENCE: internal/retrieval/opensearch.go:528 (`query["_source"] = map[string]any{"excludes": []string{"text_embedding", "fact_embedding"}}`, set after the mode switch so it applies to bm25-only, knn-only, AND hybrid branches); internal/retrieval/shape_test.go:16-43 (TestDW_1_1_BuildQueryExcludesEmbeddings).
TRACE:    Search("x") → both tier requests captured by fake server → each JSON body's `_source.excludes` == ["text_embedding","fact_embedding"] → test asserts exact equality per request; ran and passed.
VERDICT:  PASS

### DW-1.2
PREMISE:  `projectFields` reduces each source (episodic/semantic/graph/unknown) to its allowlist; no `*_embedding`/`tenant_id`/`team_id`/`scope`/`owner_agent_id` survives (table test over all four shapes, incl. the `edgeHit` graph shape).
EVIDENCE: internal/retrieval/opensearch.go:306-337 (allowedFields map, defaultAllowed, projectFields); internal/retrieval/project_test.go:30-98 (TestDW_1_2_ProjectFieldsAllowlists — 4 subtests, one per shape) + project_test.go:15-24 (forbiddenFields assertion covering all six banned keys); end-to-end via shape_test.go:85-132 (TestDW_1_2_SearchReturnsProjectedFields).
TRACE:    graph case input is the literal edgeHit shape (verified field-for-field against internal/graph/expand.go:233-253: tenant_id/team_id/scope/owner_agent_id/subject/predicate/object/statement/hop) → projectFields("graph", in) → exactly {statement, subject, predicate, object, hop}; episodic/semantic/unknown analogous; DeepEqual + forbidden-key assertions passed for all four.
VERDICT:  PASS

### DW-1.3
PREMISE:  per-tier query `size` is clamped to `[1, MaxK]`; below/at/above-bound cases covered.
EVIDENCE: internal/retrieval/opensearch.go:57-66 (clampK: k<=0→DefaultK=10, k>MaxK→MaxK=100, else pass-through — result always in [1,100]); applied at opensearch.go:204 (MultiRetriever.Search) and :399 (tierRetriever.search). Tests: project_test.go:130-146 (TestDW_1_3_ClampK unit: -1, 0, 1, 10, 99, 100, 101, 100000) and shape_test.go:48-79 (TestDW_1_3_QuerySizeClampedInRequestBody: `size` in the actual request body for k=-1, 0, 42, MaxK, MaxK+150).
TRACE:    Search(K=250) → clampK(250)=100 → request body `size`=100 asserted on both tier requests; Search(K=0) → size=10; Search(K=100) → size=100. All subtests passed.
VERDICT:  PASS

### DW-1.4
PREMISE:  every returned hit has a populated `Score`, including graph hop hits.
EVIDENCE: internal/retrieval/opensearch.go:294-299 (post-sort loop: `if merged[i].Score == 0 { merged[i].Score = fallbackScore }`); graph hits carry `1/(hop+1)` from construction (internal/graph/expand.go:239). Tests: project_test.go:151-178 (TestDW_1_4_HopScoreKeptAndZeroScoreGetsFallback) and shape_test.go:85-132 (episodic hit deliberately served with no `_score`).
TRACE:    tier hit {Score:0} + graph hop hit {hop:3, Score:0.25, no _score} through Search → unscored hit returns Score=1e-9 (non-zero), edge-1 returns exactly 0.25 = 1/(3+1), scored hit keeps 0.9 (fallback applied post-sort, never reorders). Passed.
VERDICT:  PASS

### DW-1.5
PREMISE:  ACL is unaffected — an ACL-enabled `MultiRetriever.Search` returns the SAME hits before and after projection (authorization runs on un-projected fields); existing `fields_json` consumers still pass (`text`/`statement` retained).
EVIDENCE: internal/retrieval/opensearch.go:265-299 — both `filterAuthorized` passes (:266, :285) and the graph post-hook (:277-283) run BEFORE projection (:294-299); the shaping comment at :288-293 documents why. Test: project_test.go:187-244 (TestDW_1_5_ACLUnaffectedByProjection) with a REAL `acl.NewFilter`, a registered tier source, and a graph-shaped post-hook. `fields_json` consumer: internal/server/server_test.go TestSearchMapsQueryFilterAndHits (asserts `statement` survives into fields_json) passed inside the full 607-test run.
TRACE:    authorized private hit (owner a1) + team hit (teamX) + two unauthorized hits → Search under identity u1/a1 → exactly {base-auth, edge-auth} returned (identical to the pre-projection ACL contract; if projection ran first, recordFromHit at opensearch.go:354-362 would read empty provenance and fail-closed deny everything — the test would fail), AND returned Fields contain none of the six provenance/embedding keys while `statement`/`hop` are retained. Passed.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] DW-1.1 → TestDW_1_1_BuildQueryExcludesEmbeddings (ran, passed)
- [x] DW-1.2 → TestDW_1_2_ProjectFieldsAllowlists, TestDW_1_2_ProjectFieldsToleratesNilAndOddValues, TestDW_1_2_SearchReturnsProjectedFields, TestDW_1_2_SearchToleratesMissingSource
- [x] DW-1.3 → TestDW_1_3_ClampK, TestDW_1_3_QuerySizeClampedInRequestBody
- [x] DW-1.4 → TestDW_1_4_HopScoreKeptAndZeroScoreGetsFallback
- [x] DW-1.5 → TestDW_1_5_ACLUnaffectedByProjection (+ server_test.go fields_json mapping test in the full run)
- [x] Coverage matches the stated 100% level: every DW item has a DW-named automated test that executed in Step 0.

## Edge Cases (prompt-listed — verdict standing)
| Edge case | Handled | Evidence |
|---|---|---|
| nil/absent `Fields` | YES | projectFields nil→nil (opensearch.go:323-325); TestDW_1_2_ProjectFieldsToleratesNilAndOddValues + TestDW_1_2_SearchToleratesMissingSource (hit with no `_source` returned without panic) |
| Unknown `source` safe default | YES | defaultAllowed (opensearch.go:315) is exactly {text, statement, subject, predicate, object}; "unknown source falls back to the safe default" subtest ("experience" source keeps only `statement`, drops all provenance/extras) |
| `k`≤0 and `k`>MaxK | YES | clampK cases -1/0→DefaultK, 100000→MaxK; asserted both at unit level and in the wire `size` |
| Graph hop hits (score `1/(hop+1)`, no `_score`) projected and kept | YES | edgeHit shape verified against expand.go:233-253; TestDW_1_4 (score 0.25 for hop 3) and TestDW_1_5 (edge-auth kept, hop/statement retained, provenance stripped) |

## Dead Code
None found. Diff-scoped scan of internal/retrieval/opensearch.go: no unused imports (compiles + vet clean), no unreachable code, no debug statements, no commented-out blocks. clampK, fallbackScore, projectFields, allowedFields, defaultAllowed all reachable from Search.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | Tier goroutines write to distinct pre-sized `results` indices, read only after wg.Wait (opensearch.go:224-242); `go test -race ./internal/retrieval/...` clean (38 passed) |
| Error Handling | PASS | HTTP/status/decode errors wrapped per tier; all-tiers-fail → error (opensearch.go:253-255); embed timeout degrades to BM25 (tested); adversarial probe: partial-failure path exercised by TestMultiRetrieverOneTierFailingStillReturnsOtherTiersHits — no demonstrated defect (doc-only mismatch, see Notes) |
| Resources | PASS | `defer resp.Body.Close()` (opensearch.go:430); embed context `defer cancel()` (:453); no leaked handles |
| Boundaries | PASS | clampK bounds external K; parseHits tolerates malformed hit entries (`continue`, :540-542) and missing hits/hits.hits; nil-Fields hit traced through projection and filterAuthorized (nil-map reads are safe in Go) without panic — tested |
| Security | PASS | K is external input, clamped at entry; embeddings never fetched (`_source` excludes) and provenance stripped at the retrieval boundary AFTER all authorization; ACL fail-closed on compile error and on unreadable provenance (recordFromHit empty-string deny) — TestDW_1_5 + TestTierHitsAuthorizedBeforeTruncation |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-routine-and-class-design | Parameter count ≤7 | PASS | buildQuery has exactly 7 params (opensearch.go:499) — at the limit, PASS per threshold table (6-7 = pass w/ minor concern; noted below); all other new routines ≤2 |
| cc-routine-and-class-design | Functional cohesion | PASS | clampK, projectFields, buildQuery each perform one operation; Search is an orchestrator (temporal-acceptable: delegates to tier.search/hooks/projectFields) |
| cc-routine-and-class-design | LSP / containment | N/A | No inheritance introduced; MultiRetriever contains tierRetrievers (containment, correct) |
| cc-defensive-programming | External input validated at entry | PASS | K crosses the MCP process boundary → clampK at both Search entry points (opensearch.go:204, :399); traced -1/0/100000 through the wire body |
| cc-defensive-programming | No empty catch / swallowed errors | PASS | No demonstrated swallow that changes results; parseHits skips malformed entries by design (robustness contract); adversarial trace of a non-map hit entry → skipped, remaining hits returned correctly |
| cc-defensive-programming | Barricade / fail-closed ordering | PASS | Projection (data leaving the barricade) runs strictly after both filterAuthorized passes; stripping earlier would fail-closed-deny — TestDW_1_5 is the ordering guard and passed |
| cc-defensive-programming | Assertions vs error handling | PASS | No assertions used for runtime conditions; anticipated failures (embed timeout, tier error, malformed response) all use error handling/degradation |

## Notes (non-blocking)
- Search's docstring claims "partial failures are logged" (opensearch.go:196) but the errs collected at :246-251 are never logged when at least one tier succeeds. Pre-existing behavior (the phase diff only appended to that sentence); doc-only inaccuracy, no wrong result demonstrable.
- buildQuery sits exactly at the 7-parameter CC threshold; a params struct would help if Phase 2+ adds another knob.
- `body, _ = json.Marshal(query)` (opensearch.go:529) ignores the marshal error; unreachable failure for this map's value types, and pre-existing pattern.
- clampK maps k≤0 to DefaultK (10) rather than 1 — still inside the required [1, MaxK] range and matches the "k≤0" edge-case spec; noting only that "clamp" here means normalize-with-default at the low end.

## Issues (if FAIL)
None.

**Verdict: PASS**

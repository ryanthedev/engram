# Review: Phase 1 - Shape hits at retrieval boundary (sample 1)

## Executed Results (Step 0)
- Test suite (package): `go test ./internal/retrieval/...` → 38 passed, 0 failed
- Test suite (full): `go test ./...` → 607 passed in 41 packages, 0 failed
- Typecheck/vet: `go vet ./...` → no issues found
- DW-named tests (verbose, isolated run): all 9 `TestDW_1_*` tests PASS individually
- Coverage: `go test ./internal/retrieval/... -coverprofile` → 93.3% package total; every phase-added function 100% (clampK, projectFields, buildQuery, Search shaping loop). The only uncovered unit-test blocks (`WithACL` option body at opensearch.go:114–118, `tierRetriever.Search` wrapper at :387) are pre-existing code untouched by this phase (verified against `git diff main`, which contains neither).

## Requirement Fulfillment

### DW-1.1
PREMISE:  `buildQuery` sets `_source` to exclude `text_embedding` and `fact_embedding`; a query-shape test asserts the exclusion is present.
EVIDENCE: internal/retrieval/opensearch.go:528 (`query["_source"] = map[string]any{"excludes": []string{"text_embedding", "fact_embedding"}}` — placed after the mode switch, so it applies to all three modes); test internal/retrieval/shape_test.go:16–43 (`TestDW_1_1_BuildQueryExcludesEmbeddings`).
TRACE:    Search("x") → both tier requests captured by fake server → each body's `_source.excludes` deep-equals `["text_embedding","fact_embedding"]` → test PASS.
VERDICT:  PASS

### DW-1.2
PREMISE:  `projectFields` reduces each source (episodic/semantic/graph/unknown) to its allowlist; no `*_embedding`/`tenant_id`/`team_id`/`scope`/`owner_agent_id` survives (table test over all four shapes, incl. the `edgeHit` graph shape).
EVIDENCE: internal/retrieval/opensearch.go:306–337 (`allowedFields`, `defaultAllowed`, `projectFields`); table test internal/retrieval/project_test.go:30–98 (`TestDW_1_2_ProjectFieldsAllowlists`, 4 subtests: episodic/semantic/graph/unknown, each asserting exact output map AND no forbidden key); the graph case (project_test.go:67–78) replicates the `edgeHit` field set from internal/graph/expand.go:241–251 field-for-field — I diffed the two shapes: identical keys. End-to-end: shape_test.go:85–132 (`TestDW_1_2_SearchReturnsProjectedFields`) shows raw `_source` documents carrying embeddings + ACL provenance come out of `Search` reduced to the allowlist.
TRACE:    e.g. graph input {tenant_id,team_id,scope,owner_agent_id,subject,predicate,object,statement,hop} → projectFields("graph", …) → exactly {subject,predicate,object,statement,hop}, new map, input unmutated (project_test.go:120–125 asserts non-mutation) → tests PASS.
VERDICT:  PASS

### DW-1.3
PREMISE:  per-tier query `size` is clamped to `[1, MaxK]`; below/at/above-bound cases covered.
EVIDENCE: internal/retrieval/opensearch.go:57–66 (`clampK`: k≤0→DefaultK(10), k>MaxK(100)→MaxK, else pass-through), applied at both entry points — MultiRetriever.Search (:204) and tierRetriever.search (:399) — and used as the request `size` in buildQuery (:518/:519/:522). Unit test project_test.go:130–146 (`TestDW_1_3_ClampK`: −1, 0, 1, DefaultK, MaxK−1, MaxK, MaxK+1, 100000); wire test shape_test.go:48–79 (`TestDW_1_3_QuerySizeClampedInRequestBody`) asserts the actual `size` in captured request bodies for −1, 0, 42, MaxK, MaxK+150.
TRACE:    Search with K=250 → clampK→100 → request body `size:100` asserted at the fake server; K=−1 → DefaultK=10 in body → tests PASS.
VERDICT:  PASS

### DW-1.4
PREMISE:  every returned hit has a populated `Score`, including graph hop hits.
EVIDENCE: internal/retrieval/opensearch.go:294–299 (post-sort, post-hook loop assigns `fallbackScore` (1e-9) to any zero Score; graph hop scores 1/(hop+1) from expand.go:239 pass through untouched). Tests: project_test.go:151–178 (`TestDW_1_4_HopScoreKeptAndZeroScoreGetsFallback`: hop-3 graph hit keeps exactly 0.25, unscored tier hit gets non-zero, scored hit keeps 0.9) and shape_test.go:85–132 (a wire hit with no `_score` returns non-zero Score).
TRACE:    hits [scored=0.9, unscored=0, graph hop=3 score=0.25] → Search → unscored→1e-9, scored→0.9, graph→0.25; no zero Score in output → tests PASS.
VERDICT:  PASS

### DW-1.5
PREMISE:  ACL is unaffected — an ACL-enabled `MultiRetriever.Search` returns the SAME hits before and after projection (authorization runs on un-projected fields); existing `fields_json` consumers still pass (`text`/`statement` retained).
EVIDENCE: internal/retrieval/opensearch.go:288–299 — projection runs LAST, after both `filterAuthorized` passes (:265–267, :284–286) and all post-hooks (:277–283). Test project_test.go:187–244 (`TestDW_1_5_ACLUnaffectedByProjection`) with a real `acl.NewFilter`: authorized {base-auth, edge-auth} returned, unauthorized {base-unauth, edge-unauth} dropped, returned Fields carry no provenance while content (`statement`, `hop`) is retained. The guard is genuine: `acl.Enforcer.Authorize` (internal/acl/filter.go:132–146) denies any record whose `TenantID` mismatches — if projection ran before authorization, `recordFromHit` would read blank provenance and deny every hit, so this test failing-if-reordered demonstrates the ordering. Consumers: allowlists retain `text` (episodic) and `statement` (semantic/graph/default); `go test ./internal/server/... ./internal/mcp/...` → 34 passed, including `TestSearchMapsQueryFilterAndHits` (server_test.go:152–188) asserting `fields_json` round-trips `statement`.
TRACE:    ACL-enabled Search with mixed authorized/unauthorized tier + graph hits → filterAuthorized (un-projected) → post-hook → filterAuthorized → projectFields → exactly the pre-projection authorized set, provenance-free fields → tests PASS.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have corresponding tests that ran in Step 0, named by DW-ID:
  - DW-1.1 → TestDW_1_1_BuildQueryExcludesEmbeddings
  - DW-1.2 → TestDW_1_2_ProjectFieldsAllowlists, TestDW_1_2_SearchReturnsProjectedFields, TestDW_1_2_ProjectFieldsToleratesNilAndOddValues, TestDW_1_2_SearchToleratesMissingSource
  - DW-1.3 → TestDW_1_3_ClampK, TestDW_1_3_QuerySizeClampedInRequestBody
  - DW-1.4 → TestDW_1_4_HopScoreKeptAndZeroScoreGetsFallback
  - DW-1.5 → TestDW_1_5_ACLUnaffectedByProjection (+ server TestSearchMapsQueryFilterAndHits for fields_json)
- [x] Coverage matches the stated 100% level for phase-added code: clampK 100%, projectFields 100%, buildQuery 100%, Search shaping loop covered (the two uncovered blocks in the unit profile are pre-existing, untouched by the phase diff).

### Edge cases (prompt-listed)
| Edge case | Handled at | Execution evidence |
|---|---|---|
| nil/absent `Fields` | opensearch.go:323–325 (nil in → nil out) | TestDW_1_2_ProjectFieldsToleratesNilAndOddValues; TestDW_1_2_SearchToleratesMissingSource (hit with no `_source` flows through and is returned) |
| unknown `source` safe default | opensearch.go:315, 326–329 (`defaultAllowed` = text/statement/subject/predicate/object) | "unknown source falls back to the safe default" subtest ("experience" source: keeps `statement`, drops everything incl. provenance) |
| k≤0 and k>MaxK | opensearch.go:57–66, applied :204 and :399 | TestDW_1_3_ClampK (−1, 0, 100000) + TestDW_1_3_QuerySizeClampedInRequestBody (wire-level) |
| graph hop hits (score 1/(hop+1), no `_score`) projected and kept | opensearch.go:294–299 after post-hooks; graph allowlist :309 | TestDW_1_4 (hop=3 → 0.25 retained) + TestDW_1_5 (edge-auth kept with hop=1, statement intact) |

## Dead Code
None found in the phase diff. No unused imports (vet clean), no unreachable code, no debug statements, no commented-out blocks.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | Shaping loop (:294–299) runs after `wg.Wait()` (:242) in the single caller goroutine; per-goroutine writes go to distinct slice indices (unchanged pre-existing pattern). No defect demonstrable. |
| Error Handling | PASS | No new error paths added; degraded-embed, HTTP, and decode errors unchanged. `parseHits` on a hit with missing `_score` yields 0 → fallback populates it (traced; wire-tested). Pre-existing swallowed `json.Marshal` error noted below (not phase-introduced). |
| Resources | N/A | Phase adds no handles, connections, locks, or caches — pure in-memory map projection and an int clamp. |
| Boundaries | PASS | nil Fields → nil (tested); empty merged → loop no-op; k at −1/0/1/MaxK/MaxK+1 all traced and wire-tested; nil-valued and wrong-typed field values pass projection without panic (dirty test). |
| Security | PASS | Adversarial ordering case traced: projection-before-authorization would blank `tenant_id` → `Authorize` (filter.go:133) denies all → TestDW_1_5 would fail; it passes, so authorization demonstrably reads un-projected fields. Provenance/embeddings verified absent from every returned hit in three tests. K is external input and clamped at both entry points. |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | External input validated at entry | PASS | `q.K` (MCP caller, process boundary) clamped at MultiRetriever.Search:204 AND tierRetriever.search:399 — both public entry points; wire-tested |
| cc-defensive-programming | No empty catch / swallowed errors introduced | PASS | Phase diff adds no ignored errors; `body, _ = json.Marshal` (:529) is pre-existing context, unchanged (see Notes) |
| cc-defensive-programming | Barricade / fail-closed ordering | PASS | Projection (display barricade) sits strictly after both fail-closed authorization passes; demonstrated by the DW-1.5 ordering guard |
| cc-defensive-programming | No executable code in assertions / assertions-for-bugs-only | N/A | Go; no assert mechanism used |
| cc-routine-and-class-design | Functional cohesion of new routines | PASS | `clampK` (one operation: normalize k), `projectFields` (one operation: reduce to allowlist) — both functionally cohesive, no "and" naming |
| cc-routine-and-class-design | Parameter count ≤ 7 | PASS | `buildQuery` sits at exactly 7 (mode, textField, vectorField, text, vec, k, filters) — within the PASS band (minor concern, pre-existing signature; phase added no parameter); `projectFields` 2, `clampK` 1 |
| cc-routine-and-class-design | Inheritance/LSP | N/A | No inheritance introduced; `MultiRetriever`/`tierRetriever` satisfy the `Retriever` interface by composition (unchanged) |

## Notes (non-blocking)
- Pre-existing: `body, _ = json.Marshal(query)` (opensearch.go:529) discards a marshal error. Undemonstrated as a defect (a NaN/Inf embedding component would be needed; failure mode is a nil body → OpenSearch 400 → surfaced error, not silent corruption), and the line is untouched by this phase. Worth a wrapped error in a future pass.
- `clampK` maps k≤0 to DefaultK (10), not to the lower bound 1 — consistent with the documented "unset becomes DefaultK" contract and still inside [1, MaxK]; the DW-1.3 wording is satisfied.
- `buildQuery` at 7 parameters is at the Code Complete ceiling; if Phase 2+ needs another knob, fold into a params struct.
- The double clamp (Search clamps `q.K`, tier clamps again) is intentional defense for the direct `tierRetriever.Search` path (eval harness), not dead code.

## Issues
None.

**Verdict: PASS**

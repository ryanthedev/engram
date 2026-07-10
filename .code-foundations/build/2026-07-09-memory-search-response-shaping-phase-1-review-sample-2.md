# Review: Phase 1 - Shape hits at retrieval boundary (sample 2)

## Executed Results (Step 0)
- Test suite (package): `go test ./internal/retrieval/...` → 38 passed, 0 failed
- Test suite (full): `go test ./...` → 607 passed in 41 packages, 0 failed
- DW-tagged tests: `go test ./internal/retrieval/ -run 'TestDW_1' -v` → 9/9 PASS
  (TestDW_1_1_BuildQueryExcludesEmbeddings, TestDW_1_2_ProjectFieldsAllowlists,
  TestDW_1_2_ProjectFieldsToleratesNilAndOddValues, TestDW_1_2_SearchReturnsProjectedFields,
  TestDW_1_2_SearchToleratesMissingSource, TestDW_1_3_ClampK,
  TestDW_1_3_QuerySizeClampedInRequestBody, TestDW_1_4_HopScoreKeptAndZeroScoreGetsFallback,
  TestDW_1_5_ACLUnaffectedByProjection)
- Typecheck/vet: `go vet ./...` → no issues; `go vet -tags integration ./internal/retrieval/` → no issues (build-tagged integration tests compile)
- Consumer packages: `go test ./internal/server/ ./internal/mcp/` → ok, ok

## Requirement Fulfillment

### DW-1.1
PREMISE:  `buildQuery` sets `_source` to exclude `text_embedding` and `fact_embedding`; a query-shape test asserts the exclusion is present.
EVIDENCE: internal/retrieval/opensearch.go:528 (`query["_source"] = map[string]any{"excludes": []string{"text_embedding", "fact_embedding"}}`); test internal/retrieval/shape_test.go:16-43
TRACE:    Search("x") → both tier bodies captured by fake server → each carries `_source.excludes == ["text_embedding","fact_embedding"]`. The assignment sits after the mode switch (opensearch.go:516-528), so BM25-only, kNN-only, hybrid, and degraded-fallback bodies all carry it.
VERDICT:  PASS (TestDW_1_1_BuildQueryExcludesEmbeddings ran and passed)

### DW-1.2
PREMISE:  `projectFields` reduces each source (episodic/semantic/graph/unknown) to its allowlist; no `*_embedding`/`tenant_id`/`team_id`/`scope`/`owner_agent_id` survives (table test over all four shapes, incl. the `edgeHit` graph shape).
EVIDENCE: internal/retrieval/opensearch.go:306-337 (allowedFields, defaultAllowed, projectFields); tests internal/retrieval/project_test.go:30-98 (table over episodic/semantic/graph/unknown with `assertNoForbidden` over all six forbidden keys) and shape_test.go:85-132 (end-to-end via Search)
TRACE:    projectFields("graph", edgeHit-shaped map with tenant_id/team_id/scope/owner_agent_id + statement/spo/hop) → new map containing only {statement, subject, predicate, object, hop}; unknown source "experience" → defaultAllowed keeps only {statement}. Verified against the real edgeHit shape at internal/graph/expand.go:233-253 — its field set is exactly the graph allowlist plus the four provenance keys, so the test replica is faithful.
VERDICT:  PASS (TestDW_1_2_ProjectFieldsAllowlists, TestDW_1_2_SearchReturnsProjectedFields ran and passed)

### DW-1.3
PREMISE:  per-tier query `size` is clamped to `[1, MaxK]`; below/at/above-bound cases covered.
EVIDENCE: internal/retrieval/opensearch.go:57-66 (clampK), :204 (Search) and :399 (tier search) both call it; tests project_test.go:130-146 (unit: -1,0,1,DefaultK,MaxK-1,MaxK,MaxK+1,100000) and shape_test.go:48-79 (request-body `size` for -1, 0, 42, MaxK, MaxK+150)
TRACE:    Search{K:MaxK+150} → clampK → 100 → captured OpenSearch body has `"size":100`; K:-1 → DefaultK(10); K:42 → 42. The same clamped k feeds the knn clause's `k` (opensearch.go:508).
VERDICT:  PASS (TestDW_1_3_ClampK, TestDW_1_3_QuerySizeClampedInRequestBody ran and passed)

### DW-1.4
PREMISE:  every returned hit has a populated `Score`, including graph hop hits.
EVIDENCE: internal/retrieval/opensearch.go:52 (fallbackScore=1e-9), :294-299 (post-sort shaping loop assigns fallback when Score==0); graph hop score at internal/graph/expand.go:239 (`1.0/float64(hop+1)`); tests project_test.go:151-178 and shape_test.go:85-132 (episodic hit served with no `_score`)
TRACE:    Hit parsed from a response with no `_score` → parseHits gives Score 0 → shaping loop sets 1e-9 (non-zero); graph hop hit (hop=3) enters via post-hook with Score 0.25 ≠ 0 → fallback branch not taken, 0.25 preserved through projection. Both asserted by the passing tests.
VERDICT:  PASS (TestDW_1_4_HopScoreKeptAndZeroScoreGetsFallback, TestDW_1_2_SearchReturnsProjectedFields ran and passed)

### DW-1.5
PREMISE:  ACL is unaffected — an ACL-enabled `MultiRetriever.Search` returns the SAME hits before and after projection (authorization runs on un-projected fields); existing `fields_json` consumers still pass (`text`/`statement` retained).
EVIDENCE: internal/retrieval/opensearch.go:265-299 — both `filterAuthorized` passes (:266, :285) and all post-hooks (:277-283) execute BEFORE the projection loop (:294-299); `recordFromHit` (:354-362) therefore reads un-projected tenant_id/team_id/scope/owner_agent_id. Test project_test.go:187-244 (real `acl.NewFilter`, tier source + graph-shaped post-hook: authorized base-auth/edge-auth kept, base-unauth/edge-unauth denied, returned Fields carry no provenance but keep statement/hop). Consumers: internal/server/server_test.go:180-184 asserts `fields_json` parses and retains `statement`; internal/mcp/mcp.go:36 serializes it.
TRACE:    ACL-enabled Search → filterAuthorized(un-projected) keeps {base-auth, edge-auth}, drops {base-unauth, edge-unauth} → projection strips provenance from the returned two only. Had projection run first, recordFromHit would read empty provenance and fail-closed deny everything — the test would fail. `go test ./internal/server/ ./internal/mcp/` → ok.
VERDICT:  PASS (TestDW_1_5_ACLUnaffectedByProjection + server/mcp suites ran and passed)

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have corresponding tests that ran in Step 0 (test names carry DW IDs: TestDW_1_1 … TestDW_1_5)
- [x] Coverage matches the stated 100% level: every DW item has at least one dedicated automated test; DW-1.2 and DW-1.3 each have both a unit-level and a request/end-to-end test
- Note: the two modified integration tests (acl_integration_test.go, opensearch_integration_test.go) are `//go:build integration`-tagged and did not execute here; they were verified to compile under the tag (`go vet -tags integration`). Their change is test-side only (leak checks now consult the seeded id→scope/tenant maps instead of the now-projected fields), and the projection behavior they depend on is covered by the unit tests above.

## Edge Cases (prompt-listed)
| Edge case | Handling | Evidence |
|---|---|---|
| nil/absent Fields | `projectFields(nil)` → nil, no panic; a hit with no `_source` still returned | opensearch.go:323-325; TestDW_1_2_ProjectFieldsToleratesNilAndOddValues, TestDW_1_2_SearchToleratesMissingSource — PASS |
| unknown source safe default | falls back to `defaultAllowed` = text/statement/subject/predicate/object; drops all else incl. embeddings/provenance | opensearch.go:315, :326-329; "unknown source falls back to the safe default" subtest — PASS |
| k≤0 and k>MaxK | clampK: ≤0→DefaultK(10), >MaxK→MaxK(100) | opensearch.go:57-66; TestDW_1_3_ClampK (-1, 0, 100000) — PASS |
| graph hop hits (score 1/(hop+1), no `_score`) projected and kept | hop score preserved (≠0 so fallback untouched), fields reduced to graph allowlist incl. `hop` | expand.go:239; opensearch.go:294-299, :309; TestDW_1_4, TestDW_1_5 — PASS |

## Dead Code
None found in the changed code. The phase diff (git diff HEAD -- internal/retrieval/opensearch.go) adds only clampK/MaxK/fallbackScore/projectFields/allowlists/_source-excludes/shaping loop — no unused imports, no unreachable branches, no debug statements. `go vet` clean.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | Tier goroutines write disjoint `results[i]` indices, joined by `wg.Wait()` before any read (opensearch.go:224-242); shaping/projection is single-threaded after the join; `projectFields` returns a NEW map so a hook/tier-source sharing the input map is never mutated (mutation asserted by TestDW_1_2_ProjectFieldsToleratesNilAndOddValues:120-125). Traced the adversarial case — post-hook holding a reference to a hit's Fields map — no write ever lands on it. |
| Error Handling | PASS | HTTP build/do/read/decode and non-200 all return wrapped errors (opensearch.go:421-441); ACL compile failure is fail-closed zero-results with a logged denial (:209-215); all-tiers-failed returns joined errors (:253-255). Traced malformed `_source` (non-map) → parseHits yields nil Fields → projection returns nil → no panic (TestDW_1_2_SearchToleratesMissingSource). |
| Resources | PASS | `resp.Body` closed via defer (:430); embed context cancel deferred (:452-453); no new handles/locks introduced by this phase. |
| Boundaries | PASS | k=-1/0/1/MaxK/MaxK+1/100000 all traced through clampK (tests above); empty query short-circuits (:201); nil fields map, nil-valued keys, wrong-typed values traced through projectFields without panic (TestDW_1_2_ProjectFieldsToleratesNilAndOddValues). |
| Security | PASS | K is treated as external input and clamped at entry (:204); embeddings and ACL provenance stripped at the retrieval boundary before crossing the wire (:294-299); ordering guarantees authorization always reads un-projected provenance (TestDW_1_5); fail-closed on both ACL compile error and unreadable provenance (:341-362). |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-routine-and-class-design | Parameter count ≤7 | PASS | New routines: clampK(1), projectFields(2). Pre-existing buildQuery grew to 7 params (opensearch.go:499) — at the PASS/"minor concern" limit, not a violation; noted below. |
| cc-routine-and-class-design | Functional cohesion | PASS | clampK ("normalize one count") and projectFields ("reduce one map to an allowlist") each do exactly one operation; Search's added shaping loop is orchestrated inside the existing pipeline routine. |
| cc-routine-and-class-design | Inheritance/LSP | N/A | No inheritance introduced; composition via existing TierSource/PostHook interfaces. |
| cc-defensive-programming | External input validated at entry | PASS | K crosses the MCP process boundary and is clamped at both Search entry (:204) and tier entry (:399) — validated at the barricade, not trusted. |
| cc-defensive-programming | No empty catch / silent swallow | PASS | No error is discarded in the new code paths; `body, _ = json.Marshal(query)` (:529) is pre-existing and cannot fail for this map shape (all values are JSON-marshalable primitives/slices/maps) — noted, not a violation. |
| cc-defensive-programming | Fail-closed on security paths | PASS | Projection ordered AFTER authorization so ACL never reads stripped fields; unreadable provenance denies (recordFromHit empty-string reads, :351-362); traced the blackout counterfactual via TestDW_1_5. |
| cc-defensive-programming | Assertions vs error handling | PASS | No production assertions; anticipated runtime conditions (missing _score, missing _source, unknown source) handled with safe defaults, matching the codebase's error-handling pattern. |

## Notes (non-blocking)
- Pre-existing, untouched by this phase: in Search, `merged == nil && len(errs) > 0` (:253) treats "one tier succeeded with zero hits, another errored" as all-tiers-failed, because appending an empty slice to a nil slice leaves it nil. Behavior predates this diff; no DW covers it. Report only.
- `buildQuery` now has exactly 7 parameters — at the Code Complete limit. A future param should trigger a parameter-object refactor.
- Cross-tier score normalization (RRF fusion scores vs 1/(hop+1) hop scores share one ranking) is explicitly documented as a v1 non-goal (opensearch.go:321).
- The `_source.excludes` names are duplicated between buildQuery (:528) and the tier vectorField config; harmless (excluding an absent field is a no-op) and documented inline.

## Issues (if FAIL)
None.

**Verdict: PASS**

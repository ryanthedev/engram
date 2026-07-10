# Discovery + Design: Phase 1 - Shape hits at the retrieval boundary

## Files Found
- `internal/retrieval/opensearch.go` — `MultiRetriever.Search` (final `filterAuthorized` at line 257), `buildQuery` (line 425), `parseHits`, `recordFromHit` (line 277), `DefaultK` (line 39)
- `internal/retrieval/retriever.go` — `Hit`, `Query`, `Filter`, seam interfaces
- `internal/retrieval/opensearch_test.go` — `httptest` fake (`newFakeSearchServer` + `reqCapture`) and table-test pattern
- `internal/retrieval/acl_test.go` — in-package (white-box) ACL test pattern: real `acl.NewFilter` over a fake `EdgeSource`, `stubTier`, `stubHook`
- `internal/retrieval/{acl,opensearch}_integration_test.go` — behind `//go:build integration`; both read `scope`/`tenant_id` from post-`Search` hits
- `internal/graph/expand.go` — `edgeHit` (line 233): graph hit shape (`statement/subject/predicate/object/hop` + ACL provenance), score `1/(hop+1)`; `seedTenantID` (line 205) reads `tenant_id` from seed hits *inside* the post-hook
- `internal/experience/tier.go` — registered TierSource, `Source: "experience"`, fields: ACL provenance + `statement/distilled_skill/task/utility`
- `internal/eval/halu.go:160` — reads `statement` from Search hits (retained by semantic allowlist)
- `internal/server/server.go:146` — marshals `Hit.Fields` to `fields_json` unchanged
- `e2e/scenarios_*.go` (`//go:build e2e`) — all `fields_json` readers match on `statement`/subject/object content — retained by the allowlists

## Current State
`MultiRetriever.Search` returns raw `_source` maps (incl. 1024-float embeddings). No `_source` filtering in `buildQuery`; `q.K` clamped only at the low end (`<=0 → DefaultK`); no upper bound. ACL: `filterAuthorized` runs twice (pre-truncation at line 238, post-hook at line 257), both via `recordFromHit` reading `tenant_id/team_id/scope/owner_agent_id` from `Hit.Fields` fail-closed.

## Assumption Verification (from dispatch)
**CONFIRMED.** `recordFromHit` (opensearch.go:277–285) reads the four ACL fields from `Hit.Fields`; the last `filterAuthorized` call is at line 257. Additionally, the graph post-hook itself reads `tenant_id`/`subject`/`object` from *seed* hits (`expand.go:195–207`) — post-hooks run before line 257, so end-of-`Search` projection is safe for that consumer too. Projection inserted after line 258, before `return merged, nil`, leaves both ACL passes and the expander byte-for-byte on un-projected fields. `projectFields` builds a NEW map (never mutates the input), so aliased maps held by tiers/hooks are untouched.

## Gaps
1. **Integration tests read stripped fields** — `acl_integration_test.go:239` (`Fields["scope"]`) and `opensearch_integration_test.go:247` (`Fields["tenant_id"]`) assert leak-freedom via post-`Search` fields that projection removes. Fix: assert via seeded doc-ID→scope/tenant maps instead (the ID-membership data already exists in each test). They can't be *run* here (need live OpenSearch) but will be compile-checked with `go vet -tags integration`.
2. **Experience tier hits** (`Source: "experience"`, not in the plan's named allowlists) fall to the unknown safe-default → keep only `statement`; `task`/`utility`/`distilled_skill` are dropped. Verified safe: no non-test consumer reads them post-`Search`; the e2e experience scenario matches its marker inside `statement`. This is the plan's explicit "registered tier sources → safe default" rule.
3. `tierRetriever.search` has its own `k<=0` clamp — must also gain the MaxK upper bound (it is directly reachable via `tierRetriever.Search`).

## Code Standards
`docs/code-standards.md` found. Applicable: return errors, never panic in library code; table-driven tests; ≥1 dirty test per phase; consumer-defined interfaces (no new interface needed); `log/slog` (no new logging needed).

## Test Infrastructure
- Black-box (`retrieval_test`): `newFakeSearchServer` httptest fake with request capture — used for query-shape (DW-1.1, DW-1.3) assertions.
- White-box (`package retrieval`, per `acl_test.go` precedent): real `acl.NewFilter` + `stubTier`/`stubHook` — used for `projectFields` table test (unexported) and the ACL-projection-ordering guard (DW-1.5). `edgeHit` is unexported in `internal/graph` (and importing graph from an in-package retrieval test would be a cycle), so the graph shape is replicated literally in the table, field-for-field from `expand.go:241–251`.

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-1.1 | `buildQuery` excludes `text_embedding`/`fact_embedding` via `_source`; query-shape test | COVERED | `TestDW_1_1_BuildQueryExcludesEmbeddings` (captured request body asserts the excludes on both tiers) |
| DW-1.2 | `projectFields` reduces episodic/semantic/graph/unknown to allowlists; no `*_embedding`/ACL fields survive | COVERED | `TestDW_1_2_ProjectFieldsAllowlists` (table over 4 shapes incl. literal `edgeHit` shape), `TestDW_1_2_ProjectFieldsToleratesNilAndOddValues` (dirty) |
| DW-1.3 | per-tier query `size` clamped to `[1, MaxK]`; below/at/above covered | COVERED | `TestDW_1_3_ClampK` (unit table: ≤0/1/mid/MaxK/MaxK+1), `TestDW_1_3_QuerySizeClampedInRequestBody` (fake-server body asserts `"size":100` for K=250 and `"size":10` for K=-1) |
| DW-1.4 | every returned hit has populated `Score`, incl. graph hop hits | COVERED | `TestDW_1_4_EveryHitCarriesPopulatedScore` (fusion `_score`, hop-scored post-hook hit, and a zero-score hit gaining the fallback) |
| DW-1.5 | ACL unaffected — same hits before/after projection; existing `fields_json` consumers pass | COVERED | `TestDW_1_5_ProjectionRunsAfterACL` (real ACL filter + tier source + graph-shaped post-hook: authorized IDs identical to the pre-projection contract, returned Fields carry no ACL keys — a pre-ACL projection would blackout and fail this), plus full `go test ./...` (covers `internal/server`; e2e readers verified to match on retained fields) |

**All items COVERED:** YES

## Design Decisions
- **Projection point:** a single loop at the very end of `MultiRetriever.Search` (after line 257's `filterAuthorized`), replacing `merged[i].Fields` with a freshly built map. Post-hooks (graph expander reads seed `tenant_id/subject/object`) and both ACL passes see raw fields; nothing downstream of `Search` needs the stripped ones.
- **`projectFields(source, fields)`** — table-driven per the plan: package-level `allowedFields = map[string][]string` (episodic/semantic/graph) + `defaultAllowed` for unknown sources. Functional cohesion: one operation (project), 2 params. Returns nil for nil input; skips absent and nil-valued keys ("tolerate by omitting, never panic"); never mutates input.
- **`clampK(k)`** — one routine, applied in both `MultiRetriever.Search` and `tierRetriever.search` (the latter is directly callable). Defensive at the boundary: `k` is external MCP-caller input — clamp (substitute closest legal value), never assert. `MaxK = 100` exported next to `DefaultK`.
- **Score fallback:** after sort/truncate/hooks, ranking is settled — a `fallbackScore` const (small positive) is assigned only when `Score == 0` (e.g. a backend that omitted `_score`). Applied post-sort so it can never reorder results. Graph hop hits already carry `1/(hop+1)`; fusion hits carry `_score`. Cross-tier score normalization: documented OUT (YAGNI, per plan).
- **`_source` excludes in `buildQuery`:** one `"_source": {"excludes": [...]}` key added to the query map after the mode switch (all three branches share it). Excluding a field absent from an index is a no-op, so both embedding names are excluded on both tiers. Belt (bandwidth) + suspenders (projection).
- **No per-field type validation in `projectFields`** — deliberate deviation from T1.6's "wrong-typed → omitted" wording: an allowlist *copy* cannot panic on any JSON-decoded type, every such type marshals cleanly to `fields_json`, and a type table would be brittle (`hop` is Go `int` from `edgeHit` but `float64` when decoded from OpenSearch). The defensive requirement (no panic, no leak) is met; the dirty test pins pass-through-without-panic instead.

## Prerequisites
- [x] Required files exist
- [x] Dependencies available (no new imports beyond stdlib)
- [x] Prior phase outputs: none required (first phase)

## Recommendation
**BUILD** — implement `MaxK`/`clampK`, `_source` excludes in `buildQuery`, end-of-`Search` projection + score fallback, `projectFields` table routine; update the two integration-test leak assertions to ID-based checks; add DW-tagged tests per the table above.

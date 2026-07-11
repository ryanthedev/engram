# Discovery + Design: Phase 3 - hint → overflow_path escape hatch

## Files Found
- `internal/mcp/budget.go` — `searchResult` envelope, `packSearchResult`, `buildSearchResult`, `refineHint` (the hint text lived here, pre-Phase-3 it never mentioned `overflow_path` or `memory_read`).
- `internal/mcp/spill.go` — `spillFullResult` (atomic temp-write + rename), `spillDir`, `maxSpillPath`. Unchanged this phase.
- `internal/mcp/tools.go` — `callSearch`: packs → conditionally spills → sets/leaves `result.OverflowPath` → renders. `callRead`/`memory_read` (Phase 2) already exists here.
- `internal/mcp/render.go` — `renderSearchResult` copies `searchResult`'s envelope fields (`Hint`, `OverflowPath`, ...) verbatim into `renderedResult`. Unchanged this phase.
- `internal/mcp/budget_test.go`, `internal/mcp/spill_test.go` — existing test infra: `searchViaWire` drives the full JSON-RPC pipeline (including `tools.go`) and decodes `structuredContent`; `TestDW_3_4_UnwritableSpillDirDegradesGracefully` (spill_test.go) already exercises the "spill write failed" path end-to-end.

## Current State
`refineHint(omitted int, facets map[string]string) string` (pre-Phase-3) built only: "N more hit(s) omitted...narrow your query (top omitted field=val,...)." No mention of `overflow_path` or `memory_read` — exactly the gap the plan targets.

## Gaps vs. Plan (design tension found during discovery)

The plan's File scope for this phase lists only `internal/mcp/budget.go`, `internal/mcp/spill.go`, + tests. Tracing the actual call order in `tools.go:callSearch` surfaced a real ordering conflict:

1. `result := packSearchResult(hits, budget)` — this is where `Hint` gets built (via `buildSearchResult` → `refineHint`), and it runs **before** any spill is attempted.
2. `if result.Omitted > 0 { spill; on success set result.OverflowPath; on failure, leave it unset + log }` — this is where the **real, final** state of `overflow_path` becomes known.
3. `renderSearchResult(result)` copies `Hint`/`OverflowPath` verbatim into the output.

DW-3.2 requires the hint to never dangle a promise when "the spill write failed" — but that failure is only known at step 2, strictly *after* step 1 already froze `Hint`'s text. A hint computed entirely inside `budget.go` (step 1) cannot react to a fact (spill success/failure) that doesn't exist yet at that point, no matter how `refineHint` itself is worded.

**Resolution:** two-phase hint construction, no change to spill mechanics or the packing algorithm:
- `buildSearchResult` (budget.go, step 1) builds the hint **optimistically** — assuming `overflow_path` *will* end up set whenever there's a remainder to spill. This matches the existing "reserve the worst case" convention already used for the `overflow_path` field's own byte-budget headroom (`searchResultFits` reserving `maxSpillPath()`), so budget accounting stays honest (the optimistic/longer hint text is what's reserved for; the real hint is never longer than what was reserved).
- `tools.go`'s **existing** spill-failure branch (the one that already logs `slog.Warn` — no new branch, no restructuring) gets one added line: rebuild the hint via `refineHint(..., overflowPathSet=false)` so it no longer dangles the promise.

This is a one-line addition to a conditional that already exists in `tools.go`, not a redesign of the pipeline, a signature change to any exported cross-phase seam, or any change to `spillFullResult`/packing behavior. It's called out here explicitly as a deviation from the plan's literal file-scope list, with rationale, per the Scope Latitude discipline (return UPDATE_PLAN only when a requirement is truly unmeetable within the goal — here it *is* meetable, just not without a one-line touch to the file that resolves the fact the hint depends on).

## Code Standards
`docs/code-standards.md` exists; conventions applied: doc comments on every exported/package-level function explaining *why*, not just *what*; DW-ID references in comments tying code to done-when items; table-driven and `t.Run` subtests; `searchViaWire` end-to-end wire tests alongside direct unit tests on the pure functions.

## Test Infrastructure
- `searchViaWire(t, backend, args) (text, decoded)` — drives `memory_search` through the real JSON-RPC server and decodes `structuredContent`; the established way to assert on envelope fields (`omitted`, `omitted_facets`, `hint`, `overflow_path`) as the client actually sees them.
- `fixedHitsBackend` / `semanticHit` / `manyHits` — build controllable hit sets that force omission deterministically via `t.Setenv(searchBudgetBytesEnv, ...)`.
- Spill-failure simulation precedent: `TestDW_3_4_UnwritableSpillDirDegradesGracefully` chmods the spill dir to 0500 (no write) — reused for the new hint-on-failure test.

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-3.1 | when `omitted>0` and `overflow_path` is set, the `hint` names `overflow_path` as the full-set source | COVERED | `TestDW_3_1_HintNamesOverflowPathWhenSet` (wire-level, writable spill dir) |
| DW-3.2 | when `omitted==0` or the spill write failed, the `hint` does NOT reference a nonexistent `overflow_path` | COVERED | `TestDW_3_2_HintOmitsOverflowPathWhenNotSet` — subtests `"all fit"` (omitted==0, hint absent entirely) and `"spill write failed"` (unwritable spill dir, hint present but no `overflow_path` mention) |
| DW-3.3 | the `hint` names `memory_read` as the single-hit drill path | COVERED | `TestDW_3_3_HintNamesMemoryReadDrill` (wire-level, asserts `memory_read(` substring); also implicitly covered by DW-3.1/3.2's hint-present subtests |

**All items COVERED:** YES

## Design Decisions

**Decision: parameterize `refineHint` with an explicit `overflowPathSet bool` rather than inferring it from `omitted`/facets.** Inferring "will overflow_path be set" from `omitted > 0` alone is exactly the bug DW-3.2 exists to prevent (omission and successful spill are correlated but not identical — a failed spill is omission without a path). Making the caller state the fact explicitly, by name, keeps the function a pure, easily-tested mapping from (count, facets, path-exists) → text, and keeps `refineHint` blind to *how* that fact got resolved (pack-time optimism vs. post-spill downgrade) — which is exactly the separation of concerns needed for the two call sites (budget.go's optimistic first call, tools.go's corrective second call) to each stay simple.

**Decision: mention `memory_read` unconditionally whenever `omitted > 0`, regardless of `overflowPathSet`.** DW-3.3 doesn't condition the drill-down mention on overflow_path's state, and `memory_read` is valid to call in both cases (it addresses hits *within* the packed page, not the omitted remainder) — always naming it costs nothing and steers the caller off inventing a private cache path even on the spill-failure path, where the caller most needs a sanctioned alternative.

**Decision: budget headroom stays correct without extra reservation logic.** The optimistic (longer, overflow_path-mentioning) hint text is exactly what `buildSearchResult`/`searchResultFits` already produce and budget for at pack time; the post-spill-failure downgrade in `tools.go` only ever *shortens* the hint, so the previously-verified fit bound (`TestDW_2_1_OverflowPathHeadroomKeepsFinalResponseInBudget`) remains a valid upper bound — verified by re-running that test unchanged (still passes).

## Prerequisites
- [x] Phase 1 (compact-line rendering) and Phase 2 (`memory_read`) both complete and committed — `memory_read(id, source)` exists for the hint to name.
- [x] `budget.go`/`spill.go`/`tools.go` exist with the exact shapes discovery found above.
- [x] No missing prerequisites.

## Recommendation
BUILD. Implement the two-phase hint (optimistic at pack time, corrected on the existing spill-failure branch in `tools.go`), with the file-scope deviation (a 6-line addition to an existing `tools.go` conditional) called out above and in the final report.

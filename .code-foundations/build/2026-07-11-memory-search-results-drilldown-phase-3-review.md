# Review: Phase 3 - hint to overflow_path escape hatch

## Executed Results (Step 0)
- Test suite: `go test ./internal/mcp/...` → 85 tests, all PASS (verbose confirmed for DW-3.*/refineHint subset: `TestDW_3_1_HintNamesOverflowPathWhenSet`, `TestDW_3_2_HintOmitsOverflowPathWhenNotSet` (2 subtests), `TestDW_3_3_HintNamesMemoryReadDrill`, `TestRefineHintOverflowPathGating`, `TestRefineHintNoFacets`, `TestRefineHintDeterministicFieldOrder` all PASS)
- Test suite (full repo): `go test ./...` → all packages `ok` (e2e is `-tags=e2e` gated, not run — expected per dispatch)
- Typecheck/vet: `go vet ./...` → clean, no output
- Lint: `make lint` (revive via `go run github.com/mgechev/revive@v1.12.0 -config revive.toml -set_exit_status -exclude ./api/engrampb/... ./...`) → exit 0, no findings

Diff reviewed is the uncommitted working-tree change (`git diff HEAD -- internal/mcp/budget.go internal/mcp/tools.go internal/mcp/budget_test.go`), confirmed to be exactly the hint/overflow_path-gating change (refineHint gains `overflowPathSet bool`; tools.go downgrades the hint on spill failure; new tests).

## Requirement Fulfillment

### DW-3.1
PREMISE:  when `omitted>0` and `overflow_path` is set, the `hint` names `overflow_path` as the full-set source.
EVIDENCE: internal/mcp/budget.go:203-205 (`refineHint`: `if overflowPathSet { hint += "; read the full omitted set from overflow_path" }`); internal/mcp/budget.go:129 (`buildSearchResult` calls `refineHint(result.Omitted, result.OmittedFacets, true)` optimistically); internal/mcp/tools.go:173-174 (on spill success, `result.OverflowPath = path`, leaving the optimistic hint intact).
TRACE:    `manyHits(5)` + `ENGRAM_MCP_SEARCH_BUDGET_BYTES=200` + writable spill dir → `packSearchResult` omits hits, builds hint with `overflowPathSet=true` → `callSearch` spills successfully, sets `result.OverflowPath` → response `hint` contains `"overflow_path"` and `overflow_path` field is present. Verified end-to-end by `TestDW_3_1_HintNamesOverflowPathWhenSet` (PASS).
VERDICT:  PASS

### DW-3.2
PREMISE:  when `omitted==0` or the spill write failed, the `hint` does NOT reference a nonexistent `overflow_path`.
EVIDENCE: internal/mcp/budget.go:123-126 (`buildSearchResult` returns before setting any `Hint` at all when `remainder` is empty, so `omitted==0` never emits a hint, `omitempty`-absent); internal/mcp/tools.go:166-172 (on `spillErr != nil`, `result.Hint = refineHint(result.Omitted, result.OmittedFacets, false)` rebuilds without the overflow_path clause, and `result.OverflowPath` is never set, so `omitempty` drops the field).
TRACE:    Case A (omitted==0): 1 small hit, default budget → no omission → `Hint` field never populated → response has no `hint` key at all. Case B (spill fails): `manyHits(5)` + budget=200 + spill dir `chmod 0500` (unwritable) → omission happens, hint initially built with `overflowPathSet=true`, then `spillFullResult` errors → `result.Hint` rebuilt with `overflowPathSet=false` → final hint has no `"overflow_path"` substring and `overflow_path` field absent. Both verified by `TestDW_3_2_HintOmitsOverflowPathWhenNotSet` (both subtests PASS).
VERDICT:  PASS

### DW-3.3
PREMISE:  the `hint` names `memory_read` as the single-hit drill path.
EVIDENCE: internal/mcp/budget.go:206 (`return hint + ", or drill one hit's full body with memory_read(id, source)."` — appended unconditionally, independent of `overflowPathSet`).
TRACE:    `refineHint(2, facets, true)` and `refineHint(2, facets, false)` both produce strings containing `"memory_read"`. End-to-end: `manyHits(5)` + budget=200 → hint contains `"memory_read"`. Verified by `TestDW_3_3_HintNamesMemoryReadDrill` and `TestRefineHintOverflowPathGating` (both PASS). Cross-checked against the tool definition: `ToolRead = "memory_read"` (internal/mcp/tools.go:23) with `InputSchema` properties `id` and `source`, both required (internal/mcp/tools.go:65-75) — matches the hint's `memory_read(id, source)` signature exactly; not misnamed or misused.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] DW-3.1 → `TestDW_3_1_HintNamesOverflowPathWhenSet` (ran, PASS)
- [x] DW-3.2 → `TestDW_3_2_HintOmitsOverflowPathWhenNotSet` (both subtests: "all fit" and "spill write failed", ran, PASS)
- [x] DW-3.3 → `TestDW_3_3_HintNamesMemoryReadDrill` + `TestRefineHintOverflowPathGating` (ran, PASS)
- [x] Dirty test present: `TestDW_3_2_HintOmitsOverflowPathWhenNotSet/spill_write_failed` injects a real filesystem permission failure (`os.Chmod(dir, 0o500)`) to force `spillFullResult` to error — satisfies the "≥1 dirty test" bar.
- [x] Unit-level coverage of `refineHint` gating (`TestRefineHintOverflowPathGating`, `TestRefineHintNoFacets`, `TestRefineHintDeterministicFieldOrder` updated for the new signature) backs the wire-level tests above.
- All DW items covered by automated tests that ran in Step 0; no gaps.

## Dead Code
None found. `go vet` and `make lint` (revive) both clean on the changed files. No unused imports, no unreachable code after early returns, no debug statements, no commented-out blocks.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | `refineHint`/`buildSearchResult` are pure functions over local values; no shared mutable state touched by this diff. |
| Error Handling | PASS | Spill failure path traced (DW-3.2 Case B): `spillErr != nil` triggers the hint downgrade before any response leaves the server; no path where a failed spill leaves a stale `overflow_path` mention. |
| Resources | N/A | No new file/socket/lock handling in this diff (spill.go's file I/O is out of scope per dispatch and unchanged here). |
| Boundaries | PASS | Zero-facet case traced: `refineHint(1, nil, false)` produces no dangling `"(top omitted )"` parenthetical (`TestRefineHintNoFacets`, PASS); zero-omission case traced: `buildSearchResult` short-circuits before `Hint` is ever assigned. |
| Security | N/A | Hint text is server-generated from already-validated internal state (omitted count, facet values from ingested hits); facet values are `%q`-quoted in Go's `fmt`, no injection vector for a text response. |

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| code-clarity-and-docs | Interface comment on `refineHint` reflects the new `overflowPathSet` parameter and both call sites' rationale (not stale after the signature change) | PASS | internal/mcp/budget.go:176-191 fully documents the new parameter, why it must reflect the *real* final state (not "omission happened"), and points to both call sites (`buildSearchResult`, `tools.go`'s `callSearch`) and why each passes the value it does. |
| code-clarity-and-docs | "Different words" test — comments add information beyond restating the code | PASS | e.g. budget.go:112-121's `buildSearchResult` comment explains *why* the hint is built optimistically (worst-case-length reservation already made in `searchResultFits`) rather than restating "sets Hint field"; tools.go:168-171 explains the downgrade is needed because the earlier build assumed success. |
| code-clarity-and-docs | No stale/inaccurate comments introduced by the signature change | PASS | Grepped all `refineHint(` call sites (budget.go, budget_test.go) — all pass the new 3rd argument; no leftover 2-arg call or comment describing the old signature. |
| code-clarity-and-docs | New public-ish entity documentation (test helpers, new tests) | PASS | New tests (`TestRefineHintOverflowPathGating`, `TestRefineHintNoFacets`, `TestDW_3_1_...`, `TestDW_3_2_...`) each carry a doc comment stating what they verify and which DW item(s) they back. |

## Notes (non-blocking)
- The DW numbering (`DW-3.x`) is reused between `budget_test.go` (hint/overflow_path gating — this phase's scope) and the pre-existing `spill_test.go` (spill mechanics — an earlier, differently-scoped set of DW-3 items from spill's own phase). Both files' tests pass and the dispatch prompt scopes only `budget.go`/`tools.go`/`budget_test.go`, so this is flagged only as a naming-collision observation, not a defect in the reviewed files.
- `refineHint`'s doc comment (budget.go:192) still says "This wording is a direct response to the motivating incident" without a citation/link; readable in context (the paragraph right above it and DW-3.3 explain it), so not a clarity defect, just worth noting if a future reader wants the incident's original writeup.

## Issues (if FAIL)
None.

**Verdict: PASS**

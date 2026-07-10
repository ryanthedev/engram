# Review: Phase 2 - Budget-pack + facets at MCP

## Executed Results (Step 0)
- Test suite: `go test ./internal/mcp/...` → 25 passed (verbose run confirms each: TestDW_2_1..TestDW_2_5, TestTopFacetsSkipsMalformedOrMissingFields, TestPackSearchResultZeroHits, TestRefineHintDeterministicFieldOrder, TestCallSearchDefaultKUsesDefaultRequestK, plus 5 pre-existing conformance tests)
- Full suite: `go test ./...` → all packages `ok` or `[no test files]`, no failures
- Typecheck/vet: `go vet ./...` → clean, no output
- Proto diff: `git status --porcelain -- api/proto` → empty output

## Requirement Fulfillment

### DW-2.1
PREMISE:  a default `memory_search` (no explicit `k`) returns a response whose FULL serialized size (hits + envelope) is ≤ the configured byte budget.
EVIDENCE: internal/mcp/tools.go:129-133 (k defaults to `defaultRequestK` when `args.K<=0`), tools.go:137 (`packSearchResult(hits, searchByteBudget())`); internal/mcp/budget_test.go:72-81 (`TestDW_2_1_DefaultSearchFitsBudget`)
TRACE:    60 hits, no `k` arg → `callSearch` requests `defaultRequestK=50` hits → `packSearchResult` packs/trims until `len(json.Marshal(searchResult)) <= budget` → wire text length measured and asserted `<= searchByteBudget()`
VERDICT:  PASS

### DW-2.2
PREMISE:  when hits exceed the budget, the result carries `omitted>0`, non-empty `omitted_facets`, and a `hint`; when all fit, those fields are absent/zero.
EVIDENCE: internal/mcp/budget.go:92-101 (`buildSearchResult`); internal/mcp/budget_test.go:86-125 (`TestDW_2_2_OmissionFieldsPresentOnlyWhenOmitted`, both subtests)
TRACE:    3 small hits, default budget → all fit → decoded JSON has no `omitted`/`omitted_facets`/`hint` keys (asserted). Same 3 hits, `ENGRAM_MCP_SEARCH_BUDGET_BYTES=200` → decoded `omitted>0`, non-empty `omitted_facets`, non-empty `hint` (asserted)
VERDICT:  PASS

### DW-2.3
PREMISE:  byte budget is configurable via `ENGRAM_MCP_SEARCH_BUDGET_BYTES` with the documented default 16384; invalid/unset falls back to the default (dirty test).
EVIDENCE: internal/mcp/budget.go:22-25 (constants), budget.go:46-56 (`searchByteBudget`); internal/mcp/budget_test.go:130-152 (`TestDW_2_3_SearchByteBudgetFromEnv`, 7 subtests: unset, valid, minimal valid, zero, negative, non-numeric, whitespace)
TRACE:    unset → 16384; `"8000"` → 8000; `"1"` → 1; `"0"`, `"-5"`, `"abc"`, `"  "` → all fall back to 16384 (all 7 asserted and passing)
VERDICT:  PASS

### DW-2.4
PREMISE:  a single over-budget hit is still emitted (no empty page when hits exist).
EVIDENCE: internal/mcp/budget.go:71-78 (`packSearchResult` loop guarded by `len(packed) > 1`); internal/mcp/budget_test.go:156-179 (`TestDW_2_4_SingleOverBudgetHitStillEmitted`, both subtests)
TRACE:    single 5000-byte-padded hit, budget=1 → loop never trims below 1 (guard `len(packed) > 1` is false at len=1) → `result.Hits` has exactly 1 element, `Omitted=0`. Huge+small hits, budget=10 → loop trims to `[huge]`, `Omitted=1`, huge stays first (order preserved) — both asserted and passing
VERDICT:  PASS

### DW-2.5
PREMISE:  facet counts are computed over the omitted set with stable ordering on ties.
EVIDENCE: internal/mcp/budget.go:107-144 (`topFacets`, tie-break via `firstSeen` insertion order, not map iteration); internal/mcp/budget_test.go:184-202 (`TestDW_2_5_TopFacetsStableOnTies`, 5 repeated runs per case)
TRACE:    hits ordered [alice, bob] with tied count 1 each → `topFacets` returns "alice" (first-seen); hits ordered [bob, alice] → returns "bob". Repeated 5x each to rule out Go's randomized map iteration — all 10 assertions pass
VERDICT:  PASS

### DW-2.6
PREMISE:  no `.proto` diff is introduced (verify `git status --porcelain -- api/proto` is empty).
EVIDENCE: `git status --porcelain -- api/proto` executed directly
TRACE:    command run from the worktree root → zero-byte output
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] DW-2.1 → `TestDW_2_1_DefaultSearchFitsBudget` (ran, passed)
- [x] DW-2.2 → `TestDW_2_2_OmissionFieldsPresentOnlyWhenOmitted` (ran, passed, both subtests)
- [x] DW-2.3 → `TestDW_2_3_SearchByteBudgetFromEnv` (ran, passed, 7 subtests incl. dirty cases)
- [x] DW-2.4 → `TestDW_2_4_SingleOverBudgetHitStillEmitted` (ran, passed, both subtests)
- [x] DW-2.5 → `TestDW_2_5_TopFacetsStableOnTies` (ran, passed, 5x-repeated per case)
- [x] DW-2.6 → observed behavior (git command; not automatable as a Go test — a repo-state check, not code behavior)
- [x] Edge cases additionally covered: `TestPackSearchResultZeroHits`, `TestTopFacetsSkipsMalformedOrMissingFields`, `TestRefineHintDeterministicFieldOrder`, `TestCallSearchDefaultKUsesDefaultRequestK`

Coverage matches the stated 100% level — every DW item and every listed edge case has either a passing automated test or (for DW-2.6 only) directly observed command output.

## Dead Code
None found. All imports in budget.go (`encoding/json`, `fmt`, `os`, `strconv`, `strings`) and tools.go (`context`, `encoding/json`, `fmt`) are used. No unreachable code after early returns, no commented-out blocks, no debug prints.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | `facetFields` (budget.go:29) is a package-level slice, read-only in all call sites (`for _, field := range facetFields`), never mutated; no shared mutable state is written per-request. Safe under concurrent `tools/call` dispatch. |
| Error Handling | PASS | External input validated at entry: `searchByteBudget` (budget.go:51-55) rejects unparseable/non-positive env values via `strconv.Atoi` + `err!=nil \|\| n<=0` guard, falling back rather than propagating a bad budget. `topFacets` (budget.go:112-114) treats a malformed/absent `Fields` JSON as "contributes nothing" rather than panicking — demonstrated by `TestTopFacetsSkipsMalformedOrMissingFields` with `"not json"`, wrong-typed `{"subject":42}`, and empty-string Fields, all non-panicking. |
| Resources | N/A | No file handles, connections, locks, or caches opened in this phase's code. |
| Boundaries | PASS | Traced: zero hits (`packed:=make([]Hit,0)`, loop skipped, returns non-nil empty slice — TestPackSearchResultZeroHits); exactly one hit at any size (loop guard `len(packed)>1` never fires — TestDW_2_4); budget boundary is inclusive (`len(b) <= budgetBytes`, budget.go:86), matching the "≤" requirement exactly at the edge. |
| Security | PASS | The only untrusted input crossing a process boundary in this phase's code is the `ENGRAM_MCP_SEARCH_BUDGET_BYTES` env var, validated before use; backend-returned `Fields` JSON (semi-trusted but still externally-sourced) is unmarshaled defensively with error handling, never trusted to be well-formed. |

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-control-flow-quality | Nesting depth ≤ 3, guard clauses over arrow code | PASS | `packSearchResult` (budget.go:71-78) is a flat loop + guard, no nesting beyond 1 level. `searchByteBudget` (budget.go:46-56) uses guard-clause style (early return on empty, early return on parse failure), nominal path falls through at the bottom. |
| cc-control-flow-quality | McCabe complexity ≤ 10 per routine | PASS | Highest-complexity routine is `topFacets` (budget.go:107-144) at roughly 6 (loop, unmarshal-err check, 2x inner-loop conditionals, tie-break compare) — well under the 10 "start simplifying" threshold. |
| cc-control-flow-quality | Deterministic ordering / no reliance on map iteration | PASS | `topFacets` explicitly tracks `firstSeen` insertion order to break ties (budget.go:123-124, 136), and `refineHint` iterates the fixed `facetFields` slice rather than the `facets` map (budget.go:152) — both verified non-flaky by 5x-repeated test runs (TestDW_2_5, TestRefineHintDeterministicFieldOrder). |
| cc-routine-and-class-design | Parameter count ≤ 7 | PASS | Max parameter count observed is 3 (`searchResultFits(packed, remainder, budgetBytes)`); all others ≤ 2. |
| cc-routine-and-class-design | Functional cohesion (one operation per routine) | PASS | `topFacets`, `refineHint`, `buildSearchResult`, `searchResultFits`, `searchByteBudget` each perform one clearly named operation; no "AndThen"-style routines found. |
| cc-defensive-programming | External input validated at the barricade, not asserted | PASS | `searchByteBudget` treats the env var as external input and validates/defaults it (budget.go:44-45 comment makes this explicit: "validated and defaulted, never asserted (DW-2.3)"). |
| cc-defensive-programming | No empty catch blocks | PASS | The one error-swallow point, `searchResultFits`'s `err==nil && ...` (budget.go:86), is deliberate and commented (a marshal failure on these plain data types is not expected; treating it as "doesn't fit" keeps the packer shrinking rather than emitting something unverified) — not a silent empty catch, and it doesn't hide a bug class that could otherwise be handled. |

## Notes (non-blocking)

- **Edge case "full slim result set remains available for a later phase to consume, not discarded after packing"**: verified by trace, not by an explicit stored/returned remainder. `packSearchResult`'s `packed` is always an order-preserving prefix of the input `hits` (built via `make`+`copy`, then only ever trimmed from the tail via `packed[:len(packed)-1]` — never reordered, never mutated in place). This means `result.Hits` returned to the caller is exactly `hits[:N]` for `N=len(result.Hits)`, so the omitted remainder is always trivially reconstructible as `hits[N:]` from data the caller (`callSearch`) already holds, without needing `packSearchResult` to return it explicitly. `TestDW_2_4`'s "huge first hit forces the rest into omitted" subtest corroborates the prefix/order invariant (huge stays `Hits[0]`, small becomes the 1-count remainder). Nothing in the current code destructively discards the full set during packing — it's Phase 3's job (per the project's task list, "Spill-to-disk overflow at MCP") to actually persist it past the request lifetime, and Phase 2's non-destructive design does not foreclose that.
- `packer's cumulative size is measured against the SERIALIZED result, not an estimate` is satisfied directly: `searchResultFits` (budget.go:84-87) calls `json.Marshal` and measures `len(b)` — the actual wire bytes, not a heuristic — and `TestDW_2_1` independently confirms this against the real wire `text` returned by the JSON-RPC layer, not `packSearchResult`'s internal accounting.
- `mcp.go` was reviewed for context (Hit/Backend/Server definitions used by budget.go/tools.go) but carries no changes in this phase.

## Issues (if FAIL)
None.

**Verdict: PASS**

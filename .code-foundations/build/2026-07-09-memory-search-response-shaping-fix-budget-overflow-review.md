# Review: memory-search-response-shaping — overflow_path budget-accounting fix

## Executed Results (Step 0)
- `go test ./internal/mcp/... -v -run 'TestDW_2_1|TestDW_2_4'` → 8 subtests, all PASS (0.009s)
- `go test ./internal/mcp/... -count=1 -v` → 21 top-level tests (all internal/mcp tests), all PASS
- `go test ./...` → all packages `ok` (no failures, no skips)
- `go vet ./...` → clean, no output

## Requirement Fulfillment

### R1 — final serialized response (incl. `overflow_path`) ≤ budget
PREMISE:  "the FINAL serialized `memory_search` response that a client actually receives — including the `overflow_path` field when a spill occurs — is ≤ the budget... the packing fit-check accounts for the `overflow_path` field that gets attached after packing (in tools.go), not just the omitted/facets/hint envelope."
EVIDENCE: `internal/mcp/budget.go:103-110` (`searchResultFits` sets `candidate.OverflowPath = maxSpillPath()` before marshaling whenever `len(remainder) > 0`); `internal/mcp/tools.go:138-150` (`callSearch` attaches the real `OverflowPath` only when `result.Omitted > 0`, i.e. exactly when the fit-check reserved headroom for it); "final serialized response" is the existing, established metric — `content[0].text` from the JSON-RPC result (`toolResult`, `internal/mcp/tools.go:164-171`; measured in tests via `searchViaWire`, `internal/mcp/budget_test.go:45-67`).
TRACE:    30 padded hits, budget=2048, `ENGRAM_MCP_SPILL_DIR` set to a real temp dir → `packSearchResult` shrinks until `searchResultFits` (with reserved `maxSpillPath()` headroom) passes → `callSearch` spills and attaches the real (shorter-or-equal) path → `len(content[0].text)` = 2048 or less (`TestDW_2_1_OverflowPathHeadroomKeepsFinalResponseInBudget/small_budget`, observed PASS).
VERDICT:  PASS

### R2 — reserved headroom is a genuine upper bound on the real spill path
PREMISE:  "The headroom reserved for `overflow_path` is a genuine UPPER BOUND on the real field length... derived from the actual `spillDir()` + temp-filename pattern, not a hardcoded constant."
EVIDENCE: `internal/mcp/spill.go:43-48` (`maxRandomSuffixLen = len(strconv.FormatUint(math.MaxUint32, 10))`, derived, not hand-typed); `spill.go:62-66` (`maxSpillPath()` builds from `spillTempPattern`/`spillTempSuffix` and calls `spillDir()` live); confirmed against the Go 1.26.3 stdlib (`$GOROOT/src/os/tempfile.go:22-24`): `nextRandom() = strconv.FormatUint(uint64(uint32(runtime_rand())), 10)` — a `uint32` formatted in decimal, max 10 digits, exactly matching `maxRandomSuffixLen`.
TRACE:    Any `ENGRAM_MCP_SPILL_DIR` value (long or short) → `spillDir()` returns `filepath.Abs`-cleaned absolute dir, called identically by both `maxSpillPath()` and `spillFullResult()` → the only length-variable component is the random suffix, real value ≤ 10 decimal digits (uint32 max `4294967295`) ≤ the 10-nines placeholder → real path length ≤ placeholder length in every case, including a long spill dir (the shared dir prefix is identical in both computations, not re-derived independently).
VERDICT:  PASS

### R3 — DW-2.4 one-hit floor unconditional under the new headroom
PREMISE:  "when even one hit plus the reserved headroom exceeds the budget, exactly one hit is still emitted (the one-hit floor is not defeated by the new headroom)."
EVIDENCE: `internal/mcp/budget.go:85` — loop guard `for len(packed) > 1 && !searchResultFits(...)`; the `len(packed) > 1` condition stops shrinkage at exactly one hit regardless of the `searchResultFits` result.
TRACE:    huge hit (pad 5000) + small hit, `budget = len(marshal(Hits:[huge] alone, no envelope)) + 10` (deliberately too small once headroom+envelope is added) → first iteration `packed=[huge,small]` fails fit → shrink to `packed=[huge]`, `len(packed)==1` stops the loop even though the reserved-headroom candidate still exceeds budget → result is `Hits=[h1]`, `Omitted=1` (`TestDW_2_4_SingleHitFloorHoldsWithOverflowHeadroom`, observed PASS).
VERDICT:  PASS

### R4 — regression test measures real emitted bytes
PREMISE:  "The new regression test measures the REAL emitted response bytes... not a hand-reconstructed size."
EVIDENCE: `internal/mcp/budget_test.go:191-243` (`TestDW_2_1_OverflowPathHeadroomKeepsFinalResponseInBudget`) calls `searchViaWire` (`budget_test.go:48-67`), which drives a real `startServer` + `initialize` + `tools/call` JSON-RPC round trip, decodes `res["content"][0]["text"]`, and asserts `len(text) <= budget` directly on that wire text. The one hand-computed value in the second subtest (`worstCaseOneHit`) is used only to *choose* a tight budget for the test setup — the pass/fail assertion itself is still `len(text)` from the real wire response, not the hand-computed value.
TRACE:    Ran `go test -run 'TestDW_2_1' -v`: both subtests (`small budget`, `budget near a single packed hit's size`) PASS, confirming a real spill file was written (`spillDirEnv` set to `t.TempDir()`), `overflow_path` present in the decoded wire response, and `len(text) <= budget` held for the actual bytes received.
VERDICT:  PASS

### R5 — no regression; no wasted headroom when nothing omitted
PREMISE:  "No regression... all existing internal/mcp tests and the full suite pass; when nothing is omitted there is no `overflow_path` and no wasted headroom."
EVIDENCE: `internal/mcp/budget.go:103-107` — `candidate.OverflowPath = maxSpillPath()` only runs `if len(remainder) > 0`; on the first (no-shrink) iteration `remainder` is `hits[len(hits):]` (empty), so the fit-check for the common case never reserves headroom and the full budget is available to actual hits. `internal/mcp/spill_test.go` `TestDW_3_1_SpillWrittenOnlyWhenOmitted/all_fit` asserts no file and no `overflow_path` when nothing is omitted.
TRACE:    Full-budget default search (10 extra hits over `defaultRequestK`, generous 16384-byte default budget) → first fit-check passes with `remainder` empty → no headroom reserved, no `Omitted`/`overflow_path` fields → `TestDW_2_1_DefaultSearchFitsBudget` and `TestDW_3_1_SpillWrittenOnlyWhenOmitted/all_fit` both PASS. Full suite: `go test ./...` → all packages `ok`, no failures. `go vet ./...` → clean.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] R1 → `TestDW_2_1_OverflowPathHeadroomKeepsFinalResponseInBudget` (both subtests, ran, PASS)
- [x] R2 → covered indirectly by the same test (headroom correctness is what makes it pass) plus independent stdlib verification of `maxRandomSuffixLen`'s premise
- [x] R3 → `TestDW_2_4_SingleHitFloorHoldsWithOverflowHeadroom` (ran, PASS)
- [x] R4 → verified by reading `searchViaWire` and the test body, and by running it
- [x] R5 → `TestDW_2_1_DefaultSearchFitsBudget`, `TestDW_3_1_SpillWrittenOnlyWhenOmitted`, and full `go test ./...` (ran, PASS)
- All DW items have automated-test coverage that ran in Step 0; none rely solely on unverified observed behavior.

## Dead Code
None found in the diff (`internal/mcp/budget.go`, `internal/mcp/spill.go`, `internal/mcp/budget_test.go`). New imports (`math`, `strconv` in `spill.go`) are both used (`math.MaxUint32`, `strconv.FormatUint`). No unreachable code after early returns; no debug statements; no commented-out blocks.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | `spillDir()`/`searchByteBudget()` read env vars via `os.Getenv`, which is safe for concurrent reads; both `maxSpillPath()` (packing time) and `spillFullResult()` (post-pack time) call `spillDir()` fresh, so they stay consistent within one synchronous `callSearch` call. A theoretical race only exists if another goroutine calls `os.Setenv` on `ENGRAM_MCP_SPILL_DIR` mid-request — not a behavior this diff introduces, and not exercised by any requirement; noted, not demonstrated. |
| Error Handling | PASS | `searchResultFits` treats a marshal failure as "does not fit" (safe default, `budget.go:108-109`); `spillFullResult`'s error paths (unchanged by this diff) all clean up the temp file before returning. |
| Resources | PASS | No new file handles or goroutines introduced by this diff; `maxSpillPath()` performs no I/O (pure string/path computation). |
| Boundaries | PASS | Traced the two boundary cases the diff specifically targets: budget exactly at the pre-fix failure point (`TestDW_2_1_.../budget near a single packed hit's size`) and the one-hit floor colliding with headroom (`TestDW_2_4_SingleHitFloorHoldsWithOverflowHeadroom`); both hold. |
| Security | N/A | `overflow_path`'s filesystem-path disclosure is pre-existing behavior from an earlier phase (DW-3.x), unchanged by this diff; this fix only changes size accounting, not what is disclosed. |

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | External input (env vars) validated at entry | PASS | `searchByteBudget()` validates `ENGRAM_MCP_SEARCH_BUDGET_BYTES` via `strconv.Atoi` + positivity check, defaulting on any parse/range failure (`budget.go:50-60`, unchanged by this diff but re-verified as still intact). `spillDir()` intentionally does not validate directory existence — documented as a barricade/degrade-gracefully design (`spill.go:13-18`), not an omission. |
| cc-defensive-programming | No empty catch blocks / silent swallowing | PASS | `searchResultFits`'s marshal-failure branch is not silent — it's an explicit, documented safe default (treat as "doesn't fit"), and the real spill-failure path in `tools.go:145-146` logs via `slog.Warn` rather than swallowing silently. |
| cc-defensive-programming | Assertions used only for programmer bugs, not runtime-anticipated conditions | N/A | No assertion statements in the diff; `maxRandomSuffixLen` is a derived constant, not an assertion. |
| cc-control-flow-quality | Nesting depth ≤ 3 / guard-clause style | PASS | `searchResultFits`, `maxSpillPath`, and the `packSearchResult` loop are all single-level, no nested conditionals introduced. |
| cc-control-flow-quality | McCabe complexity reasonable | PASS | All touched functions have 1-3 decision points; well under the 6-10 "start simplifying" threshold. |

## Notes (non-blocking)
- `maxSpillPath()`'s worst-case digit string (`strings.Repeat("9", 10)` = `9999999999`) exceeds the actual `uint32` max value (`4294967295`) numerically, but both are 10 decimal digits, so the *length* bound (which is what matters for byte-budget accounting) is exact, not merely safe-by-accident.
- The reserved headroom is deliberately conservative (always reserves the worst-case 10-digit random suffix even though real suffixes are typically shorter), which can cause the packer to omit slightly more hits than a tighter, real-length-aware packer would. This is the correct tradeoff for a hard budget guarantee and is not a defect.
- `filepath.Join`/`filepath.Abs` cleaning of `spillDir()` was checked for edge cases (root dir `/`, trailing-slash overrides) and produces identical-length output between `maxSpillPath()`'s and `spillFullResult()`'s use of the same `spillDir()` call, so no length mismatch can arise from path cleaning.

## Issues (if FAIL)
None.

**Verdict: PASS**

# Review: Phase 3 - Spill-to-disk overflow at MCP (sample 1)

## Executed Results (Step 0)
- Test suite (package): `go test ./internal/mcp/...` → 36 passed, 0 failed
- Test suite (full): `go test ./...` → 638 passed in 41 packages, 0 failed
- Vet: `go vet ./...` → no issues
- Phase tests: `go test ./internal/mcp/ -run 'TestDW_3' -v` → all pass, including `TestDW_3_1_SpillWrittenOnlyWhenOmitted` (2 subtests), `TestDW_3_2_OverflowPathRoundTrips`, `TestDW_3_3_SpillFileMode0600`, `TestDW_3_4_UnwritableSpillDirDegradesGracefully`, `TestDW_3_5_SpillDirOverridable` (3 subtests), `TestDW_3_6_MarshalFailureLeavesNoFile`
- Coverage (package profile): spillDir 83.3%, spillFullResult 58.3%, callSearch 82.4% — see Notes

## Requirement Fulfillment

### DW-3.1
PREMISE:  when `omitted>0`, a `0600` file is written atomically (CreateTemp+Rename) and `overflow_path` (absolute) is returned; when `omitted==0`, no file and no `overflow_path`.
EVIDENCE: internal/mcp/tools.go:139-150 (spill only when `result.Omitted > 0`); internal/mcp/spill.go:59 (CreateTemp), 84-88 (TrimSuffix `.tmp` + Rename), spill.go:30-39 (`spillDir` resolves via `filepath.Abs`); test internal/mcp/spill_test.go:43-85.
TRACE:    5 padded hits, budget 200B → packSearchResult omits >0 → `spillFullResult(hits)` → CreateTemp in override dir → Chmod 0600 → Write → Close → Rename `…json.tmp`→`…json` → absolute path set as `overflow_path`. With 2 hits/default budget: Omitted==0 → spill branch never entered → dir empty, key absent.
VERDICT:  PASS — `TestDW_3_1_SpillWrittenOnlyWhenOmitted` passed (wire-level via searchViaWire); asserts `filepath.IsAbs(path)`, file stat-able, exactly 1 dir entry (no leftover `.tmp`); omitted==0 subtest asserts 0 entries and no `overflow_path` key.

### DW-3.2
PREMISE:  reading `overflow_path` yields valid JSON that unmarshals to the FULL slim result set (end-to-end round-trip test).
EVIDENCE: internal/mcp/spill.go:53 (marshals `searchResult{Hits: hits}` — the full unsliced set, tools.go:145 passes `hits` pre-packing); test internal/mcp/spill_test.go:89-121.
TRACE:    6 hits, budget 250B → wire search → read `overflow_path` → `json.Unmarshal` into `searchResult` → 6/6 hits, IDs order-preserving, `Omitted==0` and `OverflowPath==""` in the spilled envelope.
VERDICT:  PASS — `TestDW_3_2_OverflowPathRoundTrips` passed.

### DW-3.3
PREMISE:  file mode is exactly `0600` (asserted via `os.Stat`).
EVIDENCE: internal/mcp/spill.go:69 (`tmp.Chmod(0o600)` before any write, pinning mode regardless of umask); test internal/mcp/spill_test.go:125-140.
TRACE:    `spillFullResult(manyHits(3))` → path → `os.Stat(path).Mode().Perm()` → `0o600` exactly.
VERDICT:  PASS — `TestDW_3_3_SpillFileMode0600` passed.

### DW-3.4
PREMISE:  a failed spill write degrades gracefully — capped response returned, no `overflow_path`, warning logged, no panic (dirty test with an unwritable dir).
EVIDENCE: internal/mcp/tools.go:145-149 (spill error → `slog.Warn`, `OverflowPath` left unset, `toolResult(result)` still returned); test internal/mcp/spill_test.go:156-184 (dir chmod 0500).
TRACE:    unwritable dir + budget 200B → `os.CreateTemp` fails → error propagates from spillFullResult → warning logged (log contains "WARN" and "spill") → response still carries packed hits, `omitted>0`, no `overflow_path` key → no panic, search succeeds.
VERDICT:  PASS — `TestDW_3_4_UnwritableSpillDirDegradesGracefully` passed.

### DW-3.5
PREMISE:  spill dir is `ENGRAM_MCP_SPILL_DIR`-overridable, defaulting to the OS temp dir; a nonexistent override dir degrades gracefully.
EVIDENCE: internal/mcp/spill.go:16, 30-39 (env read, empty → `os.TempDir()`, else `filepath.Abs`); test internal/mcp/spill_test.go:189-224 (3 subtests).
TRACE:    override set → spill file lands in override dir (`filepath.Dir(path) == dir`); env unset → `spillDir() == os.TempDir()`; nonexistent override → CreateTemp fails → warning logged, no `overflow_path`, no panic.
VERDICT:  PASS — `TestDW_3_5_SpillDirOverridable` passed (all 3 subtests).

### DW-3.6
PREMISE:  a write/marshal failure mid-spill leaves NO file renamed into place (no partial `overflow_path`) — atomicity holds under failure.
EVIDENCE: internal/mcp/spill.go:53-56 (marshal before any FS call), 74-78 (write error → Close+`os.Remove(tmpName)`, no rename), 79-82 (close error → Remove), 85-88 (rename error → Remove); test internal/mcp/spill_test.go:229-248.
TRACE:    Hit with `Score: math.NaN()` → `json.Marshal` fails → returned before CreateTemp → dir glob shows 0 artifacts, path == "". Write-failure arm (desk-checked, lines 74-88): any Write/Close/Rename error removes the temp file and returns err — os.Rename at line 85 is the ONLY path that makes a final-named file exist, and it is reached only after a successful full Write+Close.
VERDICT:  PASS — `TestDW_3_6_MarshalFailureLeavesNoFile` passed (executed marshal arm); write arm verified by trace — no code path renames a partially written file (Rename is strictly post-Close, every error branch removes the temp).

**All requirements met:** YES

## Test-DW Coverage
- [x] All 6 DW items have DW-ID-named automated tests that ran in Step 0 (`TestDW_3_1`…`TestDW_3_6`, wire-level via `searchViaWire` where the requirement is response-shaped)
- [x] Coverage matches the stated level read as 100% of DW items test-covered: 6/6. (Literal 100% line coverage is unattainable for the fault-injection-only branches — Chmod/Write/Close/Rename failure on a just-created temp file, `filepath.Abs` error; those are trace-verified, see Notes.)

## Dead Code
None found. All three constants in spill.go are used; no unreachable code, debug statements, or commented-out blocks in spill.go, budget.go, or tools.go.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | Adversarial case: two concurrent searches spilling to the same dir. `os.CreateTemp` guarantees unique temp names (O_EXCL); each final name derives 1:1 from its unique temp name, so no cross-request clash; same-dir Rename is atomic; `slog` is concurrency-safe. No shared mutable state in spill.go. |
| Error Handling | PASS | Every error branch in spillFullResult (spill.go:54,60,69,74,79,85) handled with cleanup + wrapped error; caller (tools.go:145-149) degrades per DW-3.4. No empty catches. Demonstrated by DW-3.4/3.5/3.6 tests. |
| Resources | PASS | Temp file handle closed on every path: Chmod-fail and Write-fail branches Close then Remove; happy path Closes before Rename; Close-fail branch Removes. No leaked FD or file — DW-3.1/3.6 dir globs confirm no leftover artifacts. |
| Boundaries | PASS | Adversarial: `hits` empty at spill — unreachable (spill gated on Omitted>0, which requires a non-empty remainder, budget.go:96-104); `TrimSuffix` no-op case — impossible, CreateTemp pattern (spill.go:21) always yields a `.json.tmp` suffix so finalName != tmpName. |
| Security | PASS | File is 0600 from the first instant: `os.CreateTemp` opens O_RDWR\|O_CREATE\|O_EXCL with perm 0600 (umask can only clear bits, never widen), then Chmod(0600) at spill.go:69 pins it exactly BEFORE any content is written — no chmod-after-write readable window. Asserted at 0600 via os.Stat (DW-3.3 test). Spill errors are logged server-side only (tools.go:146), never returned in the tool response — no error leakage to the client. Env override treated as external input, validated by failure-surfacing not trust (spill.go:11-15). |

## Loaded-Skill Criteria
Skill loaded: `code-foundations:cc-defensive-programming` (CHECKER).

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | No executable code in assertions | N/A | No assertions used anywhere in the reviewed files. |
| cc-defensive-programming | No empty catch blocks | PASS | Every `err != nil` branch in spill.go/budget.go/tools.go cleans up and returns/logs; searchResultFits treats marshal error as "does not fit" (budget.go:88-91) — a safe handled default, not a swallow. |
| cc-defensive-programming | External input validated at entry | PASS | `ENGRAM_MCP_SEARCH_BUDGET_BYTES` parsed + defaulted on invalid (budget.go:50-60); `ENGRAM_MCP_SPILL_DIR` deliberately not pre-validated — a bad dir surfaces as a handled CreateTemp error with graceful degradation (spill.go:11-15, demonstrated by DW-3.4/3.5 tests); tool arguments validated at the RPC boundary (tools.go:124-128). |
| cc-defensive-programming | Assertions for bugs only / anticipated errors get handling | PASS | All anticipated runtime failures (unwritable dir, marshal, disk I/O) use error handling, none asserted. |
| cc-defensive-programming | Security-critical path defense-in-depth (PII on disk) | PASS | 0600 pinned even though CreateTemp already creates 0600 (belt-and-suspenders against platform variance, spill.go:65-69); atomicity prevents partial-PII files; errors never leak to client. |

## Notes (non-blocking)
- Line coverage of `spillFullResult` is 58.3%: the Chmod-error, Write-error, Close-error, and Rename-error branches are untested (they require fault injection — full disk, FS faults). Each is a symmetric 3-line cleanup branch verified by trace under DW-3.6. A write-failure injection test (e.g. an io-error wrapper or tiny quota FS) would close the gap if the project ever grows a fault-injection harness.
- `spillDir`'s `filepath.Abs` error fallback (spill.go:38) returns the raw dir — untestable in practice (Abs fails only when Getwd fails); acceptable per its own comment.
- `callStatus` shows 0% in this package profile — pre-existing Phase-1 surface, not phase-3 code; reported, not chased.
- Theoretical rename-collision: a final name could collide with a prior spill file only if CreateTemp reproduces an earlier random suffix; os.Rename would then atomically replace the old file (never a partial). Negligible and harmless — noted for completeness.

## Issues
None.

**Verdict: PASS**

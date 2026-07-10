# Review: Phase 3 - Spill-to-disk overflow at MCP (sample 3)

## Executed Results (Step 0)
- Test suite (package): `go test ./internal/mcp/...` → 36 passed, 0 failed
- Test suite (full): `go test ./...` → 638 passed in 41 packages, 0 failed
- Typecheck: implied by successful `go test` compile of all 41 packages → clean
- Lint/vet: `go vet ./...` → no issues found (exit 0)
- Targeted: `go test -run 'TestDW_3' -v ./internal/mcp/` → TestDW_3_1 (2 subtests), TestDW_3_2, TestDW_3_3, TestDW_3_4, TestDW_3_5 (3 subtests), TestDW_3_6 all PASS
- Coverage: `go test -coverprofile` → budget.go functions 87.5–100%, spill.go: spillDir 83.3%, spillFullResult 58.3% (error branches chmod/write/close/rename-fail untested), tools.go callSearch 82.4%

## Requirement Fulfillment

### DW-3.1
PREMISE:  when `omitted>0`, a `0600` file is written atomically (CreateTemp+Rename) and `overflow_path` (absolute) is returned; when `omitted==0`, no file and no `overflow_path`.
EVIDENCE: internal/mcp/spill.go:52-90 (CreateTemp at :59, Chmod 0600 at :69, Rename at :85); internal/mcp/tools.go:139-150 (spill only when `result.Omitted > 0`); test internal/mcp/spill_test.go:43-85
TRACE:    5 padded hits + 200-byte budget → packSearchResult omits some → callSearch:139 `Omitted > 0` → spillFullResult: marshal full `hits` → CreateTemp(dir, "engram-mcp-search-*.json.tmp") → Chmod 0600 → Write → Close → Rename to name minus ".tmp" → absolute path assigned to `result.OverflowPath`. With 2 small hits, `Omitted == 0` → the spill block never executes → no file, `overflow_path` omitted via `omitempty` (budget.go:42).
VERDICT:  PASS — TestDW_3_1_SpillWrittenOnlyWhenOmitted (both subtests) passed in Step 0; asserts path is absolute (filepath.IsAbs), file exists, exactly 1 dir entry when omitted, 0 entries and no `overflow_path` key when all fit.

### DW-3.2
PREMISE:  reading `overflow_path` yields valid JSON that unmarshals to the FULL slim result set (end-to-end round-trip test).
EVIDENCE: internal/mcp/spill.go:53 (marshals `searchResult{Hits: hits}` with the FULL unsliced hit set passed from tools.go:145); test internal/mcp/spill_test.go:89-121
TRACE:    6 hits, 250-byte budget → wire-level tools/call → response carries `overflow_path` → os.ReadFile(path) → json.Unmarshal into searchResult → 6 hits, IDs in original order, `Omitted==0` and `OverflowPath==""` in the spilled envelope.
VERDICT:  PASS — TestDW_3_2_OverflowPathRoundTrips passed in Step 0; it is end-to-end (searchViaWire → read file → unmarshal → compare all IDs/order).

### DW-3.3
PREMISE:  file mode is exactly `0600` (asserted via `os.Stat`).
EVIDENCE: internal/mcp/spill.go:69 (`tmp.Chmod(0o600)` before any write); test internal/mcp/spill_test.go:125-140 (`info.Mode().Perm() != 0o600` → fail)
TRACE:    spillFullResult(3 hits) → CreateTemp (stdlib opens O_RDWR|O_CREATE|O_EXCL, 0600 — verified at $GOROOT/src/os/tempfile.go:49) → explicit Chmod(0600) → write → rename → os.Stat(final).Perm() == 0600.
VERDICT:  PASS — TestDW_3_3_SpillFileMode0600 passed in Step 0.

### DW-3.4
PREMISE:  a failed spill write degrades gracefully — capped response returned, no `overflow_path`, warning logged, no panic (dirty test with an unwritable dir).
EVIDENCE: internal/mcp/tools.go:145-149 (spill error → slog.Warn, capped result still returned); test internal/mcp/spill_test.go:156-184
TRACE:    dir chmod'd 0500 → CreateTemp fails EACCES → spillFullResult returns ("", err) → callSearch logs `slog.Warn(...spilling full result set to disk failed...)` and returns the packed page with no `OverflowPath` → JSON-RPC result still succeeds.
VERDICT:  PASS — TestDW_3_4_UnwritableSpillDirDegradesGracefully passed in Step 0; asserts omitted>0, no `overflow_path` key, non-empty `hits`, log contains "WARN" and "spill", and completion without panic.

### DW-3.5
PREMISE:  spill dir is `ENGRAM_MCP_SPILL_DIR`-overridable, defaulting to the OS temp dir; a nonexistent override dir degrades gracefully.
EVIDENCE: internal/mcp/spill.go:30-39 (env read, filepath.Abs on override, `os.TempDir()` fallback); test internal/mcp/spill_test.go:189-224 (3 subtests)
TRACE:    (a) env=tempdir → spill file lands with `filepath.Dir(path) == dir`; (b) env="" → spillDir() == os.TempDir(); (c) env=<tempdir>/does-not-exist → CreateTemp ENOENT → warn logged, no `overflow_path`, no panic — same degradation path as DW-3.4.
VERDICT:  PASS — all three TestDW_3_5_SpillDirOverridable subtests passed in Step 0.

### DW-3.6
PREMISE:  a write/marshal failure mid-spill leaves NO file renamed into place (no partial `overflow_path`) — atomicity holds under failure.
EVIDENCE: internal/mcp/spill.go:53-55 (marshal before ANY filesystem call), :74-82 (write/close failure → os.Remove(tmpName), return before Rename), :85-88 (rename failure → remove temp); test internal/mcp/spill_test.go:229-248
TRACE:    hit with Score=NaN → json.Marshal fails at spill.go:53 → return ("", err) with zero filesystem activity → globbing the spill dir yields 0 entries. Write-failure branch (disk full): Write error at :74 → Close + Remove(tmpName) → return err → Rename at :85 never executes, so no final-name file can ever contain partial content.
VERDICT:  PASS — TestDW_3_6_MarshalFailureLeavesNoFile passed in Step 0 (asserts err != nil, empty path, empty dir). The write-failure branch is verified by trace (no test injects a mid-write I/O error — see Notes).

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have corresponding tests, named by DW-ID, all ran in Step 0 (TestDW_3_1 … TestDW_3_6)
- [x] Coverage matches the stated level with respect to the DW items: 6/6 items have dedicated automated tests; DW-3.1/3.2/3.4/3.5 are exercised end-to-end through the wire path (searchViaWire), DW-3.3/3.6 at the spillFullResult unit boundary.
- Gap (non-blocking): spillFullResult line coverage is 58.3% — the chmod-fail, write-fail, close-fail, and rename-fail branches lack fault-injection tests. Each is handled identically (Remove temp, return error → caller degrades), and the create-fail and marshal-fail paths that share this contract ARE tested.

## Dead Code
None found. All constants (`spillDirEnv`, `spillTempPattern`, `spillTempSuffix`) are referenced; no unreachable code, debug statements, or commented-out blocks in the four reviewed files.

## Edge Cases (prompt-listed)
| Edge case | Handling | Evidence |
|---|---|---|
| Spill dir unwritable/permission-denied | Warn + capped response, no overflow_path, no panic | TestDW_3_4 passed (dir chmod 0500) |
| Disk full / marshal failure mid-write — no partial rename | Marshal precedes all FS calls; write/close/rename errors each Remove the temp and return BEFORE Rename (spill.go:74-88) | TestDW_3_6 passed (marshal); write path TRACE-verified — Rename at :85 is unreachable after any earlier error return |
| `omitted==0` → no spill written | Spill block gated on `result.Omitted > 0` (tools.go:139) | TestDW_3_1 "all fit" subtest: 0 dir entries, no key |
| Env-overridden dir that doesn't exist | CreateTemp ENOENT → same graceful degradation | TestDW_3_5 "nonexistent override" subtest passed |
| Never world/group-readable at any instant | CreateTemp opens with mode 0600 ($GOROOT/src/os/tempfile.go:49 — umask can only clear bits, never add); explicit Chmod(0600) happens BEFORE the first Write; final mode asserted 0600 via os.Stat. No chmod-after-write window exists. | TestDW_3_3 passed + stdlib source |
| No partial/leftover temp survives a failure | Every post-CreateTemp error branch calls os.Remove(tmpName) | TestDW_3_6 globs the dir → 0 entries; branches at spill.go:71, :76, :80, :86 |

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | Adversarial case: two concurrent overflowing searches. CreateTemp guarantees unique `*.json.tmp` names (O_EXCL + retry); final names are the unique temp names minus ".tmp" and a ".json" final can never collide with a ".json.tmp" temp — no cross-request rename clobber. No shared mutable state in spill.go; slog is concurrency-safe. |
| Error Handling | PASS | Every error branch in spillFullResult wraps with context and cleans up; caller treats any error as "spill did not happen" (tools.go:145-149) — demonstrated by TestDW_3_4/3_5/3_6. |
| Resources | PASS | Adversarial trace: fd is Closed on the write-fail path (:75) and close-fail path (:79-81) before Remove; success path Closes before Rename. No fd leak on any branch. |
| Boundaries | PASS | Empty hits → packSearchResult returns Omitted==0 → spill block never runs. `strings.TrimSuffix` operates on a constant suffix always present from the CreateTemp pattern, so finalName ≠ tmpName on every path. |
| Security | PASS | 0600 at creation (stdlib source), tightened-only window before Chmod, mode asserted by test; spill error text (which embeds the dir path) goes to the server-side slog only — the client response never carries the error or any path except the intended `overflow_path` on success; env-supplied dir resolved via filepath.Abs, and a hostile/bogus value degrades to a create failure rather than a panic or a relative leak. |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | No executable code in assertions | N/A | No assertions in the reviewed Go code. |
| cc-defensive-programming | No empty catch blocks / swallowed errors | PASS | Every `err` in phase-3 code (spill.go, callSearch spill block) is checked, wrapped, and acted on. (Pre-existing `text, _ := json.Marshal` in toolResult at tools.go:165 predates this phase — see Notes.) |
| cc-defensive-programming | External input validated at entry | PASS | `ENGRAM_MCP_SPILL_DIR` (env = external input) is deliberately not existence-checked but funneled into an error-handled create that degrades gracefully — the "log warning and continue" strategy, applied consistently and tested (DW-3.4/3.5). `ENGRAM_MCP_SEARCH_BUDGET_BYTES` is parsed-and-defaulted (budget.go:50-60). MCP args validated at callSearch entry (tools.go:127). |
| cc-defensive-programming | Assertions for bugs only; anticipated runtime errors get handling | PASS | Disk/permission failures — anticipated runtime conditions — are error-handled, never asserted; no panic on any tested failure path. |
| cc-defensive-programming | Barricade: security-critical (sensitive-data) path defense-in-depth | PASS | Spill content is treated as sensitive: mode pinned 0600 explicitly at :69 even though CreateTemp already guarantees it — defense-in-depth, not umask trust. |

## Notes (non-blocking)
- spillFullResult error branches (chmod/write/close/rename failure) have no fault-injection tests (58.3% function line coverage). Handling is TRACE-verified and structurally identical to the two tested failure modes; a fault-injecting writer abstraction would close this if desired.
- Spill files accumulate in the spill dir with no TTL/cleanup; no requirement asks for cleanup, but long-running servers will collect one `.json` per overflowing search.
- Pre-existing (phase 2/earlier, out of phase-3 scope): `toolResult` at tools.go:165 discards the marshal error (`text, _ := json.Marshal(payload)`); a non-finite Score reaching it would emit an empty text block. Not introduced or touched by this phase.
- spillDir's `filepath.Abs` failure fallback (spill.go:38) returns the raw possibly-relative dir; reachable only if Getwd fails, in which case CreateTemp fails too and the caller degrades — could not demonstrate a wrong outcome.
- DW-3.4's unwritable-dir test would trivially pass-through if ever run as root (chmod 0500 doesn't stop root); suite here ran unprivileged and the test asserted its own setup (`omitted > 0` and no overflow_path key).

## Issues (if FAIL)
None.

**Verdict: PASS**

# Review: Phase 3 - Spill-to-disk overflow at MCP (sample 2)

Reviewer: post-gate agent (independent; did not read build/plan narrative).
Skill loaded: code-foundations:cc-defensive-programming.

## Executed Results (Step 0)
- Test suite (package): `go test ./internal/mcp/...` → 36 passed, 0 failed
- Test suite (full): `go test ./...` → 638 passed in 41 packages, 0 failed
- Vet: `go vet ./...` → no issues
- Targeted: `go test ./internal/mcp/ -run 'TestDW_3' -v` → all PASS, including
  TestDW_3_1_SpillWrittenOnlyWhenOmitted (2 subtests), TestDW_3_2_OverflowPathRoundTrips,
  TestDW_3_3_SpillFileMode0600, TestDW_3_4_UnwritableSpillDirDegradesGracefully,
  TestDW_3_5_SpillDirOverridable (3 subtests), TestDW_3_6_MarshalFailureLeavesNoFile
- Coverage: `go test ./internal/mcp/ -coverprofile` → spillDir 83.3%, spillFullResult 58.3%
  (uncovered: Chmod/Write/Close/Rename OS-error branches — see Notes), budget.go funcs 87.5–100%.
- Wire fidelity: `searchViaWire` (budget_test.go:48) drives the real JSON-RPC path
  (`startServer` → `tools/call` → `handleToolsCall` → `callSearch`), so DW evidence is end-to-end,
  not unit-shimmed.

## Requirement Fulfillment

### DW-3.1
PREMISE:  when `omitted>0`, a `0600` file is written atomically (CreateTemp+Rename) and `overflow_path` (absolute) is returned; when `omitted==0`, no file and no `overflow_path`.
EVIDENCE: internal/mcp/spill.go:59 (CreateTemp), :85 (Rename), :30–39 (absolute dir resolution); internal/mcp/tools.go:139–150 (spill only when `result.Omitted > 0`); test internal/mcp/spill_test.go:43–85.
TRACE:    5 padded hits, budget=200B → packSearchResult omits >0 → callSearch calls spillFullResult(hits) → marshal → CreateTemp(dir, "engram-mcp-search-*.json.tmp") → fchmod 0600 → write → close → Rename to name minus ".tmp" → OverflowPath set. Test asserts filepath.IsAbs(path), os.Stat succeeds, spill dir has exactly 1 entry (no leftover .tmp). Subtest 2: 2 hits, all fit → omitted==0 → tools.go:139 branch not taken → no `overflow_path` key, spill dir has 0 entries.
VERDICT:  PASS (TestDW_3_1_SpillWrittenOnlyWhenOmitted PASS, ran in Step 0)

### DW-3.2
PREMISE:  reading `overflow_path` yields valid JSON that unmarshals to the FULL slim result set (end-to-end round-trip test).
EVIDENCE: internal/mcp/spill.go:52–56 (marshals `searchResult{Hits: hits}`); internal/mcp/tools.go:145 (spills the unsliced `hits` — packSearchResult splits order-preservingly at budget.go:75–82, so `hits` == packed+remainder); test internal/mcp/spill_test.go:89–121.
TRACE:    6 hits, budget=250B → wire search → read decoded overflow_path → os.ReadFile → json.Unmarshal into searchResult → len==6 (all hits, none omitted), IDs match input order, envelope carries no omitted/overflow_path metadata.
VERDICT:  PASS (TestDW_3_2_OverflowPathRoundTrips PASS, ran in Step 0)

### DW-3.3
PREMISE:  file mode is exactly `0600` (asserted via `os.Stat`).
EVIDENCE: internal/mcp/spill.go:59 (CreateTemp — Go stdlib opens O_RDWR|O_CREATE|O_EXCL, 0600; verified at $GOROOT/src/os/tempfile.go:49), :69 (explicit fchmod 0600 before any write); test internal/mcp/spill_test.go:125–140.
TRACE:    spillFullResult(3 hits) → os.Stat(path).Mode().Perm() == 0o600.
VERDICT:  PASS (TestDW_3_3_SpillFileMode0600 PASS, ran in Step 0)

### DW-3.4
PREMISE:  a failed spill write degrades gracefully — capped response returned, no `overflow_path`, warning logged, no panic (dirty test with an unwritable dir).
EVIDENCE: internal/mcp/tools.go:145–149 (spillErr → slog.Warn, OverflowPath left unset, toolResult still returned); test internal/mcp/spill_test.go:156–184 (dir chmod 0500).
TRACE:    unwritable dir + budget=200B → CreateTemp fails (spill.go:59–62) → callSearch logs warning, returns packed page → decoded has omitted>0, non-empty hits, NO overflow_path key; captured slog output contains "WARN" and "spill"; searchViaWire completes (no panic).
VERDICT:  PASS (TestDW_3_4_UnwritableSpillDirDegradesGracefully PASS, ran in Step 0)

### DW-3.5
PREMISE:  spill dir is `ENGRAM_MCP_SPILL_DIR`-overridable, defaulting to the OS temp dir; a nonexistent override dir degrades gracefully.
EVIDENCE: internal/mcp/spill.go:16, :30–39 (env read, empty → os.TempDir(), else filepath.Abs); test internal/mcp/spill_test.go:189–224.
TRACE:    (a) override set → spillFullResult writes into exactly that dir (filepath.Dir(path)==dir); (b) env empty → spillDir()==os.TempDir(); (c) override = <tmp>/does-not-exist → CreateTemp fails → no overflow_path, warning containing "spill" logged, wire call completes.
VERDICT:  PASS (TestDW_3_5_SpillDirOverridable PASS with 3 subtests, ran in Step 0)

### DW-3.6
PREMISE:  a write/marshal failure mid-spill leaves NO file renamed into place (no partial `overflow_path`) — atomicity holds under failure.
EVIDENCE: internal/mcp/spill.go:53–56 (marshal BEFORE any filesystem call), :69–88 (every post-CreateTemp error branch does os.Remove(tmpName) and returns before/instead of Rename); test internal/mcp/spill_test.go:229–248.
TRACE:    hit with Score=NaN → json.Marshal fails → return before any FS call → path=="", err!=nil, spill dir globbed empty (0 artifacts). Write-failure branch (spill.go:74–78) traced: Write err → Close → Remove(tmpName) → return; Rename at :85 unreachable, so no final-named file can ever exist partially; Close-failure (:79–82) and Rename-failure (:85–88) branches likewise Remove the temp and return error, and callSearch treats any non-nil error as "no spill" (tools.go:145–147).
VERDICT:  PASS (TestDW_3_6_MarshalFailureLeavesNoFile PASS, ran in Step 0; non-marshal branches covered by recorded trace — see Notes on why they are not portably automatable)

**All requirements met:** YES

## Test-DW Coverage
- [x] All 6 DW items have named automated tests (TestDW_3_1…TestDW_3_6) that ran and passed in Step 0
- [x] End-to-end items (3.1, 3.2, 3.4, 3.5) go through the real JSON-RPC wire via searchViaWire
- Gap (non-blocking): spillFullResult statement coverage is 58.3% — the fchmod-fail, write-fail, close-fail, and rename-fail branches are exercised by no automated test. These require fault injection the design doesn't expose (no injectable fs/writer; /dev/full is Linux-only, this is darwin), so per-branch evidence is the recorded TRACE in DW-3.6 above. The DW items themselves each have automated execution evidence (DW-3.6 is disjunctive "write/marshal" and the marshal arm is tested; the create-failure arm is tested via DW-3.4/3.5).

## Edge Cases (prompt-listed)
| Edge case | Handled | Evidence |
|---|---|---|
| Spill dir unwritable/permission-denied | YES | TestDW_3_4 (chmod 0500 dir) — capped page, no overflow_path, WARN logged, no panic |
| Disk full / marshal failure mid-write, no partial rename | YES | TestDW_3_6 (marshal, 0 artifacts); write/close/rename branches traced: Remove(tmp) then error, Rename never reached (spill.go:74–88) |
| `omitted==0` → no spill written | YES | TestDW_3_1 subtest "all fit" — 0 dir entries, no overflow_path key |
| Env-overridden dir that doesn't exist | YES | TestDW_3_5 subtest "nonexistent override degrades gracefully" |
| Never world/group-readable at any instant | YES | os.CreateTemp creates at 0600 (verified in $GOROOT/src/os/tempfile.go:49, O_CREATE|O_EXCL,0600 — umask can only clear bits, never add); explicit fchmod(0600) at spill.go:69 runs BEFORE the first Write (:74); Rename preserves mode. No chmod-after-write window exists. TestDW_3_3 stat-asserts the final mode. |
| No partial/leftover temp file survives failure | YES | Every error branch after CreateTemp removes tmpName (spill.go:70–72, 75–77, 80–81, 86–87); TestDW_3_6 and TestDW_3_1 glob the spill dir and assert exact entry counts |

## Dead Code
None found in spill.go, spill_test.go, tools.go, budget.go. The explicit `tmp.Chmod(0o600)` at spill.go:69 is redundant with CreateTemp's hardcoded 0600 but is deliberate, commented defense-in-depth on a security-critical path — not dead code.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | No shared mutable state; concurrent searches spill via CreateTemp's unique O_EXCL names, and final names collide only if temp names do (impossible); slog default logger is goroutine-safe. No defect demonstrable. |
| Error Handling | PASS | Marshal/create/chmod/write/close/rename each checked and wrapped with context (spill.go:53–88); caller degrades per DW-3.4; spill error text goes to server log only, never to the client response. Adversarial case (NaN score → marshal fail) traced and tested. |
| Resources | PASS | Temp file handle closed on every path (success :79, write-fail :75, chmod-fail :70); temp file removed on every failure. Adversarial trace: no branch leaks an open fd or an orphaned .tmp. |
| Boundaries | PASS | Empty hits → packSearchResult returns non-nil empty page, omitted==0, spill branch never taken (tools.go:139); TrimSuffix target guaranteed by spillTempPattern's ".tmp" suffix (const-coupled, spill.go:21–25); hits[len(packed):] always in range since packed never grows. |
| Security | PASS | 0600 at creation instant (O_EXCL prevents symlink/pre-existing-file hijack of the temp name), fchmod-on-fd (no path TOCTOU) before any content write, mode-preserving rename, stat-asserted final mode; env-var dir treated as external input with graceful create-failure handling; spill errors not leaked to the client. |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | No executable code in assertions | N/A | No assertions in Go code under review |
| cc-defensive-programming | No empty catch blocks / silently swallowed errors | PASS | All error returns in spill.go handled or wrapped; spill failure logged via slog.Warn (tools.go:146). One pre-existing ignored error in toolResult (see Notes) — not demonstrable as a defect at this phase's trust boundary. |
| cc-defensive-programming | External input validated at entry | PASS | ENGRAM_MCP_SEARCH_BUDGET_BYTES parsed + defaulted on invalid/non-positive (budget.go:50–60); ENGRAM_MCP_SPILL_DIR deliberately validated by attempted use with graceful degradation (spill.go:11–15, tested nonexistent + unwritable); tool args (query, k) validated at handler entry (tools.go:127–133) |
| cc-defensive-programming | Assertions for bugs only / anticipated errors get handling | PASS | Zero panics/asserts; every anticipated runtime failure (bad env, bad dir, disk error, marshal error) uses error handling |
| cc-defensive-programming | Security-critical path gets defense-in-depth | PASS | Sensitive memory text spilled with CreateTemp-0600 + explicit fchmod + O_EXCL + atomic rename; mode asserted by test |

## Notes (non-blocking)
1. spillFullResult's chmod/write/close/rename failure branches (spill.go:69–88) lack automated tests (58.3% func coverage) — not portably fault-injectable without an injectable fs the design didn't include; verified by trace instead (recorded in DW-3.6).
2. tools.go:165 `text, _ := json.Marshal(payload)` ignores the marshal error; an unmarshalable payload would yield an empty text block with isError=false. Not demonstrable end-to-end here (backend hits are shaped/validated at the Phase 1 barricade), and pre-dates this phase's diff surface, but worth hardening.
3. Spill files are never cleaned up — repeated over-budget searches accumulate 0600 files in the spill dir. No DW item requires cleanup/TTL; flagging for a future phase.
4. tools.go `toolError("search failed: %v")` forwards backend error text to the MCP client — acceptable for a local stdio server, but a consideration if the transport ever becomes remote.
5. DW-3.4's dirty test (chmod 0500) would not block writes when run as root; suites here ran unprivileged and passed.

## Issues
None blocking.

**Verdict: PASS**

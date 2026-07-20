# Review: Phase 5 - vault assembly + CLI wiring (sample 3)

## Executed Results (Step 0)
- Test suite: `go test ./internal/cli/ -count=1` → ok (all pass); `-run TestDW -v` → 50 DW-named tests, all PASS (16 of them Phase 5)
- Typecheck/build: `go build ./...` → clean
- Vet: `go vet ./internal/cli/` → clean
- Format: `gofmt -l internal/cli/` → **`vault_test.go` unformatted** (comment-column alignment only; see Notes)
- Coverage: `go test ./internal/cli/ -cover` → 70.4% package; per-function figures below
- Reviewer adversarial probes (overlay-injected, `/tmp/rev3-p5/zz_probe_test.go`, 6 tests): all PASS after fixture corrections — details under DW-5.2/DW-5.3 and Edge cases

## Requirement Fulfillment

### DW-5.1
PREMISE:  `engram export <dir>` writes `events/`, `concepts/`, `maps/`; entity-per-note format is gone; `fetchExport` drains episodics across pages.
EVIDENCE: internal/cli/export.go:52-98 (runExport), :104-125 (fetchExport accumulates `page.Episodics/Entities/Edges` until empty NextCursor); internal/cli/vault.go:90-127 (writeVault: three render loops); no `renderEntity`/root-note code remains anywhere in `internal/cli/`.
TRACE:    stub server serves 3 pages (episodic p1 → episodic+entities p2 → edges p3) → `Run(["export","-addr",addr,dir])` → fetchExport loops cursors ""→"1"→"2" → writeVault renders → vault holds `events/2026/2026-03-01 First event prose.md` + `events/undated/Second event prose.md`; rich fixture yields `events/…`, `concepts/Alpha.md`, `maps/Alpha.md`, no root-level `.md`.
VERDICT:  PASS — TestDW_5_1_RichVaultLayoutEndToEnd, TestDW_5_1_FetchExportDrainsEpisodicsAcrossPages, TestDW_5_1_WriteVaultRichLayout all executed green; non-advancing-cursor abort also executed (TestExport_NonAdvancingCursorAborts).

### DW-5.2
PREMISE:  Every write stays inside the vault dir incl. nested folders (path-escape across all three folders).
EVIDENCE: internal/cli/vault.go:57-82 (confinedVaultPath: refuses abs, backslash, unknown root, wrong depth, empty/`.`/`..`/dots-and-spaces elements, plus final `filepath.Rel` re-check); vault.go:153-162 (writeVaultNote confines BEFORE any filesystem effect).
TRACE:    22 hostile relpaths (suite) + 17 more (reviewer probe: `./`-prefixed, `EVENTS/` case-variant root, NUL-in-root, `~/`, `$HOME/`, `//etc/passwd`, `events/2026/x.md/..`, pure-dots elements) → all refused, zero files created; hostile entity/event names (`../../etc/passwd`, `..\..\win\shadow`, `/etc/shadow`) flow through full writeVault → 3 events/1 concept/1 map written, canary outside vault untouched, no `root/etc` created. Reviewer symlink probes: `events`/`concepts`/`maps` symlinks planted inside an owned vault pointing outside → prepareVaultDir removes them (export.go:179-183 RemoveAll of every entry) → end-to-end CLI re-export writes nothing through them (outside dir stayed empty).
VERDICT:  PASS — TestDW_5_2_ConfinedVaultPathRejectsEscapes, TestDW_5_2_HostileNamesStayConfined, plus reviewer probes TestProbe_ConfinedVaultPathExtraHostile / TestProbe_SymlinkInsideOwnedVaultRemoved / TestProbe_EndToEndSymlinkClobberSafe, all executed green. No escape found.

### DW-5.3
PREMISE:  Full-vault re-run is byte-identical for the same export input.
EVIDENCE: internal/cli/vault.go:90-127 (writeVault over the deterministic model); vault_test.go:321-339.
TRACE:    same records → writeVault into dirA and dirB → trees byte-identical; reversed input record order → still byte-identical; reviewer probe: degenerate names (`..`, 80 dots, control-chars, blanks) → two runs byte-identical.
VERDICT:  PASS — TestDW_5_3_ReRunByteIdentical + TestProbe_DegenerateNamesConfinedDeterministic executed green.

### DW-5.4
PREMISE:  A fetch failure leaves an existing vault untouched; empty tenant → marker-only vault; the clobber warning prints.
EVIDENCE: internal/cli/export.go:83-89 (fetchExport at :83 strictly before prepareVaultDir at :87 — clean-late ordering), :94 (warning), :185 (marker write).
TRACE:    export once (vault populated) → re-export against a server whose cursor never advances → exit ≠ 0 and before/after trees byte-identical; empty ExportResponse into a missing nested dir → exit 0, vault contains exactly `.engram-vault`, summary "0 events, 0 concepts, 0 maps"; successful export → stdout contains "warning:" + "clobbered". Non-advancing-cursor failure against a missing dir also proves the dir is never created on fetch failure.
VERDICT:  PASS — TestDW_5_4_FetchFailureLeavesVaultIntact, TestDW_5_4_EmptyTenantMarkerOnlyVault, TestDW_5_4_ClobberWarningPrints, TestExport_NonAdvancingCursorAborts executed green. (The RPC-error flavor of fetch failure is untested — that is a coverage gap, logged under Test-DW Coverage, not a behavior defect: the error return at export.go:112 precedes prepareVaultDir identically.)

### DW-5.5
PREMISE:  Summary reports events/concepts/maps/ghosts/dropped counts.
EVIDENCE: internal/cli/export.go:95-96 (summary Fprintf from vaultStats); vault.go:28-34, :134-148 (countDroppedEdges: dropped = NEITHER endpoint exported).
TRACE:    rich fixture (2 events, 1 hub, 2 ghosts, 1 fully-dangling edge) → stdout contains exactly `exported 2 events, 1 concepts, 1 maps to <dir> (2 ghosts, 1 dropped)`; a half-dangling edge (one endpoint exported) added → still Dropped=1, proving it lands as a claim, not dropped.
VERDICT:  PASS — TestDW_5_5_SummaryCountsPrinted, TestDW_5_5_StatsCounts executed green.

**All requirements met:** YES

## Edge cases (prompt-listed)
| Edge case | Status | Evidence |
|---|---|---|
| Nested path-escape via crafted name → refused, whole-export abort | HANDLED | confinedVaultPath refusal propagates out of writeVault (vault.go:103-104,114,122) → runExport returns error; TestDW_5_2_* + 17 extra reviewer probes: every escape refused, zero stray files |
| Empty tenant → marker-only vault | HANDLED | TestDW_5_4_EmptyTenantMarkerOnlyVault (exit 0, marker only, zero summary); TestWriteVault_EmptyExport (no files, no error) |
| Slug collisions across folders → disambiguated within each folder | HANDLED | Reviewer probes executed green: 3 same-title events → 3 distinct `events/undated/` files; entities `Dup/x`,`Dup:x`,`Dup*x` (all sanitize to `Dup-x`, never name-collapsed) → 3 distinct `concepts/Dup-x*` files, file count == stats.Concepts; two components with colliding hub slugs `Hub/x`/`Hub:x` → 2 distinct `maps/` files (TestProbe_SlugCollisionsDisambiguated, TestProbe_MapSlugCollisionDisambiguated) |

## Security focus — path confinement (attack surface probed)
Every attack executed against the ACTUAL `confinedVaultPath`/`writeVaultNote`/`writeVault`/`prepareVaultDir` under temp dirs (probe file kept at /tmp/rev3-p5/zz_probe_test.go, run via `go test -overlay`):
- `../` in names/slugs, absolute paths, `..`-laden segments, empty/`.`/`..` elements, backslash separators, dots-and-spaces elements → all REFUSED (39 hostile relpaths total across suite + probes); refusal happens before any filesystem effect and aborts the whole export.
- Case-variant roots (`EVENTS/`), NUL in a path element, `~`/`$HOME` literals, `//etc/passwd` → refused (unknown root / element checks).
- Names that survive sanitize+fold as weird-but-flat (`.. .md`, `a .. b.md`, trailing-space element, fullwidth `／` lookalikes) → confined inside the vault (verified via `filepath.Rel` on every accepted result).
- Symlinked escape hatches planted inside an owned vault (`events`→outside, all three roots) → removed by prepareVaultDir's clean before any write; end-to-end CLI export wrote nothing outside (outside dir empty, canary intact).
- Hostile ingested names through the full pipeline → 0 files outside the vault, no `etc/` created beside it.
- Failed fetch (non-advancing cursor) → existing vault byte-identical after; missing dir never created.
No accepted `../`, no file outside the vault, in any probe. The barricade holds.

## Test-DW Coverage
- [x] Every DW item has DW-named automated tests that ran in Step 0 (16 Phase 5 DW tests).
- [ ] Coverage does NOT match the stated 100% level.

Phase 5 function figures (`go tool cover -func`):

| Function | Coverage |
|---|---|
| vault.go confinedVaultPath | 93.3% |
| vault.go writeVault / writeVaultNote / vaultPathDepth / countDroppedEdges | 100% |
| vault.go writeFileAtomic | 66.7% |
| export.go runExport | 83.9% |
| export.go fetchExport | 93.8% |
| export.go checkVaultDir | 76.5% |
| export.go prepareVaultDir | 65.0% |
| export.go isCatastrophicVaultDir / idPrefix / sanitizeFilename / cleanInline | 100% |

**Uncovered REACHABLE behavior branches (FAIL per the stated coverage rule):**
1. export.go:111-113 — fetchExport's RPC-error branch (`ExportPage` returns error). Trivially injectable: a stub whose `Export` returns an error. This is also the canonical DW-5.4 "fetch failure"; only the cursor-loop flavor is tested.
2. export.go:137-139 — checkVaultDir "exists and is not a directory". Reachable by pointing export at an existing file; one-line test.
3. export.go:144-146 — checkVaultDir accepting a pre-existing EMPTY dir. Reachable user behavior (export into an empty dir); untested.
4. export.go:58-60 and 67-69 — flag-parse error branches (`export --bogus`, `export <dir> --bogus`). Trivially reachable.
5. export.go:161-163 and 169-171 (+ runExport wiring 87-89) — prepareVaultDir's re-check error and catastrophic-refuse branches. Reachable and safely testable: `runExportCLI(t, addr, "/", "--force")` refuses at :169 before any write; direct `prepareVaultDir(foreignDir, false)` covers :161. Only the pure helper is tested, not the wiring that enforces it.

**Below-100% branches that qualify as NON-BLOCKING per the rule (confirmed correct by inspection):**
- vault.go:78-80 — confinedVaultPath's final `filepath.Rel` re-check: genuinely unreachable defense-in-depth given the preceding element checks (reviewer probes could not reach it); refusal shape is correct.
- vault.go:172-180 — writeFileAtomic WriteString/Close failure paths: non-portably-fault-injectable OS-write failures; both close and remove the temp file correctly (temp-cleanup verified for the two injectable paths by TestWriteFileAtomic_ErrorPaths).
- export.go:134-136, 141-143, 165-167, 172-187 — Stat/ReadDir/Abs/MkdirAll/RemoveAll/WriteFile OS failures: chmod-style injection is non-portable (fails as root); all are wrap-and-return.
- export.go:79-81, 91-93 — dialClient / writeVault error wiring in runExport: underlying failures unit-tested; wiring reachable only via malformed dial target or a mid-run filesystem race.

## Dead Code
None found. No unused imports (build+vet clean), no debug prints, no TODO/FIXME, no commented-out blocks, no unreachable code after early returns in the four files. `idPrefix`/`cleanInline` are used by vaultmaps.go/vaultmodel.go.

## Correctness Dimensions
| Dimension | Status | Evidence |
|---|---|---|
| Concurrency | N/A | Single-threaded CLI flow; no shared state or goroutines in the reviewed files |
| Error Handling | PASS | Every error wrapped with context and propagated; cursor-loop guard on external input (export.go:120-122); no empty catches; refusal-before-effect ordering traced and probed. No demonstrated defect |
| Resources | PASS | `defer client.Close()` (export.go:82); temp files removed on every writeFileAtomic failure path (TestWriteFileAtomic_ErrorPaths: no `.engram-tmp-*` lingers) |
| Boundaries | PASS | Empty export (writes nothing, no error), nil OccurredAt (undated bucket), degenerate names (empty after sanitize, 80-dot, control-char) all probed green; rune-capped filenames re-trimmed after truncation (export.go:232-236) |
| Security | PASS | Full adversarial campaign above — no escape, no clobber-on-failure, catastrophic-dir guard present |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|---|---|---|---|
| cc-defensive-programming | External input validated at entry (untrusted names/prose/cursor) | PASS | sanitize at render + cursor-advance guard (export.go:120) + confinement at write (vault.go:57); probed with hostile input |
| cc-defensive-programming | Barricade + defense-in-depth on security-critical path | PASS | Two independent layers (sanitizeFilename at render, confinedVaultPath before every write); second layer verified to refuse everything the first should make unreachable |
| cc-defensive-programming | No empty catch blocks / no swallowed errors | PASS | Every `err` returned or wrapped; `os.Remove` on cleanup paths is the only ignored-effect call and only after the primary error is already being returned |
| cc-defensive-programming | Anticipated runtime errors handled, not asserted | PASS | All refusals are error returns (production-safe), none are assertions; correctness-leaning strategy (abort whole export) fits a data-destroying operation |
| cc-refactoring-guidance | Old format removed cleanly, no residue | PASS | No entity-per-note code remains; no dead renderers; e2e test asserts the old layout cannot silently return |
| cc-refactoring-guidance | Small-change rigor: lint/format clean | FAIL-adjacent → Note | `gofmt -l` flags vault_test.go (comment alignment only, zero behavior impact) — a matter of degree, kept non-blocking; fix alongside the coverage fixes |

## Notes (non-blocking)
- `gofmt -d internal/cli/vault_test.go`: comment-column realignment in the `bad` slice (vault_test.go:175-182). Run `gofmt -w` on it.
- confinedVaultPath accepts flat-but-odd filenames (trailing-space element `"x.md "`, literal `".. .md"`). All confined; purely cosmetic on some filesystems.
- The vault dir itself being a symlink resolves writes into its target — that is the user's chosen destination, not an untrusted-content escape; attacker-planted symlinks inside the vault are neutralized by the pre-write clean (probed).
- Defense-in-depth/OS-failure branches listed above are uncovered but confirmed correct by inspection, per the dispatch's carve-out.

## Issues (FAIL blockers)
1. Test coverage below the required 100% on reachable behavior branches (5 clusters).
   - File: internal/cli/export.go:111-113 (fetchExport RPC error), :137-139 (target is a file), :144-146 (empty existing dir), :58-60 + :67-69 (flag-parse errors), :161-163 + :169-171 + :87-89 (prepareVaultDir re-check / catastrophic-refuse wiring)
   - Demonstrated by: `go tool cover -func` (fetchExport 93.8%, checkVaultDir 76.5%, prepareVaultDir 65.0%, runExport 83.9%) and per-block profile showing the exact statements at 0 hits; each branch shown reachable above
   - Fix: add tests — stub `Export` returning an error (also strengthens DW-5.4: assert the existing vault survives an RPC-error fetch failure); `export` targeting an existing file; export into a pre-created empty dir; `export --bogus` / `export <dir> --bogus`; `prepareVaultDir(foreignDir,false)` and `runExportCLI(t, addr, "/", "--force")` (refuses before any write — safe)

**Verdict: FAIL — all 5 DW items and all 3 prompt-listed edge cases verified green with execution evidence, and the path-confinement attack surface held against the full adversarial campaign; the sole blocker is the Test Coverage Level requirement: uncovered REACHABLE behavior branches in export.go (Issue 1).**

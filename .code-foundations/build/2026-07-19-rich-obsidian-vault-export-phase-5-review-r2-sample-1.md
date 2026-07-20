# Review: Phase 5 - Vault assembly + CLI wiring (r2, sample 1)

Independent post-gate review. All commands run from the worktree root; reviewer's adversarial probes run against an unmodified copy of the tree under `/tmp/r2rev1-p5/repo` (white-box package access without mutating the repo).

## Executed Results (Step 0)

- Test suite: `go test ./internal/cli/ -count=1` → **ok** (all pass, 0 failures)
- Race: `go test ./internal/cli/ -count=1 -race` → **ok**
- Build: `go build ./...` → clean
- Vet: `go vet ./internal/cli/` → clean
- gofmt: `gofmt -l ./internal/cli/` → **no output** (required: any output is a FAIL — none)
- Coverage: `go test ./internal/cli/ -cover` → 72.0% package-wide; Phase 5 per-function figures below
- All 32 Phase 5 tests run verbosely (`-run 'TestDW_5|TestPrepareVaultDir|TestExport|TestWriteVault|TestWriteFileAtomic|TestSanitizeFilename|TestCheckVaultDir'`) → 32× PASS, 0 SKIP on this host

## Requirement Fulfillment

### DW-5.1
PREMISE:  "`engram export <dir>` writes `events/`, `concepts/`, `maps/`; entity-per-note format is gone; `fetchExport` drains episodics across pages."
EVIDENCE: internal/cli/export.go:52-98 (runExport), export.go:104-125 (fetchExport), internal/cli/vault.go:90-127 (writeVault); tests export_test.go:120 (TestDW_5_1_RichVaultLayoutEndToEnd), export_test.go:157 (TestDW_5_1_FetchExportDrainsEpisodicsAcrossPages), vault_test.go:113 (TestDW_5_1_WriteVaultRichLayout)
TRACE:    stub gRPC server serving `richPage()` → `Run(["export","-addr",addr,dir])` → fetch drains → prepareVaultDir → writeVault renders `events/2026/2026-03-01 Alpha shipped the beta.md`, `events/undated/Gamma joined Beta.md`, `concepts/Alpha.md`, `maps/Alpha.md` + marker; 3-page fixture spreading episodics across pages 0–1 yields both event notes and "2 events" in the summary. Entity-per-note is structurally impossible: confinedVaultPath (vault.go:65) refuses any root other than events/concepts/maps at exact depth, and the end-to-end test asserts no root-level `.md`. Grep confirms zero old-format residue in non-test code.
VERDICT:  **PASS** (all three named tests executed and pass)

### DW-5.2
PREMISE:  "every write stays inside the vault dir incl. nested folders (path-escape across all three folders)."
EVIDENCE: internal/cli/vault.go:57-82 (confinedVaultPath), vault.go:153-162 (writeVaultNote — confinement before any filesystem effect); tests vault_test.go:159 (TestDW_5_2_ConfinedVaultPathRejectsEscapes, 22 hostile relPaths × both the checker and the write wrapper), vault_test.go:217 (TestDW_5_2_HostileNamesStayConfined — hostile episodic titles and entity names through the full writeVault, canary + WalkDir escape sweep)
TRACE:    `"events/2026/../pwn.md"` → split on "/" → element ".." fails `strings.Trim(el, ". ") == ""` → refusal error → writeVaultNote returns before MkdirAll → whole export aborts; hostile names `../../etc/passwd`, `..\..\win\shadow`, `/etc/shadow` flow through sanitizeFilename into confined paths — WalkDir over the parent proves every file is inside the vault, canary untouched, no `etc/` created outside. Reviewer's own probes (TestReviewer_ConfinementAdversarial in the /tmp copy): 15 additional hostile relPaths (`./`-prefixed, `//etc`, NUL-bearing, case-flipped root `EVENTS/`, `c:/`, encoded-dot, trailing-`..`) — every accepted path proven strictly inside dir, every escape refused. All PASS.
VERDICT:  **PASS**

### DW-5.3
PREMISE:  "full-vault re-run is byte-identical for the same export input."
EVIDENCE: internal/cli/vault.go:90-127 + vaultmodel.go's sorted assembly; test vault_test.go:321 (TestDW_5_3_ReRunByteIdentical — two runs into fresh dirs byte-compared, plus reversed input order)
TRACE:    same records → writeVault into dirA and dirB → assertTreesEqual over full file contents → identical; reversed episodics/entities/edges → still identical. Reviewer probe (TestReviewer_EndToEndRerunByteIdentical): two full CLI exports into the SAME dir, sha256 over sorted (path, content) pairs → identical hashes. PASS.
VERDICT:  **PASS**

### DW-5.4
PREMISE:  "a fetch failure leaves an existing vault untouched; empty tenant → marker-only vault; the clobber warning prints."
EVIDENCE: internal/cli/export.go:83-94 (fetch precedes prepareVaultDir — clean-late ordering; warning printed on success); tests export_test.go:208 (TestDW_5_4_FetchFailureLeavesVaultIntact — existing vault byte-compared before/after a failed second run), export_test.go:226 (TestDW_5_4_EmptyTenantMarkerOnlyVault), export_test.go:245 (TestDW_5_4_ClobberWarningPrints); also export_test.go:187 and :519 prove no dir is even created on fetch failure
TRACE:    non-advancing-cursor server → fetchExport errors at export.go:121 → runExport returns before prepareVaultDir (line 87) → prior vault tree byte-identical after the failure. Empty page server + missing nested dir → exit 0, tree contains exactly `.engram-vault`, summary "0 events, 0 concepts, 0 maps". Successful export output contains "warning:" and "clobbered" (export.go:94).
VERDICT:  **PASS**

### DW-5.5
PREMISE:  "summary reports events/concepts/maps/ghosts/dropped counts."
EVIDENCE: internal/cli/export.go:95-96 (summary line), vault.go:28-34 (vaultStats), vault.go:134-148 (countDroppedEdges); tests export_test.go:259 (TestDW_5_5_SummaryCountsPrinted — exact line "exported 2 events, 1 concepts, 1 maps to <dir> (2 ghosts, 1 dropped)"), vault_test.go:343 (TestDW_5_5_StatsCounts — half-dangling edge with ONE exported endpoint is NOT counted dropped)
TRACE:    richPage → 2 events, 1 hub, 1 map, 2 ghosts, 1 fully-dangling edge → stats struct → Fprintf → exact summary asserted; edge `(e-a, e-unknown)` → `exported[e-a]` true → not dropped.
VERDICT:  **PASS**

**All requirements met:** YES

## Test-DW Coverage

- [x] Every DW item has named, DW-ID-referencing automated tests that ran in Step 0 (DW-5.1 ×3, DW-5.2 ×2, DW-5.3 ×1, DW-5.4 ×3, DW-5.5 ×2 — all PASS)
- [x] Coverage matches the stated 100% level, subject to the dispatch's carve-outs (analysis below)

### Phase 5 function coverage (`go tool cover -func`)

| Function | Coverage |
|---|---|
| export.go runExport | 96.8% |
| export.go fetchExport | 100% |
| export.go checkVaultDir | 100% |
| export.go prepareVaultDir | 88.9% |
| export.go isCatastrophicVaultDir | 100% |
| export.go idPrefix | 100% |
| export.go sanitizeFilename | 100% |
| export.go cleanInline | 100% |
| vault.go vaultPathDepth | 100% |
| vault.go confinedVaultPath | 93.3% |
| vault.go writeVault | 100% |
| vault.go countDroppedEdges | 100% |
| vault.go writeVaultNote | 100% |
| vault.go writeFileAtomic | 66.7% |

### Below-100% branch dispositions (each verified against the raw profile)

| Uncovered block | What it is | Disposition |
|---|---|---|
| export.go:91-93 | `writeVault` error propagation in runExport (single `return err`) | Non-blocking. writeVault's error behavior is unit-tested (TestWriteVault_WriteFailurePropagates, all three folders). End-to-end injection is not portable: prepareVaultDir has just emptied and marker-stamped the dir, so a subsequent write failure needs a mid-run permission flip or a >255-BYTE filename — reviewer probed the worst case (60×4-byte emoji title, dated, collision-suffixed) through the real CLI and it succeeds on APFS (255-char limit); only ext4's byte limit could trigger it. Correct by inspection. |
| export.go:165-167 | `filepath.Abs` error return | Non-blocking. TestPrepareVaultDir_AbsErrorWithDeletedCwd exists and passes, but on darwin the error surfaces later (MkdirAll), so this branch is not hit on this platform — non-portably injectable single-statement OS-error return; correct by inspection. |
| export.go:175-177 | `EvalSymlinks` non-ENOENT error return | Non-blocking. Effectively unreachable: checkVaultDir has already os.Stat'ed the same path, and every EvalSymlinks failure mode (ELOOP, EACCES, ENOTDIR) fails that earlier stat first. Defense-in-depth; correct by inspection. |
| export.go:193-195 | prepareVaultDir ReadDir error after MkdirAll | Non-blocking. Requires ReadDir to fail on a directory checkVaultDir just listed (or MkdirAll just created with 0o755) — a race, not portably injectable. Single-statement OS-error return; correct by inspection. |
| vault.go:78-80 | confinedVaultPath final `filepath.Rel` re-check refusal | Non-blocking. Genuinely unreachable defense-in-depth by design (file-header comment says exactly this): the earlier abs/backslash/root/depth/dot-element checks exhaust every input that could make the joined path diverge. Reviewer's 37 combined hostile relPaths never reached it. This is the precise carve-out the dispatch names. |
| vault.go:172-180 | writeFileAtomic WriteString/Close error paths (cleanup + return) | Non-blocking. Disk-full / fd-failure class errors — not portably injectable without mocking the FS. Both paths close and `os.Remove` the temp file then return a wrapped error; CreateTemp and Rename failure paths ARE tested including temp-file-cleanup assertions (TestWriteFileAtomic_ErrorPaths). Correct by inspection. Slightly beyond "single-statement" (error + cleanup), disclosed as a judgment call under the carve-out's intent. |

No uncovered branch is a reachable, portably-testable behavior branch. cleanInline's remaining block is an empty (0-statement) switch case.

## Edge Cases (dispatch-listed — FAIL if unhandled)

| Edge case | Status | Evidence |
|---|---|---|
| Nested path-escape via crafted name → refused (whole-export abort) | HANDLED | TestDW_5_2_ConfinedVaultPathRejectsEscapes: writeVaultNote refuses before any FS effect and writeVault propagates → export aborts; hostile-name canary test proves nothing lands outside; reviewer probes concur. |
| Empty tenant → marker-only vault | HANDLED | TestDW_5_4_EmptyTenantMarkerOnlyVault: exit 0, exactly one file (`.engram-vault`), all-zero summary. |
| Slug collisions across folders → disambiguated within each folder by VaultRefs | HANDLED | Shipped: TestDW_5_2 good-path `concepts/Alpha (abcd1234).md`; homonym tests live in vaultmodel_test.go/vaultmaps_test.go (Phase 2/4 surface). Reviewer demonstration through Phase 5's writeVault: same-slug events → `events/undated/Same title here (ev1).md` + `(ev2).md`; entities "Alpha/x" vs "Alpha:x" (distinct normalize keys, identical sanitized filename) → `concepts/Alpha-x (e-1).md` + `(e-2).md` and `maps/Alpha-x (e-1).md` + `(e-2).md` — file count matches stats in every folder, no silent overwrite. Same-named entities (`"Alpha"` vs `" Alpha "` vs `"Alpha?"`) intentionally merge into ONE concept at the model level (normalizeConceptName), which is deduplication, not a collision loss. |

## Security Focus (dispatch-directed attacks)

### 1. Path confinement
Reviewer fed 15 additional hostile relPaths (beyond the suite's 22) through the ACTUAL `confinedVaultPath`/`writeVaultNote` in a temp vault under /tmp/r2rev1-p5 — `../`-chains at every depth, absolute paths, `//`, `./` prefixes, NUL-bearing elements, backslash separators, case-flipped roots, `c:/`, dot-only and space-dot elements, trailing separators. Every escape refused; every accepted path proven strictly inside the vault via `filepath.Rel`; zero files written outside (canary + WalkDir sweep). Confinement is enforced BEFORE MkdirAll/write (vault.go:154-157), and a refusal aborts the whole export (writeVault returns the error up through runExport).

### 2. CATASTROPHIC-DIR / SYMLINK guard
- Shipped TestPrepareVaultDir_SymlinkToHomeRefused: vault dir = symlink to a scratch `$HOME` (t.Setenv), canary inside, real `engram export --force` through cli.Run → exit != 0, "refusing to clobber" on stderr, canary byte-identical. PASS (executed).
- Shipped TestPrepareVaultDir_SymlinkToRootRefused: symlink to `/` + `--force` → refused. PASS (executed).
- Reviewer probe TestReviewer_ForceSymlinkHomeDeep: independent construction, canary in `$HOME/Documents/thesis.txt`, `--force` → refused, deep canary survives. PASS.
- Reviewer probe TestReviewer_MarkerInHomeDoesNotLaunderSymlink: attacker plants `.engram-vault` inside `$HOME` and symlinks the vault dir to it (marker would let checkVaultDir accept WITHOUT --force) → the catastrophic guard still refuses after EvalSymlinks resolution; home contents unchanged. PASS — the marker cannot launder a catastrophic target.
- Guard mechanics verified in code: EvalSymlinks resolves the vault dir BEFORE the comparison (export.go:173), `$HOME` is itself resolved (export.go:182), root detection is `abs == filepath.Dir(abs)` (parent-of-itself, portable), and the cleaner removes entries INSIDE dir only, never dir itself (export.go:196-199).
- Failed fetch never clobbers: TestDW_5_4_FetchFailureLeavesVaultIntact (byte-compare) + TestExport_FetchErrorAborts / TestExport_NonAdvancingCursorAborts (dir never created). All executed, all pass.

No deletion outside the vault and no accepted symlink-to-home/root was producible.

## Dead Code

None found. No unused imports (build/vet clean), no debug statements, no commented-out blocks, no unreachable code after early returns, no residue of the old entity-per-note format in non-test code (grep verified).

## Correctness Dimensions

| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | Single-threaded CLI path; no shared mutable state; `-race` run clean. |
| Error Handling | PASS | Adversarial search found no swallowed error on the destructive path: every I/O error wrapped ("export: ...") and propagated to a non-zero exit; fetch errors abort before any FS effect (executed: 3 tests); non-advancing cursor (hostile server input) detected and aborted. The one ignored error (`os.UserHomeDir`, export.go:178) degrades to a narrower guard — see Notes. |
| Resources | PASS | writeFileAtomic closes and removes the temp file on every failure path (CreateTemp/Rename failures executed with temp-file-lingering assertions; WriteString/Close paths correct by inspection); `defer client.Close()` on every exit from runExport. |
| Boundaries | PASS | Probed: empty export (executed — marker-only), empty entity IDs (countDroppedEdges skips them; edge with both endpoints empty counts dropped — correct, unlinkable), idPrefix over-length id (bounds-checked, 100% covered), 60-rune filename cap incl. re-trim after truncation, empty-after-sanitize fallback names. Worst-case 265-byte filename on ext4 would abort loudly, not corrupt — see Notes. |
| Security | PASS | See Security Focus: 37 combined hostile paths refused, hostile names confined with canary proof, symlink-to-home/root refused under --force, marker-laundering refused, clean-late ordering proven byte-for-byte. |

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | External input validated at entry | PASS | Untrusted prose/names pass sanitizeFilename/cleanInline barricades; server cursor (external input) checked for non-advance (export.go:120, executed test); flag/arg validation executed. |
| cc-defensive-programming | Security-critical paths re-validate inside the barricade (defense in depth) | PASS | confinedVaultPath re-verifies every renderer-produced path immediately before the write even though sanitization "should" make refusal unreachable — exactly the skill's rule; executed against 37 hostile inputs. |
| cc-defensive-programming | No empty catch blocks / no silently swallowed errors | PASS | Grep + read of both files: single ignored error (`os.UserHomeDir`) is guarded (`home != ""`) and note-level, not a demonstrated defect. |
| cc-defensive-programming | Assertions for bugs only / anticipated errors get handling | PASS | All runtime-anticipatable conditions (hostile input, OS errors) use error returns; the "should-be-unreachable" barricade still fails safe by aborting rather than asserting. |
| cc-defensive-programming | Correctness-vs-robustness stance fits the domain | PASS | Destructive-write path leans correctness: any confinement doubt aborts the whole export; catastrophic targets refused even under --force; clean-late ordering protects existing data. |
| cc-refactoring-guidance | Old format removed completely; behavior change is tested, not mixed with hidden fixes | PASS | Entity-per-note format has zero code residue; its absence is asserted by an executed test (no root-level notes); full suite green after the change; format replacement was the planned deliverable, not a smuggled refactor. |
| cc-refactoring-guidance | Small-change rigor: tests re-run and reported after change | PASS | Full suite + race + vet + gofmt executed by this review (Step 0), all clean. |

## Notes (non-blocking)

1. **TestPrepareVaultDir_AbsErrorWithDeletedCwd covers a different branch than its name claims on darwin** — the test passes, but the error it catches comes from MkdirAll, not filepath.Abs (coverage profile shows export.go:165-167 at 0 hits). The test is still valuable (deleted-cwd refusal end-to-end); the comment/name slightly oversells it on macOS.
2. **ext4 worst-case filename length**: dated event with a 60×4-byte-rune slug plus a collision suffix can reach ~265 bytes — over ext4's 255-byte name limit (APFS's 255-char limit accommodates it; reviewer-executed probe passes on darwin with 2 notes written). On ext4 the export would abort loudly with a wrapped ENAMETOOLONG, never escape or corrupt. A byte-aware cap (or lower rune cap for dated events) would remove the platform edge.
3. `home, _ := os.UserHomeDir()` (export.go:178): if the home lookup fails the home-clobber guard silently narrows to the root-only check. Unavoidable in substance (unknown home cannot be compared) but a stderr warning would make the degradation visible.
4. runExport's local `fs := flag.NewFlagSet(...)` shadows the `io/fs` package import within the function (checkVaultDir uses `fs.ErrNotExist` elsewhere). Legal and vet-clean; a different variable name would read cleaner.
5. WriteString/Close error blocks in writeFileAtomic are 2–3 statements (cleanup + return), marginally beyond the dispatch carve-out's "single-statement" wording; classified non-blocking on the carve-out's substance (non-portably fault-injectable OS errors, confirmed correct by inspection, with the sibling error paths tested including cleanup assertions).

## Issues (if FAIL)

None.

**Verdict: PASS**

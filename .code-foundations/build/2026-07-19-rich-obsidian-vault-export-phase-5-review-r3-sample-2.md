# Review: Phase 5 - vault assembly + CLI wiring (round 3, sample 2)

Reviewer: independent post-gate agent. All commands run from the worktree; adversarial tests run against a pristine copy under the session scratchpad (`scratchpad/r3rev2-p5/repo` — direct `/tmp/r3rev2-p5` writes are sandbox-denied; no repo files were mutated).

## Executed Results (Step 0)
- Test suite: `go test ./internal/cli/ -count=1` → **ok** (all tests pass, 0 failures)
- Typecheck: `go vet ./internal/cli/` → clean
- Lint: `gofmt -l ./internal/cli/` → **no output** (clean)
- Coverage: `go test ./internal/cli/ -cover` → 72.2% of statements; per-function via `go tool cover -func` below

## Requirement Fulfillment

### DW-5.1
PREMISE:  writes `events/`,`concepts/`,`maps/`; entity-per-note gone; `fetchExport` drains episodics.
EVIDENCE: internal/cli/vault.go:90–127 (writeVault three render loops), export.go:121–142 (fetchExport)
TRACE:    richPage fixture → CLI `export` → tree contains `events/2026/2026-03-01 Alpha shipped the beta.md`, `events/undated/Gamma joined Beta.md`, `concepts/Alpha.md`, `maps/Alpha.md`; no root-level `.md` (entity-per-note gone); episodics split across 3 pages all land as event notes.
VERDICT:  PASS — `TestDW_5_1_RichVaultLayoutEndToEnd`, `TestDW_5_1_WriteVaultRichLayout`, `TestDW_5_1_FetchExportDrainsEpisodicsAcrossPages` all executed passing.

### DW-5.2
PREMISE:  every write stays inside the vault dir incl. nested folders.
EVIDENCE: internal/cli/vault.go:57–82 (confinedVaultPath: root allowlist, exact depth, per-element dot/space refusal, final `filepath.Rel` re-check), vault.go:153–161 (barricade before any FS effect)
TRACE:    hostile relPaths (`../pwn.md`, `events/2026/../pwn.md`, `events\2026\pwn.md`, `/etc/passwd`, wrong-root, wrong-depth, empty element) → refusal, zero files; hostile entity/event names (`../../etc/passwd`, `..\..\win\shadow`, `/etc/shadow`) through real writeVault → all confined, canary untouched, no `root/etc` created.
VERDICT:  PASS — `TestDW_5_2_ConfinedVaultPathRejectsEscapes`, `TestDW_5_2_HostileNamesStayConfined` plus my `TestZZAdv_CraftedNames_Confined` (dot-soup `..`, `. .`, 300 dots, `café/../../x`, deep traversal first-lines) all executed passing.

### DW-5.3
PREMISE:  full-vault re-run byte-identical.
EVIDENCE: internal/cli/vaultmodel.go:409–431 (sorted (id, folder) assignment order), vaultmaps.go:262–310 (ascending-Key assignment); vault.go header determinism contract
TRACE:    same input → two fresh dirs → identical trees; reversed input → identical tree; my CLI-level double export into the SAME dir → identical trees incl. marker; my emoji-collision stress fixture re-run + permuted → identical trees.
VERDICT:  PASS — `TestDW_5_3_ReRunByteIdentical`, `TestZZAdv_CLI_ReRunSameDirByteIdentical`, determinism section of `TestZZAdv_ByteBudget_ManyCollidingLongEmoji` all executed passing.

### DW-5.4
PREMISE:  fetch failure leaves vault untouched; empty tenant → marker-only; clobber warning prints.
EVIDENCE: internal/cli/export.go:100–106 (prepareVaultDir only after fetch succeeds), export.go:111 (warning), export.go:218–221 (marker)
TRACE:    good export → tree snapshot; second run against non-advancing-cursor server → non-zero exit, tree byte-identical; empty page → vault contains only `.engram-vault`, summary `0 events, 0 concepts, 0 maps`; successful export prints `warning: … clobbered`.
VERDICT:  PASS — `TestDW_5_4_FetchFailureLeavesVaultIntact`, `TestDW_5_4_EmptyTenantMarkerOnlyVault`, `TestDW_5_4_ClobberWarningPrints` executed passing.

### DW-5.5
PREMISE:  summary reports events/concepts/maps/ghosts/dropped.
EVIDENCE: internal/cli/export.go:112–113 (summary line), vault.go:28–34 + 134–148 (vaultStats, countDroppedEdges)
TRACE:    richPage + half-dangling edge → `exported 2 events, 1 concepts, 1 maps to <dir> (2 ghosts, 1 dropped)`; half-dangling edge (one exported endpoint) correctly NOT counted dropped.
VERDICT:  PASS — `TestDW_5_5_SummaryCountsPrinted`, `TestDW_5_5_StatsCounts` executed passing.

**All requirements met:** YES (DW items); one primary-focus defeat found — see Issues.

## Primary focus — filename byte budget (adversarial verification)

Ran my own adversarial fixtures against the real `writeVault` in a scratch copy (`zz_adversarial_review_test.go`):

| Attack | Result |
|---|---|
| 12 events, one 60-emoji (240-byte) first line + one date (date prefix + forced collision suffixes) AND 4 hub concepts with distinct names whose slugs all rune-cap to the same 240-byte emoji base (4-cycle → all degree 2 → all get files), long name reaches maps/ via top concept | PASS — export succeeds, 17 files, max basename **239 bytes**, every basename ≤255 and valid UTF-8 |
| Residual id-extension growth: 100 colliding 320-byte all-emoji bases whose ids share the first 24 bytes (id extension can never disambiguate → counter path) | PASS — all names unique, ≤240-byte budget, valid UTF-8 |
| Truncation splitting a rune in the TITLE path | PASS — `truncateBytes` backs up to rune start (`TestTruncateBytes_RuneBoundary`); no invalid UTF-8 from title/slug truncation |
| Determinism under name stress (re-run + permuted input) | PASS — byte-identical trees |
| **Multibyte-rune entity ID → collision suffix** | **CONFIRMED DEFECT** — see Issues #1 |

## Test-DW Coverage
- [x] All DW items have corresponding automated tests that ran in Step 0 (test names carry DW ids: `TestDW_5_1_*` … `TestDW_5_5_*`, `TestDW_Fix_LongNamesFitNameMax`)
- [x] Coverage matches the stated level: new helpers (`idPrefix`, `sanitizeFilename`, `truncateBytes`, `fitNoteName`, `uniqueNoteName`, `cleanInline`) and refactored `buildVaultRefs`/`assignClusterFilenames` are all **100.0%**; `runExport` 96.8% / `prepareVaultDir` 88.9% misses are the accepted non-portable OS-error returns (non-blocking per dispatch)

## Dead Code
None found (no debug statements, TODOs, commented-out blocks, or unreachable code in the reviewed files; `go vet` clean).

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | Single-threaded CLI path; no shared mutable state, no goroutines in reviewed code |
| Error Handling | PASS | Every I/O and RPC error wrapped and propagated (`export:` prefix); cursor-stall guard aborts a looping server (export.go:137); writeFileAtomic cleans its temp file on every failure branch (verified: no `.engram-tmp-*` lingers, `TestWriteFileAtomic_ErrorPaths`) |
| Resources | PASS | Client `defer client.Close()` (export.go:99); temp files removed on all three failure paths (vault.go:166–186); no handles leaked in loops |
| Boundaries | FAIL | `idPrefix` (export.go:237) byte-slices an id without rune-boundary care — demonstrated invalid-UTF-8 filename + EILSEQ export abort (Issues #1). Title/slug truncation paths are rune-safe (traced + executed) |
| Security | PASS | Path confinement held under every hostile input I constructed, including separator-bearing IDs (`concepts/concept (../../pw).md` → barricade refusal, nothing escaped, canary intact); `--force` into symlink→scratch-$HOME and symlink→/ refused before any cleaning (`TestPrepareVaultDir_SymlinkToHomeRefused/RootRefused`); failed fetch never clobbers |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | External input validated at entry ("internal team API is still external") | FAIL | Entity/event IDs cross the RPC boundary and flow **raw** into filenames via the collision suffix (`uniqueNoteName` → `idPrefix`), unlike every other server field (names/prose/predicates are sanitized, and the server's cursor is explicitly distrusted at export.go:117–120). Demonstrated: Issues #1 |
| cc-defensive-programming | No empty catch blocks / errors not swallowed | PASS | All error returns propagate; the one tolerated error (`EvalSymlinks` ErrNotExist, export.go:190–194) is deliberate, commented, and correct for a not-yet-created dir |
| cc-defensive-programming | Barricade design: validation at boundary, re-verified on security-critical path | PASS | `confinedVaultPath` re-verifies every renderer path immediately before each write (defense in depth); catastrophic-target guard resolves symlinks on both sides before comparing |
| cc-defensive-programming | Correctness over robustness for data-destroying operations | PASS | Fetch-before-clean ordering; foreign-dir refusal; marker ownership; `--force` still refuses `/` and `$HOME` |
| cc-refactoring-guidance | Behavior preserved by the filename-assignment refactor (same suffixing semantics, single shared algorithm) | PASS | `uniqueNoteName` is the single suffixing path for events+concepts (vaultmodel.go:425) and maps (vaultmaps.go:307); determinism, collision disambiguation, and misc-bucket immunity all re-verified by execution (my `TestZZAdv_MiscBucketImmunity`: concept named `misc-01` → `maps/misc-01 (c-a1).md`, real bucket keeps canonical `maps/misc-01.md`) |
| cc-refactoring-guidance | No regression across the refactor (tests pass, small-change rigor) | PASS | Full suite green; permuted-input and re-run trees byte-identical under collision stress |

## Notes (non-blocking)
- `writeFileAtomic` 66.7% and `confinedVaultPath` 93.3% coverage misses are the same non-portable OS-error-return class the dispatch already accepts for `runExport`/`prepareVaultDir` (WriteString/Close failures, `filepath.Rel` error).
- Separator-bearing IDs (e.g. `../../pwn-id-x`) are the second face of Issues #1: confinement **holds** (barricade refuses `concepts/concept (../../pw).md`, nothing escapes — executed), but the refusal aborts the export after the old vault was cleaned. Fixing Issues #1 by sanitizing suffix material closes this variant too.
- `used`-map case-insensitivity (`strings.ToLower`) correctly guards case-folding filesystems (APFS); Unicode case-folding beyond `ToLower` (e.g. dotless-i) is theoretically imperfect but undemonstrated — not pursued.

## Issues (FAIL blockers)
1. **Invalid-UTF-8 filename from the collision suffix: `idPrefix` splits a multibyte rune, aborting the export AFTER the old vault was clobbered** — the exact data-loss class this round's fix targets, and a listed FAIL condition ("invalid-UTF-8 filename").
   - File: internal/cli/export.go:237–242 (`idPrefix`, used by `uniqueNoteName` at export.go:310–318)
   - Demonstrated by: `TestZZAdv_Probe_MultibyteID_EndToEnd` (scratch copy) — entity `{ID: "ab🦄🦄rest-of-id", Name: "..."}` as a degree-2 hub → forced suffix ` (` + `id[:8]` + `)` → `id[:8]` = `ab🦄` + **2 orphan bytes of the second 🦄** → basename `concept (ab🦄\xf0\x9f).md` → real `writeVault` fails on APFS: `rename …: illegal byte sequence` — export aborted after `prepareVaultDir` already emptied the vault (on Linux the invalid-UTF-8 name would instead be written to disk; both outcomes are listed FAIL conditions). Unit trace: `buildVaultRefs` returns the invalid ref (`TestZZAdv_Probe_MultibyteID`).
   - Why in scope: IDs are external input crossing the RPC boundary (loaded cc-defensive-programming: "internal team API is still external"), and the code already distrusts this same server elsewhere (cursor-stall guard, export.go:117–120; every other server field is sanitized before touching a path). `idPrefix` is a truncation that can split a UTF-8 rune — precisely the defeat vector this round's focus names — and it is the one un-barricaded route from server bytes into a filename.
   - Fix: make the id suffix rune-safe and path-safe — e.g. `idPrefix` → `truncateBytes(id, n)` (the rune-safe helper already exists at export.go:275) **and** pass suffix material through a character filter (reuse `sanitizeFilename`'s illegal-rune/control-char stripping) so a separator- or control-bearing id can neither abort post-clobber nor reach the barricade. Add a regression test with multibyte and separator ids.

**Verdict: FAIL — blocker #1 (invalid-UTF-8 filename / post-clobber export abort via unsanitized id bytes in the collision suffix). All five DW items, path confinement, determinism, misc-bucket immunity, catastrophic guards, coverage, and gofmt otherwise pass.**

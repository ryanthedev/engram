# Review: Phase 5 - Vault assembly + CLI wiring (r4, sample 1)

## Executed Results (Step 0)
- Test suite: `go test ./internal/cli/ -count=1` → ok (107 tests, 0 failures; re-verified verbose)
- Typecheck: `go vet ./internal/cli/` → clean
- Lint: `gofmt -l ./internal/cli/` → no output (clean)
- Coverage: `go test ./internal/cli/ -cover` → 72.5% of statements (targets below)
- Reviewer adversarial tests: 4 new test functions + NFC/NFD probe, injected via `go test -overlay` (repo tree untouched) → all PASS

Mid-review a foreign untracked file `internal/cli/zz_adv_review_test.go` (another review sample's scratch, non-compiling) appeared in the shared worktree; it was neutralized via the overlay for all runs. The phase suite passes without it. It is NOT phase code and must not be committed.

## Requirement Fulfillment

### DW-5.1
PREMISE:  writes `events/`,`concepts/`,`maps/`; entity-per-note gone; `fetchExport` drains episodics.
EVIDENCE: internal/cli/vault.go:90-127 (writeVault: three render loops → events/, concepts/, maps/), internal/cli/export.go:121-142 (fetchExport accumulates Episodics+Entities+Edges per page until empty NextCursor)
TRACE:    richPage fixture → `Run(["export", dir])` → fetchExport drains pages (incl. a 3-page split with episodics on pages 1-2) → writeVault → `events/2026/2026-03-01 Alpha shipped the beta.md`, `events/undated/Gamma joined Beta.md`, `concepts/Alpha.md`, `maps/Alpha.md`; no root-level `.md` (entity-per-note gone), ghost `Beta` gets no file.
VERDICT:  PASS — TestDW_5_1_WriteVaultRichLayout, TestDW_5_1_RichVaultLayoutEndToEnd, TestDW_5_1_FetchExportDrainsEpisodicsAcrossPages all executed and passing.

### DW-5.2
PREMISE:  every write stays inside the vault dir incl. nested folders.
EVIDENCE: internal/cli/vault.go:57-82 (confinedVaultPath: root allowlist, exact depth per root, per-element dot/space refusal, final filepath.Rel re-check), vault.go:153-162 (writeVaultNote confines before any FS effect)
TRACE:    22 hostile relPaths (`../pwn.md`, `events/2026/../pwn.md`, backslashes, `/etc/passwd`, wrong roots/depths, empty elements) → all refused with zero files created; hostile episodic titles and entity names (`../../etc/passwd`, `..\..\win\shadow`) through real writeVault → all 5 notes confined, canary outside vault untouched, no `etc/` dir created.
VERDICT:  PASS — TestDW_5_2_ConfinedVaultPathRejectsEscapes, TestDW_5_2_HostileNamesStayConfined, plus reviewer TestReview_WriteVault_HostileEverything (walks entire scratch root asserting confinement), all executed and passing.

### DW-5.3
PREMISE:  full-vault re-run byte-identical.
EVIDENCE: internal/cli/vaultmodel.go (sorted-key drains throughout), vault_test.go:545-563
TRACE:    same records → writeVault into dirA, dirB → trees byte-identical; reversed input slices → dirC byte-identical to dirA; CLI-level owned-dir re-run regenerates (TestExport_RerunClobbersOwnedDir); reviewer hostile fixture (multibyte/invalid-UTF-8/separator ids) permuted → byte-identical.
VERDICT:  PASS — TestDW_5_3_ReRunByteIdentical, TestDW_Fix_MultibyteIDsSafeBasenames (re-run leg), TestReview_WriteVault_HostileEverything (permuted leg), all executed and passing.

### DW-5.4
PREMISE:  fetch failure leaves vault untouched; empty tenant → marker-only; clobber warning prints.
EVIDENCE: internal/cli/export.go:100-110 (fetchExport before prepareVaultDir — clean-late ordering), export.go:111 (warning line)
TRACE:    export succeeds → vault exists; second run against non-advancing-cursor server → exit != 0, tree byte-identical before/after; empty ExportResponse into a missing nested dir → exactly one file (`.engram-vault` marker), summary "0 events, 0 concepts, 0 maps"; every successful export prints "warning: … will be clobbered".
VERDICT:  PASS — TestDW_5_4_FetchFailureLeavesVaultIntact, TestDW_5_4_EmptyTenantMarkerOnlyVault, TestDW_5_4_ClobberWarningPrints, plus TestExport_NonAdvancingCursorAborts / TestExport_FetchErrorAborts / TestExport_DialErrorSurfaces (dir never created on failed fetch), all executed and passing.

### DW-5.5
PREMISE:  summary reports events/concepts/maps/ghosts/dropped.
EVIDENCE: internal/cli/export.go:112-113 (summary Fprintf), vault.go:28-34 + 129-148 (vaultStats; countDroppedEdges counts only both-endpoints-unexported edges)
TRACE:    richPage + one half-dangling edge → stats {2 events, 1 concept, 1 map, 2 ghosts, 1 dropped} (half-dangling edge lands as a claim, NOT dropped); CLI prints exactly "exported 2 events, 1 concepts, 1 maps to <dir> (2 ghosts, 1 dropped)".
VERDICT:  PASS — TestDW_5_5_SummaryCountsPrinted, TestDW_5_5_StatsCounts, executed and passing.

**All requirements met:** YES

## PRIMARY focus — attempts to produce an unsafe basename

Reviewer-authored adversarial tests (run via `-overlay`, all passing; scratch under /tmp/r4rev1-p5):

| Attack | Vector | Result |
|---|---|---|
| Multibyte id sliced into suffix | 3-byte CJK and 4-byte emoji ids where every prefix width 8/12/16/20/24 cuts mid-rune | truncateBytes backs up to rune start; all names `utf8.Valid` |
| Hostile id characters | ids with `/`, `\`, `..`, NUL, control chars, `C:\...` | separators → `-`, control dropped by safeNoteName on the ASSEMBLED name; no escape, no abort |
| Invalid-UTF-8 id | `\xff\xfe…`, lone partial 4-byte rune, 30–40 bytes of `\xff` | `strings.ToValidUTF8` strips; suffix degrades to `()`, counter path keeps uniqueness |
| Title sanitizes to empty | `..`, control-only, invalid-only titles | "event"/"concept"/"map" fallback + forced id suffix; safeNoteName floor `"note"` unreachable but present |
| Mass collisions (80× same base+id) | CJK base + CJK id, emoji base + emoji id, invalid-UTF-8 id, all-NUL id, empty id, all-`/` id | residual id-extension then counter path; 80 distinct names each ≤ 240 bytes incl. `.md`, all valid |
| 4-byte rune at length boundary | 320-byte emoji base with suffix lengths 0–11 (cut lands on every offset mod 4); 236-byte + straddling rune; dot-before-rune | budget held, suffix never truncated, no partial rune, no trailing dot/space |
| End-to-end pipeline | 14 hostile episodics + 7 hostile entities through real writeVault | export SUCCEEDS (no clobber-then-abort), every written basename valid UTF-8 / ≤255 bytes / no fsIllegal / confined; canary intact; permuted re-run byte-identical |

Why it holds (traced): uniqueNoteName (export.go:348-365) routes EVERY candidate — bare, homonym-suffixed, residual-extended, counter — through fitNoteName (byte budget; truncates only the base, on a rune boundary, re-trimmed) then safeNoteName (export.go:313-335: ToValidUTF8 → control-drop → fsIllegal→`-` → trim `". "` → 237-byte rune-safe cap → non-empty floor) BEFORE the uniqueness check, and records `used` on the final form. safeNoteName never lengthens (fsIllegal runes are all 1-byte ASCII mapped to 1-byte `-`; everything else dropped or kept), so a fitNoteName-budgeted input stays budgeted. The counter's ASCII digits survive safeNoteName verbatim and the suffix always fits the budget, so termination and distinctness hold even when the id contributes nothing (empty/NUL/invalid ids). The misc-immune path (vaultmaps.go:296-305) still passes safeNoteName. **No unsafe basename was produced from any field.**

## Regression — uniqueNoteName routing through safeNoteName
- Determinism: export twice + fully permuted input → byte-identical trees, on both the benign fixture (TestDW_5_3) and the hostile multibyte-id fixture (TestDW_Fix_MultibyteIDsSafeBasenames, TestReview_WriteVault_HostileEverything). PASS.
- Uniqueness under sanitization collapse: `Dup/Name`, `Dup-Name`, `Dup\Name` (and `a/b` vs `a-b` id-suffix material) collapse to one sanitized base but the used-map is keyed on the FINAL safeNoteName form, so the clash loop re-fires until distinct — 80-way same-base/same-id floods produced 80 distinct in-budget names, terminating via the counter. PASS.
- Misc-bucket immunity: concept titled `misc-01` is forced through the suffix path (`maps/misc-01 (日本).md` observed) and never bumps the canonical `misc-NN`. PASS (TestDW_Fix_MultibyteIDsSafeBasenames fixture assertion executed).

## Security (must still hold)
- Path confinement: hostile names/separators refused or flattened; nothing outside the vault (executed, incl. reviewer canary walk). HOLDS.
- `--force` into symlink→scratch-`$HOME`: refused with "refusing to clobber", canary `precious.txt` intact (TestPrepareVaultDir_SymlinkToHomeRefused, executed); symlink→`/` refused (TestPrepareVaultDir_SymlinkToRootRefused). HOLDS.
- Failed fetch never clobbers: fetch precedes prepareVaultDir (export.go:100-104); existing vault byte-identical after failed re-run; dir never created on fetch/dial failure. HOLDS.

## Coverage & lint
| Function | Coverage |
|---|---|
| idPrefix, sanitizeFilename, truncateBytes, fitNoteName, safeNoteName, uniqueNoteName | 100% |
| buildVaultRefs, assignClusterFilenames, writeVault, writeVaultNote, fetchExport | 100% |
| runExport | 96.8% — miss is export.go:108-110, writeVault-error plumbing (covered at writeVault level) |
| prepareVaultDir | 88.9% — misses are export.go:182-184 (Abs failure; darwin's Getwd survives a deleted cwd), 192-194 (EvalSymlinks non-NotExist error), 210-212 (post-MkdirAll ReadDir race) — non-portable OS-error returns, accepted |
| confinedVaultPath | 93.3% — miss is vault.go:78-80, the final filepath.Rel re-check: by-design-unreachable defense-in-depth |

`gofmt -l ./internal/cli/` → clean. `go vet` → clean.

## Dead Code
None found. No leftover entity-per-note code, no debug statements, no TODO/FIXME, no unreachable code after early returns in the reviewed files.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | Single-threaded CLI pipeline; the only goroutine is the test stub server |
| Error Handling | PASS | Probed blocked root dirs, read-only parents/vaults, symlink loops, unlistable dirs, deleted cwd, dead/unparsable servers, non-advancing cursors — every failure propagates wrapped and aborts loudly (executed) |
| Resources | PASS | writeFileAtomic closes+removes temp on every failure path (executed TestWriteFileAtomic_ErrorPaths asserts no `.engram-tmp-*` lingers); `defer client.Close()` export.go:99 |
| Boundaries | PASS | Empty export → marker-only; empty ids skipped; nil OccurredAt → undated; 4-byte rune at every byte-budget offset traced and executed (reviewer tests) |
| Security | PASS | Confinement + symlink + no-clobber all executed with canaries; per-write barricade re-verification (writeVaultNote) is defense-in-depth on the untrusted-name path |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | External input validated at entry (untrusted prose/names/ids → paths) | PASS | Renderer barricades + confinedVaultPath before EVERY write; adversarial pipeline run produced zero escapes/invalid names |
| cc-defensive-programming | Barricade does not replace defense-in-depth on security-critical path | PASS | Names sanitized upstream AND re-confined per write (vault.go:153-161); refusal aborts, never escapes |
| cc-defensive-programming | No empty catch / swallowed errors | PASS | All errors wrapped+returned; ignored errors are only best-effort cleanup (`os.Remove` after a primary error) and the explicitly-guarded `home==""` degradation (see Notes) |
| cc-defensive-programming | Correctness-vs-robustness fits domain (user data) | PASS | Correctness-leaning: clean-late ordering, catastrophic-target refusal even under --force, bug-stop on barricade refusal |
| cc-refactoring-guidance | Naming refactor is behavior-preserving (determinism, uniqueness, misc immunity) | PASS | Byte-identical re-runs + permuted input, 80-way collision floods distinct, misc-NN never bumped — all executed |
| cc-refactoring-guidance | Working-code discipline: suite green, pickiest checks clean | PASS | 107/107 tests, gofmt clean, go vet clean |

## Notes (non-blocking)
1. **Unicode-normalization collision (demonstrated, outside this review's FAIL scope):** on APFS, concept names "café" (NFC) and "café" (NFD) produce byte-distinct basenames that the filesystem treats as ONE file — reviewer probe showed stats.Concepts=4 but 3 files on disk (the later atomic rename silently replaces the earlier note; deterministic, no abort, no escape, no invalid name). This is the normalization-folding sibling of the case-folding hazard the `used` map already handles via ToLower. Fix suggestion: NFC-normalize the name (or the `used` key) in safeNoteName. Not a FAIL here: it is not in the dispatch's FAIL definition (valid UTF-8, ≤255 bytes, legal chars, confined, no write failure — all hold), and the collapse is the filesystem's, not the sanitizer's (uniqueNoteName's own names stay distinct).
2. **Foreign scratch file in the shared worktree:** untracked, non-compiling `internal/cli/zz_adv_review_test.go` appeared mid-review (concurrent review sample). Neutralized via `-overlay` for all runs here; left in place for its owner. The orchestrator must ensure it is not committed with the phase.
3. `home, _ := os.UserHomeDir()` (export.go:195) ignores the error; the guard degrades gracefully (`home != ""` skips only the home comparison, the root check still applies). Acceptable, worth a comment at most.
4. runExport's writeVault-error branch (export.go:108-110) is uncovered at the CLI level; the same failure class is executed at the writeVault level (TestWriteVault_WriteFailurePropagates). Accepted per dispatch.

## Issues (if FAIL)
None.

**Verdict: PASS**

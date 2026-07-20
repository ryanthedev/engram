# Review: Phase 5 - vault assembly + CLI wiring (round 3, sample 1)

## Executed Results (Step 0)
- Test suite: `go test ./internal/cli/ -count=1` → ok (all tests pass), 0.054s
- Coverage: `go test ./internal/cli/ -cover` → 72.2% of statements
- Typecheck: `go vet ./internal/cli/` → clean (exit 0)
- Lint: `gofmt -l ./internal/cli/` → no output (clean)
- Independent adversarial probes (overlay-injected, repo tree untouched): 4 PASS, **2 FAIL** — see Issues.

## Requirement Fulfillment

### DW-5.1
PREMISE:  writes `events/`,`concepts/`,`maps/`; entity-per-note gone; `fetchExport` drains episodics.
EVIDENCE: internal/cli/vault.go:101-125 (three render loops), export.go:121-142 (fetchExport accumulates episodics/entities/edges until empty cursor)
TRACE:    richPage fixture → CLI `export <dir>` → tree contains `events/2026/2026-03-01 Alpha shipped the beta.md`, `events/undated/Gamma joined Beta.md`, `concepts/Alpha.md`, `maps/Alpha.md`; no root-level .md (entity-per-note gone); 3-page fixture with episodics split across pages drains both events.
VERDICT:  PASS — TestDW_5_1_RichVaultLayoutEndToEnd, TestDW_5_1_FetchExportDrainsEpisodicsAcrossPages, TestDW_5_1_WriteVaultRichLayout all pass (executed).

### DW-5.2
PREMISE:  every write stays inside the vault dir incl. nested folders.
EVIDENCE: internal/cli/vault.go:57-82 (confinedVaultPath: root allowlist, exact depth per root, per-element dot/space refusal, final Rel re-check), vault.go:153-161 (writeVaultNote confines before any filesystem effect)
TRACE:    hostile names (`../../etc/pwn`, `..\..\win\pwn`, `/etc/passwd`) as event titles AND entity names → writeVault succeeds, WalkDir over the parent root finds every file inside the vault, canary untouched, no `etc/` created outside. 22-path refusal matrix (abs, `..`, wrong depth, unknown root, backslash, empty element) all refused. My probes added `events/. ./x.md`, trailing `..` element, `Events/` case-variant root, and a NUL-bearing element (refused loudly at the syscall, nothing written) — all held.
VERDICT:  PASS — TestDW_5_2_ConfinedVaultPathRejectsEscapes, TestDW_5_2_HostileNamesStayConfined, TestREV_PathEscape_Crafted pass (executed).

### DW-5.3
PREMISE:  full-vault re-run byte-identical for the same export input.
EVIDENCE: internal/cli/vault_test.go:444-462; determinism machinery in vaultmodel.go (sorted candidate order at buildVaultRefs:409-414) and vaultmaps.go (sorted Key order at assignClusterFilenames:263-264)
TRACE:    same records → writeVault into dirA and dirB → trees byte-identical; REVERSED input slices → dirC tree byte-identical to dirA.
VERDICT:  PASS — TestDW_5_3_ReRunByteIdentical passes (executed).

### DW-5.4
PREMISE:  fetch failure leaves an existing vault untouched; empty tenant → marker-only; clobber warning prints.
EVIDENCE: internal/cli/export.go:100-107 (fetch precedes prepareVaultDir — clean-late), export.go:111 (warning line), export.go:218-221 (marker write)
TRACE:    good export → non-advancing-cursor server → exit != 0, before/after trees byte-identical. Empty page → missing nested dir created, tree = {`.engram-vault`} only, "0 events, 0 concepts, 0 maps" printed. Successful export prints "warning: … clobbered".
VERDICT:  PASS — TestDW_5_4_FetchFailureLeavesVaultIntact, TestDW_5_4_EmptyTenantMarkerOnlyVault, TestDW_5_4_ClobberWarningPrints pass (executed). (As WORDED this item holds — a *fetch* failure never clobbers. A *write* failure after the clobber can still destroy the vault; see Issue 1.)

### DW-5.5
PREMISE:  summary reports events/concepts/maps/ghosts/dropped.
EVIDENCE: internal/cli/export.go:112-113 (summary Fprintf), vault.go:28-34 + 90-127 (stats), vault.go:134-148 (countDroppedEdges: dropped = NEITHER endpoint exported)
TRACE:    richPage + one half-dangling edge → "exported 2 events, 1 concepts, 1 maps to <dir> (2 ghosts, 1 dropped)"; the half-dangling edge lands as a claim and is NOT counted dropped.
VERDICT:  PASS — TestDW_5_5_SummaryCountsPrinted, TestDW_5_5_StatsCounts pass (executed).

**All requirements met:** YES (as worded) — but the round's primary-focus criterion FAILS, see below.

## Edge Cases (prompt-listed)
| Edge case | Status | Evidence |
|---|---|---|
| nested path-escape via crafted name → refused | PASS | TestDW_5_2_* + TestREV_PathEscape_Crafted (executed) |
| empty tenant → marker-only vault | PASS | TestDW_5_4_EmptyTenantMarkerOnlyVault (executed) |
| slug collisions across folders → disambiguated by VaultRefs | PASS | TestVaultRefsFoldersFilesAndCollisions (event "Widget" vs concept "Widget" cross-folder homonyms both suffixed), TestVaultRefsResidualClashExtendsSuffix, TestVaultRefsCrossKindIDCollision, TestDW_4_1_TitleCollisionSuffixed, TestDW_4_1_ConceptTitleCollidesWithMiscPrefixReserved (misc-bucket immunity) — all pass (executed) |

## Primary Focus — filename BYTE budget
Independent adversarial probes (overlay-injected `TestREV_*`, run against the real `writeVault`/`uniqueNoteName`, repo tree untouched):

| Probe | Result |
|---|---|
| 30 homonym events (same 60-emoji/240-byte first line, same date, long ids) + 30 concepts whose distinct names rune-cap to the same 60-emoji slug, chained into one component (concepts + a map with the emoji title) → real writeVault | PASS — export succeeds, 59 files, worst basename 240 bytes, all ≤255, all valid UTF-8 |
| All-4-byte-rune base (80×🌍 = 320 bytes), 200 forced collisions through uniqueNoteName (deep into id-extension AND counter suffix paths) | PASS — every candidate ≤ maxNoteBaseBytes+3, valid UTF-8, all 200 distinct |
| Truncation splitting a rune (truncateBytes table incl. mid-emoji cuts) | PASS — TestTruncateBytes_RuneBoundary (executed) |
| **Multibyte entity ID → collision suffix** | **FAIL — invalid-UTF-8 filename + real write failure + old vault destroyed. See Issue 1.** |

The length vector is genuinely closed: `fitNoteName` truncates only the base, on a rune boundary, re-trims trailing dots/spaces, and `uniqueNoteName` keeps every candidate inside the budget with a terminating counter. But the **encoding vector is open**: `idPrefix` (export.go:237-242) slices **bytes** (`id[:n]`), and the id-prefix suffix bypasses `sanitizeFilename` entirely — an id whose 8th (or 12th/…/24th) byte falls mid-rune puts raw invalid UTF-8 into a written basename. The prompt's FAIL rule ("any invalid-UTF-8 filename, or any write failure … is a FAIL") is met, and the demonstrated consequence is the exact clobber-then-abort data loss this round set out to close.

## Regression Focus — filename-assignment refactor
- Determinism: TestDW_5_3_ReRunByteIdentical (same input twice + reversed input → byte-identical trees), TestDW_4_1_DeterministicAcrossRuns — PASS (executed).
- Concept/map collision disambiguation: TestDW_4_1_TitleCollisionSuffixed, TestFilenameCollision_ThreeWayForcesExtendedSuffix, TestVaultRefs* — PASS (executed).
- Misc-bucket immunity: vaultmaps_test.go:215/226 — a real misc bucket keeps canonical unsuffixed `maps/misc-01.md` and a concept title sanitizing to `misc-*` is force-suffixed — PASS (executed).
- The refactor onto shared `uniqueNoteName` (vaultmodel.go:425, vaultmaps.go:307) preserves the Phase 2–4 algorithm (all-homonyms-suffixed, sorted assignment order, case-insensitive `used`); no behavioral drift observed in any executed test.

## Security Refocus
- Path confinement: real confinedVaultPath/writeVault, hostile `../`/absolute/separator names refused, canary survives, nothing outside the vault — PASS (executed; my crafted-name probes also held).
- Catastrophic guard: `--force` into a symlink→scratch-$HOME refused (scratch HOME via t.Setenv, precious file survives), symlink→/ refused — TestPrepareVaultDir_SymlinkToHomeRefused, TestPrepareVaultDir_SymlinkToRootRefused — PASS (executed).
- Failed fetch never clobbers: TestDW_5_4_FetchFailureLeavesVaultIntact, TestExport_FetchErrorAborts, TestExport_NonAdvancingCursorAborts (vault dir not even created) — PASS (executed).

## Test-DW Coverage
- [x] Every DW item has DW-ID-named automated tests that ran in Step 0 (DW-5.1/5.2/5.3/5.4/5.5 across export_test.go and vault_test.go).
- [x] Byte-budget fix has dedicated tests (TestDW_Fix_LongNamesFitNameMax, TestTruncateBytes_RuneBoundary, TestFitNoteName_ByteBudget, TestUniqueNoteName_BoundedGrowth).
- Coverage: `truncateBytes` 100%, `fitNoteName` 100%, `uniqueNoteName` 100%, `buildVaultRefs` 100%, `assignClusterFilenames` 100% (go tool cover -func). Remaining misses: runExport 96.8% (writeVault-error return, export.go:108), prepareVaultDir 88.9% (filepath.Abs / EvalSymlinks-non-NotExist / post-Mkdir ReadDir OS-error returns at 182/192/210), confinedVaultPath 93.3% (the documented-unreachable final Rel re-check, vault.go:78), writeFileAtomic 66.7% (WriteString/Close error branches, non-portable). All single-statement OS-error returns or defense-in-depth barricades — matches the previously-accepted set; non-blocking.

## Dead Code
None found. No unused imports (build + vet clean), no debug statements, no commented-out blocks, no unreachable code after early returns. The uncovered confinedVaultPath re-check is live defense-in-depth, not dead code.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | single-threaded CLI command; no goroutines or shared mutable state in the reviewed code |
| Error Handling | PASS | every I/O error wrapped and propagated (writeVault aborts on first write error); non-advancing-cursor guard on external pagination; no swallowed errors |
| Resources | PASS | temp files removed on every writeFileAtomic failure path (TestWriteFileAtomic_ErrorPaths asserts no `.engram-tmp-*` lingers); client closed via defer (export.go:99) |
| Boundaries | **FAIL** | `idPrefix` slices bytes mid-rune: `idPrefix("日本語-entity-x", 8)` = `日本` + 2 of 3 bytes of `語` → invalid UTF-8 in a written filename (demonstrated, TestREV_MultibyteID_SuffixSplitsRune) — Issue 1 |
| Security | PASS | path confinement holds under every hostile probe (nothing ever escapes the vault, including in the Issue-1 scenario — the failure is a loud in-vault EILSEQ, not an escape); catastrophic guard + symlink resolution verified |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | External input validated at entry (barricade) | **FAIL** | Entity/event ids are external input by the package's own security model (vault.go:9-14 "note paths derive from UNTRUSTED ingested content"; export.go:120 treats the cursor as external), yet the collision-suffix path injects raw id bytes into filenames, bypassing the sanitizeFilename barricade — demonstrated write failure + data loss (Issue 1) |
| cc-defensive-programming | No empty catch blocks / errors never swallowed | PASS | every error path returns a wrapped error; intentional best-effort cases (EvalSymlinks fallthrough, os.Remove after failed rename) are documented and safe |
| cc-defensive-programming | Assertions for bugs only / no executable code in assertions | N/A | Go; no assertion mechanism used — bug-stops are explicit refusals (confinedVaultPath), correct for this correctness-leaning domain |
| cc-defensive-programming | Barricade reduces redundant validation, defense-in-depth on security paths | PASS | confinedVaultPath re-verifies every renderer path immediately before write; prepareVaultDir re-checks the dir post-dial; catastrophic guard survives --force |
| cc-refactoring-guidance | Refactor preserves observable behavior (same tests pass before/after) | PASS | determinism byte-identical across runs and permuted input; all Phase 2-4 collision/misc-immunity tests still pass against the refactored uniqueNoteName path |
| cc-refactoring-guidance | Refactor toward an existing target pattern | PASS | buildVaultRefs and assignClusterFilenames now share one suffixing algorithm (uniqueNoteName) instead of two divergent copies — duplication removed, single point of truth for the byte budget |

## Notes (non-blocking)
- This phase combines a fix (byte budget) with a refactor (shared uniqueNoteName) in one change; cc-refactoring-guidance prefers separate commits. Commit hygiene is the orchestrator's call — noted only.
- `fitNoteName` returns an over-budget name when the suffix alone exceeds the budget (export.go:291-295; vault_test.go:357-360 pins this) — unreachable in practice (worst real suffix ≈ 35 bytes: 24 id bytes + counter), so the pinned behavior is acceptable, but it is the one input class where the budget is not absolute.
- A fully-truncated base yields a leading-space filename (`" (id…)"`); legal and confined, merely odd.
- `eventTitle`'s id fallback (`idPrefix(eventID, 8)`, vaultmodel.go:364) has the same byte-slicing behavior, but sanitizeFilename launders it for filenames (invalid bytes become U+FFFD via WriteRune); only the note *content*/display could carry replacement chars. Fixing idPrefix (Issue 1) covers this too.
- `concepts/Alpha.md` and `maps/Alpha.md` share a basename across folders by design (piped wikilinks disambiguate poorly in Obsidian's shortest-path resolution); pre-existing Phase 4 design, out of scope.

## Issues (FAIL)
1. Collision suffix embeds raw id bytes sliced mid-rune → invalid-UTF-8 filename → APFS write fails EILSEQ AFTER the old vault was clobbered (data loss — the exact class this round's fix targets, via the encoding vector instead of the length vector).
   - File: internal/cli/export.go:241 (`idPrefix` — `return id[:n]`, a byte slice), reached from export.go:313-318 (`uniqueNoteName` suffix construction, which bypasses `sanitizeFilename`)
   - Demonstrated by: overlay probes `TestREV_MultibyteID_SuffixSplitsRune` (uniqueNoteName("...", "日本語-entity-x", true, …) → `" (日本\xe8\xaa)"`, bytes `20 28 e6 97 a5 e6 9c ac e8 aa 29`; real writeVault then fails `rename …: illegal byte sequence`) and `TestREV_MultibyteID_DestroysOldVault` (full CLI: first export OK; re-export against a server serving that entity id → exit 1 AND all four pre-existing notes gone — clobber-then-abort data loss). Probe file: /tmp/r3rev1-p5/rev_adversarial_test.go (overlay /tmp/r3rev1-p5/overlay.json).
   - TRACE: entity `{ID: "日本語-entity-x", Name: "..."}` → name sanitizes empty → forced suffix → `idPrefix(id, 8)` = `id[:8]` splits `語` (e8 aa 9e) after 2 bytes → basename `concept (日本\xe8\xaa).md` → confinedVaultPath accepts (no UTF-8 check) → os.Rename → EILSEQ on APFS → writeVault aborts → runExport returns AFTER prepareVaultDir already emptied the dir → old vault destroyed.
   - Fix: make the id prefix rune-safe — e.g. `func idPrefix(id string, n int) string { return truncateBytes(id, n) }` (helper already exists, deterministic, keeps every existing ASCII-id test green), or sanitize/validate ids at the fetch barricade. Add a regression test with a multibyte id through the forced-suffix path asserting `utf8.ValidString` on every written basename.

**Verdict: FAIL — blocker: Issue 1 (invalid-UTF-8 filename from byte-sliced id suffix; demonstrated write failure and old-vault destruction, violating the round's primary-focus criterion and the loaded defensive-programming skill's external-input barricade).**

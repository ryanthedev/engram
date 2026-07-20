# Review: Phase 5 - Vault assembly + CLI wiring (r2, sample 3)

## Executed Results (Step 0)
- Test suite: `go test ./internal/cli/ -count=1` → ok (all pass, 0.05s). Verbose run: 32/32 Phase-5-relevant tests PASS, 0 FAIL, 0 SKIP (euid != 0, so no permission skips fired).
- Typecheck: `go build ./...` → clean; `go vet ./internal/cli/` → clean.
- Lint/format: `gofmt -l ./internal/cli/` → **no output** (clean).
- Coverage: `go test ./internal/cli/ -cover` → 72.0% package. Phase 5 files via `go tool cover -func`:
  - vault.go: vaultPathDepth 100%, confinedVaultPath 93.3%, writeVault 100%, countDroppedEdges 100%, writeVaultNote 100%, writeFileAtomic 66.7%
  - export.go: runExport 96.8%, fetchExport 100%, checkVaultDir 100%, prepareVaultDir 88.9%, isCatastrophicVaultDir 100%, idPrefix 100%, sanitizeFilename 100%, cleanInline 100%
- Reviewer adversarial tests (run via `go test -overlay`, scratch file under /tmp/r2rev3-p5, repo untouched): `TestAdv_ConfinedVaultPathExtraHostile` PASS, `TestAdv_SlugCollisionsDisambiguated` PASS, `TestAdv_AbsErrorInjectable` (reachability probe) PASS.

## Requirement Fulfillment

### DW-5.1
PREMISE:  writes `events/`, `concepts/`, `maps/`; entity-per-note gone; `fetchExport` drains episodics.
EVIDENCE: internal/cli/vault.go:90-127 (writeVault: three render loops), internal/cli/export.go:104-125 (fetchExport pagination loop appending Episodics/Entities/Edges until empty cursor).
TRACE:    richPage fixture (2 events, 3 entities, 3 edges) → CLI `export` → vault contains `events/2026/2026-03-01 Alpha shipped the beta.md`, `events/undated/Gamma joined Beta.md`, `concepts/Alpha.md`, `maps/Alpha.md`; no `.md` at vault root, ghost `Beta` gets no concepts/ file. Multi-page fetch: episodics on pages 0 and 1, entities on page 1, edges on page 2 → both event notes present.
VERDICT:  PASS — TestDW_5_1_RichVaultLayoutEndToEnd, TestDW_5_1_WriteVaultRichLayout, TestDW_5_1_FetchExportDrainsEpisodicsAcrossPages all PASS. Also grepped export.go: no per-entity note rendering remains; export.go only calls writeVault.

### DW-5.2
PREMISE:  every write stays inside the vault dir incl. nested folders.
EVIDENCE: internal/cli/vault.go:57-82 (confinedVaultPath: abs/backslash refusal, allowed-root allowlist, exact per-root depth, per-element dot/space refusal, final Join+Rel re-check), vault.go:153-161 (writeVaultNote confines BEFORE MkdirAll/write).
TRACE:    22 repo escape vectors (`../pwn.md`, `events/2026/../pwn.md`, `events\2026\pwn.md`, `/etc/passwd`, wrong-depth, empty-element, dot-only elements…) → refusal from both confinedVaultPath and writeVaultNote, zero files created. My 16 additional vectors (`EVENTS/2026/pwn.md` case variant, `//etc/pwn.md`, `./events/...`, `C:/evil`, `maps/..\pwn.md`, `~/pwn.md`, `events/2026/x/..`, …) → all refused, dir stays empty; odd-but-safe names accepted and confined. End-to-end: hostile episodic titles and entity names (`../../etc/pwn`, `..\..\win\shadow`, `/etc/shadow`) flow through the real renderers → 3 events/1 concept/1 map written, WalkDir over the parent proves nothing outside the vault, canary untouched, no `etc/` created outside.
VERDICT:  PASS — TestDW_5_2_ConfinedVaultPathRejectsEscapes, TestDW_5_2_HostileNamesStayConfined, TestAdv_ConfinedVaultPathExtraHostile all PASS.

### DW-5.3
PREMISE:  full-vault re-run byte-identical.
EVIDENCE: internal/cli/vault.go:90-127 (deterministic model + ordered render loops); vault_test.go:321-339.
TRACE:    same records → writeVault into dirA and dirB → trees byte-identical; reversed input record order → still byte-identical; CLI re-run over an owned dir regenerates (stale note gone, new note present); my colliding-slug input re-run → byte-identical.
VERDICT:  PASS — TestDW_5_3_ReRunByteIdentical, TestExport_RerunClobbersOwnedDir, TestAdv_SlugCollisionsDisambiguated all PASS.

### DW-5.4
PREMISE:  fetch failure leaves vault untouched; empty tenant → marker-only; clobber warning prints.
EVIDENCE: internal/cli/export.go:83-89 (fetchExport at :83 returns before prepareVaultDir at :87 — clean-late ordering), :94 (warning), :144/:201-204 (empty dir + marker).
TRACE:    successful export → tree snapshot → second run against a non-advancing-cursor server → non-zero exit, tree byte-identical after. Empty tenant (`{}` page) into a missing nested dir → exit 0, vault contains exactly `.engram-vault`, summary "0 events, 0 concepts, 0 maps". Every successful export prints "warning: … clobbered". Failed fetch/dial against a fresh dir → dir never created.
VERDICT:  PASS — TestDW_5_4_FetchFailureLeavesVaultIntact, TestDW_5_4_EmptyTenantMarkerOnlyVault, TestDW_5_4_ClobberWarningPrints, TestExport_FetchErrorAborts, TestExport_NonAdvancingCursorAborts all PASS.

### DW-5.5
PREMISE:  summary reports events/concepts/maps/ghosts/dropped.
EVIDENCE: internal/cli/export.go:95-96 (summary line), vault.go:28-34/90-148 (vaultStats + countDroppedEdges: dropped = edges with NEITHER endpoint exported).
TRACE:    richPage → stdout contains `exported 2 events, 1 concepts, 1 maps to <dir> (2 ghosts, 1 dropped)`. Unit level: half-dangling edge (one endpoint exported) is NOT counted dropped — stats {2,1,1,2,1} exact-match.
VERDICT:  PASS — TestDW_5_5_SummaryCountsPrinted, TestDW_5_5_StatsCounts PASS.

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have DW-ID-named automated tests that ran in Step 0 (DW_5_1 ×3, DW_5_2 ×2, DW_5_3, DW_5_4 ×3, DW_5_5 ×2).
- [x] Coverage level 100% with the prompt's stated carve-out — every reachable, portably-testable branch is covered; the six uncovered blocks are all defense-in-depth or non-portably-injectable OS-error branches (itemized under Notes, each confirmed correct by inspection, one confirmed non-injectable by experiment).

Edge cases (prompt-listed):
- Nested path-escape via crafted name → refused: PASS (see DW-5.2; repo tests + 16 reviewer vectors).
- Empty tenant → marker-only vault: PASS (TestDW_5_4_EmptyTenantMarkerOnlyVault; TestWriteVault_EmptyExport confirms zero notes at unit level).
- Slug collisions across folders → disambiguated by VaultRefs: PASS (reviewer test TestAdv_SlugCollisionsDisambiguated: `Alpha/Beta` vs `Alpha-Beta` both sanitize to `Alpha-Beta` → two distinct id-suffixed concept and map files; same-title events → `Same title prose (ev1).md` / `(ev2).md`; no silent overwrite; deterministic. Note: two entities with the IDENTICAL name intentionally merge into one concept via normalizeConceptName — homonym collapse at vaultmodel.go:177 — so identical names are not a collision by design).

## Dead Code
None found. All functions in vault.go/export.go are referenced (idPrefix and sanitizeFilename by vaultmodel.go/vaultrefs); no debug statements, no commented-out blocks, no unreachable code after early returns. `go vet` clean.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | Single-threaded CLI command; no goroutines, no shared mutable state in Phase 5 code. |
| Error Handling | PASS | Adversarial probes found no unhandled path: non-advancing server cursor aborts (external-input infinite-loop guard, tested); every os.* call checked; failed fetch/dial leaves target untouched (tested); write failure in any of the three render loops aborts loudly (TestWriteVault_WriteFailurePropagates). |
| Resources | PASS | Temp files: every writeFileAtomic error path Closes+Removes; TestWriteFileAtomic_ErrorPaths proves no `.engram-tmp-*` lingers after failure. `defer client.Close()` at export.go:82. |
| Boundaries | PASS | Empty export → zero writes (tested); nil/empty names fall back to id-derived slugs upstream; filename rune cap 60 with post-truncation re-trim (TestSanitizeFilename incl. 100-rune input); half-dangling vs fully-dangling edge boundary exact-matched. |
| Security | PASS | See DW-5.2 and the destructive-write findings below. |

Security focus — destructive-write guards (both prescribed attacks executed):
1. **Path confinement**: attacked through the ACTUAL confinedVaultPath/writeVaultNote with 38 hostile vectors total (repo 22 + reviewer 16) in temp vaults under /tmp; every escape refused with zero filesystem effect, and hostile ingested names driven through the real CLI/renderers stayed confined (canary + WalkDir proof).
2. **CATASTROPHIC-DIR / SYMLINK guard**: TestPrepareVaultDir_SymlinkToHomeRefused is exactly the prescribed scenario — scratch `$HOME` via `t.Setenv`, canary file, vault dir a symlink to it, real `engram export --force` through `Run` against a live stub server → non-zero exit, stderr "refusing to clobber", canary byte-identical ("mine"). Ran it: PASS. Symlink→`/` under `--force`: refused (TestPrepareVaultDir_SymlinkToRootRefused, PASS). The guard resolves BOTH sides (vault dir and $HOME) through EvalSymlinks before comparing (export.go:173-185), and the refusal fires before any RemoveAll. Failed fetch never clobbers: fetch precedes prepareVaultDir (export.go:83 vs :87), proven by TestDW_5_4_FetchFailureLeavesVaultIntact. prepareVaultDir removes entries INSIDE dir, never dir itself (export.go:196-200); os.RemoveAll does not follow symlinked entries.

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | External input validated at entry | PASS | Untrusted ingested names/prose sanitized at rendering barricades (sanitizeFilename/cleanInline, both 100% covered); server cursor validated for progress (export.go:120-122, tested); dir argument triple-checked (checkVaultDir pre-dial, re-check in prepareVaultDir, catastrophic guard). |
| cc-defensive-programming | Barricade + defense-in-depth on security-critical path | PASS | confinedVaultPath re-verifies every renderer-produced path immediately before each write even though upstream sanitization should make refusal unreachable (vault.go:57-82) — exactly the "validate again on security-critical paths" rule. Attacked directly; held. |
| cc-defensive-programming | No empty catch blocks / no swallowed errors | PASS | Every error return checked or explicitly best-effort (`os.Remove` on an already-failing cleanup path; `home, _ := os.UserHomeDir()` degrades safely to root-only guard — see Notes). |
| cc-defensive-programming | Correctness-vs-robustness: destructive op leans correctness | PASS | Aborts whole export on first refused path/write rather than best-effort continuing; clean-late ordering preserves existing vault on any pre-write failure (tested). |
| cc-defensive-programming | No executable code in assertions / assertions for bugs only | N/A | Go; no assert mechanism used. Bug-stop conditions surface as returned errors (correct choice for a CLI). |
| cc-refactoring-guidance | Old format fully removed, no half-migrated remnants | PASS | No entity-per-note rendering remains in export.go; end-to-end test asserts no root-level `.md` and no ghost files ("entity-per-note format has returned" tripwire). |
| cc-refactoring-guidance | Working state: full suite green after the change | PASS | `go test ./internal/cli/ -count=1` ok; `go build ./...` clean. |
| cc-refactoring-guidance | Matches existing codebase patterns | PASS | Flag handling, Env/Run wiring, error wrapping (`export: …: %w`) consistent with the package's existing subcommand pattern; gofmt/vet clean. |

## Notes (non-blocking)
- Uncovered blocks — all fall under the prompt's carve-out, each confirmed correct by inspection:
  - vault.go:79 (confinedVaultPath final Join+Rel re-check refusal): genuinely unreachable defense-in-depth — every input that could differ post-Join is already refused by the root/depth/element checks (my 16 extra vectors all died earlier).
  - vault.go:172-175, 177-180 (writeFileAtomic WriteString/Close failures on a just-created temp file): not portably injectable (needs ENOSPC/EIO; chmod cannot affect an open fd). Caveat: these blocks are 2–3 statements (cleanup + return), not strictly single-statement, but each was inspected — both Close and Remove the temp and return a wrapped error; the sibling CreateTemp/Rename error branches ARE tested including the no-lingering-temp assertion.
  - export.go:166 (filepath.Abs error): experimentally confirmed non-injectable on this platform — deleted cwd + `PWD` unset still yields a successful Abs (TestAdv_AbsErrorInjectable log). The repo's TestPrepareVaultDir_AbsErrorWithDeletedCwd actually exercises the MkdirAll error branch, not this one — its comment slightly overstates what it covers.
  - export.go:176 (EvalSymlinks non-ENOENT error): unreachable in practice — any ELOOP/EACCES that EvalSymlinks would hit fails checkVaultDir's os.Stat first (checkVaultDir's own stat-error branch IS tested via a symlink loop).
  - export.go:194 (prepareVaultDir ReadDir error): race-only — checkVaultDir ReadDir'd the same dir successfully moments earlier (or MkdirAll just created it 0o755).
  - export.go:91-93 (runExport propagating a writeVault error): single-statement `return err`; requires a write failure injected between prepareVaultDir and writeVault within one CLI run (race-only at CLI level); the underlying failure modes are unit-tested (TestWriteVault_WriteFailurePropagates).
  - export.go:265 (cleanInline control-char `case` body): zero-statement block (comment only); the behavior itself is exercised.
- `home, _ := os.UserHomeDir()` (export.go:178): if `$HOME` is unset the home half of the catastrophic guard silently disables (root guard still active). Not demonstrable as a defect — with no known home there is nothing to compare against — but a debug log on that path would make the degradation visible.
- TestPrepareVaultDir_SymlinkToHomeRefused relies on `os.UserHomeDir` reading `$HOME` (Unix-only); fine for this codebase's targets, would need adjustment for Windows CI.
- writeFileAtomic creates temps at the vault root and renames into subfolders — same filesystem, so atomicity holds; a crash mid-export can leave the vault partially written, but the marker + regenerate-on-rerun design makes that self-healing.

## Issues (if FAIL)
None.

**Verdict: PASS**

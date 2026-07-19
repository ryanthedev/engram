# Review: Phase 5 - vault assembly + CLI wiring (sample 2)

## Executed Results (Step 0)
- Test suite: `go test ./internal/cli/ -count=1` → ok (all tests pass, 0 failures)
- Typecheck: `go vet ./internal/cli/` → clean
- Lint: `gofmt -l internal/cli/` → **1 file flagged: `internal/cli/vault_test.go`** (comment-alignment drift only)
- Coverage: `go test ./internal/cli/ -coverprofile` → 70.4% package statements; per-function figures below
- Reviewer adversarial probes: 6 probe tests (path escape, NUL, symlinked vault, slug collision, dir guards, branch reachability) injected via `go test -overlay` from `/tmp/rev2-p5/zz_probe_test.go` → **all PASS** (no escapes, no clobbers)

## Requirement Fulfillment

### DW-5.1
PREMISE:  "`engram export <dir>` writes `events/`, `concepts/`, `maps/`; entity-per-note format is gone; `fetchExport` drains episodics across pages."
EVIDENCE: internal/cli/export.go:52–98 (runExport), export.go:104–125 (fetchExport accumulates `page.Episodics/Entities/Edges` until empty NextCursor); internal/cli/vault.go:90–127 (writeVault: events → concepts → maps loops); old format removed (git diff export.go: `-// one note per entity`, old `confinedNotePath`/`vaultFilenames` deleted)
TRACE:    stub gRPC server serving 3 pages (episodics on pages 1–2, entities page 2, edges page 3) → `Run(["export","-addr",addr,dir])` → fetchExport loops cursor ""→"1"→"2"→done → writeVault → `events/2026/2026-03-01 First event prose.md` + `events/undated/Second event prose.md` on disk; richPage e2e run yields `events/…`, `concepts/Alpha.md`, `maps/Alpha.md`, no root-level `.md`
VERDICT:  PASS — TestDW_5_1_RichVaultLayoutEndToEnd, TestDW_5_1_FetchExportDrainsEpisodicsAcrossPages, TestDW_5_1_WriteVaultRichLayout all ran green in Step 0.

### DW-5.2
PREMISE:  "every write stays inside the vault dir incl. nested folders (path-escape across all three folders)."
EVIDENCE: internal/cli/vault.go:57–82 (confinedVaultPath: abs/backslash refusal, allowlisted root, exact depth per root, per-element `Trim(". ")` check, final `filepath.Rel` re-check); vault.go:153–161 (writeVaultNote confines BEFORE MkdirAll/write)
TRACE:    `confinedVaultPath(dir, "events/2026/../pwn.md")` → split gives element ".." → Trim → "" → refuse, error propagates out of writeVault → whole-export abort. Hostile entity/event names (`../../etc/passwd`, `..\..\win\shadow`, `/etc/shadow`) through writeVault → sanitized, all files inside vault, canary outside untouched (TestDW_5_2_HostileNamesStayConfined).
    Reviewer probes (executed, all green): 17 additional refusal shapes (`events/../events/x.md`, case-twisted root `Events/…`, `maps/.. /x.md`, trailing `/..`, absolute-into-vault, backslash forms); literal-dot names (`events/2026/.. .md`) accepted AND confined; NUL-bearing element aborts at the OS write (no file, no escape); hostile names through a **symlinked vault dir** land only in the real target; 60-rune multibyte cap, empty-name and `~/.ssh/authorized_keys` names confined.
VERDICT:  PASS

### DW-5.3
PREMISE:  "full-vault re-run is byte-identical for the same export input."
EVIDENCE: internal/cli/vault_test.go:321–339 (TestDW_5_3_ReRunByteIdentical: two fresh dirs + reversed-input third dir, tree-wise byte comparison)
TRACE:    richRecords → writeVault(dirA), writeVault(dirB), writeVault(dirC, reversed inputs) → all three trees byte-identical file-for-file
VERDICT:  PASS — ran green in Step 0; reviewer collision probe re-confirmed deterministic id-suffixed names.

### DW-5.4
PREMISE:  "a fetch failure leaves an existing vault untouched; empty tenant → marker-only vault; the clobber warning prints."
EVIDENCE: internal/cli/export.go:83–89 (fetchExport error returns BEFORE prepareVaultDir — clean-late ordering), export.go:94 (warning line); export_test.go:208–224 (fetch failure → vault byte-identical), 226–243 (empty tenant → marker only, `0 events, 0 concepts, 0 maps` printed), 245–255 (warning contains "warning:" and "clobbered")
TRACE:    export vault → succeed; re-run against a non-advancing-cursor server → fetchExport errors at export.go:120–122 → runExport returns before prepareVaultDir → vault tree byte-identical before/after. Reviewer probe: dead server (127.0.0.1:1) → first RPC fails (export.go:111–113) → exit ≠ 0, vault dir never created.
VERDICT:  PASS

### DW-5.5
PREMISE:  "summary reports events/concepts/maps/ghosts/dropped counts."
EVIDENCE: internal/cli/export.go:95–96 (`"exported %d events, %d concepts, %d maps to %s (%d ghosts, %d dropped)"`); vault.go:28–34 (vaultStats), vault.go:134–148 (countDroppedEdges: dropped only when NEITHER endpoint exported)
TRACE:    richPage e2e → stdout contains `exported 2 events, 1 concepts, 1 maps to <dir> (2 ghosts, 1 dropped)` (TestDW_5_5_SummaryCountsPrinted); half-dangling edge (one endpoint exported) NOT counted dropped (TestDW_5_5_StatsCounts)
VERDICT:  PASS

**All requirements met:** YES (behavior); NO on the phase's 100% coverage bar — see Issues.

## Test-DW Coverage
- [x] Every DW item has DW-ID-named automated tests that ran in Step 0 (TestDW_5_1_* ×3, TestDW_5_2_* ×2, TestDW_5_3_*, TestDW_5_4_* ×3, TestDW_5_5_* ×2)
- [ ] Coverage does NOT match the stated 100% level for Phase 5 functions — see Issues #1

Phase 5 function figures (`go tool cover -func`):

| Function | Coverage |
|---|---|
| vault.go confinedVaultPath | 93.3% |
| vault.go writeVault / writeVaultNote / countDroppedEdges / vaultPathDepth | 100% |
| vault.go writeFileAtomic | 66.7% |
| export.go runExport | 83.9% |
| export.go fetchExport | 93.8% |
| export.go checkVaultDir | 76.5% |
| export.go prepareVaultDir | 65.0% |
| export.go isCatastrophicVaultDir / idPrefix / sanitizeFilename / cleanInline | 100% |

## Edge cases (prompt-listed)
| Edge case | Status | Evidence |
|---|---|---|
| Nested path-escape via crafted name → refused, whole-export abort | HANDLED | TestDW_5_2_ConfinedVaultPathRejectsEscapes (writeVaultNote error → writeVault returns err → runExport aborts); reviewer probes: 17 extra shapes refused, sanitized names confined, symlinked dir confined |
| Empty tenant → marker-only vault | HANDLED | TestDW_5_4_EmptyTenantMarkerOnlyVault: exactly one file (`.engram-vault`), all-zero summary |
| Slug collisions across folders → disambiguated by VaultRefs | HANDLED | Reviewer probe: hubs "Dup/X" and "dup:x" (identical slug, case-insensitively) → `concepts/Dup-X (a1).md` + `concepts/dup-x (b1).md` and same pair in `maps/` — id-suffixed, not case-only-distinct (safe on APFS), deterministic |

## Dead Code
None found. All helpers (idPrefix, writeFileAtomic, vaultPathDepth) have non-test callers; no debug statements, no unreachable code, no commented-out blocks.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | single-threaded CLI pipeline; no shared mutable state, no goroutines in the phase code |
| Error Handling | PASS | probed: every fetch/dir/write failure path returns a wrapped error and aborts (dead-server probe, blocked-folder tests, foreign-dir refusals); no empty catches; cursor-loop guard on the external pagination input |
| Resources | PASS | temp files removed on every writeFileAtomic error path (TestWriteFileAtomic_ErrorPaths asserts no `.engram-tmp-*` lingers); tmp.Close before rename; client closed via defer |
| Boundaries | PASS | probed: empty export (zero files, zero stats), empty/whitespace-only names (id-derived fallback `concept (h4).md` / `ev4.md`), 400-rune multibyte name capped at 60 runes, half-dangling edge not double-counted |
| Security | PASS | full adversarial battery against actual confinedVaultPath/writeVault under /tmp/rev2-p5: zero escapes, zero accepted `../`, canary untouched, symlinked vault confined, NUL aborts loudly, failed fetch never clobbers |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | External input validated at entry (untrusted names/prose → filesystem) | PASS | sanitizeFilename/cleanInline at the render barricade + confinedVaultPath re-verification before every write; probes could not defeat it |
| cc-defensive-programming | Barricade + defense-in-depth on security-critical path | PASS | two-layer design (sanitize at render, confine at write); the second layer independently refuses everything the first should have caught (probe-verified) |
| cc-defensive-programming | No empty catch blocks / errors never swallowed | PASS | every `err` is returned wrapped or explicitly `os.Remove` cleanup-then-return; `home, _ := os.UserHomeDir()` deliberate (guard degrades to root-only check, still safe) |
| cc-defensive-programming | Anticipated runtime errors use error handling, not assertions | PASS | non-advancing cursor, foreign dir, blocked folder, dead server all handled as errors with actionable messages |
| cc-defensive-programming | Correctness-over-robustness (refuse rather than write wrong) | PASS | barricade refusal aborts whole export; clobber only after successful fetch; catastrophic-dir guard survives `--force` (probe: `prepareVaultDir($HOME, force=true)` refused) |
| cc-refactoring-guidance | Working code, tests green, one structural change per phase; no fix+refactor mixing observed in the diff | PASS | Phase 5 diff replaces the old entity-per-note assembly wholesale with the new one, tests updated in lockstep; suite green |
| cc-refactoring-guidance | Small-change rigor: pickiest tooling clean | FAIL (minor) | `gofmt -l` flags `internal/cli/vault_test.go` — comment-alignment drift introduced this phase (Issue #2) |

## Notes (non-blocking)
- Carve-out coverage gaps, confirmed correct by inspection, correctly non-blocking under the dispatch's rule: vault.go:78–80 (final `filepath.Rel` re-check — genuinely unreachable defense-in-depth; my full adversarial set could not reach it), vault.go:172–180 (WriteString/Close failure — ENOSPC-class, non-portably injectable), export.go:134–136/141–143 (stat/ReadDir faults — chmod-based, non-portable), export.go:165–167 (`filepath.Abs` fails only if Getwd fails), export.go:172–187 (MkdirAll/ReadDir/RemoveAll/marker-write OS faults), export.go:91–93 (writeVault error via CLI needs mid-run interference; behavior covered at the unit layer).
- confinedVaultPath accepts a NUL-bearing element (`events/2026/\x00`); the OS write then fails and the export aborts — abort-not-escape holds (probe-verified), but adding NUL to the refusal set would make the barricade self-contained.
- `writeFileAtomic` creates temp files in the vault ROOT and renames into nested folders; a crash could leave `.engram-tmp-*` at the root until the next run's prepareVaultDir cleanup — acceptable, self-healing.
- Homonym entities with byte-different but case-equal names ("Dup"/"dup") are collapsed into one concept by the Phase 2 model (by design), so only sanitize-collisions exercise the suffixing; probe confirmed suffixed output is never case-only-distinct (APFS-safe).

## Issues (FAIL)
1. Test coverage below the phase's stated 100% bar: seven demonstrably REACHABLE behavior branches in export.go are uncovered by the shipped suite.
   - File: internal/cli/export.go:58–60, 67–69, 111–113, 137–139, 144–146, 161–163, 169–171 (secondary, reachable in principle: 79–81 dial error via malformed `-addr`, 87–89 prepare-failure propagation via `$HOME`-targeted dir)
   - Demonstrated by: reviewer probes TestProbe_UncoveredBranchesReachable + TestProbe_DirGuardReachableBranches (executed green via `go test -overlay`, /tmp/rev2-p5/zz_probe_test.go) — each branch flips from 0 to covered under portable inputs: `export --bogus` / `export <dir> --bogus` (flag-parse errors), dead server 127.0.0.1:1 (RPC fetch error path — the natural DW-5.4 failure mode), export target is a plain file, pre-existing EMPTY dir accepted, prepareVaultDir foreign-dir re-check refusal, catastrophic `$HOME` refusal wiring via `t.Setenv`. All behaviors are CORRECT; the gap is coverage, which the dispatch's rule makes blocking ("an uncovered REACHABLE behavior branch IS a FAIL").
   - Fix: add the seven cases above to export_test.go (each is a 3–6 line table entry or subtest; the probe file shows working recipes).
2. gofmt drift in internal/cli/vault_test.go (comment alignment in the TestDW_5_2 refusal table).
   - File: internal/cli/vault_test.go:175–182
   - Demonstrated by: `gofmt -l internal/cli/` flags the file
   - Fix: `gofmt -w internal/cli/vault_test.go`

**Verdict: FAIL — blockers: (1) 100% coverage requirement unmet on 7 reachable behavior branches in export.go; (2) gofmt-dirty vault_test.go. All five DW items pass on behavior; the security surface survived the full adversarial battery with zero escapes.**

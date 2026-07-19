# Review: Phase 5 - vault assembly + CLI wiring (security sample 1)

## Executed Results (Step 0)
- Test suite (shipped): `go test ./internal/cli/ -count=1` → **PASS** (all tests green, 0.05s)
- Coverage (shipped): `go test ./internal/cli/ -cover` → 70.4% package. Phase-5 per-function (`go tool cover -func`):
  | Function | Shipped coverage |
  |---|---|
  | vault.go writeVault | 100.0% |
  | vault.go confinedVaultPath | 93.3% |
  | vault.go writeFileAtomic | 66.7% |
  | vault.go countDroppedEdges | 100.0% |
  | export.go fetchExport | 93.8% |
  | export.go runExport | 83.9% |
- Typecheck: `go vet ./internal/cli/` → clean
- Lint: `gofmt -l internal/cli/` → clean (no files listed)
- Independent adversarial harness: injected via `go test -overlay` (file lived only under /tmp/rev1-p5, never in the repo tree — confirmed absent). Exercised the ACTUAL `confinedVaultPath`/`writeVault`/`writeVaultNote`/`runExport`/`fetchExport` with hostile inputs.

## Requirement Fulfillment

### DW-5.1
PREMISE:  `engram export <dir>` writes `events/`, `concepts/`, `maps/`; entity-per-note format is gone; `fetchExport` drains episodics across pages.
EVIDENCE: export.go:83,90-96 (runExport wires fetch→writeVault→summary); vault.go:101-125 (three render loops into events/concepts/maps); export.go:104-125 (fetchExport page drain).
TRACE:    richPage stub → fetchExport accumulates 2 pages → writeVault emits `events/2026/…`, `events/undated/…`, `concepts/Alpha.md`, `maps/Alpha.md`; no root-level `.md`; ghost leaves get no file. Cross-page drain proven by a 3-page fixture whose events span pages 1–2.
VERDICT:  PASS — `TestDW_5_1_RichVaultLayoutEndToEnd`, `TestDW_5_1_WriteVaultRichLayout`, `TestDW_5_1_FetchExportDrainsEpisodicsAcrossPages` all pass in Step 0.

### DW-5.2
PREMISE:  every write stays inside the vault dir incl. nested folders (path-escape across all three folders).
EVIDENCE: vault.go:57-82 (confinedVaultPath: root allow-list, exact depth, per-element dot/space refusal, backslash+abs refusal, final `filepath.Rel` re-check), vault.go:153-161 (writeVaultNote confines BEFORE any FS effect).
TRACE:    `events/../pwn.md`, `../pwn.md`, `/etc/passwd`, `events\2026\pwn.md`, `concepts/a/pwn.md` (wrong depth), `events//pwn.md` (empty element) → each fails a gate → `refuse()` → no write. Hostile entity/episodic names (`../../etc/passwd`, `..\..\win\shadow`, `/etc/shadow`) flow through the full model into all three folders and are sanitized to safe slugs; the canary outside the vault is untouched and no `etc/` dir appears.
VERDICT:  PASS — `TestDW_5_2_ConfinedVaultPathRejectsEscapes`, `TestDW_5_2_HostileNamesStayConfined` pass; my independent overlay probe `TestAdvConfinedVaultPathExtra` added case-variant roots, space-padded `..`, leading `./` and `../`, double-climb, trailing backslash, and a NUL byte — all refused or (NUL) failed loudly on write with nothing landing on disk. Symlinked vault dir (`TestAdvSymlinkedVaultDir`): notes land only inside the resolved target; external canary untouched.

### DW-5.3
PREMISE:  full-vault re-run is byte-identical for the same export input.
EVIDENCE: vault.go:90-127 (writeVault is pure over sorted model; buildVaultModel/refs sort by id, no clock/map-order); export.go writes deterministic content.
TRACE:    writeVault(dirA) and writeVault(dirB) over identical records → identical file set + bytes; writeVault(dirC) over REVERSED episodics/entities/edges → still identical (input order does not leak).
VERDICT:  PASS — `TestDW_5_3_ReRunByteIdentical` passes (both same-order and reversed-order trees compared byte-for-byte).

### DW-5.4
PREMISE:  a fetch failure leaves an existing vault untouched; empty tenant → marker-only vault; the clobber warning prints.
EVIDENCE: export.go:83-89 (fetch BEFORE prepareVaultDir; dir cleaned only after fetch succeeds), :226-243 empty-tenant, :94 warning, :144-146 empty-dir accept.
TRACE:    non-advancing cursor → fetchExport returns error at export.go:120-121 → runExport returns before prepareVaultDir → existing vault bytes unchanged / missing dir not created. Empty `{}` page → 0 records → writeVault writes nothing, only the marker remains. Success path prints the `warning: … clobbered` line.
VERDICT:  PASS — `TestDW_5_4_FetchFailureLeavesVaultIntact`, `TestExport_NonAdvancingCursorAborts`, `TestDW_5_4_EmptyTenantMarkerOnlyVault`, `TestDW_5_4_ClobberWarningPrints` pass. My overlay `TestAdvFetchRPCErrorLeavesExistingVaultIntact` extended this to a *transport-level* fetch failure (dead listener) — existing vault also survives byte-for-byte.

### DW-5.5
PREMISE:  summary reports events/concepts/maps/ghosts/dropped counts.
EVIDENCE: vault.go:92,107,109-118,124,134-148 (stats accumulation incl. countDroppedEdges); export.go:95-96 (summary line).
TRACE:    richPage → `exported 2 events, 1 concepts, 1 maps to <dir> (2 ghosts, 1 dropped)`; a half-dangling edge (one exported endpoint) lands as a claim and is NOT counted dropped.
VERDICT:  PASS — `TestDW_5_5_SummaryCountsPrinted`, `TestDW_5_5_StatsCounts` pass.

**All requirements met:** YES (behavioral). See coverage FAIL below — a stated Phase-5 requirement (100% coverage) is unmet on reachable branches.

## Test-DW Coverage
- [x] All DW items have named automated tests that ran green in Step 0.
- [x] Behavioral coverage matches the stated intent for each DW item.
- [ ] **100% coverage bar (stated Test Coverage Level) NOT met** on two of the named report functions — see Correctness/Issues. The DW *behaviors* are covered; several reachable, portably-injectable error branches are not.

## Dead Code
None found. No unused imports, no unreachable post-return code, no debug prints, no commented-out blocks. `go vet` clean.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | Single-threaded CLI; no shared mutable state, goroutines, or async in the reviewed paths. |
| Error Handling | PASS (behavior) / see coverage | Every FS/RPC/flag error is wrapped and returned; fetch-before-clean ordering verified. Behaviorally correct under injection; the gap is test *coverage* of these branches, not their correctness. |
| Resources | PASS | writeFileAtomic closes+removes the temp file on every failure branch (verified by inspection L166-186 and `TestWriteFileAtomic_ErrorPaths` — no temp lingers). client.Close deferred. |
| Boundaries | PASS | Empty export (nil records) → marker-only vault; single/zero-length ids handled by idPrefix; rune-capped filenames; empty-sanitize fallback to id-derived name. |
| Security | PASS (path confinement) | Path confinement — THIS sample's attack surface — holds against every hostile input I constructed (traversal, absolute, backslash, case-variant root, wrong depth, empty/`.`/`..`/dots-and-spaces elements, NUL, symlinked vault dir). Nothing written outside the vault; no `../` accepted. See the separate high-severity NOTE on the `--force` catastrophic-guard symlink gap (a *clobber/deletion* issue, not a note-escape). |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | No executable code in assertions | PASS | No assertions; confinement uses real returned errors, not asserts. |
| cc-defensive-programming | No empty catch/swallowed errors | PASS | Every error path wraps + returns; none silently swallowed. |
| cc-defensive-programming | External input validated at entry | PASS (with note) | Untrusted names sanitized at render barricade + re-confined before write. Dir path validated by checkVaultDir/catastrophic guard — but the guard does not resolve symlinks (see NOTE); classified WARNING, not a clean violation, since validation is present but incomplete and outside this phase's DW/edge scope. |
| cc-defensive-programming | Assertions for bugs only | PASS | confinedVaultPath refusal is a real error-return bug-stop (aborts export), correctly modeled as error handling, not an assertion compiled out in prod. |
| cc-refactoring-guidance | Fix-first / behavior-preserving; match existing patterns | PASS | Error style (wrapped `fmt.Errorf("export: …")`), atomic temp+rename, and confinement idiom match the module's conventions. No mixed fix+refactor observed. |

## Notes (non-blocking)
1. **[HIGH — data loss, but OUTSIDE this phase's DW/edge scope] `--force` on a symlinked vault dir resolving to `$HOME` deletes home contents.** `isCatastrophicVaultDir` (export.go:194-200) checks `filepath.Abs(dir)` (export.go:164) which does NOT resolve symlinks. TRACE: `link → $HOME` (symlink); `engram export --force <link>` → checkVaultDir passes (force) → fetch OK → prepareVaultDir → `abs = /…/link` (unresolved) → `isCatastrophicVaultDir` returns false → the clean loop `os.RemoveAll` deletes every entry inside `$HOME`. Demonstrated by overlay `TestAdvSymlinkToHomeWithForce` (exit 0, `precious.txt` in `$HOME` destroyed). This is a *clobber/deletion* gap, not a note-path escape (path confinement itself holds). It is NOT a listed DW item, NOT a listed edge case, and NOT the sampled path-confinement surface, so per the verdict rules it is a Note, not a blocker — but it is a genuine `rm -rf $HOME`-class hazard. Recommended fix: `filepath.EvalSymlinks(abs)` before the catastrophic comparison (and consider the same before the clean loop). The non-symlinked catastrophic guard itself works (`TestAdvForceNeverCleansHome` refuses a direct `$HOME` target).
2. **confinedVaultPath L78 final `filepath.Rel` re-check refusal — uncovered, genuinely unreachable.** After the root allow-list, exact-depth, per-element dot/space, backslash, and abs gates pass, the joined path cannot differ from `relPath` on a rejoin. Defense-in-depth bug-stop, correct by inspection. Exempt per the coverage rule.
3. **writeFileAtomic L172 (WriteString fail) + L177 (Close fail) — uncovered.** OS-write-failure branches, not portably fault-injectable; both correctly `os.Remove` the temp and wrap the error, matching the covered Rename branch. Exempt per the coverage rule.
4. **runExport L91 (writeVault error return) — uncovered even under injection through the CLI.** Reaching it via the full CLI requires an OS write failure mid-assembly; `TestWriteVault_WriteFailurePropagates` proves the underlying propagation at the unit level. OS-write-failure category → exempt.

## Issues (FAIL)
1. **100% coverage requirement unmet: reachable, portably-injectable error branches uncovered in the two named report functions.** The shipped suite leaves these branches uncovered, and none qualify for the stated exemptions (unreachable defense-in-depth, or *non-portably-injectable* OS-write failure) — I injected every one of them portably in the overlay harness:
   - `fetchExport` (export.go:111-113) — ExportPage RPC/transport error → abort. Reachable via a dead server (overlay `TestAdvFetchRPCErrorLeavesNoVault`/`…LeavesExistingVaultIntact`). This is the transport-level half of the DW-5.4 "fetch failure never clobbers" guarantee; the shipped suite only covers the non-advancing-cursor half.
   - `runExport` (export.go:58) — first `flag.Parse` error (malformed flag before `<dir>`). Reachable via `export --bogus x` (overlay `TestAdvBadFlagFirstParse`).
   - `runExport` (export.go:67) — second `flag.Parse` error (malformed flag after `<dir>`, the two-pass tail parse). Reachable via `export <dir> --bogus` (overlay `TestAdvBadFlagSecondParse`).
   - `runExport` (export.go:79-81) — `dialClient` error. Reachable via a malformed `-addr` (overlay `TestAdvDialErrorReachable`).
   - `runExport` (export.go:87-89) — `prepareVaultDir` error return incl. the catastrophic `--force` refusal driven through the CLI. Reachable (overlay `TestAdvForceNeverCleansHome`).
   - File: internal/cli/export.go:58, 67, 79, 87, 111
   - Demonstrated by: overlay tests named above — each branch executes and behaves correctly, but the SHIPPED test suite (`export_test.go`) never exercises them, leaving fetchExport at 93.8% and runExport at 83.9% against a stated 100% bar.
   - Fix: add CLI-level tests for a malformed flag (both parse passes), a bad `-addr` dial failure, an RPC-level fetch failure (dead listener) asserting no-clobber, and a `--force`-onto-`$HOME`-through-`runExport` catastrophic refusal. (Behavior is already correct; this closes the coverage gap.)

**Verdict: FAIL — the stated 100% Phase-5 coverage requirement is not met: `fetchExport` (93.8%) and `runExport` (83.9%) leave reachable, portably-fault-injectable behavior branches (RPC-error abort, dial error, both flag-parse errors, catastrophic-refusal-through-CLI) uncovered by the shipped suite; none fall in the exempt (unreachable / non-injectable-OS-write) categories. All five DW behaviors are individually correct and pass their named tests, and path confinement — this sample's security surface — is sound; a separate high-severity NOTE flags a `--force`+symlink `$HOME`-deletion gap that sits outside this phase's DW/edge scope.**

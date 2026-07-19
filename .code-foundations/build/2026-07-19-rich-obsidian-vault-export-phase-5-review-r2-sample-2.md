# Review: Phase 5 - Vault assembly + CLI wiring (r2, sample 2)

Independent post-gate review. Files reviewed: `internal/cli/vault.go`, `internal/cli/vault_test.go`, `internal/cli/export.go`, `internal/cli/export_test.go`. All reviewer probes ran through the ACTUAL implementation via a `go test -overlay` injected test file (`/tmp/r2rev2-p5/zz_adv_test.go`) — the repo was not modified.

## Executed Results (Step 0)

- Test suite: `go test ./internal/cli/ -count=1` → **ok** (150 tests, 0 failures)
- Coverage: `go test ./internal/cli/ -coverprofile` → package 72.0% (includes non-Phase-5 files); Phase 5 per-function figures below
- Lint: `gofmt -l ./internal/cli/` → **no output** (clean, exit 0)
- Vet: `go vet ./internal/cli/` → clean
- Reviewer adversarial suite (overlay): `TestADV_SlugCollisionsDisambiguated`, `TestADV_LongRuneCollisionFilenameLength`, `TestADV_ConfinementExtra`, `TestADV_E2EReRunByteIdentical` → all ran; findings below

## Requirement Fulfillment

### DW-5.1
PREMISE:  writes `events/`, `concepts/`, `maps/`; entity-per-note gone; `fetchExport` drains episodics.
EVIDENCE: vault.go:90-127 (writeVault three render loops), export.go:104-125 (fetchExport accumulates `page.Episodics` across pages until empty NextCursor), export_test.go:120-185
TRACE:    richPage fixture → `engram export` via `Run` → vault holds `events/2026/2026-03-01 Alpha shipped the beta.md`, `events/undated/Gamma joined Beta.md`, `concepts/Alpha.md`, `maps/Alpha.md`; no root-level `.md` (entity-per-note gone, asserted export_test.go:143-150); 3-page fetch drains episodics from pages 1 and 2 into event notes.
VERDICT:  **PASS** — `TestDW_5_1_RichVaultLayoutEndToEnd`, `TestDW_5_1_FetchExportDrainsEpisodicsAcrossPages`, `TestDW_5_1_WriteVaultRichLayout` all pass (executed). Non-advancing-cursor abort preserved (`TestExport_NonAdvancingCursorAborts`).

### DW-5.2
PREMISE:  every write stays inside the vault dir incl. nested folders.
EVIDENCE: vault.go:57-82 (confinedVaultPath: abs/backslash refusal, allowlisted root, exact depth per root, per-element `Trim(". ")` check, final `filepath.Rel` re-check), vault.go:153-161 (writeVaultNote confines BEFORE MkdirAll/write), vault.go:167 (temp file created inside the vault dir)
TRACE:    hostile relPaths (`../pwn.md`, `events/../pwn.md`, `events/2026/..`, `\`-paths, `/etc/passwd`, unknown roots, wrong depths, empty elements) → all refused, zero files created (`TestDW_5_2_ConfinedVaultPathRejectsEscapes`); hostile ingested names (`../../etc/pwn`, `..\..\win\pwn`, `/etc/passwd` as titles AND entity names) → writeVault succeeds with every file inside the vault, sibling canary untouched, no `etc/` created outside (`TestDW_5_2_HostileNamesStayConfined`). Reviewer probes (`TestADV_ConfinementExtra`) added `EVENTS/2026/x.md` (case-variant root), `./events/...`, `events/2026/./x.md`, `x.md/..`, `maps/.`, `concepts/ `, NUL-byte element — all refused, nothing written outside; parent of the temp vault clean.
VERDICT:  **PASS**

### DW-5.3
PREMISE:  full-vault re-run byte-identical.
EVIDENCE: vault.go:90-127 (deterministic model → sorted render loops), vault_test.go:321-339
TRACE:    same records → writeVault into two fresh dirs → identical trees byte-for-byte; input record order reversed → still identical (`TestDW_5_3_ReRunByteIdentical`). Reviewer e2e probe: two full `engram export` runs into the SAME dir from identical server data → entire vault incl. `.engram-vault` marker byte-identical (`TestADV_E2EReRunByteIdentical`, executed PASS).
VERDICT:  **PASS**

### DW-5.4
PREMISE:  fetch failure leaves vault untouched; empty tenant → marker-only; clobber warning prints.
EVIDENCE: export.go:83-89 (prepareVaultDir called only AFTER fetchExport returns nil error), export.go:94 (warning line), export_test.go:208-255
TRACE:    successful export, then re-run against a non-advancing-cursor server → exit non-zero, existing vault byte-identical before/after (`TestDW_5_4_FetchFailureLeavesVaultIntact`); RPC failure and dial failure never even create the dir (`TestExport_FetchErrorAborts`, `TestExport_DialErrorSurfaces`); empty tenant → vault contains exactly `.engram-vault` and prints all-zero summary (`TestDW_5_4_EmptyTenantMarkerOnlyVault`); `warning: … clobbered` printed on success (`TestDW_5_4_ClobberWarningPrints`).
VERDICT:  **PASS**

### DW-5.5
PREMISE:  summary reports events/concepts/maps/ghosts/dropped.
EVIDENCE: export.go:95-96 (summary Fprintf), vault.go:28-34 + 129-148 (vaultStats; countDroppedEdges counts only edges with NEITHER endpoint exported), export_test.go:259-270, vault_test.go:343-357
TRACE:    richPage → stdout contains `exported 2 events, 1 concepts, 1 maps to <dir> (2 ghosts, 1 dropped)` (`TestDW_5_5_SummaryCountsPrinted`); a half-dangling edge (one exported endpoint) is NOT counted dropped (`TestDW_5_5_StatsCounts`).
VERDICT:  **PASS**

**All requirements met:** YES

## Edge Cases (prompt-listed)

| Edge case | Status | Evidence |
|---|---|---|
| Nested path-escape via crafted name → refused | PASS | `TestDW_5_2_*` + reviewer `TestADV_ConfinementExtra`; refusal before any filesystem effect |
| Empty tenant → marker-only vault | PASS | `TestDW_5_4_EmptyTenantMarkerOnlyVault` (exactly 1 file, the marker) |
| Slug collisions across folders → disambiguated by VaultRefs | PASS | Reviewer `TestADV_SlugCollisionsDisambiguated` (executed): two hubs "Al/pha"/"Al:pha" (distinct concepts, identical sanitized slug) → 4 distinct concept files with id-suffix disambiguation; 2 identical undated event titles → distinct event files; no case-insensitive path clash anywhere in the tree. Same-name entities merging into ONE concept is the intended alias collapse (vaultmodel.go normalizeConceptName), not a collision loss. |

## Security Focus

**1. Path confinement** — attacked through the actual `confinedVaultPath`/`writeVaultNote`/`writeVault` with temp vaults under the reviewer scratch. Every escape vector refused (abs paths, `..`/`.`/empty/dot-space elements at every depth, backslashes, unknown/case-variant roots, wrong depths, trailing separators, NUL); refusals happen before MkdirAll or any write; hostile-name end-to-end run left a sibling canary untouched and created nothing outside the vault. The confinement is structural (allowlist + exact depth + per-element check) with a `filepath.Rel` defense-in-depth re-check — sound.

**2. CATASTROPHIC-DIR / SYMLINK guard** — verified via the executed suite: `TestPrepareVaultDir_SymlinkToHomeRefused` builds a vault dir that is a symlink to a scratch `$HOME` (`t.Setenv("HOME", …)`) containing `precious.txt`, runs the REAL `engram export --force <link>` through `Run` → exit non-zero, stderr `refusing to clobber`, canary survives byte-for-byte (PASS, executed). `TestPrepareVaultDir_SymlinkToRootRefused` → symlink to `/` refused under `--force` (PASS). The guard resolves symlinks on BOTH sides (export.go:173-185) before comparing, and cleaning removes entries INSIDE dir, never dir itself (export.go:196-200); `os.RemoveAll` on a contained symlink removes the link, not its target. Failed fetch never clobbers: prepare runs strictly after a successful drain (export.go:83-89; `TestDW_5_4_FetchFailureLeavesVaultIntact`).

## Test-DW Coverage

- [x] Every DW item has DW-ID-named automated tests that ran in Step 0 (see per-item evidence).
- Phase 5 per-function coverage (`go tool cover -func`):

| Function | Coverage | Uncovered residue |
|---|---|---|
| vault.go: vaultPathDepth, writeVault, countDroppedEdges, writeVaultNote | 100% | — |
| vault.go: confinedVaultPath | 93.3% | line 78-80: final `filepath.Rel` re-check refusal — defense-in-depth, unreachable past the structural checks (non-blocking per dispatch carve-out) |
| vault.go: writeFileAtomic | 66.7% | lines 172-176, 177-180: WriteString/Close OS-error cleanup branches — not portably injectable; correct by inspection (temp removed, error wrapped) |
| export.go: fetchExport, checkVaultDir, isCatastrophicVaultDir, idPrefix, sanitizeFilename, cleanInline | 100% | (cleanInline's 0-statement drop-case block is a profiler artifact) |
| export.go: runExport | 96.8% | lines 91-93: `writeVault` error propagation (single-statement `return err`) — not portably injectable e2e on this platform since prepare cleans the dir in the same invocation; unit-level writeVault failures covered by `TestWriteVault_WriteFailurePropagates` |
| export.go: prepareVaultDir | 88.9% | line 165-167 `filepath.Abs` error; 175-177 EvalSymlinks non-ENOENT; 193-195 second ReadDir — OS-error/race defense-in-depth branches, correct by inspection |

All uncovered residue falls under the dispatch's non-blocking carve-out (defense-in-depth or non-portably-injectable OS-error returns, each inspected correct). No reachable, portably-testable branch is uncovered.

## Dead Code

None found. No old entity-per-note remnants (`grep` for the old format across `internal/` is empty), no debug prints, no TODO/FIXME, no unreachable code, no commented-out blocks in the four files.

## Correctness Dimensions

| Dimension | Status | Evidence |
|---|---|---|
| Concurrency | N/A | Single-threaded CLI command; no shared state, no goroutines in the reviewed files (the gRPC stub goroutine is test scaffolding) |
| Error Handling | PASS | Every I/O and RPC error checked and wrapped with context; non-advancing cursor aborted; no swallowed errors; flag-parse, dial, fetch, prepare, and write failures each propagate to a non-zero exit (all exercised by executed tests) |
| Resources | PASS | Temp file closed and removed on every writeFileAtomic failure path (`TestWriteFileAtomic_ErrorPaths` proves no lingering `.engram-tmp-*`); gRPC client closed via defer (export.go:82) |
| Boundaries | **FAIL** | Demonstrated: filename byte-length bound violated — see Issue 1. Reviewer test `TestADV_LongRuneCollisionFilenameLength` (executed) produced a **265-byte** basename (`2026-03-01 ` + 60×4-byte runes + ` (event-aa)` + `.md`), exceeding the 255-byte NAME_MAX of ext4 and most Linux filesystems; export.go:43-44's own comment claims the collision suffix and `.md` fit under 255 |
| Security | PASS | Path confinement and catastrophic-dir/symlink guards hold under direct attack (sections above); untrusted names sanitized at the barricade and re-confined before every write |

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|---|---|---|---|
| cc-defensive-programming | External input validated at entry (barricade) | **FAIL** | Sanitization/confinement of untrusted names is structurally sound, but the length-validation bound is arithmetically wrong for one composition: dated event + collision suffix → 265 bytes measured (Issue 1). The barricade's own stated invariant (export.go:43-44) is demonstrably violated |
| cc-defensive-programming | No empty catch blocks / errors never swallowed | PASS | Every error in the four files is returned wrapped; refusals abort the export |
| cc-defensive-programming | Assertions vs error handling; no executable code in assertions | N/A | No assertions used (idiomatic Go); anticipated runtime errors all use error handling |
| cc-defensive-programming | Correctness-over-robustness at the destructive boundary | PASS | Refuse-and-abort on any confinement doubt (bug-stop, never an escape); catastrophic targets refused even under `--force`; clean-late ordering protects existing vaults |
| cc-defensive-programming | Defense-in-depth on security-critical path | PASS | Two independent layers: sanitizeFilename at rendering + confinedVaultPath re-verification immediately before each write; symlink resolution on both sides of the catastrophic comparison |
| cc-refactoring-guidance | Old pattern fully removed, no mixed leftovers | PASS | Entity-per-note format gone (no root-level notes asserted e2e; no dead old-format code found by grep) |
| cc-refactoring-guidance | Behavior verified by tests after the change | PASS | 150 tests pass; determinism pinned unit + e2e |

## Notes (non-blocking)

1. **`TestPrepareVaultDir_AbsErrorWithDeletedCwd` does not cover its target branch on darwin** — coverage shows export.go:165-167 at 0 hits: `os.Getwd` still succeeds after the cwd is deleted on this platform, so the test passes via the later `MkdirAll` ENOENT instead of the `filepath.Abs` error it names. The test is green for the wrong reason; the branch itself is a correct single-statement OS-error return.
2. The residual-clash loop in `buildVaultRefs` (vaultmodel.go, prior phase) can embed the FULL untrusted server id into a filename (`base + " (" + c.id + "-" + n + ")"`), which is unbounded — same byte-length class as Issue 1; worth bounding in the same fix.
3. `isCatastrophicVaultDir` protects only `/` and `$HOME`; `--force` on other sensitive writable dirs (e.g. `~/Documents`) will clean them. Matches the stated guard scope ("root or home"), so in-spec; observation only.
4. Uncovered OS-error branches enumerated in the coverage table — all within the dispatch carve-out.

## Issues (FAIL)

1. **Note filename can exceed the 255-byte NAME_MAX the code claims to stay under — export aborts on Linux after the old vault was already destroyed.**
   - File: `internal/cli/export.go:42-44` (`maxFilenameRunes` and its fits-under-255 claim); composition in `buildVaultRefs` (vaultmodel.go:392-398, date prefix + collision suffix)
   - Demonstrated by: reviewer test `TestADV_LongRuneCollisionFilenameLength` (executed via overlay): two dated episodics with identical 60×4-byte-rune titles → basename measured at **265 bytes** (`11 (date+space) + 240 (60 runes × 4 bytes) + 11 (collision suffix " (event-aa)") + 3 (".md")`). On APFS (255 Unicode chars) it writes; on ext4/most Linux filesystems NAME_MAX is 255 **bytes**, so `os.Rename` in `writeFileAtomic` fails ENAMETOOLONG → `writeVault` errors → export aborts. Because `prepareVaultDir` has already emptied the vault by then, the user's previous vault is lost and the new one is partial. This directly violates the module's own documented invariant and the loaded defensive-programming skill's input-validation criterion at the filesystem boundary.
   - Fix: enforce the cap in **bytes**, budgeted for the worst-case composition — e.g. truncate the sanitized slug so `len(date prefix) + len(slug bytes) + len(max suffix) + len(".md") ≤ 255` (a slug budget of ~200 bytes is safe), and bound the residual-clash id extension (Note 2) the same way.

**Verdict: FAIL — blocker: Issue 1 (filename byte-length overflow past NAME_MAX for dated colliding multi-byte titles; demonstrated 265-byte basename vs the documented 255-byte bound).**

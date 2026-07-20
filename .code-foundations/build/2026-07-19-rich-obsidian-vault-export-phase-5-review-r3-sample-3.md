# Review: Phase 5 - Vault assembly + CLI wiring (r3, sample 3)

Independent post-gate review. Primary focus this round: the filename BYTE budget (data-loss fix). All adversarial work executed against the real `writeVault`/`prepareVaultDir` in a scratch copy of the module under `/tmp/r3rev3-p5` (no repo mutation); scratch temp dirs via `TMPDIR=/tmp/r3rev3-p5/tmpdir`.

## Executed Results (Step 0)

- Test suite: `go test ./internal/cli/ -count=1` → **ok** (all tests pass, 0 failures)
- Coverage: `go test ./internal/cli/ -cover` → 72.2% of statements; `go tool cover -func` breakdown below
- Lint: `gofmt -l ./internal/cli/` → **clean** (no output); `go vet ./internal/cli/` → clean
- Security refocus: `TestDW_5_2_HostileNamesStayConfined`, `TestDW_5_2_ConfinedVaultPathRejectsEscapes`, `TestPrepareVaultDir_SymlinkToHomeRefused`, `TestPrepareVaultDir_SymlinkToRootRefused`, `TestDW_5_4_FetchFailureLeavesVaultIntact`, `TestExport_ForeignDirRefusedWithoutForce` → all **PASS** under `/tmp/r3rev3-p5` scratch
- Adversarial reviewer tests (scratch copy, real functions): 5 PASS, **1 FAIL** — see Issues

## Requirement Fulfillment

### DW-5.1
PREMISE:  writes `events/`,`concepts/`,`maps/`; entity-per-note gone; `fetchExport` drains episodics.
EVIDENCE: internal/cli/vault.go:90-127 (writeVault three render loops); internal/cli/export.go:121-142 (fetchExport accumulates `page.Episodics` per page)
TRACE:    richPage fixture → `writeVault` → `events/2026/2026-03-01 Alpha shipped the beta.md`, `events/undated/Gamma joined Beta.md`, `concepts/Alpha.md`, `maps/Alpha.md`; no root-level `.md` (entity-per-note gone); 3-page fixture with episodics split across pages drains both events.
VERDICT:  PASS — `TestDW_5_1_WriteVaultRichLayout`, `TestDW_5_1_RichVaultLayoutEndToEnd`, `TestDW_5_1_FetchExportDrainsEpisodicsAcrossPages` all executed green.

### DW-5.2
PREMISE:  every write stays inside the vault dir incl. nested folders.
EVIDENCE: internal/cli/vault.go:57-82 (confinedVaultPath: root allowlist, exact per-root depth, per-element dots/spaces refusal, final `filepath.Rel` re-check); vault.go:153-162 (writeVaultNote confines before any FS effect)
TRACE:    `../pwn.md`, `events/2026/../pwn.md`, `events\2026\pwn.md`, `/etc/passwd`, wrong-depth and empty-element paths → all refused with zero files written; hostile entity/event names (`../../etc/passwd` etc.) through full `writeVault` → 3 events/1 concept/1 map all inside the vault, canary outside untouched, no `etc/` dir created.
VERDICT:  PASS — `TestDW_5_2_ConfinedVaultPathRejectsEscapes`, `TestDW_5_2_HostileNamesStayConfined`, plus reviewer `TestReview_CraftedNamesRefusedOrConfined` (encoded traversal, dots-only names, 40-deep `../` — confined, canary intact), executed green under scratch.

### DW-5.3
PREMISE:  full-vault re-run byte-identical.
EVIDENCE: internal/cli/vault_test.go:444-462; deterministic assignment order at vaultmodel.go:409-414 (sorted (id, folder)) and vaultmaps.go:263-264 (sorted Key)
TRACE:    richRecords → writeVault into dirA and dirB → trees byte-identical; reversed input slices → still byte-identical. Reviewer stress fixture (25 colliding 240-byte emoji events + emoji concepts, twice + permuted) → byte-identical.
VERDICT:  PASS — `TestDW_5_3_ReRunByteIdentical` + reviewer `TestReview_StressDeterminism`, executed green.

### DW-5.4
PREMISE:  fetch failure leaves vault untouched; empty tenant → marker-only; clobber warning prints.
EVIDENCE: internal/cli/export.go:100-106 (fetch precedes prepareVaultDir), export.go:111 (warning line)
TRACE:    export into an existing vault, second run against a non-advancing-cursor server → exit ≠ 0 and before/after trees byte-identical; empty ExportResponse into a missing nested dir → exactly one file (`.engram-vault` marker) + "0 events, 0 concepts, 0 maps"; successful export prints "warning: … clobbered".
VERDICT:  PASS — `TestDW_5_4_FetchFailureLeavesVaultIntact`, `TestDW_5_4_EmptyTenantMarkerOnlyVault`, `TestDW_5_4_ClobberWarningPrints`, executed green.

### DW-5.5
PREMISE:  summary reports events/concepts/maps/ghosts/dropped.
EVIDENCE: internal/cli/export.go:112-113 (summary Fprintf); vault.go:28-34 (vaultStats), vault.go:134-148 (countDroppedEdges)
TRACE:    richPage → "exported 2 events, 1 concepts, 1 maps to <dir> (2 ghosts, 1 dropped)" asserted verbatim; half-dangling edge (one exported endpoint) correctly NOT counted as dropped.
VERDICT:  PASS — `TestDW_5_5_SummaryCountsPrinted`, `TestDW_5_5_StatsCounts`, executed green.

**All requirements met:** YES (DW items) — but see the primary-focus blocker below.

## Primary Focus — filename byte budget

- `TestTruncateBytes_RuneBoundary`, `TestFitNoteName_ByteBudget`, `TestUniqueNoteName_BoundedGrowth`, `TestDW_Fix_LongNamesFitNameMax` (repo) — executed green.
- Reviewer `TestReview_MassCollidingEmojiEvents`: 40 dated events, one 60-emoji (240-byte) title, ids sharing a 30-byte prefix so id-extension 8→12→…→24 never disambiguates and the counter ladder must resolve all 40. Real `writeVault` → 40 distinct files, max basename **240 bytes**, every basename ≤255 and valid UTF-8. PASS.
- Reviewer `TestReview_EmojiConceptAndMapNames`: colliding 60/61-rune emoji concept names + a 70-rune 🌍 name, triangle component so the emoji name drives the maps/ title. All basenames ≤255, valid UTF-8, emoji-named map file written. PASS.
- Reviewer `TestReview_MultibyteIDSuffixValidUTF8`: **FAIL — confirmed defect** (Issues #1).
- Reviewer `TestReview_SlashInIDSuffix`: barricade refuses (nothing escapes — good), but the export aborts (Issues #1, same root cause; Notes).

## Regression Focus — filename assignment refactor

- Determinism: byte-identical across re-run and permuted input, both on richRecords and the emoji-collision stress fixture. No regression.
- Collision disambiguation: global case-insensitive homonyms across events+concepts still suffixed (vaultmodel_test.go:313-318 "Widget" event vs "Widget" concept — cross-folder), residual clashes extend then counter-resolve (`TestUniqueNoteName_BoundedGrowth`: 50 distinct names, all in budget). Intact.
- Misc-bucket immunity: reviewer `TestReview_MiscBucketImmunity` — concept cluster titled "misc-01" is force-suffixed (`maps/misc-01 (…).md`), the real bucket keeps `maps/misc-01.md` and holds the loner concept. Intact.

## Test-DW Coverage

- [x] All DW items have corresponding tests, DW-named, ran in Step 0
- [x] Coverage matches the stated level: `idPrefix`/`truncateBytes`/`fitNoteName`/`uniqueNoteName` 100%; refactored `buildVaultRefs` 100%, `assignClusterFilenames` 100%; `runExport` 96.8% and `prepareVaultDir` 88.9% (accepted non-portable OS-error returns, per dispatch)
- Remaining sub-100% (not in the dispatch's 100% set, same non-portable class): `writeFileAtomic` 66.7% (temp-file WriteString/Close error branches), `confinedVaultPath` 93.3% (the final `filepath.Rel` re-check refusal — defensively unreachable by design). Non-blocking (Notes).

## Dead Code

None found. No unreachable code, debug statements, or commented-out blocks in the six reviewed files; imports compile-enforced.

## Correctness Dimensions

| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | Single-threaded CLI pipeline; no shared state, no goroutines in the reviewed code |
| Error Handling | PASS | Every write/prepare/fetch error propagates and aborts loudly: `TestWriteVault_WriteFailurePropagates` (each root blocked), `TestWriteFileAtomic_ErrorPaths`, `TestPrepareVaultDir_FilesystemErrors`, `TestExport_FetchErrorAborts`, non-advancing-cursor abort — all executed |
| Resources | PASS | Temp files removed on every writeFileAtomic failure path (asserted: no `.engram-tmp-*` lingers); `defer client.Close()` at export.go:99 |
| Boundaries | **FAIL** | `idPrefix` (export.go:237-242) slices client-supplied id BYTES mid-rune; the invalid UTF-8 lands verbatim in a real filename — demonstrated by executed test (Issues #1) |
| Security | PASS | Confinement held under every probe: hostile names, crafted ids, encoded/deep traversal — nothing ever written outside the vault, canaries intact; symlink→scratch-$HOME under `--force` refused before any cleaning; failed fetch never clobbers |

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-refactoring-guidance | Behavior preservation: refactored filename assignment produces identical output (determinism, collisions, misc immunity) | PASS | Byte-identical trees across re-run + permuted input on both fixtures; collision + immunity probes green (executed) |
| cc-refactoring-guidance | Refactor toward a target pattern: one shared suffixing algorithm, not three divergent copies | PASS | `uniqueNoteName` (export.go:307) is the single algorithm consumed by buildVaultRefs (vaultmodel.go:425) and assignClusterFilenames (vaultmaps.go:307) |
| cc-defensive-programming | External input validated at entry before reaching the filesystem namespace | **FAIL** | Client-supplied `event_id` (proto: "client-supplied idempotency identity"; server rejects only EMPTY ids — internal/server/server.go:94) reaches filenames via `idPrefix` with NO sanitization; names/titles pass `sanitizeFilename`, id-derived suffixes do not. Demonstrated (Issues #1) |
| cc-defensive-programming | No empty catch blocks / errors never swallowed | PASS | All error returns propagate; refusals abort the export (executed error-path tests above) |
| cc-defensive-programming | Barricade design: path confinement re-verified immediately before every write | PASS | confinedVaultPath before every atomic write; refused a crafted `/`-bearing suffix before any FS effect (reviewer `TestReview_SlashInIDSuffix`: zero files written) |

## Notes (non-blocking)

- `writeFileAtomic` temp-file WriteString/Close error branches and `confinedVaultPath`'s final `Rel` re-check are uncovered — same non-portable/defensively-unreachable class as the dispatch's accepted misses.
- A `/` (or `\`) inside a client-supplied event id reaches the suffix and is REFUSED by the barricade (no escape, nothing written — verified), but in a real run that refusal fires after `prepareVaultDir` cleaned the old vault: safe-but-lossy abort. Same root cause and same fix as Issues #1.
- `confinedVaultPath` does not check UTF-8 validity of path elements; adding it would turn the EILSEQ surprise into an explicit barricade refusal (still an abort — the real fix is at suffix composition).
- `assignClusterFilenames` embeds cluster `Key` (an entity id) in map-name suffixes with the same lack of sanitization — the Issues #1 fix should cover this call site too.

## Issues (FAIL)

1. **Client-supplied id bytes enter collision-suffix filenames unsanitized → invalid-UTF-8 basename / write failure after the vault is clobbered** (the exact data-loss class this round's fix was meant to close)
   - File: internal/cli/export.go:237-242 (`idPrefix` — raw byte slice, can split a rune); consumed at export.go:311-318 (`uniqueNoteName` suffix composition), vaultmodel.go:425, vaultmaps.go:307
   - Demonstrated by: reviewer test `TestReview_MultibyteIDSuffixValidUTF8` (scratch copy, real `writeVault`): two events sharing a title with ids `世×8` and `世×8 + "x"` (event_id is client-supplied per api/proto/engram.proto; the server validates only non-emptiness, internal/server/server.go:94). Homonym → suffix `idPrefix(id, 8)` = 2 full runes + 2 bytes of the third → basename `dup title (世世\xe4\xb8).md` is invalid UTF-8. Executed result on darwin/APFS: `rename … dup title (世世�).md: illegal byte sequence` — `writeVault` FAILS, i.e. in a real export the abort lands AFTER `prepareVaultDir` destroyed the old vault. On byte-transparent filesystems (ext4) the same run writes an invalid-UTF-8 filename instead. Both outcomes are enumerated FAIL conditions for this round ("invalid-UTF-8 filename", "write failure" post-clobber), and rune-splitting truncation is the named adversarial probe.
   - Fix: never embed raw id bytes — truncate the id on a rune boundary (`truncateBytes`, already written and tested) AND sanitize the suffix the same way bases are (or restrict/encode suffix bytes to a safe alphabet, e.g. hex/base32 of the id prefix). Apply at `uniqueNoteName`'s suffix composition so both call sites (events/concepts and maps) are covered; keeps the "/" abort-after-clobber note fixed for free.

**Verdict: FAIL — one blocker: unsanitized id-derived collision suffixes produce invalid-UTF-8 basenames / post-clobber write failures (Issues #1); everything else, including all five DW items, the byte budget for title-derived names, determinism, collision/misc regressions, and all security invariants, verified green.**

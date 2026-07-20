# Review: Phase 5 - vault assembly + CLI wiring (r4, sample 3)

Independent post-gate review. All commands run from the worktree root; adversarial
harness run against a full copy of the tree at /tmp/r4rev3-p5/repo (repo untouched).

## Executed Results (Step 0)
- Test suite: `go test ./internal/cli/ -count=1` → **ok** (whole package, 0 failures)
- Coverage: `go test ./internal/cli/ -cover` → 72.5% pkg; `go tool cover -func` → **all filename helpers 100%** (idPrefix, sanitizeFilename, truncateBytes, fitNoteName, safeNoteName, uniqueNoteName, cleanInline; also every function in vaultmodel.go, vaultmaps.go, vaultnotes.go)
- Lint: `gofmt -l ./internal/cli/` → **empty output** (clean); `go vet ./internal/cli/` → clean
- Reviewer adversarial harness (scratch copy, real `writeVault` + real CLI):
  `TestREVIEW_AdversarialBasenames`, `TestREVIEW_AdversarialCLIEndToEnd` → **PASS**

## Requirement Fulfillment

### DW-5.1
PREMISE:  writes `events/`,`concepts/`,`maps/`; entity-per-note gone; `fetchExport` drains episodics.
EVIDENCE: internal/cli/vault.go:90-127 (writeVault: three render loops), internal/cli/export.go:121-142 (fetchExport accumulates `page.Episodics` across pages), internal/cli/vaultnotes.go:44,72 (relPath = Folder + "/" + File + ".md")
TRACE:    richPage fixture (2 events, 3 entities, 3 edges) → CLI `export` → files at `events/2026/2026-03-01 Alpha shipped the beta.md`, `events/undated/Gamma joined Beta.md`, `concepts/Alpha.md`, `maps/Alpha.md`; no `.md` at vault root (entity-per-note asserted gone); multi-page fixture drains episodics from pages 1 and 2. Grep for `renderEntity|entityNote` → no remnants.
Tests: TestDW_5_1_WriteVaultRichLayout, TestDW_5_1_RichVaultLayoutEndToEnd, TestDW_5_1_FetchExportDrainsEpisodicsAcrossPages — all PASS.
VERDICT:  PASS

### DW-5.2
PREMISE:  every write stays inside the vault dir incl. nested folders.
EVIDENCE: internal/cli/vault.go:57-82 (confinedVaultPath: root whitelist, exact depth, per-element dots/spaces refusal, backslash/abs refusal, final Rel re-check), vault.go:153-161 (barricade before any filesystem effect)
TRACE:    22 hostile relPaths (`../pwn.md`, `events/2026/..`, `events\2026\pwn.md`, `secrets/pwn.md`, wrong depths, empty elements, `/etc/passwd`) → all refused with zero files created; hostile entity/title data (`../../etc/passwd`, `..\..\win\shadow`, `/etc/shadow`) driven through real writeVault → 3 events/1 concept/1 map written, canary outside vault byte-intact, no `etc/` created outside. My harness re-verified with traversal ids (`../../../../etc`, `....//....`) and separator ids through the real CLI: canary intact, WalkDir found no file outside the vault.
Tests: TestDW_5_2_ConfinedVaultPathRejectsEscapes, TestDW_5_2_HostileNamesStayConfined, TestREVIEW_AdversarialBasenames/CLIEndToEnd — all PASS.
VERDICT:  PASS

### DW-5.3
PREMISE:  full-vault re-run byte-identical.
EVIDENCE: internal/cli/vaultmodel.go (sorted-key drains throughout), vaultmaps.go:14-15 (fixed traversal/sort orders), vault_test.go:545-563
TRACE:    same input → two dirs → trees byte-identical; reversed episodics/entities/edges → still byte-identical. My harness repeated this over the full hostile fixture (36 events incl. 20 counter-path collisions, 12 entities, invalid-UTF-8 ids): re-run and permuted-input run both byte-identical.
Tests: TestDW_5_3_ReRunByteIdentical, TestDW_Fix_MultibyteIDsSafeBasenames (re-run leg), TestREVIEW_AdversarialBasenames — all PASS.
VERDICT:  PASS

### DW-5.4
PREMISE:  fetch failure leaves vault untouched; empty tenant → marker-only; clobber warning prints.
EVIDENCE: internal/cli/export.go:100-111 (fetch before prepareVaultDir — clean-late ordering), export.go:111 (warning line), export_test.go:208-255
TRACE:    export → vault exists; second run against a non-advancing-cursor server → exit != 0 and tree byte-identical to before (also re-proven over a hostile-name vault in my harness). Empty page fixture → vault holds exactly `.engram-vault` marker + "0 events, 0 concepts, 0 maps" summary. Successful export output contains "warning:" + "clobbered".
Tests: TestDW_5_4_FetchFailureLeavesVaultIntact, TestDW_5_4_EmptyTenantMarkerOnlyVault, TestDW_5_4_ClobberWarningPrints, TestREVIEW_AdversarialCLIEndToEnd — all PASS.
VERDICT:  PASS

### DW-5.5
PREMISE:  summary reports events/concepts/maps/ghosts/dropped.
EVIDENCE: internal/cli/export.go:112-113 (printf of all five counters), vault.go:28-34 (vaultStats), vault.go:134-148 (countDroppedEdges: dropped = neither endpoint exported)
TRACE:    richPage + one half-dangling edge → stats {2,1,1,2,1} (half-dangling edge lands as claim, NOT dropped); CLI output contains exact line "exported 2 events, 1 concepts, 1 maps to <dir> (2 ghosts, 1 dropped)". My harness cross-checked stats against on-disk file counts per folder (silent-overwrite detector): equal.
Tests: TestDW_5_5_StatsCounts, TestDW_5_5_SummaryCountsPrinted, TestDW_5_1_WriteVaultRichLayout — all PASS.
VERDICT:  PASS

## PRIMARY focus — unsafe-basename hunt (data-loss class)

Constructed my own adversarial inputs and drove them through the REAL `writeVault` and the REAL CLI (stub gRPC server), scratch at /tmp/r4rev3-p5. Every written basename asserted `utf8.Valid`, `len<=255` bytes, no fsIllegal, no control chars, no empty/dots-only path element, file-count == stats (no silent overwrite):

| Attack | Fixture | Result |
|---|---|---|
| Mass collision, multibyte base + shared 24-byte CJK id prefix | 20 homonym dated events, 240-byte emoji title, ids `世×8 + "-tail-i"` — extension exhausts maxSuffixIDBytes, forces counter path | 20 distinct files, all ≤240B, valid UTF-8 |
| Invalid-UTF-8 ids into suffixes | ids `\xff\xfe…`/`\xf8…\xff` on homonym titles | suffix strips to `()`/`(-8)`; valid, distinct |
| All-control / NUL ids | 3 homonym events, ids of `\x00`–`\x10` bytes | suffixes collapse to `()` then counter; distinct, no control chars written |
| Separator/traversal ids | `../../../../etc`, `..\..\win\shadow`, `/etc/passwd`, `....//....` | separators → `-` in suffix; confined |
| Empty-sanitizing titles | `...`, `\x00\n\t`, `. . .` | forced `event (id…)` fallback; never empty element |
| Truncation-induced collision (not homonym-detected) | two titles distinct only at rune 60, date prefix pushes both past budget → identical 237-byte truncation | residual loop suffixes the second; 2 files, no overwrite |
| Boundary-length 4-byte-rune title | 59×4B + 1×3B = 239 bytes, undated | truncated on rune boundary, ≤240B |
| Concept homonyms w/ emoji ids + invalid-UTF-8 entity name | `Conc/Dup`/`Conc-Dup` hubs, ids `🦄🦄🦄x/y`; name `\xffBad\xfeName` hub | residual extension resolves on rune boundary; U+FFFD name valid UTF-8 |
| Map homonyms w/ CJK cluster keys exhausting extension | two components topping `Map/Dup`/`Map-Dup`, keys sharing `日×8` prefix | counter path in maps/; distinct |
| Clobber-then-abort end-to-end | hostile export → re-export same dir → failed fetch | both exports exit 0, failed fetch leaves hostile vault byte-identical |

**No unsafe basename was produced.** Wire note: gRPC/proto3 refuses to marshal invalid UTF-8 strings at all (server errored when my stub tried), so invalid-UTF-8 ids cannot even arrive over the real wire — `safeNoteName`'s ToValidUTF8 is genuine defense-in-depth behind that transport barricade.

Static trace of the choke point (export.go:313-335): ToValidUTF8 → per-rune drop(control)/map(fsIllegal→'-')/keep → Trim(". ") → truncateBytes(237, rune-boundary) → TrimRight(". ") → ""→"note". Result is provably valid UTF-8, ≤237B (so name+".md" ≤240 ≤ NAME_MAX), no illegal/control chars, no leading/trailing dots/spaces. Every write path reaches it: buildVaultRefs → uniqueNoteName → safeNoteName (vaultmodel.go:425); assignClusterFilenames suffixed path → uniqueNoteName (vaultmaps.go:309); misc immune path → safeNoteName directly (vaultmaps.go:302). Folder components are code-generated (`events/`+`Format("2006")`, `events/undated`, `concepts`, `maps`) — digits/'-'/ASCII only — and confinedVaultPath re-verifies the joined path before every write.

## Regression — uniqueNoteName / determinism / misc immunity

- Uniqueness is checked on the FINAL (post-safeNoteName) form and recorded case-folded (export.go:353,363), so sanitization-collapsed distinct candidates re-enter the loop instead of overwriting — demonstrated by the truncation-collision and all-control-id fixtures (file counts matched stats).
- Termination/distinctness: counter digits survive safeNoteName verbatim and the suffix is never truncated (fitNoteName truncates only the base); TestUniqueNoteName_BoundedGrowth (50 clashes ≤ budget, 50 distinct) + my 20-way multibyte counter-path run.
- Misc immunity: `misc-NN` recorded in `used`, never suffixed; a concept base prefixed `misc-` (case-insensitive) is unconditionally forced through the suffix path (vaultmaps.go:288-290), and forced names always carry a ` (id…)` suffix, so no concept map can ever equal a canonical misc name. `maps/misc-01 (日本).md` fixture confirms.
- Determinism under permutation: byte-identical on reversed input, both in the shipped suite and over my hostile fixture.

## Security (still holds)

- Confinement: hostile names/separators refused or neutralized — TestDW_5_2_* + harness (canary intact, nothing outside vault).
- `--force` + symlink→scratch-$HOME refused before any cleaning, canary (`precious.txt`) intact — TestPrepareVaultDir_SymlinkToHomeRefused PASS (real-path comparison, export.go:190-204). Symlink→/ likewise — TestPrepareVaultDir_SymlinkToRootRefused PASS.
- Failed fetch never clobbers: clean-late ordering (fetch at export.go:100 precedes prepareVaultDir at :104) + TestDW_5_4_FetchFailureLeavesVaultIntact + harness re-proof over a hostile vault.

## Test-DW Coverage
- [x] All DW items have DW-named automated tests that ran in Step 0 (DW-5.1 ×3, DW-5.2 ×2, DW-5.3 ×1, DW-5.4 ×3, DW-5.5 ×2, plus DW_Fix_* for the data-loss class)
- [x] Filename helpers 100% covered; remaining sub-100% functions are OS-error/non-portable branches only (detailed in Notes)

## Dead Code
None found. No TODO/FIXME/debug prints in the six reviewed files; no entity-per-note remnants; go vet clean.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | single-threaded CLI pipeline; no goroutines or shared mutable state in the reviewed code |
| Error Handling | PASS | probed: fetch failure, non-advancing cursor, dial failure, blocked folders, rename-onto-dir, read-only parents, deleted cwd — all fail loud with wrapped errors, no partial vault; no swallowed errors (yaml.Marshal ignore is documented cannot-fail; os.Remove ignores are best-effort cleanup after a primary error) |
| Resources | PASS | temp files removed on every writeFileAtomic failure path (TestWriteFileAtomic_ErrorPaths asserts no `.engram-tmp-*` lingers); gRPC client closed via defer (export.go:99) |
| Boundaries | PASS | probed: empty export (marker-only), empty ids skipped, nil OccurredAt (undated bucket, nil-last ordering), 237/239/240-byte name boundaries, rune-boundary truncation, maxSuffixIDBytes exhaustion, empty-sanitizing titles — all handled (traces above) |
| Security | PASS | primary-focus table above: traversal, separators, NUL/control, invalid UTF-8, NAME_MAX overflow, symlink catastrophic targets — all refused or neutralized with execution evidence |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | External input validated at entry (barricade) | PASS | untrusted titles/names/ids pass sanitizeFilename/safeNoteName/cleanInline/sanitizeBody at the render barricade; server cursor guarded against non-advance (export.go:137) |
| cc-defensive-programming | No empty catch equivalents / swallowed errors | PASS | every ignored error is documented-deliberate (`home, _` guarded by `home != ""`; `yaml.Marshal` cannot-fail comment; cleanup `os.Remove` after primary error) |
| cc-defensive-programming | Defense-in-depth on security-critical path, not barricade-only | PASS | three layers: sanitize at render → safeNoteName choke point → confinedVaultPath re-check immediately before each write; plus catastrophic-target guard resolving symlinks on both sides |
| cc-defensive-programming | Correctness over robustness where wrong output = data loss | PASS | over-budget/escaping names abort the export rather than writing approximations; clean-late ordering ensures abort never follows a clobber (harness: clobber-then-abort unproducible) |
| cc-defensive-programming | Assertions vs error handling | N/A | Go idiom — anticipated bad input handled as errors throughout; no assert misuse possible |
| cc-refactoring-guidance | Behavior preserved / no determinism regression from routing through safeNoteName | PASS | byte-identical re-run + permuted-input runs over both plain and hostile fixtures |
| cc-refactoring-guidance | Target-pattern reuse (one algorithm, not divergent copies) | PASS | uniqueNoteName is the single suffixing/budget algorithm shared by buildVaultRefs and assignClusterFilenames; safeNoteName the single choke point (verified all three call sites) |
| cc-refactoring-guidance | Small-change rigor: tests exercise the changed lines | PASS | safeNoteName/fitNoteName/truncateBytes/uniqueNoteName all 100% covered with boundary-case tables |

## Notes (non-blocking)
1. **Coverage misses are non-portable/unreachable, with one propagation-line exception**: prepareVaultDir 88.9% (Abs-error with deleted cwd — darwin's Getwd survives, documented in the test; EvalSymlinks non-ENOENT; post-MkdirAll ReadDir race), writeFileAtomic 66.7% (WriteString/Close failures need ENOSPC-style injection), confinedVaultPath 93.3% (final `filepath.Rel` re-check — the documented unreachable bug-stop; I could not construct an input reaching it past the earlier checks, which is the intended property), runExport 96.8% (line 108: writeVault-error propagation through the CLI — reachable only via mid-run FS mutation; the writeVault error paths themselves are unit-tested in TestWriteVault_WriteFailurePropagates). All fit "accepted non-portable misses"; line 108 is the softest case but is a bare `return err`.
2. gRPC/proto3 rejects invalid-UTF-8 strings on the wire, so the invalid-UTF-8 id defense in safeNoteName is second-layer only — worth keeping (a future non-gRPC ingress would need it), just noting the transport already barricades it.
3. Extreme `OccurredAt` years (e.g. pre-0001) would produce year folder elements like `-0001`; still digits/'-' only, confined and depth-correct — no defect, noted for completeness.
4. Cross-folder filename duplicates (an event and a map sharing a basename) are possible since `buildVaultRefs` and `assignClusterFilenames` keep separate `used` maps; filesystem-safe (different folders), only an Obsidian link-ambiguity nuance and events/concepts already share one map. Design observation, no requirement touched.

## Issues
None.

**Verdict: PASS**

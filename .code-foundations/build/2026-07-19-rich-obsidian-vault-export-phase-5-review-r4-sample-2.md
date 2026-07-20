# Review: Phase 5 - Vault assembly + CLI wiring (r4, sample 2)

## Executed Results (Step 0)
- Test suite: `go test ./internal/cli/ -count=1` → **ok** (all tests pass, 0.058s)
- Coverage: `go test ./internal/cli/ -count=1 -cover` → 72.5% of statements; `go tool cover -func` captured per-function
- Typecheck/vet: `go vet ./internal/cli/` → clean
- Lint: `gofmt -l ./internal/cli/` → **no output** (clean), re-verified after review probes removed
- Reviewer adversarial probes: 3 reviewer-authored tests (temporary `zz_adv_review_test.go`, run via real `writeVault`, then deleted; tree restored to pre-review state) → **all PASS**

## Requirement Fulfillment

### DW-5.1
PREMISE:  writes `events/`,`concepts/`,`maps/`; entity-per-note gone; `fetchExport` drains episodics.
EVIDENCE: internal/cli/vault.go:38 (allowedVaultRoots = events/concepts/maps only), vault.go:101-125 (three render loops), export.go:121-142 (fetchExport accumulates page.Episodics/Entities/Edges until NextCursor empty, aborts non-advancing cursor)
TRACE:    richPage fixture (2 events, 3 entities, 3 edges) → `engram export <dir>` via Run against stub gRPC → tree contains `events/2026/2026-03-01 Alpha shipped the beta.md`, `events/undated/Gamma joined Beta.md`, `concepts/Alpha.md`, `maps/Alpha.md`; no root-level .md (entity-per-note gone); 3-page fixture with episodics split across pages 1-2 → both event notes present.
VERDICT:  PASS — TestDW_5_1_RichVaultLayoutEndToEnd, TestDW_5_1_FetchExportDrainsEpisodicsAcrossPages, TestDW_5_1_WriteVaultRichLayout all pass (executed).

### DW-5.2
PREMISE:  every write stays inside the vault dir incl. nested folders.
EVIDENCE: internal/cli/vault.go:57-82 (confinedVaultPath: root allowlist, exact depth per root, per-element dot/space refusal, backslash refusal, final filepath.Rel re-check), vault.go:153-161 (writeVaultNote confines BEFORE any filesystem effect)
TRACE:    22 hostile relPaths (`../pwn.md`, `events/2026/../pwn.md`, `events\2026\pwn.md`, `/etc/passwd`, wrong-depth, empty-element, dot-only element…) → all refused, zero files written; hostile episodic titles + entity names (`../../etc/passwd`, `..\..\win\shadow`, `/etc/shadow`) through real writeVault → WalkDir over parent shows every file inside vault, canary untouched, no `etc/` created outside. Reviewer probe: hostile IDs (`../../etc/passwd`, `..\..\win\shadow`, `////////`, NUL) as suffix material through writeVault → confinement held, canary intact.
VERDICT:  PASS — TestDW_5_2_ConfinedVaultPathRejectsEscapes, TestDW_5_2_HostileNamesStayConfined, reviewer TestAdvReview_HostileIDsEveryField all pass (executed).

### DW-5.3
PREMISE:  full-vault re-run byte-identical.
EVIDENCE: internal/cli/vault_test.go:545-563; model determinism grounded in vaultmodel.go (sorted-key map drains throughout) and vaultmaps.go (fixed traversal/sort orders)
TRACE:    richRecords → writeVault into dirA, dirB → trees byte-identical; reversed episodics/entities/edges into dirC → still byte-identical. Reviewer probe: 40 homonym events + 10 homonym concepts with colliding CJK ids, permuted input → byte-identical trees.
VERDICT:  PASS — TestDW_5_3_ReRunByteIdentical, reviewer TestAdvReview_MassCollisionMultibyte (permutation leg) pass (executed).

### DW-5.4
PREMISE:  fetch failure leaves vault untouched; empty tenant → marker-only; clobber warning prints.
EVIDENCE: internal/cli/export.go:100-106 (prepareVaultDir called only AFTER fetchExport returns nil error), export.go:111 (warning line), export.go:177-223 (marker written into cleaned dir)
TRACE:    (a) successful export → snapshot tree → second run against non-advancing-cursor server exits non-zero → tree byte-identical to snapshot; failed fetch into missing dir → dir never created. (b) empty ExportResponse → nested missing dir created containing exactly `.engram-vault`, summary "0 events, 0 concepts, 0 maps". (c) successful export stdout contains "warning:" + "clobbered".
VERDICT:  PASS — TestDW_5_4_FetchFailureLeavesVaultIntact, TestDW_5_4_EmptyTenantMarkerOnlyVault, TestDW_5_4_ClobberWarningPrints, TestExport_FetchErrorAborts all pass (executed).

### DW-5.5
PREMISE:  summary reports events/concepts/maps/ghosts/dropped.
EVIDENCE: internal/cli/export.go:112-113 (summary Fprintf with all five stats), vault.go:28-34 (vaultStats), vault.go:134-148 (countDroppedEdges: dropped = edges with NEITHER endpoint exported)
TRACE:    richPage → stdout contains exactly "exported 2 events, 1 concepts, 1 maps to <dir> (2 ghosts, 1 dropped)"; assembly-level: half-dangling edge (one endpoint exported) lands as claim and does NOT count as dropped → stats {2,1,1,2,1}.
VERDICT:  PASS — TestDW_5_5_SummaryCountsPrinted, TestDW_5_5_StatsCounts pass (executed).

**All requirements met:** YES

## Test-DW Coverage
- [x] Every DW item has DW-named automated tests that ran in Step 0 (verbose run captured: all `TestDW_5_*` pass, plus DW-2/3/4 regression suites still green)
- [x] Coverage matches the stated level: filename helpers 100% (`idPrefix`, `sanitizeFilename`, `truncateBytes`, `fitNoteName`, `safeNoteName`, `uniqueNoteName`, `cleanInline` — all 100.0% per `go tool cover -func`)
- [x] Remaining misses verified block-by-block from the cover profile — all accepted non-portable/unreachable:
  - export.go:108-110 — writeVault error propagation in runExport (writeVault error paths unit-tested directly in vault_test.go)
  - export.go:182-184, 192-194, 210-212 — filepath.Abs / EvalSymlinks / post-MkdirAll ReadDir OS-error branches (darwin's Getwd survives a deleted cwd; the deleted-cwd test still proves fail-loud at MkdirAll)
  - export.go:376 — zero-statement switch case (profile artifact)
  - vault.go:78-80 — confinedVaultPath's final Rel re-check refusal: pure defense-in-depth, unreachable by construction on unix (every input that could diverge is refused by the earlier element checks)
  - vault.go:172-180 — writeFileAtomic WriteString/Close error branches (need ENOSPC/fd injection; CreateTemp and Rename error branches ARE covered)

## Dead Code
None found. No unused imports (compiler-enforced), no debug statements, no commented-out blocks, no unreachable code after early returns. safeNoteName's `return "note"` fallback is a documented defensive guarantee (path element must never be empty), exercised by tests — not dead.

## PRIMARY Focus — adversarial attack on safeNoteName (reviewer-authored, executed via real writeVault)

Static trace of the choke point (export.go:313-335): `ToValidUTF8(name, "")` → per-rune loop drops C0/DEL, maps all of `fsIllegal` (`/\:*?"<>|#^[]`, all ASCII) to one-byte `-` → `Trim ". "` → `truncateBytes(s, 237)` rune-boundary cut → `TrimRight ". "` → `"note"` on empty. Invariants proven by the trace: valid UTF-8 (truncateBytes never splits a rune, vault_test.go:322-344), ≤237 bytes + ".md" = ≤240 < 255, no fsIllegal/control chars, no leading/trailing dots/spaces, non-empty, and sanitization never lengthens (so a fitNoteName-budgeted input stays budgeted). All three producers route through it: buildVaultRefs → uniqueNoteName (vaultmodel.go:425), assignClusterFilenames concept path → uniqueNoteName (vaultmaps.go:309), misc path → safeNoteName directly (vaultmaps.go:302). Folder elements (`events/<year>`, `events/undated`, `concepts`, `maps`) are code-generated (Go time formatting emits only digits/dashes). Uniqueness is checked and recorded on the FINAL safeNoteName form (export.go:353, 363), so post-sanitization collapse cannot break it; id-prefix rungs strictly grow (step 4 ≥ max rune width) and the ASCII counter guarantees termination and distinctness.

Three reviewer-authored probes (temporary test file, since deleted; tree verified restored via `git status`):

| Probe | Attack | Result |
|---|---|---|
| TestAdvReview_MassCollisionMultibyte | 40 homonym events (same 60-emoji title + date) with CJK ids sharing an identical 30-byte prefix — forces the FULL ladder: 8-byte suffix → 12/16/20/24-byte residual extensions (all colliding) → counter path; 10 concepts with distinct names all rune-capping to one slug; every basename asserted utf8.Valid, ≤255B, no fsIllegal, no control, no dot/space edges; counter suffix `-24)/-28)/-32)` presence asserted; permuted-input byte-identity asserted | PASS — 40 + 10 distinct files, counter engaged, deterministic |
| TestAdvReview_HostileIDsEveryField | invalid UTF-8 (`\xff\xfe\xfd`), NUL+control+DEL, `../../etc/passwd`, `..\..\win\shadow`, all-separator, all-dot, emoji, CJK-then-partial-rune, trim-fodder, all-fsIllegal ids as event AND entity ids, with empty-sanitizing titles forcing the id-suffix fallback path; canary outside vault | PASS — export succeeds (no clobber-then-abort), all basenames safe, nothing escaped, canary intact |
| TestAdvReview_BoundaryRuneTitles | 0-3 ASCII padding shifting a 60×4-byte-rune title against the 237-byte cut at every grid offset, 3 homonyms per offset stacking suffixes atop max-length bases | PASS — 12 distinct safe basenames |

One probe iteration initially "failed" on my fixture, not the code: 10 entities sharing one identical name collapse into a single concept by the Phase 2 normalized-name-collapse contract (vaultmodel.go:170) — intended behavior, fixture corrected.

**No unsafe basename could be produced from any field. The data-loss class (invalid-UTF-8 / >NAME_MAX / illegal-char / vault-escape / clobber-then-abort) is closed at the choke point.**

## Regression — uniqueNoteName routes every candidate through safeNoteName
- All four candidate constructions in uniqueNoteName pass `safeNoteName(fitNoteName(...))` (export.go:349, 351, 355, 360) BEFORE the uniqueness check — verified by reading, and behaviorally by the mass-collision probe.
- No determinism regression: TestDW_5_3 + reviewer permuted adversarial fixture → byte-identical.
- Collision uniqueness/termination intact: 40-deep ladder terminated with 40 distinct names; TestUniqueNoteName_BoundedGrowth (50 clashes, all ≤240B, all distinct) passes.
- Misc immunity intact: misc bucket names are recorded in `used` and never suffixed but still pass safeNoteName (vaultmaps.go:302); a concept sanitizing to `misc-*` is unconditionally forced through the suffix path (vaultmaps.go:288) — TestDW_4_1_ConceptTitleCollidesWithMiscPrefixReserved and the suite's `maps/misc-01 (日本).md` fixture assertion pass.
- Sanitization collapsing distinct names cannot break uniqueness: the used-map check happens on the final sanitized form; a post-sanitization collision enters the suffix loop (traced; exercised by the `Dup/Concept`/`Dup-Concept` pair in TestDW_Fix_MultibyteIDsSafeBasenames and by the probes).

## Security (must still hold)
| Invariant | Status | Evidence |
|---|---|---|
| Confinement: hostile names/separators refused | PASS | TestDW_5_2_* + reviewer hostile-ID probe; canary + WalkDir escape scan clean |
| `--force` symlink→scratch-$HOME refused, canary intact | PASS | TestPrepareVaultDir_SymlinkToHomeRefused (t.Setenv HOME to scratch, full CLI, `refusing to clobber`, precious.txt survives); symlink→/ also refused (TestPrepareVaultDir_SymlinkToRootRefused) |
| Failed fetch never clobbers | PASS | TestDW_5_4_FetchFailureLeavesVaultIntact (byte-identical before/after), TestExport_FetchErrorAborts + TestExport_NonAdvancingCursorAborts (dir never created); trace: prepareVaultDir strictly after fetchExport success (export.go:100-106) |

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | Single-threaded CLI pipeline; no shared mutable state, no goroutines beyond the test stub server |
| Error Handling | PASS | Every OS/RPC call checked and wrapped; adversarial traces: non-advancing cursor aborts, dial/fetch/flag errors surface with dir untouched, write failure in any render loop aborts loudly (TestWriteVault_WriteFailurePropagates), temp file removed on every failure leg |
| Resources | PASS | client.Close deferred (export.go:99); writeFileAtomic closes+removes temp on all three failure paths (traced 166-186; TestWriteFileAtomic_ErrorPaths confirms no `.engram-tmp-*` lingers) |
| Boundaries | PASS | Probed the exact 237-byte cut against a 4-byte-rune grid at all four offsets; empty export, empty ids, nil times, 0-concept model all traced/tested |
| Security | PASS | Primary-focus probes above; untrusted content sanitized at every sink (filenames via safeNoteName, links via cleanInline, bodies via sanitizeBody) with confinement re-checked pre-write |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | External input validated at entry (episodic prose, entity names, client ids are untrusted) | PASS | Barricades at every sink: safeNoteName (filenames), confinedVaultPath (paths), cleanInline/sanitizeBody (markdown); adversarial probes could not breach any |
| cc-defensive-programming | No empty catch / no swallowed errors | PASS | Every error path returns wrapped errors; cleanup errs on os.Remove after a primary failure are intentionally best-effort (temp-file GC), primary error always propagates |
| cc-defensive-programming | Barricade + defense-in-depth on security-critical path | PASS | Double guard: sanitize-at-source AND confinedVaultPath re-verification pre-write; clobber path double-guarded (marker check + catastrophic-target check with symlink resolution on BOTH sides) |
| cc-defensive-programming | Correctness over robustness for data-destroying ops | PASS | Refusal aborts the whole export rather than writing anything (vault.go:57 doc + behavior); clean-late ordering means a failed fetch destroys nothing |
| cc-defensive-programming | Assertions vs error handling: anticipated bad input handled, not asserted | PASS | Hostile ids/names degrade gracefully (confined, suffixed), never panic; "should-never-happen" cases (empty safeNoteName result, refs miss in wikilink) get safe fallbacks with documented rationale |
| cc-refactoring-guidance | Behavior-preserving where claimed: shared suffixing algorithm reused, not duplicated divergently | PASS | uniqueNoteName is the single algorithm for events/concepts (vaultmodel.go:425) and maps (vaultmaps.go:309); byte-identical determinism regression held under permuted adversarial input |
| cc-refactoring-guidance | Small-change rigor: choke-point insertion did not regress existing invariants | PASS | Full suite green including all Phase 2-4 DW regression tests; misc immunity, homonym suffixing, and collision termination re-verified by probes |

## Notes (non-blocking)
- vault.go:78-80 (confinedVaultPath's final `filepath.Rel` re-check refusal) is uncovered — unreachable by construction on unix because the earlier element checks refuse every diverging input. It is deliberate defense-in-depth; a comment already frames it that way. No action needed.
- `checkVaultDir` treats a dir containing only the marker as owned even if the marker is a directory (os.Stat succeeds either way). Harmless: prepareVaultDir's clean removes it, and only tool-owned dirs plausibly contain `.engram-vault`. Could not demonstrate any damage.
- `uniqueNoteName`'s ladder is O(k) probes for the k-th colliding homonym (O(k²) total). At 40 collisions this is instant; a pathological tenant with tens of thousands of identical titles would slow the export but terminate. Matter of degree, not a defect.
- Year folders from `Format("2006")` can render pre-1583 or negative years oddly (e.g. `-005`) if a client supplies such a timestamp — the output is still digits/dash, flat-safe, depth-correct, and deterministic, so no invariant breaks.

## Issues (if FAIL)
None.

**Verdict: PASS**

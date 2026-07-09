# Review: Phase 3 - CLI export + Obsidian vault rendering (sample 2)

## Executed Results (Step 0)
- Test suite: `go test ./internal/cli/... ./internal/engramclient/...` → 27 passed, 0 failed (25 in cli, 2 in engramclient; verbose logs at /tmp/p3-review-sample-2/*.txt)
- Typecheck/build: `go build ./...` → success
- Lint: `make lint` (go vet + revive v1.12.0 `-set_exit_status`) → clean, exit 0
- e2e suite NOT run (per dispatch: shared live cluster); `e2e/scenarios_export.go` verified by reading
- Extra probes (repo untouched, via `go test -overlay` from /tmp/p3-review-sample-2/):
  `TestScratch_PostSanitizationCollisionSuffixed` → PASS, `TestScratch_HostileIDInSuffixIsBugStopped` → PASS

## Requirement Fulfillment

### DW-3.1
PREMISE:  "`engram export ./vault` against a populated tenant writes one `.md` per exported entity with an H1, frontmatter (aliases, mention_count, provenance), and edge bullets."
EVIDENCE: internal/cli/export.go:337-396 (renderNote), 420-457 (writeVault); internal/cli/cli.go:57-58 (dispatch)
TRACE:    2 entities + 1 edge → writeVault → renderNote emits `---\n<yaml>\n---\n\n# Alice\n\n- works_at [[Bob|Bob]]` → Alice.md/Bob.md on disk with engram_id, aliases [Al Ali], mention_count 3, scope/owner_agent_id/source_ids/valid_at/created_at.
VERDICT:  PASS — TestDW_3_1_WriteVaultRendersNotes (renderer) + TestExport_ProtoFieldsReachFrontmatter (full CLI path against a stub gRPC server, pinning the adapter mapping) both executed and green. The live populated-tenant path is the e2e scenario (read-verified per dispatch instructions).

### DW-3.2
PREMISE:  "Edges render as `[[file|Display]]` piped links resolving to real note files; no dangling links; dropped-edge count printed."
EVIDENCE: export.go:391-394 (piped-link render from refs), 424-434 (dangler drop + counts), 106-107 (printed report)
TRACE:    4 edges, 2 with an unexported endpoint (`e-ghost`) → refs lookup misses → Dropped=2, Edges=2; kept links target `refs[ToEntityID].File`, which is by construction a written note filename → all links resolve. CLI prints "exported 2 entities … (1 edges, 1 dropped)".
VERDICT:  PASS — TestDW_3_2_EdgeLinksResolveAndDanglersDrop (assertLinksResolve over the real files) + TestDW_3_2_DroppedCountPrinted (CLI stdout) executed and green.

### DW-3.3
PREMISE:  "Homonym display-name collisions get deterministic id-suffixed filenames, stable across re-runs; illegal chars sanitized."
EVIDENCE: export.go:215-264 (vaultFilenames: case-folded baseCount, all-homonyms-suffixed, sorted-id assignment), 294-312 (sanitizeFilename)
TRACE:    {Alice/Alice/alice/Bob/"..."/"a\x00/b"} → `Alice (aaaa1111)`, `Alice (bbbb2222)`, `alice (cccc3333)`, `Bob`, `entity (eeee5555)`, `a-b`; reversed input order → byte-identical assignment (sorted-id, not first-wins).
VERDICT:  PASS — TestDW_3_3_SanitizeFilename (15 cases incl. traversal, reserved chars, dots-only, NUL, unicode, truncation) + TestDW_3_3_HomonymFilenamesDeterministic (incl. case-insensitive no-collision invariant) executed; post-sanitization collision additionally executed via scratch probe (below).

### DW-3.4
PREMISE:  "Re-running clobbers-and-regenerates; refuses a foreign non-empty dir unless `--force`."
EVIDENCE: export.go:137-162 (checkVaultDir: marker-owned OK, foreign non-empty refused sans force), 168-197 (prepareVaultDir: re-check, clean entries only, rewrite marker), 40 (vaultMarker)
TRACE:    run1 writes Old.md + `.engram-vault`; run2 (no --force, different graph) → marker found → entries removed → New.md only. Foreign dir with precious.txt, no --force → non-zero exit naming `--force`, file byte-identical after; with --force → cleaned and regenerated with marker.
VERDICT:  PASS — TestDW_3_4_RerunClobbersOwnedDir, TestDW_3_4_ForeignDirRefusedWithoutForce, TestDW_3_4_ForceCleansForeignDir all executed at full-CLI level and green.

### DW-3.5
PREMISE:  "e2e asserts every `[[file]]` link target resolves to a real note file on disk and each note's frontmatter parses as valid YAML."
EVIDENCE: e2e/scenarios_export.go:150-155 (every wikilink target must be a key of the on-disk note map), 136-146 + 205-219 (exportParseFrontmatter yaml.Unmarshal per note, required keys)
TRACE:    ingest A—works_at→B, B—located_in→C → export → for each note: frontmatter block extracted and yaml.Unmarshal'd (error fails the scenario), every `[[target|…]]` looked up against files on disk (miss fails the scenario); note count must equal the printed entity count; re-run without --force must succeed.
VERDICT:  PASS — assertions read-verified to match the DW verbatim (e2e not executed per dispatch); executed unit proxy TestDW_3_5_FrontmatterParsesWithAdversarialContent round-trips hostile aliases (`x: y`, `"quoted"`, `line\nbreak`, `]] [[Evil|E`, `---`) through the real YAML encoder — a literal `---` alias would break the block-terminator parse if unescaped, and it doesn't.

### DW-3.6
PREMISE:  "No written file path escapes `<dir>` — an entity name containing `../` or path separators is confined inside the vault (traversal test)."
EVIDENCE: export.go:294-312 (sanitizeFilename: separators→`-`, control/NUL dropped, dot/space trim), 401-415 (confinedNotePath: flat-name + Rel re-verification before EVERY write), 447-453 (the only write path goes through both), 461-481 (temp file created inside dir)
TRACE:    `../../etc/pwn` → sanitize → `-..-etc-pwn` (flat, no separators; `..` only as an interior substring) → Join stays in vault → written as `-..-etc-pwn.md` INSIDE vault. `/etc/passwd` → `-etc-passwd`. `..`/`.`/`\x00\x00` → "" → `entity (<id8>)` fallback. `..\..\win\pwn` → `-..-win-pwn`. Canary one level above the vault: byte-identical after; WalkDir over the parent finds no file outside the vault.
VERDICT:  PASS — TestDW_3_6_TraversalNamesConfined (8 hostile names, canary + WalkDir escape sweep, all 8 confined not lost) + TestConfinedNotePath_RejectsEscapes executed and green.

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have corresponding automated tests that ran in Step 0 (DW-ID-named: TestDW_3_1…TestDW_3_6); DW-3.5's e2e clause covered by read-verification of e2e/scenarios_export.go per dispatch, with an executed unit proxy.
- [x] Coverage matches the stated level: 100% unit-level for renderer + confinement (sanitizeFilename, vaultFilenames, confinedNotePath, writeVault, prepareVaultDir/isCatastrophicVaultDir all directly unit-tested); e2e supplementary.

### Edge cases (all prompt-listed — verified)
| Edge case | Evidence | Status |
|---|---|---|
| target dir missing → create | TestExport_EmptyVaultAndMissingDirCreated (nested `does/not/exist`, marker written) — executed | PASS |
| existing foreign files → refuse w/o --force | TestDW_3_4_ForeignDirRefusedWithoutForce — executed | PASS |
| empty vault (0 entities) | Same test ("0 entities") + TestWriteVault_EmptyVaultAndNoAliases — executed | PASS |
| entity name sanitizes to empty | Homonym test: `"..."` → `entity (eeee5555)`; H1 falls back to the filename (export.go:257-260) — executed | PASS |
| two entities colliding after sanitization | Suite executes the mechanism (case-fold homonyms share the sanitized-base counter, export.go:227-243); scratch probe `a/b` vs `a:b` → `a-b (id-slash…)` / `a-b (id-colon…)`, stable across re-runs — executed via overlay | PASS |
| dangling edge → dropped | TestDW_3_2_EdgeLinksResolveAndDanglersDrop (both directions: missing target AND missing source) — executed | PASS |
| very large vault (paged, not buffered whole) | fetchExport drains page-by-page to cursor exhaustion with a non-advance abort (export.go:114-133); TestExport_PagingAssemblesAcrossPages (3 pages, cross-page link resolves) + TestExport_NonAdvancingCursorAborts — executed. See note on in-memory accumulation. | PASS |

## Dead Code
None found. No unused imports (build+vet clean), no unreachable code after returns, no debug statements, no commented-out blocks in the reviewed files.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | Single-threaded sequential CLI command; no goroutines or shared mutable state in the reviewed code (the test-stub server goroutine is test code) |
| Error Handling | PASS | Probed the worst ordering: fetch failure BEFORE prepareVaultDir → existing vault untouched (executed: TestExport_NonAdvancingCursorAborts asserts the dir is never created). All I/O errors wrapped and surfaced as exit 1; hostile server cursor bug-stopped; `os.UserHomeDir` error deliberately absorbed and guarded by `home != ""` (export.go:176, 207) |
| Resources | PASS | `defer client.Close()` (export.go:94); writeFileAtomic removes the temp file on every failure path (write/close/rename, export.go:467-479) and Close precedes Rename |
| Boundaries | PASS | Executed: nil aliases → `aliases: []`, empty export → 0 notes + marker, empty-ID entity skipped (unlinkable), 100-rune name truncated to 60 with re-trim, idPrefix beyond id length returns whole id; residual-clash loop terminates (suffix strictly grows per iteration, sorted-id deterministic) |
| Security | PASS | Adversarial traces of `../../etc/passwd`, absolute path, `.`/`..`/empty-sanitizing, embedded NUL, backslashes — all confined (executed canary/WalkDir test). Scratch probe: a hostile entity ID (`../../pwn`) reaching the collision suffix un-sanitized is bug-stopped by confinedNotePath ("refusing to write outside the vault") — export aborts, nothing escapes. Clobber: marker-based ownership gates the no-force clobber; `--force` still never cleans `/` or `$HOME` (isCatastrophicVaultDir, unit-tested) and only removes entries INSIDE dir, never dir itself; clean-late ordering executed |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | External input validated at entry (barricade) | PASS | Untrusted names/aliases/predicates sanitized once at the rendering barricade (sanitizeFilename/cleanInline, export.go:294-332); server cursor validated for non-advance (export.go:128-130); empty-ID records rejected (export.go:224-226) |
| cc-defensive-programming | Defense-in-depth on the security-critical path | PASS | Confinement re-verified immediately before EVERY write (confinedNotePath, export.go:401-415) even though sanitization should make it unreachable — and the scratch hostile-ID probe shows this second layer actually catches a real bypass class |
| cc-defensive-programming | No empty catch / silently swallowed errors | PASS | Every error path in the reviewed files wraps and returns; the one discarded error (`home, _ :=`, export.go:176) is deliberately compensated by the `home != ""` guard |
| cc-defensive-programming | Anticipated runtime errors handled, bug conditions bug-stopped (not asserted) | PASS | Runtime conditions (foreign dir, fetch failure, FS errors) get error handling; the "should be unreachable" confinement breach is a bug-stop error that refuses the write rather than a disabled-in-production assertion |
| cc-routine-and-class-design | Parameter count ≤ 7 | PASS | Max is 4 (runExport); most routines take 2–3 |
| cc-routine-and-class-design | Functional cohesion | PASS | runExport orchestrates (parse→check→dial→fetch→prepare→write→report); each helper does one operation; writeVault is sequential cohesion at its declared level (acceptable) |
| cc-routine-and-class-design | Inheritance/LSP | N/A | No type hierarchies introduced; plain structs + free functions |

## Notes (non-blocking)
- **Symlink can bypass the catastrophic guard (extra safety net only):** `checkVaultDir`/`prepareVaultDir` use `filepath.Abs`, not `EvalSymlinks`, so `export --force <symlink-to-$HOME>` would pass `isCatastrophicVaultDir` and clean $HOME's entries. Requires the user to explicitly pass `--force` at that path (DW-3.4 defines --force as consent to clobber foreign dirs), and the catastrophic guard is not a listed requirement — but `filepath.EvalSymlinks` before the guard would close it.
- **Whole export is accumulated in client memory** (fetchExport appends every page before writeVault). Paging to exhaustion is correct and executed; global homonym resolution and dangler detection genuinely need the full entity set before the first write, so this is inherent to the format — but a truly huge graph is bounded by client RAM.
- A render/write failure AFTER prepareVaultDir has cleaned an existing owned vault leaves it destroyed (only fetch failures are clean-late-protected). The only post-clean failure modes are yaml.Marshal (practically unreachable) and the confinement bug-stop; acceptable, worth knowing.
- `ExportEdge.Statement` is carried by the adapter but never rendered — intentional adapter completeness, not dead code, but the renderer ignores it.
- The local flagset variable `fs` in runExport (export.go:65) shadows the imported `io/fs` package within that function — compiles and vets clean, mild readability hazard.
- Pathological worst case: a 60×4-byte-rune name plus a fully-extended residual-clash suffix could exceed 255 filename bytes → the write errors (availability only, no escape). The plain suffix case fits at 254 bytes as the comment (export.go:42-44) claims.

## Issues (if FAIL)
None.

**Verdict: PASS**

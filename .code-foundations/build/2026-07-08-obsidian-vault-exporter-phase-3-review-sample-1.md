# Review: Phase 3 - CLI export + Obsidian vault rendering (security sample 1)

## Executed Results (Step 0)
- Test suite: `go test ./internal/cli/... ./internal/engramclient/...` → 27 passed, 0 failed (25 in cli incl. 19 export tests; engramclient ok)
- Build/typecheck: `go build ./...` → success; `go vet -tags e2e ./e2e/...` → clean (e2e scenario compiles)
- Lint: `make lint` (go vet + revive v1.12.0 -set_exit_status) → exit 0

## Requirement Fulfillment

### DW-3.1
PREMISE:  `engram export ./vault` against a populated tenant writes one `.md` per exported entity with an H1, frontmatter (aliases, mention_count, provenance), and edge bullets.
EVIDENCE: internal/cli/export.go:337-396 (renderNote: frontmatter MapSlice with engram_id/aliases/mention_count/scope/owner_agent_id/source_ids/valid_at/created_at, H1 at :372-374, edge bullets at :391-394), :420-457 (writeVault: one confined `.md` per entity).
TRACE:    entities [Alice(2 aliases), Bob] + edge works_at → Alice.md with `---` YAML block (aliases [Al Ali], mention_count 3, provenance keys), `# Alice`, `- works_at [[Bob|Bob]]`; Bob.md written; stats 2/1/0.
VERDICT:  PASS — TestDW_3_1_WriteVaultRendersNotes ran and passed; full CLI path (proto → adapter → note) pinned by TestExport_ProtoFieldsReachFrontmatter.

### DW-3.2
PREMISE:  Edges render as `[[file|Display]]` piped links resolving to real note files; no dangling links; dropped-edge count printed.
EVIDENCE: internal/cli/export.go:424-434 (edges with an unexported endpoint counted in stats.Dropped and skipped before rendering), :393 (`[[%s|%s]]` from refs, which only contains exported entities), :106-107 (printed `(%d edges, %d dropped)`).
TRACE:    4 edges, 2 with a ghost endpoint → 2 rendered piped links (both targets are note files on disk, verified by regex+stat), 2 dropped; CLI prints "2 entities … 1 edges, 1 dropped" in the stub-server test.
VERDICT:  PASS — TestDW_3_2_EdgeLinksResolveAndDanglersDrop + TestDW_3_2_DroppedCountPrinted ran and passed.

### DW-3.3
PREMISE:  Homonym display-name collisions get deterministic id-suffixed filenames, stable across re-runs; illegal chars sanitized.
EVIDENCE: internal/cli/export.go:215-264 (vaultFilenames: case-folded homonym count in pass 1; ALL homonyms suffixed with 8-char id prefix; assignment in sorted-id order → input-order independent; residual-clash loop extends the prefix deterministically), :294-312 (sanitizeFilename: `/\:*?"<>|#^[]` → '-', control chars dropped, dot/space trim, 60-rune cap).
TRACE:    {Alice/aaaa…, Alice/bbbb…, alice/cccc…, Bob/dddd…, "..."/eeee…, "a\x00/b"/ffff…} → "Alice (aaaa1111)", "Alice (bbbb2222)", "alice (cccc3333)", "Bob", "entity (eeee5555)", "a-b"; reversed input yields byte-identical assignment; no case-insensitive collisions.
VERDICT:  PASS — TestDW_3_3_SanitizeFilename (15 cases incl. traversal, absolute, Windows-illegal, dots-only, NUL, unicode, length cap) + TestDW_3_3_HomonymFilenamesDeterministic ran and passed.

### DW-3.4
PREMISE:  Re-running clobbers-and-regenerates; refuses a foreign non-empty dir unless `--force`.
EVIDENCE: internal/cli/export.go:40 (`.engram-vault` ownership marker), :137-162 (checkVaultDir: missing/empty ok, marker-owned ok, foreign non-empty refused with a message naming --force), :168-197 (prepareVaultDir: re-check, catastrophic guard, remove entries INSIDE dir only, rewrite marker).
TRACE:    (a) dir with precious.txt, no --force → exit≠0, stderr names --force, file untouched; (b) same with --force after `<dir>` (two-pass flag parse) → cleaned, note + marker written; (c) export→re-export with a different graph, no --force → Old.md gone, New.md present.
VERDICT:  PASS — TestDW_3_4_ForeignDirRefusedWithoutForce, TestDW_3_4_ForceCleansForeignDir, TestDW_3_4_RerunClobbersOwnedDir all ran and passed.

### DW-3.5
PREMISE:  e2e asserts every `[[file]]` link target resolves to a real note file on disk and each note's frontmatter parses as valid YAML.
EVIDENCE: e2e/scenarios_export.go:136-156 — for every note: exportParseFrontmatter (:205-219, yaml.Unmarshal, error on missing/unterminated/invalid block), required keys check, H1 check, and per-wikilink `notes[l[1]]` existence check (link target must be a `.md` on disk). Registered scenario at :24-26.
TRACE:    ingest A—works_at→B, B—located_in→C → export → each note's leading `---` block YAML-parses with engram_id/aliases/mention_count/scope; each `[[target|label]]` target found in the on-disk note map; note count equals the printed entity count.
VERDICT:  PASS — coverage recorded as observed behavior per dispatch: the e2e suite must not be run (shared cluster); assertions read and confirmed to match the DW, scenario compiles (`go vet -tags e2e` clean), and the identical checks run in-process in TestDW_3_5_FrontmatterParsesWithAdversarialContent (adversarial aliases incl. `---`, `]] [[Evil|E`, newline — YAML round-trip preserved) which passed.

### DW-3.6
PREMISE:  No written file path escapes `<dir>` — an entity name containing `../` or path separators is confined inside the vault (traversal test).
EVIDENCE: internal/cli/export.go:294-312 (sanitize barricade: separators → '-', NUL/control dropped, dot-trim kills `.`/`..`), :401-415 (confinedNotePath second barricade immediately before every write: rejects empty/absolute/any `/` or `\`, then Join+Rel re-verifies single flat element), :447-451 (every write goes through confinedNotePath).
TRACE:    names `../../etc/pwn`, `..\..\win\pwn`, `/etc/passwd`, `..`, `.`, `a/b\c`, `\x00\x00`, `canary.txt` → 8 notes ALL inside the vault (WalkDir over the parent found no escapee); sibling canary byte-identical; no `<root>/etc` created. Barricade unit-tested: confinedNotePath rejects `../x.md`, `..`, `.`, `a/b.md`, `a\b.md`, `/abs.md`, `""` and accepts `fine.md`.
VERDICT:  PASS — TestDW_3_6_TraversalNamesConfined + TestConfinedNotePath_RejectsEscapes ran and passed.

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have DW-named tests that ran in Step 0 (TestDW_3_1 … TestDW_3_6); DW-3.5's live-cluster half is recorded observed behavior (read + vet) per the dispatch's explicit do-not-run instruction, with an in-process unit proxy.
- [x] Coverage matches the stated 100% unit level for renderer + confinement: sanitizeFilename, vaultFilenames, confinedNotePath, renderNote/writeVault, checkVaultDir/prepareVaultDir/isCatastrophicVaultDir, fetchExport paging all directly exercised; e2e supplementary.

Edge cases (all prompt-listed, all executed):
| Edge case | Test | Result |
|---|---|---|
| target dir missing → create | TestExport_EmptyVaultAndMissingDirCreated (nested non-existent path, marker written) | PASS |
| existing foreign files → refuse without --force | TestDW_3_4_ForeignDirRefusedWithoutForce | PASS |
| empty vault (0 entities) | TestWriteVault_EmptyVaultAndNoAliases + TestExport_EmptyVaultAndMissingDirCreated ("0 entities" printed) | PASS |
| entity name sanitizes to empty | TestDW_3_3 ("..."→"entity (eeee5555)"), TestDW_3_6 ("\x00\x00") | PASS |
| two entities colliding after sanitization | TestDW_3_3_HomonymFilenamesDeterministic (exact + case-fold homonyms, all suffixed) | PASS |
| edge endpoint not exported → dropped | TestDW_3_2_EdgeLinksResolveAndDanglersDrop (both directions) | PASS |
| very large vault (paged) | TestExport_PagingAssemblesAcrossPages (3 pages, cross-page link resolves) + fetchExport (export.go:114-133) pages to cursor exhaustion with a non-advancing-cursor abort (TestExport_NonAdvancingCursorAborts) | PASS (see Note 3 on in-memory accumulation) |

Dependency contract: fetchExport uses ExportPage correctly — loops until `NextCursor == ""`, advances `cursor = page.NextCursor`, aborts on a non-advancing cursor. No misuse found.

## Dead Code
None found. No unused imports (build+vet+revive clean), no unreachable code, no debug statements, no commented-out blocks.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | Single-threaded CLI command; no shared mutable state; the only goroutine is the test stub's grpc Serve |
| Error Handling | PASS | Probed every I/O and RPC site: all errors wrapped and returned (export.go:121, 143, 149, 174, 181-195, 464-479); writeFileAtomic cleans its temp on every failure path; failed fetch aborts before any dir mutation (clean-late, executed by TestExport_NonAdvancingCursorAborts — vault dir not even created) |
| Resources | PASS | `defer client.Close()` (export.go:94); temp file Close/Remove on all writeFileAtomic branches (:461-481); leftover `.engram-tmp-*` from a crash is swept by the next prepareVaultDir clean |
| Boundaries | PASS | Probed: empty export (test), nil aliases → `[]` (test), name → empty after sanitize (test), 60-rune cap with re-trim (:307-310, test at 100 x's), idPrefix n≥len(id) guard (:279-281), empty-id entity skipped in refs AND in writeVault (:224, :439-441, TestVaultFilenames_SkipsEmptyID) |
| Security | PASS | Adversarial traces in DW-3.6 above; additionally: H1/link-label/predicate injection blocked by cleanInline (:317-332 — `[`,`]`,`|` → '-', newlines → space, so an entity name or predicate cannot forge a wikilink or break the frontmatter fence); frontmatter injection blocked by real YAML marshalling of raw aliases (executed: TestDW_3_5 round-trips `---`, `]] [[Evil|E`, `"quoted"`, newline exactly); marker collision impossible (leading-dot trim means no sanitized name starts with '.', and notes always carry `.md`); --force cannot nuke `/` or `$HOME` (isCatastrophicVaultDir, executed table test) and prepareVaultDir removes entries inside dir, never dir itself |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | External input validated at entry (barricade) | PASS | Untrusted names/aliases/predicates cross one rendering barricade (sanitizeFilename/cleanInline); server pages treated as external (cursor-loop abort :128-130) |
| cc-defensive-programming | Defense-in-depth on security-critical path (barricade does not replace re-validation) | PASS | confinedNotePath re-verifies confinement immediately before every write even though sanitize should make it unreachable (:398-415, :447) — exactly the skill's security-path override |
| cc-defensive-programming | No empty catch blocks / no swallowed errors | PASS | Every error checked or deliberately best-effort (os.Remove after a failed write; `home, _` degrades the guard and is condition-checked at :207) |
| cc-defensive-programming | Assertions for bugs only / anticipated errors handled | PASS | Go error returns throughout; the "should be unreachable" confinement check is an error-stop, not a panic (:400) |
| cc-defensive-programming | Correctness-over-robustness for a data-destroying tool | PASS | Refuse-by-default clobber, clean-late ordering, atomic per-note writes — wrong-side failures abort rather than destroy |
| cc-routine-and-class-design | Parameter count ≤7 | PASS | Max is 4 (runExport); all others ≤3 |
| cc-routine-and-class-design | Functional cohesion | PASS | runExport orchestrates (parse→check→dial→fetch→prepare→write); each helper does one operation; writeVault is sequential-cohesion-acceptable (filter feeds render feeds write) |
| cc-routine-and-class-design | LSP / inheritance depth | N/A | No inheritance; plain structs + free functions |

## Notes (non-blocking)
1. **Symlink bypass of the catastrophic guard**: prepareVaultDir uses `filepath.Abs` (no `EvalSymlinks`), so `engram export --force <symlink-to-$HOME>` would evade the home/root guard (export.go:172-179). Reachable only when the user explicitly passes `--force` on a foreign dir — which DW-3.4 already licenses to clobber — so the guard being bypassable is a gap in extra hardening, not in a stated requirement. Suggest resolving symlinks before the check.
2. **Cursor-cycle**: the non-advancing-cursor abort (:128-130) catches only an immediate repeat; a buggy server cycling A→B→A would loop (with unbounded accumulation). The Phase-2 contract is treated as given per the dispatch.
3. **Memory profile**: the whole graph is accumulated before writing (fetchExport). This is required by the design — homonym suffixing and dangling-edge dropping need the global entity set (cross-page links prove it) — but a truly huge tenant costs O(graph) RAM.
4. **Pathological filename length**: the residual-clash fallback `"%s (%s-%d)"` (:253) embeds the full entity id; an extremely long id could exceed the 255-byte filename limit and fail the write. Fails closed (error, no escape).
5. **Unicode-normalization homonyms** (PLAUSIBLE, not demonstrated): collision detection case-folds but does not NFC/NFD-normalize; on APFS two names differing only in normalization form could land on one file. Unlisted edge case.

## Issues (if FAIL)
None.

**Verdict: PASS**

# Review: Phase 3 - CLI export + Obsidian vault rendering (sample 3)

## Executed Results (Step 0)
- Test suite: `go test ./internal/cli/... ./internal/engramclient/...` → 27 passed, 0 failed (25 in internal/cli incl. all DW-named tests, verified via `go test -v`: all `--- PASS`)
- Build: `go build ./...` → success
- Typecheck/vet: `go vet ./...` → no issues, exit 0
- Lint: `make lint` (go vet + revive v1.12.0 -set_exit_status) → exit 0
- e2e suite NOT run per dispatch instruction (shared cluster); `e2e/scenarios_export.go` verified by reading.

## Requirement Fulfillment

### DW-3.1
PREMISE:  "`engram export ./vault` against a populated tenant writes one `.md` per exported entity with an H1, frontmatter (aliases, mention_count, provenance), and edge bullets."
EVIDENCE: internal/cli/export.go:337-396 (renderNote: frontmatter MapSlice with engram_id/aliases/mention_count/scope/owner_agent_id/source_ids/valid_at/created_at, H1, edge bullets), export.go:420-457 (writeVault: one note per entity); tests internal/cli/export_test.go:168-216 (TestDW_3_1_WriteVaultRendersNotes), 608-631 (TestExport_ProtoFieldsReachFrontmatter, full CLI path over stub gRPC).
TRACE:    2 entities + 1 edge → writeVault → `Alice.md` starts `---\n`…yaml…`---\n\n# Alice\n` + `- works_at [[Bob|Bob]]`; frontmatter parses with aliases=[Al Ali], mention_count=3, all 5 provenance keys present — asserted by the passing test.
VERDICT:  PASS

### DW-3.2
PREMISE:  "Edges render as `[[file|Display]]` piped links resolving to real note files; no dangling links; dropped-edge count printed."
EVIDENCE: export.go:391-394 (piped-link render from refs), export.go:424-434 (danglers dropped before rendering; refs presence guaranteed), export.go:106-107 (printed `%d dropped`); tests export_test.go:241-263 (TestDW_3_2_EdgeLinksResolveAndDanglersDrop: 2 kept/2 dropped, every rendered link target exists on disk), 265-279 (TestDW_3_2_DroppedCountPrinted via CLI, "1 dropped" in stdout).
TRACE:    edge e-a→e-ghost (target unexported) → `refs[e-ghost]` absent → Dropped++ and edge never reaches renderNote → A.md has no "knows" bullet; kept edges render `[[B|B]]` whose target file exists (assertLinksResolve).
VERDICT:  PASS

### DW-3.3
PREMISE:  "Homonym display-name collisions get deterministic id-suffixed filenames, stable across re-runs; illegal chars sanitized."
EVIDENCE: export.go:215-264 (vaultFilenames: baseCount on the case-folded SANITIZED base; ALL homonyms suffixed, assigned in sorted-id order; residual-clash loop terminates deterministically), export.go:294-312 (sanitizeFilename); tests export_test.go:283-306 (TestDW_3_3_SanitizeFilename, 15 cases incl. traversal/backslash/NUL/dots/length-cap), 308-354 (TestDW_3_3_HomonymFilenamesDeterministic: exact + case-fold homonyms, reversed-input stability, case-insensitive global uniqueness).
TRACE:    ["Alice"(aaaa…), "Alice"(bbbb…), "alice"(cccc…)] → baseCount["alice"]=3 → all three get `Name (idPrefix8)`; reversing input order yields byte-identical assignments (sorted-id pass 2). `a:b*c?d"e<f>g|h` → `a-b-c-d-e-f-g-h`.
VERDICT:  PASS

### DW-3.4
PREMISE:  "Re-running clobbers-and-regenerates; refuses a foreign non-empty dir unless `--force`."
EVIDENCE: export.go:137-162 (checkVaultDir: missing/empty ok, marker-owned ok, foreign non-empty refused without force), export.go:168-197 (prepareVaultDir: re-check, catastrophic guard, removes entries INSIDE dir only, writes marker), export.go:95-101 (fetch precedes clean — clean-late); tests export_test.go:368-385 (foreign refused, foreign file untouched, error names --force), 387-407 (--force cleans + marker written; flag after <dir>), 409-431 (owned dir regenerates without --force, stale note gone).
TRACE:    dir containing precious.txt, no marker, no --force → checkVaultDir → error "…re-run with --force to clobber it" → exit 1, precious.txt intact. Same dir after a prior export (marker present) → nil → entries removed, vault regenerated.
VERDICT:  PASS

### DW-3.5
PREMISE:  "e2e asserts every `[[file]]` link target resolves to a real note file on disk and each note's frontmatter parses as valid YAML."
EVIDENCE: e2e/scenarios_export.go:136-156 — for every note: exportParseFrontmatter (yaml.Unmarshal, error on invalid; lines 205-219) with required keys, and every wikilink match checked against the on-disk note map (151-155). Registered scenario (24-26). Not executed (dispatch forbids e2e run); unit-level proxy executed: export_test.go:453-476 (TestDW_3_5_FrontmatterParsesWithAdversarialContent — aliases "x: y", "]] [[Evil|E", "---", embedded newline all round-trip through real YAML) — PASS.
TRACE:    e2e: ingest 2 chained facts → export → each note's leading `---` block yaml.Unmarshal'd (error fails the scenario) → each `[[target|…]]` must be a key of the on-disk note map. Assertions match the DW verbatim.
VERDICT:  PASS (code-read for e2e per dispatch; executed via the unit proxy)

### DW-3.6
PREMISE:  "No written file path escapes `<dir>` — an entity name containing `../` or path separators is confined inside the vault (traversal test)."
EVIDENCE: export.go:294-312 (sanitizeFilename: `/ \ :` → '-', control chars incl. NUL dropped, leading/trailing dots trimmed), export.go:401-415 (confinedNotePath: rejects empty/absolute/any separator, then Rel re-check — defense in depth immediately before every write), export.go:447-451 (only write path goes through it); tests export_test.go:480-532 (TestDW_3_6_TraversalNamesConfined: `../../etc/pwn`, `..\..\win\pwn`, `/etc/passwd`, `..`, `.`, NUL, canary one level up — all 8 confined, canary untouched, WalkDir proves nothing outside vault), 534-544 (TestConfinedNotePath_RejectsEscapes).
TRACE:    name `../../etc/pwn` → sanitize: '/'→'-', leading dots trimmed → `-..-etc-pwn` (flat) → confinedNotePath(dir, "-..-etc-pwn.md") → no separator, Rel == flat name → write lands inside vault. Adversarial bypass check: were a raw separator ever to reach confinedNotePath (e.g. a hostile '/' in a server-supplied id embedded by the suffix path), it fails CLOSED with an error — never a write.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have DW-named tests executed in Step 0 (TestDW_3_1/3_2/3_3/3_4/3_5/3_6, all PASS)
- [x] Coverage matches the stated 100% unit level for renderer + confinement; e2e supplementary (read-verified per dispatch)
- Edge cases (all handled with execution evidence):
  - target dir missing → created: TestExport_EmptyVaultAndMissingDirCreated (nested missing path, marker written)
  - foreign files refused without --force: TestDW_3_4_ForeignDirRefusedWithoutForce
  - empty vault (0 entities): TestWriteVault_EmptyVaultAndNoAliases + "0 entities" via CLI
  - name sanitizes to empty: "..." → "entity (eeee5555)" in TestDW_3_3_HomonymFilenamesDeterministic; "\x00\x00" confined in TestDW_3_6
  - two entities colliding after sanitization: collision counting happens on the sanitized, case-folded base (export.go:227-233), the exact path the executed homonym test exercises; trace: "a/b" and "a:b" both sanitize to "a-b" → baseCount=2 → both id-suffixed. The test's case-insensitive global-uniqueness sweep (345-353) pins the invariant.
  - dangling edge dropped + counted: TestDW_3_2 (both directions: missing target and missing source)
  - very large vault: transport is paged and drained to exhaustion with a non-advancing-cursor abort (export.go:114-133; TestExport_PagingAssemblesAcrossPages incl. cross-page link resolution; TestExport_NonAdvancingCursorAborts). See Notes on in-memory accumulation.

## Dead Code
None found in the phase files (build, vet, revive all clean; no debug statements, no commented-out blocks, no unreachable code).

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | Single-threaded CLI command path; no goroutines or shared mutable state in phase code (test server goroutine is test code) |
| Error Handling | PASS | Fetch failure aborts BEFORE any dir mutation — traced export.go:95-101 and executed (TestExport_NonAdvancingCursorAborts asserts the vault dir was never created); every I/O error wrapped and returned; renderNote YAML error propagates; probed flag misuse (TestExport_ArgValidation) |
| Resources | PASS | client.Close deferred (export.go:94); writeFileAtomic closes and removes the temp file on every error path (461-481); temp+rename means no half-written note |
| Boundaries | PASS | Probed: empty entity list, empty id (skipped, TestVaultFilenames_SkipsEmptyID), name → empty after sanitize, 100-rune name capped at 60 with re-trim, nil aliases rendered as `[]`, 0-entity export |
| Security | PASS | Traversal/absolute/NUL/backslash/dot names all confined (executed, TestDW_3_6); marker-based ownership gates the no-force clobber (checkVaultDir:155-157); --force never cleans `/` or `$HOME` (isCatastrophicVaultDir, executed TestPrepareVaultDir_CatastrophicGuard); clean-late ordering proven by code order + executed cursor-abort test; wikilink forgery via names/predicates neutralized by cleanInline (`[ ] \|` → '-'); adversarial aliases round-trip through a real YAML encoder without breaking the frontmatter delimiter (executed TestDW_3_5) |

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | External input validated at entry (barricade) | PASS | Untrusted names/aliases/predicates sanitized at the render barricade (sanitizeFilename/cleanInline); server cursor validated for advance (export.go:128-130); aliases never hand-escaped — real YAML encoder |
| cc-defensive-programming | Defense-in-depth on security-critical path | PASS | Confinement re-verified immediately before every write (confinedNotePath), independent of the sanitizer; fails closed |
| cc-defensive-programming | No empty catch / silently swallowed errors | PASS | Every error in phase code checked or intentionally designed-around (`home, _ := os.UserHomeDir()` — empty home handled explicitly at export.go:207) |
| cc-defensive-programming | Correctness vs robustness strategy | PASS | Fails closed (abort, no write) on any confinement or cursor anomaly — correct for a tool that deletes directories |
| cc-routine-and-class-design | Parameter count ≤7 | PASS | Max is 4 (runExport); most routines 2-3 |
| cc-routine-and-class-design | Functional cohesion | PASS | Each routine one operation at its level; runExport is an orchestrator (parse→check→dial→fetch→prepare→write→report) |
| cc-routine-and-class-design | Inheritance/LSP | N/A | No inheritance introduced; plain structs + free functions |

## Notes (non-blocking)
- **Whole-export buffering**: fetchExport accumulates all entities+edges in memory before writing (export.go:114-133). Paging to exhaustion is correct and tested; global homonym suffixing and dangling-edge detection genuinely require the full entity set, so streaming writes are not straightforwardly possible under DW-3.3/3.2. If the plan's "not buffered whole" meant CLI memory (rather than transport paging), revisit — as written the paged edge case is satisfied and executed.
- **Symlinked `--force` target**: `filepath.Abs` does not resolve symlinks, so `engram export --force ./link-to-slash` would pass isCatastrophicVaultDir on the link's path while ReadDir/RemoveAll operate on the target. The dir argument is operator-supplied (not untrusted ingested content) and --force is explicit consent, so this is outside DW-3.6's threat model — but `filepath.EvalSymlinks` before the catastrophic check would harden it cheaply.
- **Pathological filename length**: the residual-clash fallback `fmt.Sprintf("%s (%s-%d)", base, id, n)` (export.go:253) embeds the full server id and could exceed 255 bytes with an abnormally long id; ids are Phase-2 server-generated UUIDs per the given contract, and a hostile id containing a separator fails closed at confinedNotePath rather than escaping.
- Pre-existing (not phase code): `b, _ := json.MarshalIndent` in cli.go runStatus/runAudit ignores a marshal error; untouched by this phase.
- Aliases containing `[[...]]` remain verbatim inside YAML frontmatter (correct YAML round-trip); Obsidian treats frontmatter as metadata, but the e2e whole-content link scan would flag such an alias as a "link" if extraction ever produced one — cosmetic test-robustness point only.

## Issues (if FAIL)
None.

**Verdict: PASS**

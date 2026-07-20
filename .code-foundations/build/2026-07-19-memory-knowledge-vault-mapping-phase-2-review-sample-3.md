# Review: Phase 2 - Memory-Knowledge Vault Mapping (sample 3)

## Executed Results (Step 0)
- Test suite: `go test -count=1 ./internal/cli/ ./internal/engramclient/` → ok (all pass, 0 failures); verbose run shows all 6 `TestDW_2_*` tests + `TestKnowledgeCollectionTruncationWarning` + 12 vaultknowledge unit tests passing
- Full suite: `go test -count=1 ./...` → all packages ok
- Build/typecheck: `go build ./...` → exit 0
- Lint: `make lint` (go vet + revive) → exit 0
- Coverage: `go tool cover -func` → every function in vaultknowledge.go ≥ 90.9% (most 100%); writeVaultModel 100%, runExport 95.3%

## Requirement Fulfillment

### DW-2.1
PREMISE:  "with a stub server serving one public collection of docs whose `memory_ref`s hit exported entities, export writes one `knowledge/<name>.md` per doc (rendered in deterministic doc-id order), each containing a `[[<concept note>]]` that matches an actual concept filename."
EVIDENCE: internal/cli/vaultknowledge.go:94 (sort by ID), :147-165 (resolve), :203-240 (render); test internal/cli/export_test.go:756-803
TRACE:    stub serves `curated_notes` with kd2 before kd1 (id-descending, shared title "Shared title", both memory_ref=e-a, the one hub) → fetchKnowledgeDocs sorts by ID → resolveKnowledgeDocs gives kd1 the bare name, kd2 the id-suffixed name → `knowledge/Shared title.md` holds kd1's body + `[[Alpha|Alpha]]`; `concepts/Alpha.md` exists. Test asserts all of this and passed.
VERDICT:  PASS — `TestDW_2_1_KnowledgeNotesWikilinkToExportedConcepts` executed, passing.

### DW-2.2
PREMISE:  "each mapped concept note gains a 'Referenced by' section listing the knowledge note(s) that map to it, deterministically ordered by doc id; a concept mapped by two docs lists both."
EVIDENCE: internal/cli/vaultknowledge.go:218-239 (grouping, sorted outer iteration; inner order = fetch's doc-id sort), :295-317 (append); tests export_test.go:808-842, vaultknowledge_test.go:161-192
TRACE:    kd-b fed before kd-a, both → e-a → sorted to kd-a,kd-b → `concepts/Alpha.md` gains `## Referenced by` with `[[Note A|Note A]]` at a lower index than `[[Note B|Note B]]`. Tests assert both links and their order and passed.
VERDICT:  PASS — `TestDW_2_2_ConceptNoteGetsReferencedByBacklinks` + `TestRenderKnowledgeVault_WritesNotesAndOrderedBacklinks` executed, passing.

### DW-2.3
PREMISE:  "a doc whose `memory_ref` matches no exported entity renders with an inert unresolved marker (labeled with `memory_ref_name` when present, else the raw id) and produces no dangling wikilink and no backlink."
EVIDENCE: internal/cli/vaultknowledge.go:158-161 (hub-only resolution), :186-194 (ghosts excluded), :273-286 (marker with cleanInline'd label); tests export_test.go:849-894, vaultknowledge_test.go:98-112, :125-143
TRACE:    three docs — missing id + name, missing id bare, real ghost e-b → all get ConceptID "" → `**Memory:** unresolved: <label>` (name when present, raw id otherwise); test asserts each marker, that no `[[` appears in any of the three notes, and that no concept note gained "Referenced by". Passed.
VERDICT:  PASS — `TestDW_2_3_UnresolvedMemoryRefRendersInertMarker` executed, passing.

### DW-2.4
PREMISE:  "injection trap — a doc with `title`/`text`/`memory_ref` carrying control chars, `[[`/`]]`, `../`, and an over-long NFC/NFD name is sanitized, byte-budgeted, NFC-folded, and written strictly inside `<dir>` (path-confinement + wikilink-sanitizer assertions)."
EVIDENCE: internal/cli/vaultknowledge.go:172-178 → export.go:289-307 (sanitizeFilename), export.go:356-378 (safeNoteName: NFC fold at :357, byte budget at :370), vault.go:59-84 (confinedVaultPath: root allowlist, depth, per-element `..` refusal, Rel re-check); test export_test.go:903-987
TRACE:    hostile title `../../etc/pwn\x00[[Injected]] ` + 300 chars, NFD `café` title (byte-verified in test source: `65 cc 81`), body with forged callout/fence/frontmatter/wikilink/obsidian://, hostile memory_ref_name → export exits 0; WalkDir canary check shows every file strictly inside `dir`, canary untouched, no `root/etc` created; basenames ≤ maxNoteBaseBytes; no `\x00`, no live `> [!danger]`, no `[[wikilink inject]]` in any knowledge note. NFC folding runs on every knowledge basename via safeNoteName (export.go:357), pinned by the executed `TestSafeNoteName_NFCFoldPreventsSilentDrop`. Passed.
VERDICT:  PASS — `TestDW_2_4_InjectionTrapSanitizedAndConfined` + sanitizer corpus tests executed, passing.

### DW-2.5
PREMISE:  "knowledge fetch failure leaves the fully-assembled memory vault byte-intact and exits with a soft warning, not a hard error."
EVIDENCE: internal/cli/export.go:120-133 (fetch after writeVaultModel; kerr → warning only, render skipped); vaultknowledge.go:73-96 (fetch is RPC-only, zero disk I/O); test export_test.go:993-1022
TRACE:    stub returns `codes.Unavailable` from KnowledgeCollections → fetchKnowledgeDocs errors → runExport prints `warning: knowledge fetch failed...`, never calls renderKnowledgeVault → exit 0, memory notes + marker present, no `knowledge/` file, no knowledge count in summary. Byte-intactness holds by construction: the fetch path performs no filesystem operation, and on kerr the only side effect is the warning line. Passed.
VERDICT:  PASS — `TestDW_2_5_KnowledgeFetchFailureIsSoftWarning` executed, passing.

### DW-2.6
PREMISE:  "zero knowledge collections → no `knowledge/` folder and the memory-only vault is identical to pre-change output."
EVIDENCE: internal/cli/vaultknowledge.go:203-206 (0 docs → no-op), vault.go:99-102 (writeVault = thin wrapper over identical assembly); test export_test.go:1027-1056, vaultknowledge_test.go:145-159
TRACE:    zero-collections run vs knowledge-RPC-unimplemented baseline → `assertTreesEqual` byte-compares every file → identical; no `knowledge/` path; no knowledge count printed. Pre-change equivalence of the memory tree: writeVault delegates to writeVaultModel with unchanged rendering logic (diff-verified), and every pre-existing DW-5.x memory-vault test still passes unmodified.
VERDICT:  PASS — `TestDW_2_6_ZeroCollectionsNoKnowledgeFolder` + `TestRenderKnowledgeVault_EmptyDocsIsNoOp` executed, passing.

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have corresponding tests, ran in Step 0 (test names reference DW-IDs: `TestDW_2_1_...` through `TestDW_2_6_...`)
- [x] Coverage level "100% of new/changed functions" met: every function in vaultknowledge.go, plus writeVaultModel and the changed runExport, shows coverage (all ≥ 90.9%, most 100%); the uncovered slivers are unreachable-in-fixture error returns (e.g. fetch-loop second RPC error branch, backlink confinement refusal), each exercised at the primitive level by existing tests
- [x] Prompt-listed edge cases each have execution evidence (see below)

Edge-case verification:
| Edge case | Evidence | Status |
|---|---|---|
| zero collections / empty collection → no knowledge/, byte-identical | TestDW_2_6 (byte compare) + TestRenderKnowledgeVault_EmptyDocsIsNoOp (0 docs → dir untouched); empty-collection path traced: nil hits → 0 docs → render no-op, no warning, no count | HANDLED |
| memory_ref absent/empty → no wikilink | memoryRefLine returns "" (vaultknowledge.go:278-280); TestMemoryRefLine "absent" case; kd2 in TestDW_2_4 has no memory_ref | HANDLED |
| ghost/filtered entity → inert marker, no dangling link, no backlink | TestDW_2_3 covers missing-id AND real ghost e-b; hubConceptIDs excludes ghosts (:186-194) | HANDLED |
| knowledge fetch error preserves memory vault, soft warning | TestDW_2_5; fetch has no disk I/O | HANDLED |
| hostile title/text/memory_ref sanitized/confined/budgeted/NFC | TestDW_2_4 + sanitizer corpus + TestSafeNoteName_NFCFoldPreventsSilentDrop | HANDLED |
| multiple docs → same concept, all listed in doc-id order | TestDW_2_2 + TestRenderKnowledgeVault_WritesNotesAndOrderedBacklinks | HANDLED |
| ADVERSARIAL: truncation warning at exactly k=100, no silent drop | TestKnowledgeCollectionTruncationWarning: warning names the collection AND all 100 docs are exported and counted | HANDLED |

Adversarial search results (no break found):
- **Path escape:** MemoryRef and collection name never enter a filesystem path (MemoryRef is only a map lookup key + cleanInline'd label; collection only a YAML value + RPC arg). Title/doc-id funnel through sanitizeFilename → fitNoteName → safeNoteName (strips `/\:*?"<>|#^[]`, control chars, NFC, budget) → confinedVaultPath (root allowlist incl. "knowledge" at depth 2, per-element `..`/dot-space refusal, backslash refusal, final filepath.Rel re-check) immediately before every write. Canary test confirms empirically.
- **Live wikilink from attacker text:** body → sanitizeBody (26-case adversarial corpus passing, incl. NUL-split bracket recombination); H1/labels → cleanInline (`[`,`]`,`|` → `-`, export.go:413-428); backlink line uses File (safeNoteName output, cannot contain `[]|`) + Display (cleanInline'd). Frontmatter values (doc id, collection) go through yaml.Marshal, which quotes/escapes newlines — no frontmatter forging.
- **Fetch/render error corrupting the memory vault:** fetch does no disk I/O; render writes are atomic (temp + rename, vault.go:182-…), so even a mid-`appendConceptBacklinks` failure leaves the original concept note intact — the read-modify-write cannot half-write.

## Dead Code
No FAIL findings: no unreachable code, no debug statements, no commented-out blocks, no unused imports (vet + revive clean). See Notes for the writeVault wrapper.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | Sequential CLI command; changed code spawns no goroutines and shares no mutable state |
| Error Handling | PASS | Traced every failure edge: both RPC errors soft-fail with warning (export.go:121-123); write/confinement errors propagate hard; ReadFile error wrapped (:301-304); malformed fields_json degrades per documented policy with a dedicated test (`TestDecodeKnowledgeHit_MalformedJSONDegradesToEmpty`); non-string JSON types degrade without panic (`..._NonStringFieldDegradesToEmpty`) |
| Resources | PASS | No new handles; writeFileAtomic closes and removes its temp file on every failure path (pre-existing, exercised by `TestWriteVault_WriteFailurePropagates`) |
| Boundaries | PASS | Traced: empty docs (no-op), empty/all-control title ("doc" forced fallback), empty memory_ref (no line), exact-k boundary (warning), over-long names (budget), NFD names (NFC fold) — all with executed tests |
| Security | PASS | DW-2.4 + adversarial traces above; server-side readability filter claim in fetchKnowledgeDocs' comment verified against internal/server/knowledge.go:188-210 |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | External input validated at entry (network-crossing data is external) | PASS | All doc fields treated as untrusted; sanitized at the rendering barricades; confinement re-verified before every write (TestDW_2_4 executed) |
| cc-defensive-programming | No empty catch blocks / silent swallowing | PASS | The one discarded error (`_ = json.Unmarshal`, vaultknowledge.go:107) is a documented, tested degrade-to-zero-value strategy for one bad row, not an accidental swallow — see Notes |
| cc-defensive-programming | Barricade design (validate at boundary; error handling outside, hard-stop inside) | PASS | Fetch (network, soft-fail) / render (disk, hard-fail) split; confinedVaultPath is a bug-stop barricade that aborts rather than writes |
| cc-defensive-programming | Assertions for bugs only / no executable code in assertions | N/A | Go; no assertion mechanism used |
| cc-defensive-programming | Consistent error-handling strategy | PASS | Matches the exporter's existing pattern exactly (clean-late, soft fetch warning mirrors the memory fetch's own discipline; wrapped `export:`-prefixed errors) |
| code-clarity-and-docs | Interface comments on every new entity, different-words test | PASS | Every new type/function carries a substantive comment (abstraction + rationale, none restating the code) |
| code-clarity-and-docs | Comment accuracy (no stale/wrong claims) | PASS | Spot-verified the load-bearing claims: server-side readable-collection filtering (internal/server/knowledge.go:210 "not readable by this caller: invisible"), no-paging KnowledgeSearch (client signature has k, no cursor), doc-id sort contract, empty-docs no-op |
| code-clarity-and-docs | Naming precision/consistency | PASS | `hubConceptIDs`, `resolvedKnowledgeDoc`, `memoryRefLine`, `knowledgeStats` — precise, in-isolation guessable, consistent with vault.go vocabulary |

## Notes (non-blocking)
- A malformed or type-mismatched `fields_json` row silently becomes an empty "doc (id)"-named note with no per-row warning. Deliberate, documented (vaultknowledge.go:99-104), and tested — but a stderr warning per degraded row would aid data quality.
- `writeVault` (vault.go:99) now has no non-test production caller; it is kept, per its own comment, so vault_test.go's call sites stay untouched. Exercised by tests, so not dead — but it is a test-only wrapper in production code.
- The DW-2.5 end-to-end test asserts file presence, not byte equality; byte-intactness rests on the (solid) trace that the fetch path performs no disk I/O, complemented by DW-2.6's byte-level tree comparison.
- `sort.Slice` (vaultknowledge.go:94) is not stable; two docs sharing one ID (only a hostile/buggy server could produce this) would get order-dependent bare-name assignment. Go's sort is deterministic for a given input, so no cross-run nondeterminism; unlisted edge, noted only.
- The DW-2.4 test feeds a genuinely NFD title (byte-verified `65 cc 81`) but does not directly assert the NFC bytes of the resulting filename; the fold is pinned instead through the shared `safeNoteName` choke point's own test.

## Issues
None.

**Verdict: PASS**

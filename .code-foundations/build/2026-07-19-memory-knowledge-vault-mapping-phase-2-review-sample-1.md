# Review: Phase 2 - Knowledge → Vault Mapping (sample 1)

## Executed Results (Step 0)
- Test suite: `go clean -testcache && go test ./...` → all 30 packages **ok**, 0 failures (internal/cli 0.176s, internal/engramclient 0.045s)
- Targeted: `go test ./internal/cli/ -run 'DW_2_|Knowledge|SafeNoteName_NFCFold' -v` → **all PASS** (TestDW_2_1 … TestDW_2_6, TestKnowledgeCollectionTruncationWarning, TestSafeNoteName_NFCFoldPreventsSilentDrop, TestRenderKnowledgeVault_*, TestDecodeKnowledgeHit_*, TestMemoryRefLine, TestAppendConceptBacklinks_MissingConceptFileErrors, plus the DW_2_1 sanitizeBody adversarial corpus)
- Build: `go build ./...` → clean
- Typecheck/vet: `go vet ./internal/cli/` → clean
- Lint: `make lint` (go vet + revive v1.12.0) → exit 0
- Coverage (function-level, new/changed): decodeKnowledgeHit 100%, stringField 100%, resolveKnowledgeDocs 100%, knowledgeDocBase 100%, hubConceptIDs 100%, renderKnowledgeNote 100%, memoryRefLine 100%, writeVaultModel 100%, fetchKnowledgeDocs 93.8%, renderKnowledgeVault 90.9%, appendConceptBacklinks 94.4%, runExport 95.3% (uncovered lines are I/O-error branches only)

## Requirement Fulfillment

### DW-2.1
PREMISE:  "with a stub server serving one public collection of docs whose `memory_ref`s hit exported entities, export writes one `knowledge/<name>.md` per doc (rendered in deterministic doc-id order), each containing a `[[<concept note>]]` that matches an actual concept filename."
EVIDENCE: internal/cli/vaultknowledge.go:94 (sort by doc id), :147–165 (resolveKnowledgeDocs), :203–216 (one note per doc under "knowledge/"), :273–277 (resolved wikilink uses refs[ConceptID].File — the actual concept filename); internal/cli/export_test.go:756–803 (TestDW_2_1, stub gRPC server, one public collection).
TRACE:    stub serves kd2 then kd1 (id-descending), both titled "Shared title", memory_ref=e-a (hub "Alpha") → fetchKnowledgeDocs sorts [kd1, kd2] → uniqueNoteName gives kd1 the bare `knowledge/Shared title.md` and kd2 a suffixed name → each note contains `[[Alpha|Alpha]]` and `concepts/Alpha.md` exists on disk; summary prints "2 knowledge docs". Test asserts all of this and passes.
VERDICT:  PASS

### DW-2.2
PREMISE:  "each mapped concept note gains a 'Referenced by' section listing the knowledge note(s) that map to it, deterministically ordered by doc id; a concept mapped by two docs lists both."
EVIDENCE: internal/cli/vaultknowledge.go:218–239 (byConcept grouping preserves doc-id order; outer concept iteration sorted), :295–317 (appendConceptBacklinks appends "## Referenced by" + one wikilink per mapped doc); export_test.go:808–842 (TestDW_2_2); vaultknowledge_test.go:161–192 (TestRenderKnowledgeVault_WritesNotesAndOrderedBacklinks).
TRACE:    stub serves kd-b ("Note B") then kd-a ("Note A"), both memory_ref=e-a → sorted [kd-a, kd-b] → concepts/Alpha.md gains "## Referenced by" with `[[Note A|Note A]]` before `[[Note B|Note B]]`. Test asserts both links present and idxA < idxB; passes.
VERDICT:  PASS

### DW-2.3
PREMISE:  "a doc whose `memory_ref` matches no exported entity renders with an inert unresolved marker (labeled with `memory_ref_name` when present, else the raw id) and produces no dangling wikilink and no backlink."
EVIDENCE: internal/cli/vaultknowledge.go:158–161 (ConceptID set only when MemoryRef is a non-ghost hub), :186–194 (hubConceptIDs excludes ghosts), :278–285 (marker labeled MemoryRefName else raw id, cleanInline'd); export_test.go:849–894 (TestDW_2_3 covers missing id, id-only, and a real exported GHOST e-b); vaultknowledge_test.go:98–112, 125–143.
TRACE:    kd1 (memory_ref="no-such-entity", name "Ghost Concept") → "unresolved: Ghost Concept"; kd2 (id only) → "unresolved: also-missing"; kd3 (e-b, an exported ghost with no file) → "unresolved: e-b". Test asserts each marker present, no "[[" anywhere in those notes, and no "Referenced by" appears in any concept note; passes.
VERDICT:  PASS

### DW-2.4
PREMISE:  "injection trap — a doc with `title`/`text`/`memory_ref` carrying control chars, `[[`/`]]`, `../`, and an over-long NFC/NFD name is sanitized, byte-budgeted, NFC-folded, and written strictly inside `<dir>` (path-confinement + wikilink-sanitizer assertions)."
EVIDENCE: export_test.go:903–987 (TestDW_2_4: title = `../../etc/pwn\x00[[Injected]]` + 300 x's; verified-NFD title `café notes` — bytes `65 cc 81` confirmed via hexdump of line 912; hostile body with wikilink/callout/fence/frontmatter/obsidian:// forgeries; hostile memory_ref_name). Sanitizers: export.go:289–307 (sanitizeFilename), :356–378 (safeNoteName: ToValidUTF8 + norm.NFC + byte budget), :413–428 (cleanInline); sanitize.go:50–120 (sanitizeBody/sanitizeInline); vault.go:59–84 (confinedVaultPath), :169–178 (writeVaultNote confines before every write); vaultknowledge.go:152 routes every knowledge filename through uniqueNoteName → safeNoteName.
TRACE:    hostile title → sanitizeFilename maps `/` to `-`, drops `\x00`, `[`/`]` become `-`, rune-capped → safeNoteName NFC-folds and byte-budgets ≤ maxNoteBaseBytes → relPath `knowledge/<name>.md` → confinedVaultPath (allowed root, depth 2, no `..` elements, filepath.Rel re-check). Test walks the whole temp root: every file strictly inside `vault/`, canary next to the vault untouched, no `root/etc` directory created, no `\x00`, no live `[[wikilink inject]]`, no live callout, basenames within byte budget, exactly 2 knowledge files. NFC-folding of the shared choke point is separately pinned by TestSafeNoteName_NFCFoldPreventsSilentDrop (export_test.go:718–742). All pass.
VERDICT:  PASS

### DW-2.5
PREMISE:  "knowledge fetch failure leaves the fully-assembled memory vault byte-intact and exits with a soft warning, not a hard error."
EVIDENCE: internal/cli/export.go:127–139 (fetch runs to completion before render; kerr → Fprintf warning, render skipped, no error returned); export_test.go:993–1022 (TestDW_2_5: KnowledgeCollections returns codes.Unavailable).
TRACE:    writeVaultModel completes → fetchKnowledgeDocs errors → only a warning is printed; renderKnowledgeVault never runs; nothing on the kerr path writes or deletes under dir. Test asserts exit 0, warning text, concepts/Alpha.md + maps/Alpha.md + marker present, zero knowledge/ files, no knowledge count in summary. Byte-level corroboration: TestDW_2_6's baseline run takes the UNIMPLEMENTED fetch-failure soft path and its tree is assertTreesEqual-compared (byte-wise) against the zero-collections run. Passes.
VERDICT:  PASS

### DW-2.6
PREMISE:  "zero knowledge collections → no `knowledge/` folder and the memory-only vault is identical to pre-change output."
EVIDENCE: internal/cli/vaultknowledge.go:203–207 (empty docs → no-op: no folder, no error); export_test.go:1027–1056 (TestDW_2_6: explicit zero-collections server vs memory-only baseline, assertTreesEqual); vaultknowledge_test.go:145–159 (TestRenderKnowledgeVault_EmptyDocsIsNoOp asserts the dir is untouched).
TRACE:    KnowledgeCollections → [] → fetch loop body never runs → docs nil → renderKnowledgeVault returns zero stats without touching dir → kstats.Docs==0 → pre-change summary format. Test byte-compares the two vault trees and asserts no knowledge/ entries and no knowledge count; passes.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have corresponding automated tests, DW-ID-named, executed in Step 0 (TestDW_2_1 … TestDW_2_6)
- [x] Coverage level "100% of new/changed functions" met: every new function in vaultknowledge.go and the changed runExport/writeVaultModel has direct test coverage (function coverage table in Step 0; the only uncovered lines are disk/RPC I/O error branches, and the load-bearing ones of those — missing concept file, fetch failure — are themselves tested)

## Edge cases (prompt-listed)
| Edge case | Evidence | Status |
|---|---|---|
| zero collections / empty collection → no knowledge/, byte-identical vault | TestDW_2_6 (assertTreesEqual) + TestRenderKnowledgeVault_EmptyDocsIsNoOp (dir untouched); empty-collection trace: 0 hits → docs empty → same no-op path, no truncation warning (0 ≠ MaxK) | PASS |
| memory_ref absent/empty → no wikilink | memoryRefLine returns "" (vaultknowledge.go:278–280), renderKnowledgeNote omits the line entirely (:256–260); TestMemoryRefLine "absent" case; DW-2.4's kd2 has empty ref and renders link-free | PASS |
| ghost/filtered entity → inert marker, no dangling link, no backlink | hubConceptIDs excludes Ghost (:186–194); TestDW_2_3 exercises a REAL exported ghost (e-b) plus two missing ids; TestResolveKnowledgeDocs_GhostAndMissingBothUnresolved | PASS |
| knowledge fetch error preserves memory vault, soft warn | TestDW_2_5; export.go:127–139 (clean-late: fetch fully precedes render) | PASS |
| untrusted title/text/memory_ref sanitized/confined/budgeted/NFC-folded | TestDW_2_4 (canary + walk + budget + control-char + live-wikilink assertions); TestSafeNoteName_NFCFoldPreventsSilentDrop; TestDW_2_1_SanitizeBodyAdversarialCorpus | PASS |
| multiple docs → same concept, all backlinked in doc-id order | TestDW_2_2 (fed out of order, asserts order); TestRenderKnowledgeVault_WritesNotesAndOrderedBacklinks | PASS |
| ADVERSARIAL: truncation at exactly k=100 warned, nothing silently dropped | TestKnowledgeCollectionTruncationWarning: 100 hits → "warning:" naming the collection AND "100 knowledge docs" in the summary (fetchKnowledgeDocs:85–89 warns; docs still rendered) | PASS |

Adversarial probes attempted (all defeated):
- **Path escape via title/memory_ref/collection**: title is the only untrusted value that reaches a path element, and it passes sanitizeFilename (`/`→`-`, control chars dropped, dots trimmed) → safeNoteName → confinedVaultPath's four-check barricade re-verified at write time (vault.go:59–84). memory_ref never touches a path (exact-match hub lookup or cleanInline'd body text). Collection name appears only as a YAML frontmatter value (yaml.Marshal escapes) and %q-quoted in the warning. Backlink paths use ref.Folder ("concepts", a constant) + memory-vault-produced File.
- **Live `[[ ]]` from attacker text**: body → sanitizeBody breaks `[[` adjacency tracking emitted runes (control-char smuggling covered by the adversarial corpus); labels → cleanInline maps `[`/`]`/`|` to `-` (TestMemoryRefLine "hostile name sanitized"); resolved wikilinks are built ONLY from safeNoteName-produced File (illegal runes → `-`) and cleanInline'd Display (vaultmodel.go:406).
- **Fetch/render error corrupting the memory vault**: fetch failure → warning only (tested). A render error propagates as a hard write failure but deletes nothing — writes are atomic temp+rename (vault.go:182–199) and appendConceptBacklinks rewrites via the same confined atomic path after a fail-loud read (tested: TestAppendConceptBacklinks_MissingConceptFileErrors).

## Dead Code
None found. No unused imports (build + revive clean), no unreachable code, no debug statements, no commented-out blocks in the changed files.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | Single-threaded CLI command path; no goroutines, no shared mutable state in the changed code (the stub server's goroutine is test-only) |
| Error Handling | PASS | Probed: RPC failure mid-export → soft warning, vault intact (TestDW_2_5); malformed fields_json → degrades to zero-value doc, no abort (TestDecodeKnowledgeHit_MalformedJSONDegradesToEmpty); non-string JSON field → "" not panic (TestDecodeKnowledgeHit_NonStringFieldDegradesToEmpty); missing concept file at backlink time → loud error (TestAppendConceptBacklinks_MissingConceptFileErrors) |
| Resources | PASS | client closed via defer (export.go:107); writeFileAtomic removes its temp file on every failure branch (vault.go:182–199); os.ReadFile leaves no handle |
| Boundaries | PASS | Probed: empty docs, empty/zero collections, empty title (forced "doc" fallback, tested), all-control-char title (tested), empty memory_ref, empty TextField (defaults to "text", tested), exactly-k hit count (warning, tested), homonym titles (suffix, tested), NFC/NFD collision (suffix not silent drop, tested) |
| Security | PASS | DW-2.4 end-to-end trap + confinement barricade + adversarial traces above; no path, link, or structure forgery survived any probe |

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | External input validated at entry (network data is external even from own server) | PASS | fields_json decoded defensively (stringField type-asserts, vaultknowledge.go:126–129); TextField fallback (:108–110); every rendered value re-sanitized at the write barricade — defense in depth, not barricade-only |
| cc-defensive-programming | No empty catch blocks / silently swallowed errors | PASS | The two ignored errors (`_ = json.Unmarshal` :107, `yaml.Marshal` in writeFrontmatter) are documented degrade-don't-abort decisions with the degradation behavior pinned by tests — not silent swallows |
| cc-defensive-programming | Assertions: executable code / bugs-only rules | N/A | Go; no assert mechanism used |
| cc-defensive-programming | Barricade placement + security paths validated twice | PASS | Sanitizers at render + confinedVaultPath re-check immediately before every write (vault.go:169–178); appendConceptBacklinks re-confines and fail-louds rather than trusting its caller (:295–306) |
| code-clarity-and-docs | Interface comments on every new public/package entity, "different words" test | PASS | Every new type/function in vaultknowledge.go carries a rationale-bearing comment (why two-phase fetch/render, why hub-only resolution, why a separate filename namespace) — none restate the code |
| code-clarity-and-docs | Naming precision/consistency | PASS | hubConceptIDs, memoryRefLine, resolvedKnowledgeDoc, knowledgeDocBase — each guessable in isolation; field comments state invariants (File: "no folder, no .md"; MemoryRef: `""` semantics) |
| code-clarity-and-docs | No stale/contradicting comments | PASS | Spot-checked load-bearing claims: "docs must already be sorted" (enforced at :94), "knowledge" in allowedVaultRoots (vault.go:40), depth-2 claim matches vaultPathDepth; header security-model comment matches actual sanitizer routing |

## Notes (non-blocking)
- TestDW_2_4 does not directly assert the on-disk filename for the NFD title is the NFC form; the NFC-fold guarantee rests on the shared choke point's own test (TestSafeNoteName_NFCFoldPreventsSilentDrop) plus the traced routing (vaultknowledge.go:152 → uniqueNoteName → safeNoteName). Adequate, but a one-line filename assertion in the trap test would make the e2e evidence self-contained.
- fetchKnowledgeDocs' per-collection KnowledgeSearch error branch (:82–84) is the one soft-fail sub-branch without its own test (the KnowledgeCollections error branch is tested); the stub's knowledgeErr fires on both RPCs so the code path is equivalent, and export.go treats both identically. Suggestion only.
- The comment "KnowledgeCollections already filters to readable collections server-side" (:64–65) is a server-contract claim not verifiable from the reviewed files; nothing client-side depends on it for confinement or sanitization, so no demonstrable defect.

## Issues
None.

**Verdict: PASS**

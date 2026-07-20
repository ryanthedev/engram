# Review: Phase 2 - Memory-Knowledge Vault Mapping (sample 2)

## Executed Results (Step 0)
- Test suite: `go test -count=1 ./...` → all 30 packages ok, 0 failures (internal/cli 0.199s incl. all DW-2.x tests; `go test -v ./internal/cli/` shows every DW_2_* test PASS)
- Build: `go build ./...` → exit 0
- Lint: `make lint` (go vet + revive) → exit 0, no findings
- Coverage: `go test -coverprofile` → every new function in vaultknowledge.go exercised (90.9–100% per function; see Test-DW Coverage)

## Requirement Fulfillment

### DW-2.1
PREMISE:  "with a stub server serving one public collection of docs whose `memory_ref`s hit exported entities, export writes one `knowledge/<name>.md` per doc (rendered in deterministic doc-id order), each containing a `[[<concept note>]]` that matches an actual concept filename."
EVIDENCE: internal/cli/vaultknowledge.go:94 (sort by doc id), :203-216 (one note per doc), :274-276 (resolved wikilink); test internal/cli/export_test.go:756-803
TRACE:    stub serves public "curated_notes" with kd2,kd1 (fed id-DESCENDING, same title) → fetchKnowledgeDocs sorts by id → resolveKnowledgeDocs gives kd1 the bare name "Shared title", kd2 the suffixed name → both notes written under knowledge/, each containing `[[Alpha|Alpha]]`; test asserts `concepts/Alpha.md` exists and "2 knowledge docs" in summary. TestDW_2_1_KnowledgeNotesWikilinkToExportedConcepts PASS.
VERDICT:  PASS

### DW-2.2
PREMISE:  "each mapped concept note gains a 'Referenced by' section listing the knowledge note(s) that map to it, deterministically ordered by doc id; a concept mapped by two docs lists both."
EVIDENCE: internal/cli/vaultknowledge.go:218-239 (byConcept grouping preserves doc-id order; outer concept iteration explicitly sorted), :295-317 (append section); test export_test.go:808-842
TRACE:    kd-b,kd-a (fed reversed) both map to e-a → concepts/Alpha.md gains "## Referenced by" with `[[Note A|Note A]]` before `[[Note B|Note B]]` (doc-id order asserted via index comparison). TestDW_2_2_ConceptNoteGetsReferencedByBacklinks PASS.
VERDICT:  PASS

### DW-2.3
PREMISE:  "a doc whose `memory_ref` matches no exported entity renders with an inert unresolved marker (labeled with `memory_ref_name` when present, else the raw id) and produces no dangling wikilink and no backlink."
EVIDENCE: internal/cli/vaultknowledge.go:158-161 (hub-only resolution; ghosts excluded via hubConceptIDs :186-194), :273-286 (marker: name label, id fallback, cleanInline'd); tests export_test.go:849-894, vaultknowledge_test.go:98-112, :125-143
TRACE:    three docs — missing id + name, missing id only, and a REAL ghost (e-b) — all render "unresolved: <label>" ("Ghost Concept" / "also-missing" / "e-b"); test asserts content contains no "[[" at all and no concept note anywhere gained "Referenced by". TestDW_2_3_UnresolvedMemoryRefRendersInertMarker PASS.
VERDICT:  PASS

### DW-2.4
PREMISE:  "injection trap — a doc with `title`/`text`/`memory_ref` carrying control chars, `[[`/`]]`, `../`, and an over-long NFC/NFD name is sanitized, byte-budgeted, NFC-folded, and written strictly inside `<dir>` (path-confinement + wikilink-sanitizer assertions)."
EVIDENCE: export.go:289-307 (sanitizeFilename), :356-378 (safeNoteName: NFC fold + byte budget), sanitize.go:50-120 (sanitizeBody/sanitizeInline), export.go:413-428 (cleanInline), vault.go:59-84 (confinedVaultPath, "knowledge" root depth 2 at :40-50); test export_test.go:903-987
TRACE:    title `../../etc/pwn\x00[[Injected]] xxx…(300)` → sanitizeFilename maps `/`→`-`, drops NUL, `[`/`]`→`-`, trims, caps → single path element; NFD "café" (verified NFD bytes cc 81 in fixture via od) → safeNoteName NFC-folds (backed by passing TestSafeNoteName_NFCFoldPreventsSilentDrop); hostile body's forged callout/fence/frontmatter/wikilink all neutralized. Test walks the WHOLE temp root: canary outside vault untouched, no `etc/` dir created, no file escapes dir, basenames ≤ maxNoteBaseBytes, no NUL/live-callout/live-wikilink in content. hostile memory_ref only ever used as a map key or cleanInline'd label (TestMemoryRefLine "a[[b]]c" → "a--b--c"). TestDW_2_4_InjectionTrapSanitizedAndConfined PASS.
VERDICT:  PASS

### DW-2.5
PREMISE:  "knowledge fetch failure leaves the fully-assembled memory vault byte-intact and exits with a soft warning, not a hard error."
EVIDENCE: internal/cli/export.go:127-139; test export_test.go:993-1022
TRACE:    KnowledgeCollections returns codes.Unavailable → fetchKnowledgeDocs errors → export.go:129-130 prints one warning to stdout and SKIPS renderKnowledgeVault; nothing on the kerr path performs any filesystem operation, so the vault writeVaultModel just finished is untouched by construction. Test asserts exit 0, warning text, memory notes present, zero knowledge/ files, no "knowledge docs" count. TestDW_2_5_KnowledgeFetchFailureIsSoftWarning PASS.
VERDICT:  PASS

### DW-2.6
PREMISE:  "zero knowledge collections → no `knowledge/` folder and the memory-only vault is identical to pre-change output."
EVIDENCE: internal/cli/vaultknowledge.go:203-206 (empty docs no-op), export.go:144-147 (memory-only summary when kstats.Docs == 0); test export_test.go:1027-1056, vaultknowledge_test.go:145-159
TRACE:    server answers zero collections → docs empty → renderKnowledgeVault returns before touching dir → assertTreesEqual (byte-identical per file, both directions) against a baseline export from a knowledge-unaware server passes; no knowledge/ path exists; summary carries no knowledge count. TestDW_2_6_ZeroCollectionsNoKnowledgeFolder PASS.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have DW-ID-named automated tests that ran in Step 0 (TestDW_2_1…TestDW_2_6, all PASS)
- [x] Coverage level "100% of new/changed functions" met: per-function coverage shows every new vaultknowledge.go function exercised (decodeKnowledgeHit/stringField/resolveKnowledgeDocs/knowledgeDocBase/hubConceptIDs/renderKnowledgeNote/memoryRefLine 100%; fetchKnowledgeDocs 93.8%, renderKnowledgeVault 90.9%, appendConceptBacklinks 94.4%) and changed functions runExport 95.3%, writeVault/writeVaultModel/vaultPathDepth 100%, confinedVaultPath 93.3%.
- Uncovered statements are four one-line error-propagation returns (vaultknowledge.go:82-84 KnowledgeSearch error wrap, :212-214 and :234-236 write-error returns, :298-300 confinement-error return) — the substantive error paths (Collections RPC failure, missing concept file) ARE tested.

## Edge Cases (prompt-listed)
| Edge case | Evidence | Status |
|---|---|---|
| Zero collections / empty collection → no knowledge/, byte-identical | TestDW_2_6 (assertTreesEqual = per-file byte compare) + TestRenderKnowledgeVault_EmptyDocsIsNoOp (empty docs → dir untouched) | HANDLED |
| memory_ref absent/empty → no wikilink | memoryRefLine returns "" (vaultknowledge.go:278-280), line omitted (:256); TestMemoryRefLine "absent"; DW-2.4's kd2 note carries no live "[[" | HANDLED |
| Ghost/filtered entity → inert marker, no dangling link, no backlink | hubConceptIDs excludes Ghost (:189); TestDW_2_3 covers a real ghost (e-b) plus missing ids, asserts no "[[" and no backlinks | HANDLED |
| Fetch error preserves memory vault, soft warning | export.go:128-131 (kerr path has zero fs effects); TestDW_2_5 | HANDLED |
| Hostile title/text/memory_ref sanitized, confined, budgeted, NFC-folded | DW-2.4 trace above; canary + root walk + budget + NFC via safeNoteName | HANDLED |
| Multiple docs → same concept, all listed in doc-id order | TestDW_2_2 (reversed feed, order asserted) | HANDLED |
| ADVERSARIAL: exact-k truncation warning, no silent drop | fetchKnowledgeDocs:85-89 warns when len(hits)==retrieval.MaxK (=100, opensearch.go:46); TestKnowledgeCollectionTruncationWarning asserts warning AND all 100 docs still exported | HANDLED |

Adversarial escape attempts (all traced, none succeed): title/collection/id can never reach a path with `/` or `\` (sanitizeFilename + safeNoteName map/strip them; confinedVaultPath re-verifies root ∈ allowlist, depth==2 for knowledge, no dot-only elements, filepath.Rel round-trip); Display/File in `[[File|Display]]` cannot contain `[`, `]`, or `|` (cleanInline + fsIllegal both neutralize them), so attacker text cannot forge or close a wikilink; doc id/collection in frontmatter go through yaml.Marshal, which quotes/indents any `---`-bearing string, so frontmatter cannot be terminated early; a render-phase error aborts hard but every write is atomic (temp+rename) so no memory note is ever left truncated.

## Dead Code
None found. Every function in vaultknowledge.go is referenced (fetchKnowledgeDocs/renderKnowledgeVault from export.go, the rest internally or from tests via the same call graph); no debug statements, no commented-out blocks, no unreachable code after returns.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | Sequential CLI flow; no goroutines, no shared mutable state in new code |
| Error Handling | PASS | Adversarial traces: RPC failure → soft warning (DW-2.5 test); malformed/mistyped fields_json degrades without panic (TestDecodeKnowledgeHit_MalformedJSONDegradesToEmpty, _NonStringFieldDegradesToEmpty); missing concept file → loud error (TestAppendConceptBacklinks_MissingConceptFileErrors) |
| Resources | PASS | writeFileAtomic closes+removes temp on every failure path (vault.go:182-202); gRPC client closed via defer (export.go:107); os.ReadFile self-closing |
| Boundaries | PASS | Empty docs no-op, empty title → "doc"+suffix (TestKnowledgeDocBase incl. all-control-chars title), empty Display → File fallback, 300-char title byte-budgeted, exactly-k boundary warns, duplicate-id/homonym collision loop terminates (counter branch caps id bytes at 24) |
| Security | PASS | DW-2.4 trace: confinement (canary + WalkDir over root), wikilink forgery (emitted-rune "[[" tracking incl. NUL-smuggle, TestDW_2_1_SanitizeBodyAdversarialCorpus 26 cases), NFC fold before uniqueness check, frontmatter unforgeable via yaml.Marshal |

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | External input validated at entry (network data is external even from own server) | PASS | fields_json / field types / textField all degrade defensively (vaultknowledge.go:105-129, tested); non-advancing cursor guard pre-existing (export.go:172-174) |
| cc-defensive-programming | No empty catch blocks / silent swallowing | PASS | The one discarded error (`_ = json.Unmarshal`, :107) is a documented, tested transform-not-reject degrade, not a swallowed bug; writeFrontmatter's discarded Marshal error carries an explicit impossibility justification (vaultnotes.go:109-112) |
| cc-defensive-programming | Barricade design: validation at boundary, re-checked on security paths | PASS | Two-layer: sanitizers at render + confinedVaultPath immediately before every write (defense in depth, vaultknowledge.go:19-25 documents it; DW-2.4 exercises it) |
| cc-defensive-programming | Assertions for bugs only / anticipated errors handled | PASS | No assertions used; all anticipated failures return wrapped errors in the package's established `fmt.Errorf("export: …: %w")` style |
| code-clarity-and-docs | Interface comment on every new entity, "different words" test | PASS | Every type/func in vaultknowledge.go has a rationale-bearing comment (e.g. fetchKnowledgeDocs explains WHY one call drains: no cursor on the RPC); file header states the security model |
| code-clarity-and-docs | Comment accuracy (no stale/contradicting claims) | PASS | Spot-verified load-bearing claims: "docs must already be sorted" ↔ :94 sorts; "resolved is already in doc-id order" ↔ order-preserving loop :151-163; "same non-ghost filter writeVaultModel used" ↔ vault.go:124-126 |
| code-clarity-and-docs | Naming precision/consistency | PASS | knowledgeDoc vs resolvedKnowledgeDoc, hubConceptIDs, memoryRefLine — guessable in isolation; follows existing vault* naming scheme |

## Notes (non-blocking)
- DW-2.5's test proves intactness by file presence + absence of knowledge writes, not a byte-compare against a baseline (DW-2.6 does do the byte-compare). The kerr path provably performs no filesystem operation, so this is evidential style, not a gap.
- A malformed fields_json row degrades to an empty doc silently — it still renders as a near-empty `doc (<id>)` note with no warning line naming the bad row. Documented and tested intent; a per-row warning would aid debugging.
- A knowledge doc titled identically to a concept note basename (e.g. "Alpha") yields knowledge/Alpha.md alongside concepts/Alpha.md — Obsidian shortest-path `[[Alpha]]` links become ambiguous (the generated links here are unambiguous only because both files' links use their own basenames). Cosmetic vault-UX concern, no requirement touches it.
- Four error-propagation one-liners uncovered (listed under Test-DW Coverage); the meaningful error behaviors around them are tested.
- If a misbehaving server returned MORE than k hits, the client exports all of them but the exact-k warning would not fire — undetectable client-side and outside the stated contract.

**Verdict: PASS**

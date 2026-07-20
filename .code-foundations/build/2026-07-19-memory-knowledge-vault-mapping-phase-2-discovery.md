# Discovery + Design: Phase 2 - Knowledge→vault export rendering with memory mapping

## Files Found
- `internal/cli/export.go` — `runExport` CLI wiring; `fetchExport` drains the Export RPC; sanitizer barricades `sanitizeFilename`, `truncateBytes`, `fitNoteName`, `safeNoteName`, `uniqueNoteName`, `cleanInline`; constants `maxFilenameRunes`/`maxNoteBaseBytes`/`maxSuffixIDBytes`.
- `internal/cli/vault.go` — `writeVault` assembles+writes the vault; `allowedVaultRoots`/`vaultPathDepth`/`confinedVaultPath` (the path-confinement barricade); `writeVaultNote`/`writeFileAtomic`.
- `internal/cli/vaultmodel.go` — `VaultModel`/`VaultRefs`/`noteRef`/`Event`/`Concept`/`Claim`; `buildVaultModel` → `buildVaultRefs` (id→noteRef map, ghosts included).
- `internal/cli/vaultnotes.go` — `renderEvent`/`renderConcept` + frontmatter helpers (`writeFrontmatter` via `yaml.MapSlice`).
- `internal/cli/vaultmaps.go` — cluster/MOC rendering; NOT in this phase's file scope (read-only reference for the `wikilink`/`uniqueNoteName` reuse pattern).
- `internal/cli/sanitize.go` — `sanitizeBody`/`quoteBlock` body barricades. NOT in file scope; reused as-is.
- `internal/cli/export_test.go` — `exportStub` (gRPC stub embedding `UnimplementedEngramServer`), `startExportServer`, `runExportCLI`, `richPage` fixture. `vaultTree`/`treeKeys`/`assertTreesEqual` live in `vault_test.go` (out of scope, but usable — same package).
- `internal/engramclient/knowledge.go` — `KnowledgeCollections`, `KnowledgeSearch` (already wired; return `[]mcp.CollectionInfo` / `[]mcp.Hit`). No paging support (`KnowledgeSearchRequest` has no cursor/offset).
- `internal/mcp/mcp.go` — `mcp.Hit{ID,Score,Source,Fields string}` (Fields = `fields_json`), `mcp.CollectionInfo` (embeds `CollectionSpec{Name,TextField,...}`).
- `internal/server/knowledge.go` — confirms the server-side row shape: `row["title"] = d.Title`, `row[textField] = d.Text`, plus whatever custom `Fields` the ingest batch carried (e.g. `memory_ref`, `memory_ref_name`) — all flow into the same JSON row returned as `fields_json`.
- `internal/retrieval/opensearch.go` — `const MaxK = 100`. `internal/cli` already transitively depends on `internal/retrieval` (via `internal/mcp`/other cli commands), confirmed with `go list -deps`, so importing it directly for the constant adds no new dependency weight.

## Current State
The rich memory vault exporter (Phase 5 of the prior plan) is complete and tested: `runExport` drains episodics/entities/edges via `fetchExport`, then `writeVault` builds the deterministic model and writes `events/`, `concepts/`, `maps/` under `confinedVaultPath`'s barricade. Knowledge RPCs (`KnowledgeCollections`/`KnowledgeSearch`) are already implemented server-side and wrapped client-side — nothing to build there. There is currently no `knowledge/` rendering path and no cross-tier mapping.

## Gaps
- `writeVault`'s public signature `(dir, episodics, entities, edges) (vaultStats, error)` is depended on by `vault_test.go` (out of scope for this phase) — it must not change. The knowledge post-pass needs the assembled `VaultModel`/`VaultRefs` to locate concept files, so `writeVault`'s internals are split into a new `writeVaultModel` (same body, three more return values) with `writeVault` becoming a one-line wrapper. This is an in-scope (`vault.go`) refactor that preserves every existing caller/test untouched.
- `"knowledge"` is not in `allowedVaultRoots`, so `confinedVaultPath` would refuse every knowledge write today — added as a flat root (same depth class as `concepts`/`maps`, i.e. 2 path elements).
- `mcp.Hit.Fields` is a JSON *string* (`fields_json`), not a struct — decoding requires `encoding/json` and defensive `.(string)` type assertions per field (untrusted, server-relayed harvester content).
- The doc's full-text key in the JSON row is the collection's `TextField` (not a fixed name) — must be read from `CollectionInfo.TextField`, falling back to `"text"` defensively if a collection spec ever omits it.

## Code Standards
No `docs/code-standards.md` in this repo. Conventions inferred and followed from the existing exporter files: package-level doc comments explaining the security model up front; every barricade/invariant documented at its declaration; table-driven tests; `TestDW_N_M_...` naming tied to Done-When IDs; pure/deterministic core functions (no wall clock, no map-iteration-order dependence — always sort before iterating when order affects output); doc comments answer "why", not "what".

## Test Infrastructure
Standard Go `testing`, table-driven where the shape fits. CLI-level tests in `export_test.go` drive `Run`/`runExportCLI` against an in-process `net.Listen` + `grpc.NewServer()` stub (`exportStub`) implementing only the RPCs a given test needs (embedding `UnimplementedEngramServer` covers the rest with automatic `Unimplemented` errors — which is itself exploited below as the "legacy server" case). Assembly-level tests (`vault_test.go`) call `writeVault`/renderers directly against a temp dir, no gRPC. This phase follows the same split: `export_test.go` gets the DW-tagged end-to-end tests; a new `vaultknowledge_test.go` gets white-box unit tests for the pure helpers in `vaultknowledge.go` (no gRPC needed for those).

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-2.1 | One `knowledge/<name>.md` per doc, deterministic doc-id order, each wikilinking a real concept filename | COVERED | `TestDW_2_1_KnowledgeNotesWikilinkToExportedConcepts` (id-descending input, same-title homonyms — proves order is doc-id, not input order; asserts the wikilink target file exists) |
| DW-2.2 | Mapped concept note gains "Referenced by"; two docs → both listed, doc-id order | COVERED | `TestDW_2_2_ConceptNoteGetsReferencedByBacklinks` |
| DW-2.3 | Unresolved `memory_ref` → inert marker (name-labeled or raw-id), no dangling link, no backlink | COVERED | `TestDW_2_3_UnresolvedMemoryRefRendersInertMarker` (covers: absent-from-graph id, named unresolved, AND a ghost entity that IS in the graph but earns no file — all three must render identically inert) |
| DW-2.4 | Injection trap: control chars, `[[`/`]]`, `../`, over-long/NFC-NFD names → sanitized, budgeted, NFC-folded, confined | COVERED | `TestDW_2_4_InjectionTrapSanitizedAndConfined` (canary-file-outside-dir pattern mirroring `vault_test.go`'s `TestDW_5_2_HostileNamesStayConfined`) |
| DW-2.5 | Knowledge fetch failure → memory vault byte-intact, soft warning, exit 0 | COVERED | `TestDW_2_5_KnowledgeFetchFailureIsSoftWarning` |
| DW-2.6 | Zero collections → no `knowledge/` folder, memory-only vault byte-identical to pre-change | COVERED | `TestDW_2_6_ZeroCollectionsNoKnowledgeFolder` (diffs against a baseline export run through the UNMODIFIED legacy stub) |

**All items COVERED:** YES

## Design Decisions

**1. Fetch/render split for the soft-fail invariant (DW-2.5).** `fetchKnowledgeDocs` (network-only: `KnowledgeCollections` + one `KnowledgeSearch` per collection) runs to completion — success or a single hard error — *before* `renderKnowledgeVault` (disk-only) touches anything. `runExport` treats a fetch error as a caught, printed warning (exit 0, no knowledge write attempted); a render-time error (e.g. disk failure) is allowed to propagate as a hard error like every other write failure in this exporter — the plan's clean-late language is specifically about *fetch* failures, and a barricade/write refusal deserves to be loud (cc-defensive: a bug-stop must not be silently swallowed), not folded into the same soft path.

**2. `writeVault` → `writeVaultModel` refactor, not a signature break.** `vault_test.go` (out of scope) calls `writeVault(dir, episodics, entities, edges) (vaultStats, error)` at ~12 call sites. Rather than touch that file, `writeVault` becomes a one-line wrapper over a new `writeVaultModel` that additionally returns `(VaultModel, VaultRefs)` — the exact values `runExport` needs to locate concept files for the backlink pass, computed once (no duplicate `buildVaultModel` call).

**3. Resolution target is HUB concepts only, not all of `VaultRefs`.** `VaultRefs` deliberately includes ghosts (they're valid link targets elsewhere in the vault, per its own doc comment) — but a ghost earns no concept *file*. The plan's edge case explicitly lumps "ghost" with "filtered out of the graph" as the same unresolved case ("memory_ref present but the entity isn't in the exported graph (ghost/filtered) → inert unresolved marker"). So resolution checks membership in `hubConceptIDs(model)` (non-ghost concepts only), not `refs[id]` presence — a `memory_ref` pointing at a real-but-ghost entity renders exactly like one pointing at nothing.

**4. Own flat namespace for knowledge filenames, not the shared `buildVaultRefs` `used` map.** `knowledge/` is a separate folder with no possible name collision against `events/`/`concepts/`/`maps/` (different root), so it gets its own `used` map, assigned in doc-id order via the same `uniqueNoteName`/`safeNoteName` choke points events/concepts/maps already use. Unlike `buildVaultRefs`, this does NOT pre-tally homonyms to force ALL of them into suffixed form — first-come (by doc id) keeps the bare name, later homonyms get suffixed via `uniqueNoteName`'s own residual-clash extension. This is a deliberate simplification: `buildVaultRefs`'s all-homonyms-suffixed policy exists because Obsidian's bare `[[Name]]` linking is global and ambiguous — but nothing ever bare-links a knowledge note (only explicit `[[File|Display]]` piped links point at them, always with the exact assigned `File`), so uniqueness (guaranteed either way) is the only correctness requirement in play, not homonym-ambiguity-avoidance.

**5. No `internal/engramclient` changes.** The plan permits "a thin drain helper there only if genuinely needed." It isn't: `KnowledgeSearch` is already a single bounded call (`k=100`, no pagination exists server-side), so `fetchKnowledgeDocs` calls the two existing wrappers directly from `internal/cli/vaultknowledge.go`. `internal/engramclient/knowledge.go` and `knowledge_test.go` are therefore untouched (YAGNI, confirmed against the RPC's actual shape — `KnowledgeSearchRequest` has no cursor).

**6. `retrieval.MaxK` reused directly, not duplicated as a local constant.** Confirmed via `go list -deps ./internal/cli/...` that `internal/retrieval` is already in `internal/cli`'s transitive closure (through `internal/mcp`), so importing it in `vaultknowledge.go` adds no new dependency weight and keeps the `k=100` cap as a single source of truth instead of a copy that could drift.

**7. Summary line stays byte-identical when there is no knowledge.** `runExport`'s final summary only appends `, %d knowledge docs` when `kstats.Docs > 0`; the zero-knowledge case (including every pre-existing `export_test.go` test run against the legacy stub, which answers `KnowledgeCollections` with `Unimplemented` and is treated as a soft-fail) prints the EXACT pre-change summary line — required for DW-2.6 and to keep every already-passing `export_test.go`/`vault_test.go` assertion green (test anchoring).

**8. `knowledge/` notes carry no date-bucketing.** Unlike events (`events/2026/...` vs `events/undated/...`), knowledge docs have no `occurred_at` concept in this phase's scope — flat `knowledge/<name>.md`, matching `concepts/`/`maps/`'s existing flat-2-element depth class in `vaultPathDepth`.

## Prerequisites
- [x] Required files exist (export.go, vault.go, vaultnotes.go, engramclient/knowledge.go all present and match the orientation hints)
- [x] Dependencies available (`KnowledgeCollections`/`KnowledgeSearch` already implemented server- and client-side; `retrieval.MaxK` already in the CLI's dependency closure)
- [x] No missing prerequisites — Phase 1 (role-bearing tokens) is a live-run prerequisite only, not a code dependency for this phase (read RPCs work on the current token for a public collection)

## Recommendation
BUILD. No plan/reality mismatch found. Implementation: (1) `vault.go` — add `"knowledge"` to `allowedVaultRoots`; split `writeVault` into `writeVault` (unchanged signature) + `writeVaultModel` (returns model/refs too). (2) New `vaultknowledge.go` — `knowledgeDoc`/`resolvedKnowledgeDoc`/`knowledgeStats` types; `fetchKnowledgeDocs` (network); `decodeKnowledgeHit`, `resolveKnowledgeDocs`, `hubConceptIDs`, `knowledgeDocBase`, `renderKnowledgeVault`, `renderKnowledgeNote`, `memoryRefLine`, `appendConceptBacklinks` (render/disk). (3) `export.go` — `runExport` calls `writeVaultModel` instead of `writeVault`, then the fetch/render knowledge post-pass with the soft/hard error split from Design Decision 1, then the conditional summary line. (4) `export_test.go` — add `startStub`/`knowledgeCollectionInfo`/`knowledgeHit` test helpers (new imports: `encoding/json`, `fmt`, `google.golang.org/grpc/codes`, `google.golang.org/grpc/status`) and the six DW tests plus one bonus truncation-warning test. (5) New `vaultknowledge_test.go` — unit tests for the pure helpers past the DW floor.

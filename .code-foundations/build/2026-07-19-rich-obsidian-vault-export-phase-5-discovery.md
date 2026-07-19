# Discovery + Design: Phase 5 - Client — vault assembly, CLI wiring, determinism

## Files Found
- `internal/cli/export.go` — the entire old pipeline: `runExport`, `fetchExport` (entities+edges only), `checkVaultDir`/`prepareVaultDir`/`isCatastrophicVaultDir`, `vaultFilenames`, `renderNote` (entity-per-note), old flat `confinedNotePath`, old `writeVault`, `writeFileAtomic`, `sanitizeFilename`/`cleanInline`, `vaultStats{Entities,Edges,Dropped}`.
- `internal/cli/export_test.go` — 651 lines: stub gRPC server fixtures (`pbEnt`/`pbEdg`/`exportStub`), CLI-driving helpers, and tests pinned to the OLD entity-per-note format (root-level `Alice.md`, entity frontmatter, edge bullets, `vaultFilenames`).
- `internal/cli/vaultmodel.go` — Phase 2: `buildVaultModel(episodics, entities, edges) (VaultModel, VaultRefs)`; `noteRef{File,Display,Folder}`; folders are `"events/YYYY"`, `"events/undated"`, `"concepts"`.
- `internal/cli/vaultnotes.go` — Phase 3: `renderEvent(Event, VaultRefs) (relPath, content)`; `renderConcept(Concept, VaultRefs, events map[string]Event) (relPath, content)`; relPath = `Folder + "/" + File + ".md"` (forward slashes).
- `internal/cli/vaultmaps.go` — Phase 4: `clusterConcepts(VaultModel) []Cluster`; `renderMap(Cluster, VaultRefs) (relPath, content)`; RelPath = `"maps/<name>.md"`.
- `internal/engramclient/export.go` — `ExportPage.Episodics []ExportEpisodic{EventID,Kind,Text,OccurredAt,SourceIDs}` already on the client wire (Phase 1).
- `internal/cli/cli.go` — `Run` dispatches `runExport(ctx, rest, env, out)`; errors go to errW via Run.

## Current State
Phases 1–4 are complete and committed. The old exporter still renders one note per entity at the vault root; `fetchExport` ignores `page.Episodics`. All rich-model/render primitives exist and are tested; nothing calls them from the CLI path yet.

## Gaps
1. **`internal/cli/vault.go` / `vault_test.go` do not exist** (plan file scope names them). The old write layer lives in `export.go`. Resolution: create `vault.go` to hold the new assembly layer (`vaultStats`, new `writeVault`, nested-path confinement, `writeFileAtomic`) and `vault_test.go` for its tests; `export.go` keeps fetch/flags/dir-guard logic. This matches the plan's file scope without touching files outside it.
2. **Old format tests must be replaced, not preserved.** DW-5.1 mandates "entity-per-note format is gone"; the plan's Constraints say breaking changes are sanctioned. Tests pinned to the old format (root-level notes, entity frontmatter, edge bullets, `vaultFilenames`) are rewritten to pin the same *invariants* (confinement, clobber guards, cursor abort, determinism) against the new format. This is plan-directed scope, not test-weakening.
3. **`vaultStats.Dropped` semantics:** the model join is total (claims never disappear), so "dropped" is recomputed at the assembly layer as *edges neither of whose endpoints was exported* — exactly the edges that contribute no claim in `assembleConcepts` (both `canonicalByEntity` lookups miss). Computable in `vault.go` from the raw records without touching model internals (OUT of scope).
4. **Clobber-warning stream:** `runExport` receives only `out` (cli.go:59 passes no errW). Changing cli.go is outside file scope, so the warning prints to `out`. Acceptable: it is part of the export's user-facing report.

## Code Standards
`docs/code-standards.md` present. Applicable: stdlib `testing` only (no testify); table tests with `t.Run` + got/want; tests named for the DW they pin (`TestDW_5_1_...`); error wrap shape `"export: verb-ing noun: %w"`; three-group imports; no transport imports in business code (cli already at the engramclient edge — unchanged).

## Test Infrastructure
`export_test.go` has a full in-process stub gRPC harness (`exportStub` serving canned `ExportResponse` pages, cursor = page index; `runExportCLI` drives `cli.Run`). Reused as-is; a `pbEpi` fixture is added for episodic pages. `readVault` is flat (root-only) — a recursive `readVaultTree` helper is added for the nested layout.

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-5.1 | `engram export <dir>` writes `events/`, `concepts/`, `maps/`; entity-per-note format is gone; `fetchExport` drains episodics | COVERED | `TestDW_5_1_RichVaultLayoutEndToEnd` (CLI e2e: three folders populated, no root-level notes besides the marker), `TestDW_5_1_FetchExportDrainsEpisodicsAcrossPages` (episodics accumulated across pages, cursor-abort preserved by existing `TestExport_NonAdvancingCursorAborts`) |
| DW-5.2 | every write stays inside the vault dir incl. nested folders (path-escape test across all three folders) | COVERED | `TestDW_5_2_ConfinedVaultPathRejectsEscapes` (unit, dirty corpus: `..` elements, backslash, absolute, wrong root, wrong depth, empty), `TestDW_5_2_HostileNamesStayConfined` (hostile event titles + concept names + map titles through the full writeVault; tree walk asserts no escape, canary untouched) |
| DW-5.3 | full-vault re-run is byte-identical for the same export input | COVERED | `TestDW_5_3_ReRunByteIdentical` (same records → two writeVault runs → recursive byte comparison; plus CLI-level re-run over same stub) |
| DW-5.4 | fetch failure leaves an existing vault untouched; empty tenant → marker-only vault; clobber warning prints | COVERED | `TestDW_5_4_FetchFailureLeavesVaultIntact`, `TestDW_5_4_EmptyTenantMarkerOnlyVault`, `TestDW_5_4_ClobberWarningPrints` |
| DW-5.5 | summary reports events/concepts/maps/ghosts/dropped counts | COVERED | `TestDW_5_5_SummaryCounts` (stats struct from writeVault + printed line via CLI) |

**All items COVERED:** YES

## Design Decisions
- **New `writeVault(dir, episodics, entities, edges) (vaultStats, error)`** in `vault.go`: `buildVaultModel` → events map → `renderEvent` per event → `renderConcept` per non-ghost concept → `clusterConcepts` + `renderMap` per cluster; every relPath passes the nested barricade before `MkdirAll(parent)` + atomic temp+rename.
- **Nested confinement `confinedVaultPath(dir, relPath)`** replaces flat `confinedNotePath` (old one deleted with its sole caller): validates the vault-relative path *structurally* — forward-slash split; first element must be one of `events|concepts|maps`; exact depth (events → 3 elements, concepts/maps → 2); every element individually flat-safe (non-empty, not `.`/`..`, no `/\`, not abs); final `filepath.Rel` re-check. Any violation is a bug-stop error that aborts the whole export at the barricade (defensive-programming: renderers + sanitizeFilename should make this unreachable; if it fires, refuse loudly, never write).
- **Stats:** `vaultStats{Events, Concepts, Maps, Ghosts, Dropped int}` — Concepts counts written (non-ghost) notes; Ghosts counts `Concept.Ghost`; Dropped counts edges with zero exported endpoints (mirrors the model's no-claim edges without reaching into model internals).
- **Summary + warning to `out`:** warning line first ("re-export regenerates every note; manual edits will be lost"), then `exported N events, M concepts, K maps to <dir> (G ghosts, D dropped)`.
- **`fetchExport`** gains an `[]ExportEpisodic` first return, accumulating `page.Episodics`; the cursor-non-advance abort is untouched.
- **Refactoring discipline (cc-refactoring-guidance):** this is a plan-sanctioned behavior *replacement*, not a behavior-preserving refactor — old renderer code (`vaultFilenames`, `displayNames`, `renderNote`, old `writeVault`, old `confinedNotePath`) is deleted in the same change as its replacement is wired, with the invariant-pinning tests rewritten against the new format. Shared helpers (`sanitizeFilename`, `cleanInline`, `idPrefix`, dir guards, `writeFileAtomic`) are kept where the model/renderers already consume them.

## Prerequisites
- [x] Phases 1–4 outputs present (`Episodics` on wire; `buildVaultModel`; both renderers; clustering)
- [x] Stub gRPC test harness available
- [x] `make build` / `make test` / `make lint` targets exist

## Recommendation
BUILD — extend `fetchExport`, create `vault.go`/`vault_test.go` with the rich assembler under a nested-path barricade, rewire `runExport`, delete the entity-per-note layer, rewrite the format-bound tests, keep every invariant test green.

## Review-Fix Addendum (post 3-sample security review)
1. **Symlink bypass of the catastrophic-dir guard (high)**: `prepareVaultDir` compared `filepath.Abs(dir)` without resolving symlinks, so `--force` on a symlink to `$HOME` or `/` deleted the target's contents. Fixed: `filepath.EvalSymlinks` on both the vault path (not-exist tolerated — an absent dir cannot be a catastrophic target; any other resolution error fails loud) and on `$HOME` (both sides of the comparison are real paths; macOS tempdirs live behind `/var → /private/var`). Pinned by `TestPrepareVaultDir_SymlinkToHomeRefused` (full CLI, `--force`, scratch `$HOME` contents survive) and `TestPrepareVaultDir_SymlinkToRootRefused`.
2. **Coverage**: all reviewer-enumerated branches now executed — `fetchExport` 100%, `checkVaultDir` 100%, `runExport` 96.8%, `prepareVaultDir` 88.9%. The four remaining single-statement `return err` blocks are not portably constructible: `runExport` writeVault-error (needs an FS fault between a successful prepare and the write), `prepareVaultDir` Abs error (Go's `Getwd` `$PWD`/fd fallbacks survive a deleted cwd on darwin), EvalSymlinks non-not-exist error (every constructible trigger — ELOOP, ENOTDIR, perms — fails `checkVaultDir` first), and the second `ReadDir` (requires a mid-call permission race).
3. **gofmt**: `vault_test.go` reformatted; `gofmt -l ./internal/cli/` clean.

## Review-Fix Addendum 2 (re-review blocker)
**Filename byte-length overflow (data loss)**: `maxFilenameRunes` capped RUNES; a 60-rune emoji slug is 240 BYTES, and date prefix + collision suffix + `.md` pushed basenames past the 255-byte NAME_MAX — on ext4 the atomic rename fails ENAMETOOLONG *after* `prepareVaultDir` already cleaned the old vault (clobber-then-abort). Fixed with a byte budget at the single filename-assignment layer:
- `maxNoteBaseBytes = 240` — every full basename (`prefix + base + suffix + ".md"`) fits this, 15 bytes under NAME_MAX.
- `fitNoteName(base, suffix)` truncates the BASE in bytes on a UTF-8 rune boundary (never the uniqueness-carrying suffix; date prefixes sit at the front of the base and survive), re-trimming trailing dots/spaces.
- `uniqueNoteName(base, id, suffix, used)` (export.go) is now the ONE suffixing algorithm consumed by both `buildVaultRefs` (events + concepts) and `assignClusterFilenames` (maps): residual-clash extension embeds at most `maxSuffixIDBytes = 24` id bytes, then a growing counter — a pathological id can no longer grow a name past the budget. Misc buckets keep their canonical tiny names. Suffix strings are byte-identical to the old algorithm for ids ≤ 24 bytes, so all prior naming tests pass unchanged. (Touching vaultmodel.go/vaultmaps.go was directed by the coordinator's fix instruction — "apply this wherever note filenames are assigned".)
- Tests: `TestDW_Fix_LongNamesFitNameMax` (60-emoji titles with date prefix AND forced collision suffixes across events/concepts/maps; write succeeds; measured worst-case basename **239 bytes**), `TestTruncateBytes_RuneBoundary`, `TestFitNoteName_ByteBudget` (incl. over-budget suffix), `TestUniqueNoteName_BoundedGrowth` (50 clashes on a 100-byte id, all names in budget, all distinct).
- Cosmetic: `TestPrepareVaultDir_AbsErrorWithDeletedCwd` comment corrected (darwin errors at MkdirAll, not Abs); `fs` flag-set variable in `runExport` renamed `flags` (shadowed `io/fs`).

## Review-Fix Addendum 3 (final blocker — root-cause the filename class)
**Invalid-UTF-8 basenames from client-supplied ids (data loss sibling)**: `idPrefix` did `id[:n]` — a byte slice — and its output entered collision suffixes unsanitized. Multibyte ids (`世×8`, `日本語-…`, `ab🦄🦄rest`) sliced mid-rune produced invalid-UTF-8 basenames; on APFS the rename fails after the old vault was cleaned (clobber-then-abort), and a `/`-bearing id would have hit the confinement barricade (abort) instead of flattening. Closed at the root, both halves:
1. `idPrefix` is now rune-safe (`truncateBytes` — at most n bytes, never splitting a rune). Byte-identical for ASCII ids, so all prior determinism tests pass unchanged.
2. `safeNoteName(name)` (export.go) is the ONE final choke point every assembled basename passes through before use as a path element — events, concepts, maps, collision-suffixed and misc variants alike: `strings.ToValidUTF8` (partial/invalid runes stripped), the `fsIllegal`/control-char policy re-applied to the ASSEMBLED name (so id-derived suffix material is covered), leading/trailing dot-and-space trim, byte cap on a rune boundary, and a non-empty fallback. Wired inside `uniqueNoteName` on every candidate BEFORE the uniqueness check (guarantees hold for the name actually written; counter digits survive sanitization, so residual-clash termination and distinctness are preserved) and on the misc-bucket path in `assignClusterFilenames`. Correctness no longer depends on any upstream field being independently clean.
- Tests: `TestDW_Fix_MultibyteIDsSafeBasenames` (CJK/emoji/separator ids drive homonym suffixes, a residual-clash extension, and a forced map suffix across all three folders; every basename `utf8.Valid`, ≤255 bytes, no `fsIllegal` char; export succeeds; deterministic re-run) and `TestSafeNoteName_ChokePoint` (partial-rune strip, nothing-survives fallback, separators, full illegal set, byte cap). Filename helpers all at 100% coverage.

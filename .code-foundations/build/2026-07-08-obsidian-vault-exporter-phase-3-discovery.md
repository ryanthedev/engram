# Discovery + Design: Phase 3 - CLI `export` + Obsidian vault rendering

## Files Found
- `internal/cli/cli.go` — subcommand dispatch (`Run`, line 42 switch), `dialClient` (line 206), flag pattern (`runStatus`/`runAudit`), `usage` const. All existing subcommands print to an `io.Writer`; none write files.
- `internal/cli/cli_test.go` — package `cli_test` (black-box); in-process stub gRPC server pattern (`stubEngramServer` + `startStubEngramd`) driving `cli.Run`.
- `internal/engramclient/client.go:130` — `Export(ctx, cursor) (*engrampb.ExportResponse, error)` from Phase 2, exactly as the plan promises.
- `api/proto/engram.proto:162-214` — `ExportRequest{cursor}`, `ExportEntity{id, name, aliases, mention_count, source_ids, scope, team_id, owner_agent_id, valid_at, created_at}`, `ExportEdge{id, from_entity_id, to_entity_id, predicate, statement, source_ids, scope, team_id, owner_agent_id, valid_at, created_at}`, `ExportResponse{entities[], edges[], next_cursor}`.
- `e2e/harness.go` — `Boot()` (self-host or `ENGRAM_E2E_ADDR`), `MintToken` (:142), `RunCLI` (:163). `e2e/registry.go` — `RegisterScenario` zero-core-edit extension point. `e2e/scenarios_graph.go` — fixture-fact ingest via pipe directives + `waitForHit` polling; `uniqueTenant` in `scenarios_sample.go`.
- `Makefile` — e2e runs `go test -tags=e2e ./e2e/` against a compose stack (`make e2e`) or self-hosted against OpenSearch at `ENGRAM_OPENSEARCH_URL` (default localhost:9200).
- `go.mod`/`go.sum` — `go.yaml.in/yaml/v2 v2.4.4` already in the module graph (indirect).

## Current State
Phases 1–2 are committed: paginated tenant/ACL-scoped `Export` RPC + client method exist and are tested. The CLI has seven subcommands, all output-only. There is no file-writing code path anywhere in `internal/cli` — this phase introduces it fresh, as the plan flags.

## Gaps
- Plan matches reality; no contract drift found. One decision the plan leaves open: **how the tool knows it created `<dir>`** ("refuses a non-empty dir the tool didn't create"). Resolved below with an ownership marker file (`.engram-vault`) — DW-3.4 requires a *re-run without `--force`* to clobber-and-regenerate, so ownership must be detectable on disk.
- Frontmatter must "parse as valid YAML" under adversarial entity names (aliases carry untrusted content). Hand-rolled YAML escaping is an injection risk; `go.yaml.in/yaml/v2` (already in the module graph) is used to marshal frontmatter. Promoting an indirect dep to direct — smallest-possible dependency change, noted as a deviation candidate.
- Go's `flag` stops at the first positional arg; existing subcommands require flags-first. `export` re-parses trailing args after the positional `<dir>` so both `export --force <dir>` and `export <dir> --force` work.
- **Found during implementation (missed in initial discovery):** `internal/importlint` (an earlier phase's DW-3.7 architectural gate, enforced by `go test`) forbids importing `api/engrampb` outside allowlisted transport edges — and `internal/cli` is not allowlisted. Phase 2's pinned `Client.Export(ctx, cursor) (*engrampb.ExportResponse, error)` is therefore unusable from CLI production code as-is. Resolution: keep the pinned method untouched and add an additive plain-struct adapter `Client.ExportPage` (+ `ExportEntity`/`ExportEdge`/`ExportPage` records) in the allowlisted `internal/engramclient` — the exact pattern `Audit`/`AuditResult` already establishes there ("adapts the proto surface", per the package doc). One new file (`internal/engramclient/export.go`) outside this phase's `internal/cli/**, e2e/**` file scope; reported as a deviation for orchestrator sign-off.
- **Found live in e2e:** entity dedup-merge across two ingest events is timing-dependent upstream (graph-stage `CandidateEntities` vs OpenSearch index refresh) — the fixture entity B sometimes exports twice. The exporter handles it correctly (deterministic id-suffixed homonym filenames; links resolve), so the e2e scenario asserts B by prefix and note-count == the printed exported count, never an exact fixture count.

## Code Standards
`docs/code-standards.md` applies: return wrapped errors (`fmt.Errorf("...: %w")`), no panics in library code, `context.Context` first param on RPC calls, table-driven tests, every phase ships error-path tests. Followed throughout.

## Test Infrastructure
- Unit: `go test ./internal/cli/...`; existing tests use an in-process stub gRPC server + `cli.Run` over buffers. New white-box tests (`package cli`) can coexist with the black-box `cli_test` package in the same dir.
- e2e: build-tagged `e2e`, scenario registered via `init()` + `RegisterScenario` (zero core edits); requires a live OpenSearch (self-host mode) or `ENGRAM_E2E_ADDR`. Executability checked at validation time; if no cluster is reachable, the e2e scenario is delivered correct-under-`-tags e2e` and the DW behaviors are additionally unit-covered per the dispatch instructions.

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-3.1 | one `.md` per entity with H1, frontmatter (aliases, mention_count, provenance), edge bullets | COVERED | `TestDW_3_1_WriteVaultRendersNotes` (unit, render-to-temp-dir + YAML re-parse); e2e `export/obsidian-vault` |
| DW-3.2 | `[[file\|Display]]` piped links resolve to real files; no danglers; dropped count printed | COVERED | `TestDW_3_2_EdgeLinksResolveAndDanglersDrop` (unit), `TestDW_3_2_DroppedCountPrinted` (CLI-level vs stub server); e2e link-resolution walk |
| DW-3.3 | homonym collisions → deterministic id-suffixed filenames, stable across re-runs; illegal chars sanitized | COVERED | `TestDW_3_3_HomonymFilenamesDeterministic` (two runs byte-identical), `TestDW_3_3_SanitizeFilename` (table-driven illegal chars, case-fold collision) |
| DW-3.4 | re-run clobbers-and-regenerates; foreign non-empty dir refused unless `--force` | COVERED | `TestDW_3_4_RerunClobbersOwnedDir`, `TestDW_3_4_ForeignDirRefusedWithoutForce`, `TestDW_3_4_ForceCleansForeignDir` (CLI-level vs stub server) |
| DW-3.5 | e2e: every `[[file]]` target resolves on disk; every note's frontmatter parses as valid YAML | COVERED | e2e scenario `export/obsidian-vault` (walks vault, resolves links, `yaml.Unmarshal` on every frontmatter block); same assertions also run in unit `TestDW_3_1`/`TestDW_3_2` against the renderer |
| DW-3.6 | no written path escapes `<dir>`; `../` / separators confined | COVERED | `TestDW_3_6_TraversalNamesConfined` (unit, adversarial table: `../../etc/x`, absolute paths, `..`, `.`, `a/b\c`, empty-sanitizing, NUL); post-write walk asserts all files under dir and canary outside dir untouched |

**All items COVERED:** YES

## Design Decisions

Per the design steer + cc-routine-and-class-design (functional cohesion, ≤7 params, no inheritance anywhere):

- **New file `internal/cli/export.go`** — pure rendering layer separated from RPC/CLI wiring. `cli.go` gains only `case "export"` + a usage line.
- Routines (each one operation; `runExport` is the organizer that only orchestrates):
  - `runExport(ctx, args, env, out)` — parse flags (two-pass so `--force` may follow `<dir>`) → `checkVaultDir` (fail fast, read-only, *before* dialing) → dial → `fetchExport` → `prepareVaultDir` → `writeVault` → print stats.
  - `fetchExport(ctx, client) (entities, edges, error)` — pages until `next_cursor` empty, accumulating structured records; **guards against a non-advancing cursor** (external input; would otherwise loop forever).
  - `checkVaultDir(dir, force) error` — refusal decision: missing → OK (created later); file → error; empty → OK; marker `.engram-vault` present → owned, OK; non-empty foreign → error unless `force`.
  - `prepareVaultDir(dir, force) error` — re-runs `checkVaultDir` (TOCTOU defense: the dir may have changed since the pre-dial check), creates the dir if missing, removes every entry *inside* it (never the dir itself), writes the marker. Catastrophic-mistake guard: refuses to clean when the absolute cleaned path is the filesystem root or the user's home dir, even with `--force`.
  - `vaultFilenames(entities) map[id]filename` — deterministic, order-independent: sanitize each name; names colliding case-insensitively (macOS FS) get ` (id[:8])` suffixes on **all** members of the collision group (not first-wins, so the assignment is iteration-order-independent and stable across re-runs); suffix extends until unique (entity ids are unique → terminates). Empty-sanitizing names fall back to `entity (id[:8])`. Empty-id entities are dropped (cannot be linked deterministically).
  - `sanitizeFilename(name) string` — strips/replaces FS+Obsidian-illegal runes (`/ \ : * ? " < > | # ^ [ ]`, control chars), trims leading/trailing dots+spaces, caps length; `.`/`..`/empty results → "" (caller falls back).
  - `renderNote(entity, outEdges, filenames) string` — frontmatter via `yaml.Marshal` of an ordered `yaml.MapSlice` (safe escaping of adversarial aliases — hand-rolled YAML quoting rejected as an injection risk), H1 with inline-cleaned display name, edge bullets `- <predicate> [[file|Display]]` sorted (predicate, target, edge id) for determinism; predicate/display cleaned of `[ ] |` + newlines so untrusted content can't forge or break link syntax.
  - `writeVault(dir, entities, edges, out?) (stats, error)` — builds the filename map **first**, drops any edge whose *either* endpoint is unmapped (counted), then writes each note **atomically** (`os.CreateTemp` in dir + rename). **Path confinement enforced here as the last line before every write**: the joined target must resolve strictly inside `dir` (`filepath.Rel` check, no `..` escape) — a violation is treated as a bug-stop error, never a write. This is defense-in-depth on top of sanitization, per cc-defensive-programming's security-critical-path rule.
- **Barricade** (cc-defensive-programming): entity names, aliases, predicates, and the server's cursor are external input — validated/sanitized at the renderer entry; inside the write loop the only re-check is the path-confinement one (security path validates twice). All errors returned + wrapped; no panics; correctness over robustness (refuse rather than risk clobbering a foreign dir or escaping the vault).
- **Ownership marker** `.engram-vault`: written on every export; its presence marks a tool-owned dir, enabling DW-3.4's no-`--force` re-run while keeping foreign dirs refused by default.
- **Memory**: structured records accumulate in memory (required — the id→filename map must be complete before edge rendering); notes are written per-file, never one giant buffer. Matches the "paged, not buffered whole" edge case as scoped by Phase 2's accumulate-until-exhausted contract.

## Prerequisites
- [x] `engramclient.Export` exists (Phase 2 committed)
- [x] Export proto messages carry everything the note needs (aliases, mention_count, provenance fields)
- [x] e2e harness + scenario registry available
- [x] yaml lib in module graph (test + frontmatter marshal)

## Recommendation
BUILD — plan matches reality. Work: add `export` dispatch + usage in `cli.go`; new `internal/cli/export.go` (wiring + pure renderer); white-box unit tests `internal/cli/export_test.go`; e2e scenario `e2e/scenarios_export.go`.

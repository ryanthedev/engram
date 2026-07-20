# Discovery + Design: Phase 3 - Seed tooling for the mapped collection

## Files Found
- `cmd/engram-seed-knowledge/**` — does not exist yet (new).
- `internal/cli/seedknowledge.go` — does not exist yet (new).
- `internal/cli/seedknowledge_test.go` — does not exist yet (new).
- `scripts/seed-curated-notes.sh` — does not exist yet (new).
- `internal/cli/cli.go` — has `Env` type (`func(string) string`), `dialClient(env, addr, token) (*engramclient.Client, error)` (resolves `-addr`/ENGRAM_ADDR, `-token`/ENGRAM_TOKEN), `firstNonEmpty`, and `parseRoles` (Phase 1). All package-private — the seed routine's core logic must live in package `cli` to reuse them, matching the plan's file scope.
- `internal/cli/export.go` — `runExport` is the closest sibling: `flag.NewFlagSet(..., flag.ContinueOnError)`, `-addr`/`-token` flags, `dialClient`, `defer client.Close()`. Reused as the template for `RunSeedKnowledge`'s flag handling.
- `internal/engramclient/knowledge.go` — `CreateCollection(ctx, mcp.CollectionSpec) error` and `KnowledgeIngest(ctx, collection, source, harvestID string, docs []mcp.KnowledgeDoc) (int, error)` — exact signatures the plan named. Both are thin proto-translation wrappers around `c.api.CreateCollection`/`c.api.KnowledgeIngest`, propagating the gRPC status error untouched.
- `internal/mcp/mcp.go` — `KnowledgeDoc{ID, Title, Text, SourceVersion, Fields map[string]any}` (memory_ref/memory_ref_name live in `Fields`, not dedicated struct fields); `CollectionSpec{Name, TextField, Mappings map[string]FieldSpec, Public, Roles}`; `FieldSpec{Type, Filterable, Sortable}`.
- `internal/server/knowledge.go` — confirms the exact gRPC codes to test against: `codes.AlreadyExists` (line 262, "collection %q already exists") on `CreateCollection` when the name is taken; `codes.PermissionDenied` (lines 98, 159, "not authorized for this knowledge operation" / "...to read this collection") on role failure.
- `internal/cli/export_test.go` — the `exportStub`/`startStub` pattern: an in-process `grpc.NewServer()` on `127.0.0.1:0` embedding `engrampb.UnimplementedEngramServer`, overriding only the RPCs a given test needs, with `t.Cleanup(srv.Stop)`. This is the stub-server pattern Phase 3's tests reuse for `CreateCollection`/`KnowledgeIngest`.
- `cmd/engram-harvester/main.go` — precedent for classifying a returned error's gRPC status code via `errors.As` to an interface exposing `GRPCStatus() *status.Status`, then special-casing `codes.PermissionDenied`/`codes.Unauthenticated` with an operator-facing remedy message. Confirms the idiom this phase's `wrapPermissionDenied` follows (using `status.Code(err)` directly, since `KnowledgeIngest`/`CreateCollection` return gRPC errors straight through, no extra wrapping needed for the code to be inspectable).
- `cmd/engram-goldgen/main.go`, `cmd/engram-apply-templates/main.go` — precedent for a small standalone `cmd/` binary: `main()` parses flags (or delegates flag parsing to the internal package), calls one internal entry point, prints a one-line summary, `os.Exit(1)` on error. `engram-seed-knowledge`'s thin main follows this shape, delegating flag parsing to `cli.RunSeedKnowledge` (which owns `-addr`/`-token` — needed for env-var fallback via `Env`).
- `scripts/backfill-reextract-rtd.sh` — the project's shell-script conventions: `#!/usr/bin/env bash`, `set -euo pipefail`, `ENGRAM_*` env var overrides with sane localhost defaults, banner `echo "== ... =="` lines.

## Current State
Phases 1 and 2 are already built and merged onto this branch (`7a9db9b`, `58b20d2`). Role-bearing token minting (`--roles`) and the knowledge→vault export renderer both exist and are tested. Nothing in Phase 3's file scope exists yet — this is a from-scratch build with a clean, disjoint file scope (confirmed by `git status` showing a clean tree and the plan's own note that Phases 1/2/3 have disjoint scopes and no code dependency).

## Gaps
None against the plan text — the orientation hints in the dispatch prompt (engramclient wrapper signatures, token/addr resolution, stub-server test pattern) all verified exactly as described. One small extra decision was needed: the plan doesn't specify HOW `RunSeedKnowledge` is wired into `cmd/engram-seed-knowledge/main.go` (a dedicated binary, not a subcommand of `engram`). Resolved by following the `goldgen`/`apply-templates` "thin main, exported internal entry point" precedent rather than adding a new `cli.Run` subcommand branch — the plan's file scope explicitly excludes `internal/cli/cli.go` from Phase 3, so `cli.Run`'s switch is not touched.

## Code Standards
No `docs/code-standards.md` found in the repo. Conventions are instead derived directly from the surrounding code (see Files Found above): doc-comment-first files explaining security model and design rationale, `flag.NewFlagSet(name, flag.ContinueOnError)` + `fs.SetOutput(io.Discard)`, errors wrapped with a `"<component>: <context>: %w"` prefix, table-driven tests named `TestDW_<phase>_<item>_<Description>` for DW-covering tests and plain `Test<Description>` for supplementary tests, in-process `net.Listen("tcp","127.0.0.1:0")` + `grpc.NewServer()` stubs with `t.Cleanup(srv.Stop)`.

## Test Infrastructure
Standard `go test`, table-driven where the plan calls for a table test (DW-3.2, DW-3.3 dirty case). The stub-server pattern from `export_test.go` (`exportStub`/`startStub`) is reused in spirit but a fresh, phase-scoped `seedStub` type is added in `seedknowledge_test.go` (Phase 3's own file scope) rather than extending `exportStub` — the two stubs serve disjoint RPC sets (`Export`/`KnowledgeCollections`/`KnowledgeSearch` vs. `CreateCollection`/`KnowledgeIngest`) and disjoint files, so no cross-file coupling is introduced. Tests drive the real `*engramclient.Client` dialed against the in-process stub (no interface-mock layer), matching how `export_test.go` exercises `runExport` end-to-end.

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-3.1 | Against a stub server, the seed routine issues one create-collection (with the `memory_ref` mapping) and one ingest batch of the demo docs; a re-run issues ingest again without a duplicate-create failure (idempotency asserted — the stub returns an already-exists error on the 2nd create and the routine tolerates it). | COVERED | `TestDW_3_1_SeedIssuesCreateThenIngest`, `TestDW_3_1_RerunToleratesAlreadyExistsAndReingests` |
| DW-3.2 | The demo-doc set is defined in one place, each doc carrying a non-empty `memory_ref` (entity id) + `memory_ref_name`; a table test asserts every doc has a non-empty `memory_ref`. | COVERED | `TestDW_3_2_DemoDocsCarryMemoryRefAndName` (table test over `seedDemoDocs`) |
| DW-3.3 | A missing/role-less token path surfaces the server's PermissionDenied with a message naming the `--roles admin` remedy (dirty test — simulate the stub returning PermissionDenied and assert the wrapped message). | COVERED | `TestDW_3_3_PermissionDeniedOnCreateNamesRolesRemedy`, `TestDW_3_3_PermissionDeniedOnIngestNamesRolesRemedy` |

**All items COVERED:** YES

## Design Decisions

**Core routine location & shape.** `internal/cli/seedknowledge.go` (package `cli`, matching the plan's file scope) exports `RunSeedKnowledge(ctx, args []string, env Env, out io.Writer) error` — same shape as `runExport` minus the `errW` (no distinct warning stream is needed here; nothing here has export's "soft-fail, keep going" duality). It owns `-addr`/`-token` flag parsing and calls `dialClient` (reused unchanged from `cli.go`), then delegates to the unexported `seedKnowledge(ctx, *engramclient.Client, out) error` that does the actual create+ingest. Splitting these two lets tests exercise `seedKnowledge` directly against a dialed stub client without re-parsing flags, while `RunSeedKnowledge` is still covered by a thin flag-parsing test.

**No new abstraction layer over `*engramclient.Client`.** Considered an interface (`knowledgeSeeder`) to mock `CreateCollection`/`KnowledgeIngest` in tests. Rejected: `export_test.go` already establishes the project convention of testing CLI routines against a *real* dialed `engramclient.Client` pointed at an in-process stub `grpc.Server` — introducing a mock interface here would be a second, inconsistent testing style for no benefit (the stub-server path already gives full black-box coverage of request shape, including proto marshaling).

**Idempotency handling.** `createSeedCollection` calls `client.CreateCollection`; on `err == nil` it returns nil (created), on `status.Code(err) == codes.AlreadyExists` it also returns nil (tolerated — this is the documented idempotent path), otherwise the error is passed through `wrapPermissionDenied` and returned. This directly encodes the plan's Edge Cases: "collection already exists → skip/tolerate create, proceed to ingest (no fatal error)."

**Error wrapping.** `wrapPermissionDenied(action string, err error) error` checks `status.Code(err) == codes.PermissionDenied` (following the `cmd/engram-harvester/main.go` precedent of inspecting the gRPC status code directly — `CreateCollection`/`KnowledgeIngest` propagate the server's status error untranslated, so `status.Code` works without an `errors.As` dance) and if so returns a new error naming the failing action and the `engram token create --roles admin` remedy, wrapping the original with `%w` so `errors.Is`/`status.Code` still resolve through it for any caller that cares. Non-PermissionDenied errors (e.g. `codes.Internal`, dial failure) pass through unchanged — the plan only asks for a clean message on the permission-denied path, and inventing a generic wrapper for every other error would obscure the real cause.

**Demo-doc set as one constant.** `seedDemoDocs []mcp.KnowledgeDoc` is a single package-level `var` in `seedknowledge.go` (DW-3.2's "single source"), each entry's `Fields` map carrying `memory_ref` and `memory_ref_name` (matching the `Fields`-based shape `mcp.KnowledgeDoc` actually has — there are no dedicated struct fields for these, confirmed by reading `mcp.go`). The six docs are exactly the ids/names given in the dispatch prompt, including the deliberately-unresolvable `curated-unresolved-demo` entry (its `memory_ref` is the literal string `"entity-does-not-exist-000"` — non-empty, so it still satisfies DW-3.2's "non-empty memory_ref" invariant while demonstrating Phase 2's inert-unresolved-marker path on export).

**Collection spec.** `seedCollectionSpec()` returns `mcp.CollectionSpec{Name: "curated_notes", TextField: "text", Public: true, Mappings: {"memory_ref": {Type:"keyword", Filterable:true, Sortable:true}, "memory_ref_name": {Type:"keyword"}}}` — exactly the mapping the dispatch prompt specifies. `TextField: "text"` matches every `KnowledgeDoc.Text` being the BM25 target and mirrors the `"text"` field name `export_test.go`'s `knowledgeHit` fixture and `vaultknowledge.go`'s field decoding already assume.

**Stable source/harvest_id.** `seedSource = "curated-demo"`, `seedHarvestID = "curated-demo-seed"` as package constants — stable across runs (re-running seeds the same harvest id), matching the plan's "stable `source` + `harvest_id`" requirement and enabling a future `KnowledgeDelete` mark-and-sweep against this exact `(source, harvest_id)` pair if ever needed (out of this phase's scope, but the constants make it possible).

**`cmd/engram-seed-knowledge/main.go` shape.** Thin: builds a `signal.NotifyContext` (mirroring `cmd/engram/main.go`), calls `cli.RunSeedKnowledge(ctx, os.Args[1:], os.Getenv, os.Stdout)`, and on error prints to stderr and `os.Exit(1)` — the `goldgen`/`apply-templates` shape, not the `cli.Run(...) int` multiplexed-subcommand shape (this is a single-purpose binary, not a new `engram` subcommand, and the plan's file scope confirms `internal/cli/cli.go` is untouched).

**`scripts/seed-curated-notes.sh`.** A convenience wrapper: `set -euo pipefail`, resolves `ENGRAM_ADDR`/`ENGRAM_TOKEN` env vars (already read directly by `RunSeedKnowledge` via `Env`, so the script mostly just documents the two ways to invoke: `go run ./cmd/engram-seed-knowledge` for dev, or the built `./bin/engram-seed-knowledge` binary if present), forwarding any extra args. No new behavior — matches the plan's "convenience script wrapping the binary."

## Prerequisites
- [x] Required files' dependencies (`internal/cli.Env`, `dialClient`, `internal/engramclient.Client.{CreateCollection,KnowledgeIngest}`, `internal/mcp.{CollectionSpec,FieldSpec,KnowledgeDoc}`) all exist and match the plan's described signatures exactly.
- [x] Phase 1 (`--roles`) and Phase 2 (knowledge export) are already built, giving Phase 3 real things to point at (an admin-token mint path and a renderer that will pick up `curated_notes` on the next export) — though Phase 3's build/tests have no code dependency on either, only the orchestrator live-run does.
- [x] `google.golang.org/grpc/{codes,status}` already a project dependency (used throughout `internal/server`, `internal/engramclient`, `cmd/engram-harvester`).

## Recommendation
BUILD. The plan fits reality exactly — no UPDATE_PLAN needed. Proceed to stub the interface, implement, and validate.

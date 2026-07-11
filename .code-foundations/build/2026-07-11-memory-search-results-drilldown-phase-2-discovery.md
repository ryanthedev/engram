# Discovery + Design: Phase 2 - memory_read(id, source) drill-down

## Files Found
- `api/proto/engram.proto` — service `Engram` with Ingest/Search/Status/Audit/Export; `Provenance` + `FactVersion` messages reusable for the semantic read.
- `api/engrampb/engram.pb.go`, `engram_grpc.pb.go` — buf-generated stubs; regen via `make proto` → `scripts/codegen.sh` (version-pinned `go run buf@v1.55.1`).
- `internal/store/facts.go:26` — `GetFact` (realtime `_doc/{id}` GET on the semantic index) — the mirror template for the episodic getter. `FindByEventID` (may return several) confirmed unusable for by-id read.
- `internal/store/audit.go:17` — `AuditFact(id) -> (target, versions, ok, err)`; server authorizes AFTER fetch.
- `internal/server/server.go:165` — `Audit` handler: the fail-closed reference (empty id → InvalidArgument; unknown/cross-tenant/ACL-denied → one opaque `NotFound`; `s.ACL.CanRead` on the fetched record's ACL fields BEFORE building the response).
- `internal/server/server.go:42` — `ReadAuthorizer` seam (`CanRead`), satisfied by `*acl.Filter` (fail-closed `Enforce`/`Authorize`, zero value denies).
- `internal/mcp/mcp.go:52` — `Backend` seam (Ingest/Search/Status only today); `internal/mcp/tools.go` — tool registration + `toolResult` (structured JSON + structuredContent).
- `internal/engramclient/client.go` — `var _ mcp.Backend = (*Client)(nil)`; already has `Audit` returning typed `AuditResult`/`FactVersion`.
- `internal/retrieval/opensearch.go:265–299` — the fetch→authorize→project ordering precedent (`filterAuthorized` before `projectFields`) and the episodic display allowlist (`text,kind,occurred_at,event_id,source_ids`).
- `cmd/engram-server/main.go:270–277` — wiring point (`svc.Auditor = st; svc.ACL = aclFilter`).
- Tests: `internal/server/audit_test.go` (fakeAuditor/fakeReadAuthz/authedCtx pattern), `internal/store/opensearch_test.go` (httptest fakeOS already handles GET `_doc/{id}`; `TestOpenSearchStoreGetFact` to mirror), `internal/mcp/mcp_test.go` (fakeBackend + in-process refClient over io.Pipe).

## Current State
Phase 1 landed (commit ac9fb65): compact-line search results exposing `id`+`source`. No `Read` RPC, no episodic get-by-id, no `memory_read` tool, no `Backend.Read`. Only `engramclient.Client` (and the test fakeBackend) implement `mcp.Backend`, so widening the seam breaks exactly two implementers — both in scope.

## Gaps
1. **Plan file list omits `cmd/engram-server/main.go`**, but the phase Goal says "wired through engramclient → Backend.Read seam → MCP tool" — the server handler needs `svc.Episodic = st` (one line, mirroring `svc.Auditor = st`) or every production episodic read is UNIMPLEMENTED. Treating this as an implied-wiring oversight, not new scope; adding the one line.
2. Plan says the getter mirrors `GetFact` "at internal/store/facts.go:26" — `facts.go` already hosts the episodic `FindByEventID`, so `GetEpisodic` lands there per the plan's file list. No gap.
3. Semantic read needs the TARGET version explicitly (superseded-id edge case), but `AuditResponse` carries only provenance+versions. Solved in the proto design below (`fact` field) without touching `Audit`.

## Code Standards
`docs/code-standards.md` found and applied: errors wrapped `fmt.Errorf("...: %w", err)`, no panics; `context.Context` first param; consumer-defined interfaces, no vendor types in signatures; table-driven tests, ≥1 dirty test; `log/slog`. Note: file is marked "greenfield/intended" but matches actual code.

## Test Infrastructure
- `internal/server`: pure unit tests with fake seams (`fakeAuditor`, `fakeReadAuthz`, `authedCtx` via `authgrpc.WithIdentity`) — extend with a fake `EpisodicReader` + call-recording spies.
- `internal/store`: `httptest` fakeOS (already serves GET `_doc/{id}`) — mirror `TestOpenSearchStoreGetFact`.
- `internal/mcp`: in-process MCP refClient over io.Pipe + `fakeBackend` — add `Read` to the fake, drive `tools/call memory_read`.
- `internal/engramclient`: no test file today; the proto→`mcp.ReadResult` conversion is extracted as a package-level function so it unit-tests without a gRPC conn.
- e2e (`-tags=e2e`) will not run here — expected per dispatch.

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-2.1 | episodic read returns full untruncated text for a Phase-1-surfaced id | COVERED | `TestServerReadEpisodicReturnsFullRecord` (server, >200-rune body), `TestToolsCallMemoryRead_DW_2_1_EpisodicFullText` (MCP, body longer than the Phase-1 snippet cap) |
| DW-2.2 | semantic read returns full fact + provenance/version history via Audit | COVERED | `TestServerReadSemanticDelegatesToAudit` (server: fact/provenance/versions populated), `TestReadResultFromProtoSemantic` (engramclient conversion) |
| DW-2.3 | cross-tenant/unauthorized id → fail-closed NOT_FOUND, no content/existence leak | COVERED | `TestServerReadFailClosed` (table: cross-tenant episodic, cross-tenant semantic, missing identity — all one opaque NotFound, message identical to unknown-id) |
| DW-2.4 | ACL-denying read rejected fail-closed; fetch→authorize→project ordering explicit | COVERED | `TestServerReadFailClosed` (denied-read row via `fakeReadAuthz{allow:false}`), `TestServerReadAuthorizesBeforeProjecting` (spy authorizer captures the acl.Record with pre-projection tenant/team/scope/owner; response asserted to carry NO ACL fields) |
| DW-2.5 | output is structured JSON, `fields` a real object (no stringified fields_json) | COVERED | `TestToolsCallMemoryRead_DW_2_5_StructuredFields` (parse tool text as JSON; `fields.text` is a string inside a real object; no `fields_json` key) |
| DW-2.6 | proto regenerated, `Read` in stubs, build/tests green | COVERED | `make proto` clean + `grep Read api/engrampb` + `go build ./... && go test ./...` + vet + lint (execution evidence in output) |
| DW-2.7 | id/source mismatch or unknown id → NOT_FOUND, no cross-index probing | COVERED | `TestServerReadNoCrossIndexProbe` (semantic id with source=episodic → NotFound AND spy Auditor never called; unknown episodic id → NotFound AND spy Auditor never called; semantic path never calls the episodic reader) |

**All items COVERED:** YES

## Design Decisions

**Trust-boundary map (cc-defensive-programming barricade design):** two external entries — (1) the MCP tool args (agent-supplied JSON) validated in `callRead` (non-empty id/source, source ∈ {episodic, semantic, graph}); (2) the gRPC `Read` RPC (any client can call — "internal team API is still external") re-validates in the handler. Inside the barricade, denial paths are error handling (anticipated runtime conditions), never assertions. Security-critical path → correctness lean: deny on any doubt, one opaque NotFound for every denial so ACL denial is indistinguishable from absence.

**Proto** (`api/proto/engram.proto`): `rpc Read(ReadRequest) returns (ReadResponse)`.
- `ReadRequest{ string id; string source }` — source is `"episodic"|"semantic"`; graph → UNIMPLEMENTED (no drill per research), anything else → INVALID_ARGUMENT.
- `ReadResponse{ string source; EpisodicRecord episodic; FactVersion fact; Provenance provenance; repeated FactVersion versions }` — exactly one branch populated. Reuses `Provenance`/`FactVersion`; new `EpisodicRecord{ id, event_id, kind, text, source_ids, occurred_at, created_at }` deliberately carries NO ACL fields (the projection step) and no embedding/outbox state. `fact` is the target version explicitly, so a superseded semantic id visibly returns THAT immutable version with its closed interval (plan edge case).

**Server** (`internal/server/read.go`): new consumer-defined seam `EpisodicReader{ GetEpisodic(ctx, id) (memory.Episodic, bool, error) }`, new `Server.Episodic` field (nil → UNIMPLEMENTED, mirroring Auditor). Handler dispatches on source; each branch touches exactly ONE index path by construction (DW-2.7's no-cross-probing). Episodic branch mirrors `Audit` line-for-line: validate id → fetch full record → tenant check → `s.ACL.CanRead` on the record's ACL fields → only then project into `EpisodicRecord`. Semantic branch DELEGATES to `s.Audit` (zero duplication of the fail-closed logic — considered re-implementing and rejected: two copies of an ACL check diverge) and pulls the target `fact` out of the returned versions by id.

**Store** (`internal/store/facts.go`): `GetEpisodic` mirrors `GetFact` — realtime `_doc/{id}` GET on the episodic index, 200 → decodeSource into `memory.Episodic`, 404 → ok=false, anything else → wrapped error. Returns the FULL record including ACL fields (fetch precedes authorize; the server is the authorizer, same division of duty as GetFact/AuditFact vs the Audit handler).

**engramclient** (`client.go`): `Read(ctx, id, source) (mcp.ReadResult, error)` calling the RPC, converting via an extracted `readResultFromProto` (pure, unit-testable). Timestamps rendered RFC3339, empty fields omitted.

**MCP** (`mcp.go`, `tools.go`): `Backend.Read(ctx, id, source) (ReadResult, error)`; `ReadResult{ id, source, fields map[string]any, provenance map[string]any (semantic), versions []map[string]any (semantic) }` — `fields` a real object (DW-2.5). New `ToolRead = "memory_read"` schema with required `id`+`source`; `callRead` validates at entry, short-circuits `graph` with a tool error explaining the statement is already whole, and returns `toolResult(ReadResult)` (structured JSON both as text and structuredContent). A full read deliberately spends the caller's context — separate explicit call, never inlined into search.

**Wiring** (`cmd/engram-server/main.go`): `svc.Episodic = st` next to `svc.Auditor = st` (Gap 1).

## Prerequisites
- [x] Phase 1 merged (id+source addressing contract present in compact lines)
- [x] `GetFact`/`AuditFact`/`Audit`/`CanRead` all exist as documented
- [x] buf codegen path exists (`make proto` → pinned `go run`); network/module cache availability verified at run time (BLOCKED if regen cannot execute)
- [x] Test fakes/patterns exist in all three packages

## Recommendation
BUILD — plan matches reality except the one-line `main.go` wiring omission (noted in Gaps, absorbed as implied by the phase Goal). Implement proto → regen → store getter → server handler → engramclient → MCP tool, then the test matrix above.

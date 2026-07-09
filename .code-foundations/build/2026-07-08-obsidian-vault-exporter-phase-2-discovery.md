# Discovery + Design: Phase 2 - Export RPC + server wiring

## Files Found
- `api/proto/engram.proto` — service Engram with 4 RPCs (Ingest/Search/Status/Audit); generated code in `api/engrampb/{engram.pb.go, engram_grpc.pb.go}`
- `internal/server/server.go` — handlers + consumer-defined seams `StatusProbe`/`Auditor`/`ReadAuthorizer` (:28-44), tenant-pin at :131, ACL fail-closed at :182-194, Unimplemented-on-nil-seam at :166
- `internal/graph/store.go` — Phase 1 `ScanEntities`/`ScanEdges` on `Backend`/`Store`/`MemBackend`; opaque `Cursor{after string}` (unexported field); `scanBatchSize = 500` (package-private var)
- `internal/graph/graph.go` — `Entity`/`Edge` records (both carry TenantID/TeamID/Scope/OwnerAgentID provenance → map directly onto `acl.Record`)
- `internal/engramclient/client.go` — `Audit` at :92 is the client-method pattern to mirror; no test file exists yet in this package
- `cmd/engram-server/main.go` — `wireGraph` (:179) already returns `graphStore *graph.Store` into a local var; `svc := server.New(...)` at :270 with seam fields set :271-273
- `internal/authgrpc/interceptor.go` — the unary auth barricade; opaque `Unauthenticated` on every rejection; `Verifier` is a consumer-defined seam (fakeable in tests)
- `internal/server/{server_test.go, audit_test.go}` — fake-seam + `authedCtx(...)` test patterns to reuse

## Current State
Phase 1 delivered the complete scan foundation: cursor-paginated, live- and tenant-filtered
`ScanEntities`/`ScanEdges` with the "zero cursor in = start, zero cursor out = exhausted" contract,
faithfully mirrored between `MemBackend` and the OpenSearch backend (`search_after` + `sort:[{id:asc}]`).
No `Export` RPC, no `Exporter` seam, no client method exists yet. `graphStore` is built in `main.go`
but not threaded into `svc`.

## Gaps
1. **`graph.Cursor` is not serializable outside its package** — `after` is unexported and Phase 1
   shipped no marshal hook. The plan's approach notes require the wire cursor to "carry the Phase 1
   graph `Cursor` state ... track BOTH sub-cursors", which is impossible from `internal/server`
   without graph-package support. **Resolution:** add `MarshalText`/`UnmarshalText` (~10 lines) to
   `graph.Cursor`. This touches `internal/graph/**` (outside the listed file scope) but is the
   minimal enabling change the plan's own pinned wire contract necessitates; documented as a
   deviation, covered by a round-trip unit test.
2. `internal/engramclient` has no test file — DW-2.5 needs one (real TCP listener + real auth
   interceptor + fake Verifier, so "rejected by the existing interceptor" is tested against the
   actual barricade, not a stand-in).
3. `scanBatchSize` (500) is package-private, so server-level pagination tests exercise real page
   boundaries with 500/501-record fixtures rather than shrinking the batch.

## Code Standards
`docs/code-standards.md` applies: wrap errors with `%w`; `context.Context` first param;
consumer-defined interfaces, deep modules; table-driven tests; every phase ships at least one
error-path test; no vendor types in public signatures (graph types are project-internal, matching
`Auditor`'s use of `store.VersionedFact`).

## Test Infrastructure
`internal/server` uses package `server_test` black-box tests with fake seams and
`authgrpc.WithIdentity` contexts (`authedCtx` helper in audit_test.go). `graph.MemBackend`
satisfies the new `Exporter` seam directly (same Scan signatures) — realistic pagination behavior
for free. Integration-tagged tests need a live cluster and are NOT run. `make proto` regenerates;
`make proto-check` is the CI guard.

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-2.1 | `Export` RPC in proto, regenerated code committed, `make proto-check` clean | COVERED | `make proto` + `make proto-check` executed and clean (command evidence); compile of handler/client against generated types |
| DW-2.2 | Handler returns bounded page of live entities+edges for caller's tenant via Phase 1 scan; `next_cursor` advances and empties on exhaustion | COVERED | `TestDW_2_2_ExportPagesToExhaustion` (501 entities + edges, multi-page walk, terminal empty cursor, per-page bound), `TestDW_2_2_ExportEmptyTenantOneTerminalPage`, `TestDW_2_2_ExportPageExactlyOnBoundContinues` |
| DW-2.3 | Tenant pinned from verified identity; tenant A token never receives tenant B records | COVERED | `TestDW_2_3_ExportTenantIsolation` (A+B records seeded; identity A sees only A across ALL pages), `TestDW_2_3_ExportNoIdentityRejected` |
| DW-2.4 | Records failing `ACL.CanRead` omitted; nil `Exporter` → `Unimplemented` | COVERED | `TestDW_2_4_ExportACLDeniedRecordsOmitted`, `TestDW_2_4_ExportACLErrorFailsClosed`, `TestDW_2_4_ExportNilExporterUnimplemented` |
| DW-2.5 | `engramclient.Export` returns page + cursor; unauthenticated call rejected by existing interceptor | COVERED | `TestDW_2_5_ClientExportReturnsPageAndCursor`, `TestDW_2_5_ClientExportUnauthenticatedRejected` (real `authgrpc.UnaryServerInterceptor` over TCP) |

**All items COVERED:** YES
(Count check: 5 DW-IDs in prompt, 5 rows above.)

Beyond-DW tests: garbage cursor → `InvalidArgument` (opaque); stale-but-well-formed cursor stays
inside the caller's tenant; entity/edge field mapping round-trip (aliases, mention_count,
source_ids, predicate, statement, from/to); `graph.Cursor` text round-trip.

## Design Decisions

**D-1 Wire cursor encoding (the explicit design item).** `next_cursor` =
`base64.RawURLEncoding( JSON{"s": stage, "a": <graph.Cursor via TextMarshaler>} )`,
`stage ∈ {"entities","edges"}`. Empty string = start (stage entities, zero sub-cursor) and, on
return, = exhausted. The graph scan advances the two tiers independently; the stage field + one
sub-cursor track both: entities page first, and only after the entity tier exhausts does the
cursor move to stage `edges`. Decode failure or unknown stage → `codes.InvalidArgument`
`"invalid cursor"` (opaque — no internals echoed). The cursor NEVER carries tenant; tenant is
re-pinned from the verified identity on every call, so a stale/forged cursor can only reposition
within the caller's own tenant — no cross-tenant oracle exists by construction.

**D-2 Stage chaining.** When the entity scan exhausts (zero next) inside a call, the handler
immediately runs one edge scan in the same response. Consequences: an empty tenant yields exactly
one empty page with a terminal cursor (plan edge case); the response bound stays <
2×`scanBatchSize` records (a full entity page never chains — full page ⇒ non-zero next), which at
realistic record sizes (~1 KB, embeddings and NameKey excluded) is ≲1 MB, comfortably under the
4 MB gRPC cap → **assumption 1 verified**.

**D-3 `Exporter` seam** (consumer-defined, mirrors `StatusProbe`/`Auditor`):
```go
type Exporter interface {
    ScanEntities(ctx context.Context, tenantID string, cursor graph.Cursor) ([]graph.Entity, graph.Cursor, error)
    ScanEdges(ctx context.Context, tenantID string, cursor graph.Cursor) ([]graph.Edge, graph.Cursor, error)
}
```
Satisfied by `*graph.Store` (production) and `*graph.MemBackend` (tests) with zero adapters.
Dependency arrow: server (transport adapter) → graph (business) — inward, no cycle
(`internal/graph` imports no server/transport code). Wiring is one line in main.go
(`svc.Exporter = graphStore`; `graphStore` already exists at :179) → **assumption 2 verified**.

**D-4 Record shape.** New proto messages `ExportEntity` (id, name, aliases, mention_count,
source_ids, scope, team_id, owner_agent_id, valid_at, created_at) and `ExportEdge` (id, from/to
entity ids, predicate, statement, source_ids, scope, team_id, owner_agent_id, valid_at,
created_at). Deliberately excluded: `Embedding` (internal similarity signal, dominant payload
weight) and `NameKey` (internal lookup key). `tenant_id` omitted per record — it is the caller's
own tenant by construction.

**D-5 Identity handling (defense in depth).** `Export` refuses a context with no verified identity
or an empty tenant (`codes.Unauthenticated`, opaque). Unlike Ingest/Search there is no
request-field fallback — the request carries no tenancy at all, so past-the-barricade bypass
(in-process caller without identity) fails closed instead of scanning tenant `""`. Security-critical
path ⇒ validate again inside the barricade (cc-defensive-programming).

**D-6 ACL policy** (mirrors `Audit` exactly): `s.ACL == nil` → skip scope check (documented
`ReadAuthorizer` contract; production always wires it); `CanRead` error → `codes.Internal`, whole
call aborted, nothing returned (fail-closed on uncertainty); `allowed == false` → record silently
omitted, page continues (plan edge case: skipped, not fatal).

**D-7 Handler placement.** New `internal/server/export.go` holds the seam, cursor codec, handler,
and proto mappers — a single-actor file (the vault-export feature) rather than growing server.go.

**D-8 Client method.** `func (c *Client) Export(ctx context.Context, cursor string) (*engrampb.ExportResponse, error)`
— a thin pass-through mirroring the plan-pinned signature; Phase 3 consumes the proto page directly.

## Prerequisites
- [x] Phase 1 scan primitive present and committed (2f83d51)
- [x] `graphStore` reachable in main.go for wiring
- [x] buf regen via `make proto` (no host protoc needed)
- [x] Test seams (identity injection, MemBackend, fake Verifier) all available without a cluster

## Recommendation
**BUILD.** All 5 DW items coverable. One documented deviation: ~10-line
`MarshalText`/`UnmarshalText` on `graph.Cursor` in `internal/graph` (outside the listed file
scope) — the minimal change the plan's own wire-cursor contract requires; no Phase 1 behavior
altered, covered by a round-trip test.

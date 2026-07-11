# Discovery + Design: Phase 6 - MCP tools + server wiring + budget/spill

## Files Found

- `internal/mcp/mcp.go` — Backend seam with all 6 knowledge methods + MCP DTOs (KnowledgeDoc, Predicate, SortKey, FieldSpec, CollectionSpec, CollectionInfo) — frozen in Phase 1.
- `internal/mcp/tools.go` — 3 memory tool schemas + handlers; `handleToolsCall` dispatch switch; `toolResult`/`toolError` helpers.
- `internal/mcp/budget.go` — `packSearchResult`/`searchResultFits`/`buildSearchResult`/`topFacets`/`refineHint`; memory-shaped `facetFields = ["subject","predicate","kind"]` at line 29.
- `internal/mcp/spill.go` — `spillFullResult` (facet-agnostic; no change needed beyond being in scope).
- `internal/server/server.go` — Server struct + memory handlers; consumer-defined seam precedent (StatusProbe/Auditor, nil ⇒ Unimplemented); error-translation switch precedent.
- `internal/server/knowledge_proto_test.go` — Phase-1 test: 6 RPCs answer `codes.Unimplemented` on a bare Server (must stay green: unwired knowledge deps ⇒ Unimplemented).
- `internal/engramclient/client.go` + `knowledge.go` — real memory calls + Phase-1 knowledge stubs (this phase replaces the stubs); `var _ mcp.Backend = (*Client)(nil)`.
- `internal/knowledgeauth/knowledgeauth.go` — `Authorizer{AuthorizeRead, AuthorizeWrite}`, `ErrForbidden` (unwrapped sentinel).
- `internal/knowledge/knowledge.go` — domain types + `CollectionRegistry` (`ErrNotFound`, `ErrConflict`).
- `internal/store/knowledge.go` — `KnowledgeStore.BulkIndex(ctx, index, textField, docs, harvestID)` / `DeleteByQuery(ctx, index, collection, source, currentHarvestID)`; doc: caller injects `collection`/`source` into each doc's Fields.
- `internal/store/registry.go` — `NewCollectionRegistry` + `WithRegistryMetaIndex`; validation errors are untyped `fmt.Errorf` (only ErrConflict/ErrNotFound are sentinels).
- `internal/retrieval/knowledge.go` — `KnowledgeRetriever.Search(ctx, spec, query, filters, sort, k)` / `Collections(ctx)`; retrieval.Predicate/SortKey/CollectionMeta; clampK to [1,100].
- `api/proto/engram.proto` + `api/engrampb` — all 6 RPC messages generated (Predicate oneof scalar/Range, CollectionSpec/Info, etc.).
- `cmd/engram-server/main.go` — wiring site; `store.Apply` already PUTs the knowledge-collections template.
- `internal/authgrpc/interceptor.go` — `UnaryServerInterceptor(Verifier, ...)`, `IdentityFrom(ctx)`; Verifier is a seam ⇒ integration test can use a token→Identity stub verifier while everything else is real.

## Current State

Phases 1–5 delivered every inner module. Nothing calls them end-to-end yet: the 6 RPCs fall through to `UnimplementedEngramServer`, the MCP server advertises only the 3 memory tools, and `engramclient`'s knowledge methods fail loudly by design. This phase is pure wiring + barricade: no new storage or retrieval logic.

## Gaps

1. **No canonical write-role names exist.** `AuthorizeWrite(id, requiredRole)` takes a role string, but no constant defines the harvester/admin roles. Decision: define `RoleHarvester = "harvester"` and `RoleKnowledgeAdmin = "admin"` in `internal/server/knowledge.go` (the enforcement site). Ingest/delete accept harvester OR admin; create/update require admin (matches the plan's "create_collection by a non-admin → PermissionDenied" edge case and knowledgeauth's test vocabulary).
2. **Registry spec-validation errors are untyped** (`fmt.Errorf`, only ErrConflict/ErrNotFound are sentinels). A malformed collection name on Create maps to `codes.Internal`, not InvalidArgument. Fixing needs a typed error in `internal/store` — outside this phase's file scope. Handler pre-validates the empty-name/empty-spec cases; grammar violations land Internal. Documented trade-off, not a DW item.
3. **Two anchored tests pin the pre-Phase-6 posture and must be superseded by intended behavior**: `TestDW_3_5_ConformanceListTools` asserts exactly 3 tools (now 9); `TestDW_1_2_KnowledgeStubsReturnNotImplemented` asserts the stubs error (they're replaced by real calls — the test's own comment says "before Phase 6 replaces them"). Both are replaced with stronger equivalents, not deleted: the list test grows to 9 and still asserts the 3 memory tools verbatim (DW-6.5); the stub test becomes a real translation test against an in-process fake gRPC server.

## Code Standards

- Transport only at edges (`internal/server`, `internal/authgrpc`, `cmd/`, `engramclient` per importlint); `internal/mcp` stays proto-free — translation happens in engramclient and server.
- Seams are consumer-defined: server declares `KnowledgeWriter`/`KnowledgeReader` interfaces; concrete store/retrieval types satisfy them.
- Errors: `"pkg: verb-ing noun: %w"`; sentinels unwrapped for `errors.Is`; boundary maps sentinels to codes.
- Identity always from `authgrpc.IdentityFrom(ctx)`, never request fields.
- Tests: stdlib `testing` only, table tests, DW-named tests, integration behind `//go:build integration` with scratch indices.

## Test Infrastructure

- `internal/mcp/mcp_test.go`: internal test package with `fakeBackend`+`knowledgeStubs`, `refClient` over io.Pipe (exact wire framing).
- `internal/server/*_test.go`: external package; bufconn precedent in server_integration_test.go; `testutil` (OpenSearchURL, HTTPClient, DeleteIndex, RefreshIndex, ScratchIndexName).
- Budget/spill tests use `t.Setenv(ENGRAM_MCP_SEARCH_BUDGET_BYTES / ENGRAM_MCP_SPILL_DIR)`.

## Assumption Verification (from dispatch)

**"Budget-packer/spill is reusable with only a facet-list change" — HOLDS.** `packSearchResult`/`spillFullResult` are typed on `mcp.Hit`; knowledge hits already arrive as `mcp.Hit` from the Backend. The only memory-shaped element is the package-level `facetFields` var, consumed by `topFacets` and `refineHint` (fixed iteration order) — threading a `facetFields []string` parameter through `packSearchResult → searchResultFits → buildSearchResult → topFacets/refineHint` is a pure signature change; the memory caller passes the same three fields (byte-identical behavior), knowledge passes nil (no facets in v1 — the collection's field vocabulary isn't known at the MCP layer). `spillFullResult` never touches facets. No UPDATE_PLAN needed.

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-6.1 | all 6 MCP tools dispatch through Backend to real store/registry/retriever; knowledge_search budget-packs with overflow_path spill | COVERED | `TestDW_6_1_KnowledgeToolsDispatchThroughBackend` (mcp, per-tool table), `TestDW_6_1_KnowledgeSearchBudgetPackAndSpill` (mcp), `TestDW_6_1_KnowledgeEndToEnd` (integration: real registry/store/retriever through engramclient Backend) |
| DW-6.2 | role-gated read w/o role → PermissionDenied; public read OK for any authenticated caller | COVERED | `TestDW_6_2_ReadAuthorization` (server unit, table: gated-no-role, gated-with-role, public-authenticated, no-identity), integration denial path in `TestDW_6_2_6_3_AuthDenialsEndToEnd` |
| DW-6.3 | ingest/delete/create/update w/o harvester/admin role → PermissionDenied | COVERED | `TestDW_6_3_WriteAuthorization` (server unit, table over 4 RPCs × roles incl. harvester-cannot-create), integration `TestDW_6_2_6_3_AuthDenialsEndToEnd` |
| DW-6.4 | knowledge_collections reports count + staleness; malformed filter → InvalidArgument naming valid fields | COVERED | `TestDW_6_4_CollectionsCountAndStaleness` (server unit), `TestDW_6_4_MalformedFilterNamesValidFields` (server unit: unknown field, bad op, empty range, bad sort), staleness surfaced through MCP in `TestDW_6_4_KnowledgeCollectionsToolSurfacesStaleness`, integration list assertions |
| DW-6.5 | memory MCP tools + gRPC RPCs behave identically (regression) | COVERED | existing memory suites unchanged and green (`mcp_test.go` conformance incl. 3 memory tools verbatim, `budget_test.go`/`spill_test.go` re-anchored on `memoryFacetFields`, `server_test.go`); `TestDW_6_5_MemoryToolsUnchanged` pins memory tool names+schemas still advertised |

**All items COVERED: YES**

## Design Decisions

### Design: knowledge gRPC edge (server seams + barricade)

#### Approaches Considered
1. **A — concrete fields**: Server holds `*store.CollectionRegistry`, `*store.KnowledgeStore`, `*retrieval.KnowledgeRetriever` directly.
2. **B — consumer-defined seams**: Server declares `KnowledgeWriter` (BulkIndex/DeleteByQuery) and `KnowledgeReader` (Search/Collections) interfaces + reuses `knowledge.CollectionRegistry`; concrete types satisfy them structurally. Nil ⇒ Unimplemented (Auditor precedent).
3. **C — facade service**: a `knowledgeService` struct wrapping registry+store+retriever+authorizer behind domain methods; handlers delegate.

#### Comparison
| Criterion | A | B | C |
|-----------|---|---|---|
| Interface simplicity | fields only | 2 small interfaces (2 methods each) | +1 wrapper type, pass-through methods |
| Information hiding | leaks concrete OpenSearch types into the edge | hides implementations; house precedent (StatusProbe) | hides, but duplicates the handler layer |
| Caller ease of use (tests) | needs live OpenSearch for every unit test | trivial fakes | trivial fakes but more plumbing |
| Consistency with codebase | violates "seams are consumer-defined" | exact match (code-standards §4) | no precedent; aposd pass-through red flag |

#### Choice: B
Rationale: matches the enforced house rule, unit-testable without a cluster, no pass-through layer. Sacrifice: two more small interfaces in server.go's family — acceptable, they're 2 methods each.

#### Depth Check
- Interface methods: KnowledgeWriter 2, KnowledgeReader 2, registry reused.
- Hidden details: OpenSearch mechanics, index/alias names (registry-internal), bulk NDJSON, aggregations.
- Common case complexity: simple — handler = identity → authorize → validate → one seam call → translate.

### Barricade layout (cc-defensive)

- **Order in every write handler**: authorize (AuthorizeWrite; harvester∨admin for ingest/delete, admin for create/update) → validate shape (non-empty collection/source/harvest_id/doc ids) → resolve collection (`ErrNotFound` → InvalidArgument naming it) → seam call. Authorization first: an unauthorized caller learns nothing about arguments or existence.
- **Order in read handlers**: resolve collection first (auth needs its policy), unknown → InvalidArgument naming it (plan explicitly accepts existence naming); then AuthorizeRead → PermissionDenied (opaque message). Collections filters per-collection by AuthorizeRead, silently skipping unauthorized entries (leak-free listing).
- **Filter/sort validation lives in the handler** (field ∈ Mappings ∧ Filterable/Sortable, op ∈ enum, range has ≥1 bound, order ∈ {asc,desc}) because DW-6.4 requires InvalidArgument with valid-field lists and the proto→retrieval translation must walk these structures anyway. The retriever revalidates — deliberate defense-in-depth (barricade + inner check on a security-adjacent path), NOT accidental duplication; retriever errors after a passed barricade map to Internal.
- **Error map**: `knowledgeauth.ErrForbidden`→PermissionDenied (opaque), `knowledge.ErrConflict`→AlreadyExists, `knowledge.ErrNotFound`→NotFound (update) / InvalidArgument-naming (resolve-by-request-field), validation→InvalidArgument (self-correcting field lists), rest→Internal.
- Identity exclusively from `authgrpc.IdentityFrom(ctx)`; invalid/absent identity fails closed inside knowledgeauth.

### Budget/spill parametrization

`facetFields` becomes a parameter threaded through pack/build/facet/hint functions; the package var is renamed `memoryFacetFields` and passed by `callSearch` (byte-identical memory behavior); `callKnowledgeSearch` passes nil (omitted count + generic hint, no facets). Shared `packAndSpill(hits, facetFields, toolName)` helper hosts the pack→spill→overflow_path sequence for both tools; spill failure degrades to capped-response-without-overflow_path (existing rule). Same `ENGRAM_MCP_SEARCH_BUDGET_BYTES` budget governs both tools (documented).

### Ingest translation

Handler copies each proto doc's `fields` (structpb → map), injects `collection` and `source` (per the Phase-4 contract: batch-level concerns injected by the Phase-6 caller — required for DeleteByQuery's term filters), builds `knowledge.Document`, and calls `BulkIndex(ctx, spec.Index, spec.TextField, docs, harvestID)` — spec.TextField, never a literal `"text"`.

### engramclient

Replaces the 6 stubs with real `engrampb` calls + pure translation helpers (mcp DTO ↔ proto: op string ↔ enum, scalar/range ↔ oneof via structpb, CollectionSpec flatten/unflatten, Timestamps ↔ *time.Time). Client-side op/value shape errors return descriptive errors before dialing (cheap self-correction), server remains the authority.

### MCP tool schemas

6 new tools in tools.go (`knowledge_ingest`, `knowledge_search`, `knowledge_collections`, `knowledge_delete`, `knowledge_create_collection`, `knowledge_update_collection`), constants `ToolKnowledge*`. Handlers validate required strings and arg JSON shape (protocol barricade), then delegate to Backend; backend/gRPC errors surface as `toolError` text so gRPC's self-correcting messages reach the LLM. `knowledge_search` defaults k like memory (defaultRequestK) and budget-packs + spills.

## Prerequisites

- [x] All Phase 1–5 modules present and frozen (verified by reading each seam above)
- [x] Live-OpenSearch harness available (`make e2e` cluster; integration tag pattern)
- [x] `store.Apply` provisions the knowledge-collections meta template (Phase 3)

## Recommendation

**BUILD** — wire 6 gRPC handlers (internal/server/knowledge.go) + Server seams, 6 MCP tools (tools.go) with parametrized budget/spill, real engramclient calls, main.go construction; unit tests for the barricade (server + mcp) and one live end-to-end integration test through the Backend with real registry/store/retriever + auth-denial paths.

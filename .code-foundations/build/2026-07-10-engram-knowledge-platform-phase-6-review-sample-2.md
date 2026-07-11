# Review: Phase 6 - MCP tools + server wiring (sample 2)

## Executed Results (Step 0)
- Build: `go build ./...` → success (EXIT=0)
- Test suite: `make test` → all packages ok (EXIT=0); re-verified uncached with `command go test -count=1 ./...` → 807 passed, 43 packages (EXIT=0)
- Focused unit run: `command go test -count=1 -v ./internal/server/ ./internal/mcp/ ./internal/engramclient/` → 72 PASS, 0 FAIL, 0 SKIP (EXIT=0, raw log `/tmp/p6-review-sample-2/unit-v.log`)
- Typecheck: covered by `go build` + `go vet` (in `make lint`) → clean
- Lint: `make lint` (go vet + revive) → clean (EXIT=0)
- Integration (live OpenSearch @ localhost:9200): `ENGRAM_OPENSEARCH_URL=http://localhost:9200 command go test -tags=integration -count=1 ./internal/server/ ./internal/mcp/ ./internal/store/ ./internal/retrieval/` → all 4 packages ok (EXIT=0). Verbose re-runs confirmed `TestDW_6_1_KnowledgeEndToEnd` (0.14s) and `TestDW_6_2_6_3_AuthDenialsEndToEnd` (0.11s) executed against the cluster, not skipped.

## Requirement Fulfillment

### DW-6.1
PREMISE:  all 6 MCP tools (ingest/search/collections/delete/create_collection/update_collection) dispatch through `Backend` to the real store/registry/retriever; `knowledge_search` returns budget-packed hits with `overflow_path` spill on overflow.
EVIDENCE: internal/mcp/tools.go:200-221 (dispatch switch), 300-408 (six handlers calling `s.backend.Knowledge*`/`CreateCollection`/`UpdateCollection`), 273-283 (`packAndSpill` shared verbatim with memory_search); internal/engramclient/knowledge.go:24-125 (real gRPC Backend adapter); internal/server/knowledge.go:103-293 (handlers into registry/store/retriever); cmd/engram-server/main.go:279-282 (real seams wired).
TRACE:    tools/call `knowledge_search {collection, query, filters, sort, k}` → callKnowledgeSearch (tools.go:320) → backend.KnowledgeSearch → engramclient → gRPC KnowledgeSearch → registry.Get → retriever.Search → hits → packAndSpill(hits, nil, ...) → budget-packed page; on overflow, full set spilled and `overflow_path` attached. Executed: `TestDW_6_1_KnowledgeToolsDispatchThroughBackend` (all six subtests PASS), `TestDW_6_1_KnowledgeSearchBudgetPackAndSpill` (600-byte budget → shrunken page, omitted count, overflow_path file holds all 20 hits in order) PASS, and live end-to-end `TestDW_6_1_KnowledgeEndToEnd` (create → ingest → filtered/sorted search → collections → sweep → live update) PASS.
VERDICT:  PASS

### DW-6.2
PREMISE:  read of a role-gated collection WITHOUT the required role → `PermissionDenied`; a public collection read succeeds for any authenticated caller.
EVIDENCE: internal/server/knowledge.go:157-160 (AuthorizeRead with identity from ctx only); internal/server/knowledge_test.go:138-161; internal/server/knowledge_integration_test.go:304-330.
TRACE:    identity{roles:["reader"]} + KnowledgeSearch on collection gated to "curator" → AuthorizeRead returns ErrForbidden → codes.PermissionDenied; same call with "curator" → OK; public collection + roleless authenticated identity → OK; no identity in ctx → denied even for public (fail closed). Executed: `TestDW_6_2_ReadAuthorization` (4 cases) PASS; live `TestDW_6_2_6_3_AuthDenialsEndToEnd` (reader denied, curator admitted, gated collection invisible in reader's listing) PASS.
VERDICT:  PASS

### DW-6.3
PREMISE:  `knowledge_ingest`/`_delete`/`_create_collection`/`_update_collection` WITHOUT the harvester/admin role → `PermissionDenied`.
EVIDENCE: internal/server/knowledge.go:91-99 (authorizeKnowledgeWrite), 107, 230, 253, 275 (ingest/delete require harvester|admin; create/update admin-only, checked BEFORE argument validation); internal/server/knowledge_test.go:167-227; internal/server/knowledge_integration_test.go:332-346.
TRACE:    identity{roles:[]} or {roles:["reader"]} → all four writes → PermissionDenied; harvester → ingest/delete OK but create/update PermissionDenied; admin → all four OK. Executed: `TestDW_6_3_WriteAuthorization` (4 role tiers × 4 ops) PASS; live `TestDW_6_2_6_3_AuthDenialsEndToEnd` (reader/curator denied writes, harvester denied create/update, unknown token → Unauthenticated at the interceptor) PASS.
VERDICT:  PASS

### DW-6.4
PREMISE:  `knowledge_collections` reports count + staleness; a malformed filter yields `InvalidArgument` naming valid fields.
EVIDENCE: internal/server/knowledge.go:191-222 (count + NewestHarvestedAt/NewestDocDate per readable collection), 301-344 (predicatesFromProto → InvalidArgument naming valid filterable fields/ops), 348-372 (sort validation), 376-388 (fieldList); internal/mcp/tools.go:347-355; tests knowledge_test.go:232-281, 424-463; knowledge_tools_test.go:197-224.
TRACE:    filter {field:"yr"} on papers{year,category filterable} → InvalidArgument "valid filterable fields: category, year"; collections listing → count=42 + both staleness timestamps surfaced through proto and MCP tool JSON (`newest_harvested_at`, `newest_doc_date`). Executed: `TestDW_6_4_MalformedFilterNamesValidFields` (7 malformed shapes) PASS, `TestDW_6_4_CollectionsCountAndStaleness` PASS, `TestDW_6_4_KnowledgeCollectionsToolSurfacesStaleness` PASS; live assertions inside `TestDW_6_1_KnowledgeEndToEnd` (count=3, recent NewestHarvestedAt, NewestDocDate=2026-06-01, self-correcting unknown-field error) PASS.
VERDICT:  PASS

### DW-6.5
PREMISE:  memory MCP tools + gRPC RPCs behave identically (regression) — the memory path is behaviorally untouched.
EVIDENCE: internal/mcp/knowledge_tools_test.go:331-349 (`TestDW_6_5_MemoryToolsUnchanged`: memory ingest/search/status through the knowledge-capable backend); internal/mcp/mcp_test.go conformance suite; internal/server/server.go:88-272 (memory handlers).
TRACE:    memory_ingest {event_id:"e1"} → "ep-e1"; memory_search "alpha" → 1 episodic hit with memory facet packing; memory_status → healthy. gRPC memory path: pre-existing `TestIngest*` (4), `TestSearch*` (3), audit (2), export (16), spill (6), budget (10), and MCP conformance (`TestDW_3_5_Conformance*`, `TestCallToolValidationIsToolError`) all PASS unmodified in the same run — the full 72-test unit sweep and the 807-test repo sweep are green.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have corresponding tests that ran in Step 0 (DW-ID-named: TestDW_6_1_* ×3, TestDW_6_2_ReadAuthorization, TestDW_6_3_WriteAuthorization, TestDW_6_2_6_3_AuthDenialsEndToEnd, TestDW_6_4_* ×3, TestDW_6_5_MemoryToolsUnchanged)
- [x] Coverage matches the stated 100% level: every handler, every denial branch, every prompt-listed edge case has an executed test at unit AND (for the lifecycle + auth paths) live-cluster integration level.

## Dead Code
None found. No debug statements, TODO/FIXME markers, commented-out blocks, or unreachable code in the six implementation files; build and revive lint are clean (Go rejects unused imports at compile time).

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | Server seams (Registry/Writer/Reader/KnowledgeAuth) are set once before Serve and read-only in handlers; toolSchemas builds fresh maps per call (tools.go:32-34); no shared mutable state in tool handlers. No defect demonstrable. |
| Error Handling | PASS | Every backend error mapped: ErrNotFound→InvalidArgument-naming/NotFound, ErrConflict→AlreadyExists, ErrForbidden→opaque PermissionDenied, infra→Internal (knowledge.go:73-99, 260-291); spill failure degrades without failing the search (tools.go:276-278, `TestDW_3_4_UnwritableSpillDirDegradesGracefully` PASS); backend errors surface verbatim as isError tool results (`TestKnowledgeToolBackendErrorsSurfaceAsToolErrors` PASS). |
| Resources | PASS | gRPC clients closed via t.Cleanup in integration tests; spill files 0600 in a dedicated dir (Phase-3 contract, `TestDW_3_3` PASS); no handles opened in the Phase-6 handlers themselves. |
| Boundaries | PASS | Traced adversarial k: MCP k≤0 → defaultRequestK (tools.go:334-337); huge k wrapping negative through int32 → clampK (retrieval/opensearch.go:57-65) → k≤0→DefaultK, k>MaxK→MaxK — bounded, tested (`retrieval knowledge_test.go:380`). Zero hits → empty non-nil page (`TestPackSearchResultZeroHits` PASS); single over-budget hit still emitted (`TestDW_2_4` PASS); doc with nil Fields gets a fresh map before provenance injection (knowledge.go:125-128). |
| Security | PASS | CRITICAL check verified: no knowledge request message carries caller identity/roles (api/proto/engram.proto:330-420 — the only `roles` field is AccessPolicy, the admin-set policy object, reachable only AFTER admin authorization); handlers take identity exclusively from `authgrpc.IdentityFrom(ctx)` (knowledge.go:92, 157, 199), injected only by the token-verifying interceptor (authgrpc/interceptor.go:47-72, wired with no exempt methods in main.go:265-268) — a caller structurally cannot self-elevate. Authorizer fails closed on absent identity, empty policy, blank roles (knowledgeauth tests + `unauthenticated caller is denied even when public` PASS). Write auth runs BEFORE argument validation (unauthorized caller learns nothing). Spoofed `fields.collection`/`fields.source` in an ingested doc are unconditionally overwritten server-side (knowledge.go:129-130). Denials are opaque (no role oracle); unknown token → opaque Unauthenticated (live-tested). Barricade validates collection (resolveCollection), filters/sort (against declared mappings), and doc ids at entry; k is bounded by the retriever's clamp per the pinned Phase-5 contract. |

## Loaded-Skill Criteria
N/A — no skills loaded (dispatch prompt had no `## Additional Skills` block).

## Edge cases (prompt-listed) — all handled
| Edge case | Evidence (executed) |
|---|---|
| unknown collection → InvalidArgument naming it | `TestUnknownCollectionIsInvalidArgumentNamingIt` PASS + live (`it6ghost` named) |
| unauthorized read of role-gated collection → PermissionDenied | `TestDW_6_2_ReadAuthorization` + live PASS |
| oversized result → budget-pack + overflow_path spill | `TestDW_6_1_KnowledgeSearchBudgetPackAndSpill` PASS (spill file verified byte-for-byte) |
| malformed predicate → self-correcting error naming valid fields | `TestDW_6_4_MalformedFilterNamesValidFields` (7 shapes) + live PASS |
| create_collection by non-admin → PermissionDenied | `TestDW_6_3_WriteAuthorization` + live (reader AND harvester denied) PASS |
| identity/roles only from verified token; barricade validates input at entry | See Security row — structural (no request field exists) + fail-closed tests PASS |

## Notes (non-blocking)
- **Existence oracle on gated reads (documented trade):** `KnowledgeSearch` resolves the collection (knowledge.go:153) before the read-auth check (:158), so an authenticated caller without the role can distinguish "unknown collection" from "exists but gated". The file header records this as a plan-pinned usability trade; the listing path stays leak-free, and both prompt-listed edge cases demand exactly these two codes, so this is consistent with the requirements as written.
- **Empty `docs` array on knowledge_ingest** passes both barricades (only collection/source/harvest_id presence is checked) and reaches `BulkIndex` with zero docs; behavior then depends on the Phase-4 store contract. Worst case is an Internal/tool error, not a crash. Not a prompt-listed edge case.
- `toolResult` (tools.go:413) discards the `json.Marshal` error; all current payloads are trivially marshalable, so no failure is demonstrable.
- Memory `Ingest` gRPC (server.go:101-104) falls back to client-supplied tenant/owner when NO identity is in ctx — pre-existing memory-path behavior for interceptor-less in-process tests, unreachable in production (interceptor rejects before the handler) and outside this phase's diff.

**Verdict: PASS**

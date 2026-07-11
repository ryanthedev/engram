# Review: Phase 6 - MCP tools + server wiring (sample 1)

## Executed Results (Step 0)
- Build: `go build ./...` → success, EXIT=0
- Test suite: `make test` → all packages ok, EXIT=0; forced fresh: `go test -count=1 ./internal/server/ ./internal/mcp/ ./internal/engramclient/ ./internal/knowledgeauth/` → **157 passed, EXIT=0**; verbose raw run (`rtk proxy go test -count=1 -v`, /tmp/p6-review-sample-1/unit-v.log) → **136 named tests `--- PASS`, 0 `--- FAIL`**
- Typecheck: `go vet ./...` (via make lint) → clean, EXIT=0
- Lint: `make lint` (vet + revive) → clean, EXIT=0
- Integration (live OpenSearch @ localhost:9200): `ENGRAM_OPENSEARCH_URL=http://localhost:9200 go test -tags=integration -count=1 ./internal/server/ ./internal/mcp/ ./internal/store/ ./internal/retrieval/` → **308 passed in 4 packages, EXIT=0**; verbose raw re-run of the two Phase-6 e2e tests (/tmp/p6-review-sample-1/it6.log) → `--- PASS: TestDW_6_1_KnowledgeEndToEnd (0.15s)`, `--- PASS: TestDW_6_2_6_3_AuthDenialsEndToEnd (0.09s)`, EXIT=0

## Requirement Fulfillment

### DW-6.1
PREMISE:  all 6 MCP tools (ingest/search/collections/delete/create_collection/update_collection) dispatch through `Backend` to the real store/registry/retriever; `knowledge_search` returns budget-packed hits with `overflow_path` spill on overflow.
EVIDENCE: internal/mcp/tools.go:195-222 (dispatch switch), 300-408 (six handlers calling s.backend.Knowledge*/CreateCollection/UpdateCollection); tools.go:273-283 + 344 (packAndSpill shared with memory_search, overflow_path attached on spill); internal/engramclient/knowledge.go:24-125 (Backend → real gRPC); internal/server/knowledge.go:103-293 (handlers → Registry/KnowledgeWriter/KnowledgeReader); cmd/engram-server/main.go:279-282 (real store.NewCollectionRegistry / store.NewKnowledgeStore / retrieval.NewKnowledgeRetriever wired).
TRACE:    tools/call knowledge_search{collection:"papers", 20 fat hits, budget=600B} → callKnowledgeSearch → backend.KnowledgeSearch → packAndSpill → shrunken page + omitted count + overflow_path; spill file re-read contains all 20 hits in rank order. E2e: engramclient → auth interceptor → barricade → live registry/store/retriever → OpenSearch: create → dup=AlreadyExists → ingest 3 → filtered+sorted search [d2,d1] → range filter → collections count=3+staleness → sweep deletes 2 → live mapping update filterable.
VERDICT:  **PASS** — TestDW_6_1_KnowledgeToolsDispatchThroughBackend, TestDW_6_1_KnowledgeSearchBudgetPackAndSpill (unit-v.log), TestDW_6_1_KnowledgeEndToEnd (it6.log), all PASS.

### DW-6.2
PREMISE:  read of a role-gated collection WITHOUT the required role → `PermissionDenied`; a public collection read succeeds for any authenticated caller.
EVIDENCE: internal/server/knowledge.go:157-160 (AuthorizeRead(id, spec.Access.Public, spec.Access.Roles) → PermissionDenied); internal/knowledgeauth/knowledgeauth.go:45-58 (fail-closed: unauthenticated denied even when public).
TRACE:    identity{roles:["reader"]} + gated{roles:["curator"]} → AuthorizeRead → no role match → ErrForbidden → codes.PermissionDenied. identity{no roles} + public → nil → search proceeds. ctx with no identity → id.Valid()=false → denied even for public.
VERDICT:  **PASS** — TestDW_6_2_ReadAuthorization (4 subcases incl. unauthenticated-on-public), TestDW_6_2_6_3_AuthDenialsEndToEnd (reader denied on gated, curator admitted, gated collection invisible in reader's listing), all PASS.

### DW-6.3
PREMISE:  `knowledge_ingest`/`_delete`/`_create_collection`/`_update_collection` WITHOUT the harvester/admin role → `PermissionDenied`.
EVIDENCE: internal/server/knowledge.go:91-99 (authorizeKnowledgeWrite), 107/230 (ingest/delete: harvester|admin), 253/275 (create/update: admin only) — auth checked BEFORE argument validation.
TRACE:    identity{roles:nil or ["reader"]} → AuthorizeWrite fails for every role → PermissionDenied on all four ops. harvester → ingest/delete OK, create/update PermissionDenied. admin → all four OK.
VERDICT:  **PASS** — TestDW_6_3_WriteAuthorization (4 role matrices × 4 ops), TestDW_6_2_6_3_AuthDenialsEndToEnd (live: reader/curator ingest denied, harvester create/update denied, unknown token → Unauthenticated at interceptor), all PASS.

### DW-6.4
PREMISE:  `knowledge_collections` reports count + staleness; a malformed filter yields `InvalidArgument` naming valid fields.
EVIDENCE: internal/server/knowledge.go:191-222 (Count + NewestHarvestedAt + NewestDocDate per readable collection); knowledge.go:301-344 (predicatesFromProto: InvalidArgument naming valid filterable fields/ops), 348-372 (sort equivalents); internal/mcp/tools.go:347-355 (tool surfaces infos incl. count/staleness).
TRACE:    metas{pub: count=42, harvested, docDate} → response CollectionInfo{count=42, both timestamps}; filter{field:"yr"} on papers → `unknown or unfilterable field "yr" ... valid filterable fields: category, year` with codes.InvalidArgument; live: `nope` → error names "category, published".
VERDICT:  **PASS** — TestDW_6_4_CollectionsCountAndStaleness, TestDW_6_4_MalformedFilterNamesValidFields (7 malformed shapes), TestDW_6_4_KnowledgeCollectionsToolSurfacesStaleness, live assertions in TestDW_6_1_KnowledgeEndToEnd (count=3, NewestHarvestedAt recent, NewestDocDate=2026-06-01), all PASS.

### DW-6.5
PREMISE:  memory MCP tools + gRPC RPCs behave identically (regression) — the memory path is behaviorally untouched.
EVIDENCE: `git diff HEAD -- internal/server/server.go` → +10 lines: imports + Knowledge seam struct fields only, zero memory-handler lines changed; internal/mcp/budget.go diff → pure parameterization (facetFields param; memory path passes memoryFacetFields, same values/order as before).
TRACE:    memory_ingest e1 → ep-e1; memory_search "alpha" → 1 episodic hit; memory_status healthy=true — through the knowledge-capable fake. tools/list advertises exactly 9 tools (3 memory verbatim + 6 knowledge). gRPC: TestIngest* (4), TestSearchMapsQueryFilterAndHits, Audit ×2, Export ×15, DW-2.x budget ×7, DW-3.x spill ×6 — all pre-existing memory tests pass unchanged.
VERDICT:  **PASS** — TestDW_6_5_MemoryToolsUnchanged, TestDW_3_5_ConformanceListTools/Initialize/CallTool + full memory server/mcp suites, 0 failures in unit-v.log.

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have corresponding automated tests, run fresh in Step 0 (test names carry DW-IDs: TestDW_6_1..TestDW_6_5)
- [x] Coverage matches the stated 100% level: every handler branch (auth deny/allow, unknown collection, each malformed filter/sort shape, sentinel→code mapping, spill overflow, DTO translation both directions, live e2e lifecycle) has a passing named test
- No gaps.

## Dead Code
None found. No TODO/FIXME/debug prints/commented-out blocks in the six implementation files; all imports used.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | Handlers are stateless; Server seams written once at wiring (main.go:279-282) before Serve; toolSchemas builds fresh maps per call (tools.go:32-34) — probed for shared-mutable-map hazard, none. |
| Error Handling | PASS | Every seam error mapped (ErrNotFound→InvalidArgument-naming/NotFound, ErrConflict→AlreadyExists, ErrForbidden→PermissionDenied opaque, infra→Internal); spill failure degrades to capped page without overflow_path (tools.go:276-281, TestDW_3_4 passes); backend errors surface as MCP isError, never protocol errors (TestKnowledgeToolBackendErrorsSurfaceAsToolErrors). |
| Resources | PASS | Spill files 0600 (TestDW_3_3); no handles opened in reviewed code; integration stack cleans indices + closes clients via t.Cleanup. |
| Boundaries | PASS | Traced: doc with nil Fields → fields==nil → make (knowledge.go:126-128, tested "no fields doc"); empty docs slice → indexed 0; k<=0 → defaultRequestK at MCP edge, retriever clamp behind (given Phase-5 contract); single over-budget hit floor holds (TestDW_2_4 ×2); empty filters/sort → nil fast path. |
| Security | PASS | See CRITICAL check below. Denials opaque (no role oracle, knowledge.go:98,159); unauthorized collections invisible in listings (knowledge.go:209-211, verified live); provenance spoof blocked: caller-supplied fields["collection"]/["source"] overwritten by resolved spec.Name/req.Source (knowledge.go:129-130) — a harvester cannot aim a sweep at another collection via doc fields. |

**CRITICAL security check (identity/roles from token only):** CONFIRMED.
- Interceptor: internal/authgrpc/interceptor.go:59-70 — bearer token extracted from metadata, `v.Verify(ctx, raw)` produces the Identity, `WithIdentity(ctx, id)` injects it; missing/invalid token → Unauthenticated before any handler (verified live: unknown token → codes.Unauthenticated, TestDW_6_2_6_3).
- Handlers read identity ONLY via `authgrpc.IdentityFrom(ctx)` (knowledge.go:92,157,199); ctx key is a private type (auth.go identityCtxKey) — unforgeable from outside.
- Proto audit: no knowledge request message carries caller identity/role fields. The only `roles` field is `AccessPolicy.roles` (engram.proto:314) — the collection's stored read policy, settable only through admin-gated Create/UpdateCollection; reads authorize against the REGISTRY's stored spec (knowledge.go:158), never request data. Self-elevation via request fields is structurally impossible.
- Absent identity fails closed: zero Identity → `id.Valid()`=false → ErrForbidden (knowledgeauth.go:46-47,67), tested ("unauthenticated caller is denied even when public").
- Barricade validates external input at entry before inner seams: collection required+existence (knowledge.go:73-85), filters against declared+Filterable mappings, sort against Sortable, op/value shape, doc ids, source/harvest_id presence — all before KnowledgeWriter/KnowledgeReader calls; write auth precedes even argument validation.

## Loaded-Skill Criteria
N/A — no skills loaded (dispatch had no `## Additional Skills` block).

## Notes (non-blocking)
- Collection-existence oracle: KnowledgeSearch resolves the collection (naming an unknown one) BEFORE the read-auth check, so an authenticated role-less caller can distinguish "collection exists but is gated" (PermissionDenied) from "does not exist" (InvalidArgument naming it). This is forced by the requirements themselves (both behaviors are mandated DW/edge items) and the code comment records the plan pinning this usability-over-opacity trade (knowledge.go:11-14, 70-72). Not a defect against this phase's spec.
- `knowledge_ingest` accepts an empty `docs` array (indexed=0) although the MCP schema lists `docs` as required; the gRPC path likewise allows zero docs. Harmless no-op; no requirement forbids it.
- No unit test exercises the gRPC `Status` RPC handler directly (memory path); pre-existing situation — the handler is untouched by this phase's diff and memory_status is covered at the MCP layer.
- `make test` initially reported all-cached; fresh `-count=1` and raw `rtk proxy go test -v` runs were used for all verdicts above.

## Issues (if FAIL)
None.

**Verdict: PASS**

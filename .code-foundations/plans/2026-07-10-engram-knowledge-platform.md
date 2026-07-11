# Plan: Engram Knowledge Platform (Plan 1 of 2)

**Created:** 2026-07-10
**Status:** in-progress
**Started:** 2026-07-10 21:16
**Current Phase:** 3
**Complexity:** complex
---
## Context

Engram is memory-only; arXiv's live API is fragile under paper-grabber's discovery fan-out (per-process rate limiter → HTTP 429 → arXiv-empty runs). Extend engram into a **knowledge platform**: a second, document-oriented collection type (BM25/FTS, no fact-extraction, no embeddings) serving bulk-ingested corpora over a generic, runtime-managed collection registry.

**This is Plan 1 of 2.** It builds the engram-side platform (registry, store, retriever, 6 RPCs, RBAC). The **dedicated harvester tool** (arXiv Kaggle backfill + nightly OAI-PMH, github-repos, web-crawl, mark-and-sweep orchestration) is **Plan 2**, built against the API this plan ships — it cannot build before the API is real. Source of intent: `.code-foundations/research/2026-07-09-engram-knowledge-collection.md` (confirmed).

## Constraints
- **Memory path unchanged behaviorally** (code/API/reconciliation semantics). Transient resource contention during bulk ops is acceptable — no hard isolation.
- **FTS/BM25 only in v1**; embeddings + chunking deferred as an additive layer (no re-harvest). The knowledge retriever never calls the embedder.
- **Knowledge writes intentionally deviate from house style**: upsert-by-id + hard `_delete_by_query` (mark-and-sweep), unlike engram's append-only `op_type=create` / `invalid_at` memory writes. A builder must NOT "fix" this back to append-only.
- **Never-restart (Option A)**: collection registry source of truth is an OpenSearch `knowledge-collections` meta-index; create/update are runtime REST ops. YAML is an optional idempotent boot seed only.
- **RBAC = true role dimension (Approach B)**: `roles` claim on `auth.Identity` + token; per-collection `public` or `roles:[…]` read policy; writes require a harvester/admin role. No role concept exists today (auth is scope-only) — this is net-new.
- **Reuse existing infra**: `buildQuery` free function (`retrieval/opensearch.go:499`, add a sort block), the memory_search budget-packer + `overflow_path` spill (`mcp/budget.go`,`spill.go`), the index-template/apply idiom (`store/templates.go`,`apply.go`), raw `net/http` + `map[string]any` via `doJSON` (no vendor OS SDK), stdlib `testing` (no testify).
- **No PDF / full-text ever.** (No harvest happens in this plan; enforced in Plan 2's harvesters. This plan only stores whatever `text` a client sends.)

---
## Chosen Approach
**B — Parallel knowledge stack with a true role dimension.** A separate `KnowledgeStore` + `KnowledgeRetriever` + `knowledge_*` RPCs (no RRF fusion with memory, respecting the actor boundary: knowledge search ≠ memory reconciliation), plus a genuine `roles` claim added to `auth.Identity` and token issuance, enforced per-collection at the request barricade. **Rationale:** greenfield (nothing consumes this yet), so best-in-class from day one avoids a later token/identity migration; named roles decouple access from team membership. **Fallback:** Approach A (map "roles" onto existing team/scope grants via the ACL enforcer) if the role dimension proves too invasive to token issuance.

## Rejected Approaches
- **A — Reuse ACL scope machinery:** lower net-new surface, but "roles" = existing team membership, not standalone named roles → tech debt the user explicitly rejected. Kept as fallback.
- **C — Extend memory `MultiRetriever`/`Store` as a knowledge tier:** `filterClauses` hardcodes memory fields, forces unwanted RRF fusion, couples two actors (SRP violation), and grows every memory-path fake. Structurally worst.

---
## Implementation Phases

### Phase 1: Proto contract & Backend seam
**Model:** fable
**Skills:** aposd-designing-deep-modules
**Gate:** Full
**Depends on:** none | **Unlocks:** Phase 6
**File scope:** `api/proto/**, api/engrampb/**, internal/mcp/mcp.go, internal/engramclient/**, internal/mcp/*_test.go, buf.gen.yaml`

**Goal:** Define the wire + backend contract for all six knowledge operations so every downstream phase implements against a fixed interface.

**Constraints (added):** extending `mcp.Backend` breaks its implementers — the real gRPC adapter (`internal/engramclient/client.go`) and `fakeBackend` (`internal/mcp/mcp_test.go`) — so this phase adds **stub** implementations to both to keep the repo compiling (DW-1.2). P6 replaces the `engramclient` stubs with real gRPC calls. Filter `Value` representation is decided: a proto `oneof { google.protobuf.Value scalar; Range range }` with `Range{ Value gte; Value lte }` — engram.proto has no structured-field precedent (only `Timestamp`), so this is introduced here.

**Scope:**
- IN: 6 RPCs + request/response messages in `api/proto/engram.proto` (`KnowledgeIngest`, `KnowledgeSearch`, `KnowledgeCollections`, `KnowledgeDelete`, `CreateCollection`, `UpdateCollection`); `make proto` regen into `api/engrampb`; extend `mcp.Backend` (mcp.go:52) with the 6 matching methods; add a `roles` field to the proto identity/Provenance message.
- OUT: any handler bodies (stubs only — `UnimplementedEngramServer` covers new RPCs), store/retriever/auth logic.

**Constraints:** `KnowledgeSearchRequest` carries `collection`, `query`, `repeated Predicate filters {field, op, value}`, `repeated SortKey sort {field, order}`, `k`. Design the message set as a deep, minimal surface (design-it-twice per aposd) — generic predicate/sort, not per-collection fields.
**Edge cases:** proto enum for filter `op` (term|range|prefix) and sort `order`; `Value` as a oneof/`google.protobuf.Value` to carry scalar or range.
**Produces:** `engrampb` message types + `Engram` service methods; `mcp.Backend` interface with 6 knowledge methods (stubbed). Contract: method signatures + message field names/types are frozen here.
**Done when:**
- [ ] DW-1.1: `api/proto/engram.proto` defines all 6 RPCs + messages; `make proto` regenerates `api/engrampb` with no diff drift.
- [ ] DW-1.2: `mcp.Backend` interface lists all 6 knowledge methods; the repo compiles with stub/`Unimplemented` returns.
- [ ] DW-1.3: filter/sort/value shapes are generic (no arXiv field names in the proto).

**Difficulty:** HIGH
**Uncertainty:** `Value` representation resolved above (oneof scalar/range). Remaining: confirm `make proto` plugin versions regenerate `structpb` imports cleanly.

### Phase 2: Role identity + authorization core
**Model:** fable
**Skills:** cc-defensive-programming, aposd-designing-deep-modules
**Gate:** Full
**Security-sensitive:** yes
**Depends on:** none | **Unlocks:** Phase 6
**File scope:** `internal/auth/**, internal/knowledgeauth/**, internal/store/templates/auth-tokens.json`

**Goal:** Add a first-class role dimension to identity and a small authorizer that decides per-collection read/write access from a caller's roles.

**Scope:**
- IN: `Roles []string` on `auth.Identity` (auth.go:34); populate it from verified token claims in token issuance/verification (do NOT trust client-supplied roles); a new `knowledgeauth` package: `AuthorizeRead(id auth.Identity, public bool, requiredRoles []string) error`, `AuthorizeWrite(id auth.Identity, requiredRole string) error`, returning an `ErrForbidden` sentinel.
- OUT: where policies are stored (Phase 3), where the authorizer is called (Phase 6).

**Constraints:** Roles are authenticated data — derive only from the verified token, never from request fields (barricade rule: internal-team API is still external). Authorizer takes primitives (`public`, `requiredRoles`), not a knowledge type, to avoid coupling auth→knowledge. Public read = allow any authenticated caller.
**Edge cases:** empty roles; unknown role in a policy (deny, not error); a `public` collection still requires authentication; admin/harvester write role missing → `ErrForbidden`.
**Produces:** `auth.Identity.Roles`; role-carrying tokens; `knowledgeauth.Authorizer{AuthorizeRead, AuthorizeWrite}` + `ErrForbidden`. Contract: the two authorize signatures above.
**Rollback:** additive (new field + new package); no data migration. Revert = drop package + field.
**Done when:**
- [ ] DW-2.1: `auth.Identity` carries `Roles`, populated from verified token claims; a unit test proves client-supplied roles in a request are ignored.
- [ ] DW-2.2: `AuthorizeRead` allows public, allows a caller holding a required role, denies otherwise with `ErrForbidden`.
- [ ] DW-2.3: `AuthorizeWrite` denies a caller lacking the required role with `ErrForbidden`.

**Difficulty:** HIGH
**Uncertainty:** token issuance format — verify how tokens are minted/verified today before adding the claim.

### Phase 3: Collection registry (meta-index + runtime provisioning)
**Model:** fable
**Skills:** aposd-designing-deep-modules
**Gate:** Full
**Depends on:** none | **Unlocks:** Phases 4, 5, 6
**File scope:** `internal/knowledge/**, internal/store/registry*.go, internal/store/templates.go, internal/store/apply.go, internal/store/templates/knowledge-collections.json`

**Goal:** A durable, runtime-mutable registry of collections backed by a `knowledge-collections` meta-index, with live per-collection index provisioning — so engram never restarts for a collection change.

**Scope:**
- IN: `knowledge-collections` meta-index (template const + embed + apply row, mirroring templates.go:18/apply.go:56); `knowledge.CollectionSpec{Name, Index, TextField, Mappings map[string]FieldSpec{Type,Filterable,Sortable}, Access AccessPolicy{Public bool, Roles []string}}`; a `CollectionRegistry` deep module: `Get/Create/Update/List/Provision`; in-process cache with write-invalidation; runtime index creation (`PUT /<index>`), new-field `PUT mapping`, and field-type change via new `-vN` index + alias swap; idempotent YAML boot-seed loader.
- OUT: RPC exposure (Phase 6), auth enforcement of create/update (Phase 6 handler).

**Constraints:** Registry hides all OpenSearch/index/alias mechanics (deep module — callers name a collection, never an index). Cache invalidation on every write. YAML seed applied idempotently: seeding an existing collection is a no-op, not a conflict.
**Edge cases:** create duplicate name → `ErrConflict`; unknown collection on `Get` → not-found sentinel; mapping field-type change requires reindex (provision new index, swap alias); concurrent create race (reuse `ensureIndex` idempotency).
**Produces:** `knowledge.CollectionRegistry` (Get→`CollectionSpec`, Create, Update, List→`[]CollectionSummary`, Provision) + `knowledge.CollectionSpec`/`AccessPolicy`/`FieldSpec`/`Document` types (this phase owns the `internal/knowledge` type package; `Document{ID,Title,Text,SourceVersion,Fields map[string]any}` is defined here and consumed by Phase 4). Contract: these signatures + the spec/Document struct fields.
**Rollback:** meta-index + `knowledge-*` indices are new; revert = delete indices. No memory-path data touched.
**Done when:**
- [ ] DW-3.1: `Create` writes a meta-index doc AND provisions the live index/alias in one call; a follow-up `Get` returns the spec with no restart.
- [ ] DW-3.2: `Update` adds a mapping field via live `PUT mapping`; a field-type change provisions `-v2` + swaps the alias.
- [ ] DW-3.3: registry reads hit the cache; any write invalidates it (test: create → list reflects it without re-reading the index directly).
- [ ] DW-3.4: YAML boot-seed applied twice is idempotent (second run makes no changes).

**Difficulty:** HIGH
**Uncertainty:** alias-swap atomicity semantics in OpenSearch 3.1 — confirm the `_aliases` actions block behavior.

### Phase 4: KnowledgeStore — bulk write + mark-and-sweep delete
**Model:** sonnet
**Skills:** aposd-designing-deep-modules, cc-defensive-programming
**Gate:** Full
**Depends on:** Phase 3 | **Unlocks:** Phase 6
**File scope:** `internal/store/knowledge.go, internal/store/knowledge_test.go, internal/store/opensearch.go`

**Goal:** A batch document-write path (upsert-by-id, no embedding) plus a delete-by-query sweep — the intentional, documented deviation from engram's append-only writes.

**Scope:**
- IN: a `doNDJSON` sibling to `doJSON` (opensearch.go:220) for `application/x-ndjson`; `BulkIndex(ctx, index string, docs []knowledge.Document, harvestID string) (indexed int, err error)` — `_bulk` with `index` action (upsert-by-id), stamping `harvest_id`, `source_version`, and server-set `harvested_at` on every row, NO embedding call; `DeleteByQuery(ctx, index, collection, source, currentHarvestID string) (deleted int, err error)` via `POST /<index>/_delete_by_query` (matches `collection` AND `source` AND `harvest_id != currentHarvestID`).
- OUT: authorization (Phase 6 barricade), harvest orchestration (Plan 2).

**Constraints:** No `op_type=create`, no `if_seq_no` guard — this path is deliberately upsert/overwrite. Map non-2xx via the existing status switch; wrap errors `"store: verb-ing noun: %w"`. `harvested_at` is server-assigned, never client-trusted.
**Edge cases:** partial `_bulk` failure (some items error) → report per-item errors, don't silently succeed (aposd: surface failures); empty `docs` → no-op; `_bulk` body must end with a trailing newline; delete matching zero docs → `deleted: 0`, not an error.
**Produces:** `store.KnowledgeStore{BulkIndex, DeleteByQuery}` (the two signatures above). Consumes `knowledge.Document` from Phase 3 (not defined here).
**Rollback:** writes are upserts to `knowledge-*` indices only; a bad batch is corrected by re-ingest or sweep. No memory index touched.
**Done when:**
- [ ] DW-4.1: `BulkIndex` upserts N docs by `_id`, stamps `harvest_id`/`source_version`/`harvested_at`, issues zero embedding calls (integration test asserts doc count + fields + no embed-server hit).
- [ ] DW-4.2: re-`BulkIndex` of the same id overwrites in place (no duplicate row).
- [ ] DW-4.3: `DeleteByQuery` removes rows matching `collection` AND `source` AND `harvest_id != <currentHarvestID>` (the mark-and-sweep predicate — rows not touched by the latest run), leaving current-run rows.
- [ ] DW-4.4: a `_bulk` response containing per-item errors surfaces them (does not report full success).

**Difficulty:** MEDIUM
**Uncertainty:** none material — well-scoped against the frozen Phase 3 registry + Phase 1 contract (hence sonnet).

### Phase 5: KnowledgeRetriever — BM25 + generic filters + sort + staleness
**Model:** sonnet
**Skills:** aposd-designing-deep-modules, cc-defensive-programming
**Gate:** Full
**Depends on:** Phase 3 | **Unlocks:** Phase 6
**File scope:** `internal/retrieval/knowledge.go, internal/retrieval/knowledge_test.go, internal/retrieval/opensearch.go`

**Goal:** A BM25-only retriever that applies generic, registry-validated field filters and sort, and reports per-collection staleness — never touching the embedder or memory tiers.

**Scope:**
- IN: extend `buildQuery` (opensearch.go:499) with an optional `sort []any` block (additive; memory callers pass nil); a `KnowledgeRetriever.Search(ctx, spec knowledge.CollectionSpec, query string, filters []Predicate, sort []SortKey, k int) ([]Hit, error)` that validates each `Predicate.Field` against `spec.Mappings` (filterable/sortable) and builds term/range/prefix clauses generically; `Collections(ctx) ([]CollectionMeta, error)` returning per-collection `count` + `newest_harvested_at`/`newest_doc_date` (staleness) via an aggregation.
- OUT: budget-packing/spill (Phase 6), auth (Phase 6).

**Constraints:** BM25 mode only — no embedder call, no kNN, no RRF pipeline, not registered on `MultiRetriever`. Filters are generic (validated against the registry mapping), never hardcoded field names. `buildQuery`'s existing memory behavior must be unchanged (sort defaults to absent).
**Edge cases:** unknown filter field → return a validation error that **names the valid filterable fields** (self-correcting for the LLM caller); non-sortable field in `sort` → same; empty query with only filters → filter-only search; `k` bounds.
**Produces:** `retrieval.KnowledgeRetriever{Search, Collections}` + `Predicate{Field,Op,Value}` / `SortKey{Field,Order}` / `CollectionMeta`. Contract: the two signatures + these types.
**Done when:**
- [ ] DW-5.1: BM25 search over `text_field` returns ranked hits; `buildQuery` memory path is byte-identical when `sort` is nil (regression test).
- [ ] DW-5.2: term + range + prefix filters validated against the registry apply correctly; an unknown field errors with the valid-field list.
- [ ] DW-5.3: sort by a registered sortable field orders results; sort by a non-sortable field errors.
- [ ] DW-5.4: `Collections` reports count + newest `harvested_at`/doc date per collection.

**Difficulty:** MEDIUM
**Uncertainty:** whether to extend `buildQuery` in place vs a knowledge-local builder — extend in place only if the memory regression test (DW-5.1) stays green; else fork a builder (well-scoped either way, hence sonnet).

### Phase 6: MCP tools + server wiring + budget/spill
**Model:** fable
**Skills:** cc-defensive-programming, aposd-designing-deep-modules
**Gate:** Full
**Security-sensitive:** yes
**Depends on:** Phase 1, Phase 2, Phase 3, Phase 4, Phase 5 | **Unlocks:** —
**File scope:** `internal/mcp/tools.go, internal/mcp/budget.go, internal/mcp/spill.go, internal/mcp/knowledge_tools_test.go, internal/server/server.go, internal/server/knowledge.go, internal/server/knowledge_test.go, internal/engramclient/**, cmd/engram-server/main.go`

**Goal:** Wire the six operations end-to-end as gRPC RPCs and MCP tools — the request barricade that validates untrusted input and enforces authorization — reusing the budget-packer + spill for lean LLM responses.

**Scope:**
- IN: 6 gRPC handlers (`internal/server/knowledge.go`) translating `engrampb` ↔ store/registry/retriever with the existing error-translation switch; 6 MCP tool schemas + `callX` handlers implementing `Backend` (tools.go:88); call `knowledgeauth.AuthorizeRead` before search/collections and `AuthorizeWrite` before ingest/delete/create/update (the barricade); reuse `packSearchResult` + `spillFullResult` for `knowledge_search` — both are typed on `mcp.Hit`, so knowledge results are adapted into `mcp.Hit` and only the memory-shaped `facetFields` (budget.go:29) is parametrized (add a `topFacets` param; `spill.go` in scope); surface staleness in `knowledge_collections`; replace the `engramclient` Phase-1 stubs with real gRPC calls; wire new `Server` fields + registry/store/retriever construction in `engram-server/main.go`.
- OUT: harvester (Plan 2).

**Constraints:** This is the barricade — validate all external MCP/gRPC input at entry (collection exists, filter/sort well-formed, k bounded) before touching inner modules; inner modules may then assume validated input. Map `ErrForbidden`→`PermissionDenied`, `ErrConflict`→`AlreadyExists`, not-found leak-free→`NotFound`, validation→`InvalidArgument`. Never leak security info in errors, but DO name valid fields on a filter-field error (usability, not a security boundary).
**Edge cases:** unknown collection → `InvalidArgument` naming it; unauthorized read of a role-gated collection → `PermissionDenied`; oversized result → budget-pack + `overflow_path` spill; malformed predicate → self-correcting error; create_collection by a non-admin → `PermissionDenied`.
**Produces:** working `knowledge_ingest`/`knowledge_search`/`knowledge_collections`/`knowledge_delete`/`knowledge_create_collection`/`knowledge_update_collection` over gRPC + MCP, end-to-end.
**Rollback:** additive RPCs/tools + new server construction; revert = remove handlers/wiring. Memory tools untouched.
**Done when:**
- [ ] DW-6.1: all 6 MCP tools dispatch through `Backend` to the real store/registry/retriever; `knowledge_search` returns budget-packed hits with `overflow_path` spill on overflow.
- [ ] DW-6.2: read of a role-gated collection without the role → `PermissionDenied`; public collection read succeeds for any authenticated caller.
- [ ] DW-6.3: `knowledge_ingest`/`_delete`/`_create_collection`/`_update_collection` without the harvester/admin role → `PermissionDenied`.
- [ ] DW-6.4: `knowledge_collections` reports count + staleness; a malformed filter yields a `InvalidArgument` naming valid fields.
- [ ] DW-6.5: memory MCP tools + gRPC RPCs behave identically (regression) — the memory path is behaviorally untouched.

**Difficulty:** HIGH
**Uncertainty:** `facetFields` parametrization vs accepting memory-shaped facets for knowledge — prefer a small `topFacets` param; fall back to no facets for knowledge if it balloons scope.

---
## Test Coverage
**Level:** 100% — every done-when item covered, with boundary + dirty tests; every code-touching phase carries ≥1 dirty test (enumerated below ≈ 20 dirty / 12 clean; the security-sensitive phases 2 and 6 are the most dirty-heavy, which is where error-path coverage matters most).

## Test Plan

**Phase 1 — Proto contract** (Unit)
- [ ] clean: `make proto` regenerates `api/engrampb` with no diff; repo compiles with stubs (DW-1.1, DW-1.2)
- [ ] dirty: a proto lint/round-trip guard fails if any arXiv-specific field name leaks into filter/sort/message defs (DW-1.3)
- [ ] boundary: `Value` carries both a scalar and a range without loss (round-trip)

**Phase 2 — Role identity + authorizer** (Unit)
- [ ] clean: identity populated with roles from a verified token (DW-2.1)
- [ ] dirty: client-supplied `roles` in a request are ignored — only token claims count (DW-2.1)
- [ ] dirty: `AuthorizeRead` denies a caller lacking every required role → `ErrForbidden` (DW-2.2)
- [ ] dirty: `AuthorizeWrite` denies a caller without the harvester/admin role → `ErrForbidden` (DW-2.3)
- [ ] dirty: unknown role named in a policy → deny, not error (edge)
- [ ] boundary: empty required-roles + `public=true` → allow any authenticated caller; `public=false` + empty roles → deny

**Phase 3 — Collection registry** (Integration, `//go:build integration`)
- [ ] clean: `Create` writes meta-doc + provisions live index/alias; `Get` returns spec, no restart (DW-3.1)
- [ ] clean: `Update` adds a field via live `PUT mapping`; field-type change provisions `-v2` + swaps alias (DW-3.2)
- [ ] clean: cache read after write reflects the change without direct index re-read (DW-3.3)
- [ ] dirty: duplicate collection name → `ErrConflict` (edge)
- [ ] dirty: `Get` unknown collection → not-found sentinel (edge)
- [ ] dirty: concurrent create race resolves idempotently (no double index) (edge)
- [ ] dirty: YAML boot-seed run twice makes no changes on the second pass (DW-3.4)

**Phase 4 — KnowledgeStore** (Integration)
- [ ] clean: `BulkIndex` upserts N docs, stamps `harvest_id`/`source_version`/`harvested_at`, zero embed-server hits (DW-4.1)
- [ ] clean: `DeleteByQuery` removes stale collection+source rows, keeps current (DW-4.3)
- [ ] dirty: re-`BulkIndex` same id overwrites in place — no duplicate row (DW-4.2)
- [ ] dirty: `_bulk` response with per-item errors surfaces them, not full success (DW-4.4)
- [ ] dirty: empty `docs` → no-op; `_bulk` body malformed without trailing newline is rejected (edge)
- [ ] boundary: `DeleteByQuery` matching zero docs → `deleted: 0`, not an error (edge)

**Phase 5 — KnowledgeRetriever** (Unit + Integration)
- [ ] clean: BM25 search returns ranked hits over `text_field` (DW-5.1)
- [ ] regression: `buildQuery` memory path byte-identical when `sort` is nil (DW-5.1) — **guards the memory boundary**
- [ ] clean: term + range + prefix filters apply correctly; sort by a sortable field orders results (DW-5.2, DW-5.3)
- [ ] dirty: unknown filter field → error naming the valid filterable fields (DW-5.2)
- [ ] dirty: sort by a non-sortable field → error (DW-5.3)
- [ ] dirty: filter-only search (empty query) returns filtered set; retriever issues zero embed calls (edge)
- [ ] clean: `Collections` reports correct count + newest `harvested_at`/doc date for a populated collection (DW-5.4)
- [ ] boundary: `k` at 0 / 1 / max; `Collections` staleness with an empty collection (DW-5.4)

**Phase 6 — MCP tools + wiring** (Integration + Manual)
- [ ] clean: all 6 tools dispatch through `Backend` to real store/registry/retriever end-to-end (DW-6.1)
- [ ] clean: `knowledge_search` budget-packs + spills to `overflow_path` on overflow (DW-6.1)
- [ ] dirty: role-gated read without the role → `PermissionDenied`; public read succeeds (DW-6.2)
- [ ] dirty: ingest/delete/create/update without harvester/admin role → `PermissionDenied` (DW-6.3)
- [ ] dirty: unknown collection → `InvalidArgument` naming it; malformed predicate → self-correcting error (DW-6.4)
- [ ] dirty: `create_collection` by a non-admin → `PermissionDenied` (edge)
- [ ] regression: memory MCP tools + gRPC RPCs behave identically — memory path untouched (DW-6.5)
- [ ] manual: drive `knowledge_ingest` → `knowledge_search` → `knowledge_delete` over MCP against a live cluster; confirm staleness + spill on a real corpus sample

---
## Assumptions
| Assumption | Confidence | Verify Before Phase | Fallback If Wrong |
|---|---|---|---|
| Tokens are minted/verified in a place a `roles` claim can be added | MED | Phase 2 | If tokens are opaque/external, carry roles via a verified side-channel claim |
| `buildQuery` can gain a `sort` block without disturbing memory queries | HIGH | Phase 5 | Fork a knowledge-local query builder (DW-5.1 gates this) |
| OpenSearch 3.1 `_aliases` swap is atomic enough for zero-downtime reindex | MED | Phase 3 | Brief read-through of new alias during swap; document the window |
| Budget-packer/spill is reusable with only a facet-list change | HIGH | Phase 6 | Knowledge search ships without facets in v1 |
| No memory-path fake must implement the new store/retriever methods | MED | Phase 4 | Define narrow `KnowledgeStore`/`KnowledgeRetriever` interfaces, not additions to the memory `Store` |

## Decision Log
| Decision | Alternatives Considered | Rationale | Phase |
|---|---|---|---|
| Approach B: true role dimension on Identity/token | A (reuse ACL scope), C (extend memory tier) | Greenfield; best-in-class, avoid a later token migration; named roles decoupled from teams | 2 |
| Registry in a meta-index, not boot YAML | Boot-loaded config file; hot-reloaded YAML | Never-restart: runtime create/update as live REST (Option A) | 3 |
| Separate KnowledgeStore/Retriever, no RRF fusion | Extend memory `MultiRetriever`/`Store` | Actor boundary (search ≠ reconciliation); avoids memory-fake growth + hardcoded filter fields | 4,5 |
| Upsert-by-id + hard delete-by-query | Append-only like memory | Documents are mutable content, not reconciled facts; mark-and-sweep needs real deletes | 4 |
| Authorization at the request barricade (Phase 6) | Enforce inside store/retriever | Single validated entry; inner modules stay deep + auth-agnostic | 6 |

---
## Notes
- **Split:** the harvester tool (arXiv Kaggle+OAI-PMH, github-repos, web-crawl, mark-and-sweep orchestration, Layer-2 sources YAML) is **Plan 2**, built against this API. Provenance fields (`harvest_id`/`source_version`) are produced here but *populated* by Plan 2.
- **House-style deviation (call out to builders):** Phase 4 writes are upsert + hard delete BY DESIGN — do not convert to `op_type=create`/`invalid_at`. See `docs/code-standards.md` "no hard-delete" rule; knowledge is the documented exception.
- **No wave parallelism promised:** every phase is Full-gate (new cross-phase seams + auth), so build runs them serially in DAG order.
- **Sentinel errors returned unwrapped** at the store boundary so `errors.Is` survives to the gRPC edge (per code-standards).
- Open (deferred to Plan 2, not this plan): exact harvester home already decided (dedicated tool).

---
## Execution Log

### Phase 1: Proto contract & Backend seam (Gate: Full)
- [x] BUILD: Discovery + design + implementation (stub → implement → validate) complete
- [x] REVIEW: Verification passed (sonnet, single sample)
- [x] Committed
Commit: 84fa0f5
Summary: Froze the knowledge wire+backend contract — 6 RPCs + 13 messages in engram.proto with generic Predicate/SortKey and a Value oneof(scalar|range), Provenance.roles claim, and mcp.Backend +6 knowledge methods stubbed across engramclient + test doubles. Repo builds/tests/lints clean; proto regen deterministic and drift-free post-commit. Downstream phases now implement against frozen `engrampb` types + `mcp.Backend`/seam types (KnowledgeDoc, Predicate, SortKey, FieldSpec, CollectionSpec, CollectionInfo) in internal/mcp/mcp.go.

### Phase 2: Role identity + authorization core (Gate: Full, Security-sensitive)
- [x] BUILD: Discovery + design + implementation complete (assumption verified — TokenRecord is the claim set; roles slot into Issue/Verify)
- [x] REVIEW: 3-sample fable majority PASS (3/3), fail-closed behavior verified from scratch
- [x] Committed
Commit: 7a714ee
Summary: Added `Roles []string` to auth.Identity + TokenRecord (mint/read-time normalized, cloned), populated ONLY from the verified token — client-supplied roles proven ignored. New `internal/knowledgeauth` package: fail-closed `AuthorizeRead(id, public, requiredRoles)` / `AuthorizeWrite(id, requiredRole)` returning unwrapped `ErrForbidden` (auth-before-public ordering; unknown/empty roles deny not error). auth-tokens.json gains a `roles` keyword field. Enforcement call-sites land in Phase 6. Follow-ups: no CLI `--roles` mint flag yet; existing strict token indices need re-provisioning (omitempty keeps old tokens writable).

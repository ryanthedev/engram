# Discovery + Design: Phase 4 - KnowledgeStore — bulk write + mark-and-sweep delete

## Files Found
- `internal/store/opensearch.go` (246 lines) — `doJSON` primitive (line 220), status-switch pattern (`Create`, `Update`), `isIndexNotFound` (404-as-empty rule).
- `internal/store/store.go` — memory `Store` interface (Append/Create/Update/ClaimBatch/Complete/DeadLetter/ClaimLedger/UpdateLedger/ScanIncomplete). No knowledge methods; append-only contract documented at the type doc.
- `internal/store/registry.go` (631 lines) + `registry_test.go` (fake-cluster unit tests) + `registry_integration_test.go` (live-cluster tests, `//go:build integration`) — Phase 3's `CollectionRegistry`, the pattern this phase mirrors: functional-option constructor, `validateCollectionName` barricade before any path interpolation, `doJSON` status-switch, `isIndexNotFound` reuse.
- `internal/knowledge/knowledge.go` (101 lines) — frozen `Document{ID, Title, Text, SourceVersion, Fields map[string]any}`, `CollectionSpec`, `AccessPolicy`, `FieldSpec`. No `Collection`/`Source` fields on `Document`.
- `internal/mcp/mcp.go` — `Backend.KnowledgeIngest(ctx, collection, source, harvestID, docs)` and `KnowledgeDelete(ctx, collection, source, currentHarvestID)`: confirms `collection`/`source` are batch-level parameters at the Phase-6 seam, not per-`Document` struct fields.
- `internal/store/apply.go` — `baseDocProperties` in registry.go reserves `title`, `collection`, `source`, `source_version`, `harvest_id`, `harvested_at` as physical-index keyword/date fields on every collection index; notably **not** `text` (that key is `spec.TextField`, defaulting to `"text"`).
- No existing `_bulk`/NDJSON helper in `internal/store` or `internal/retrieval` — only in `cmd/engram-perf/main.go` and `cmd/engram-loadtest/seed.go` (non-package scripts), used as a request-shape reference only.
- Live OpenSearch 3.1.0 confirmed reachable at `http://localhost:9200` (checked directly) — integration tests can run for real.

## Current State
Phase 3 shipped `knowledge.CollectionRegistry` (interface in `internal/knowledge`, OpenSearch impl in `internal/store/registry.go`). Nothing in `internal/store` yet writes or deletes knowledge documents. The memory `Store` interface and `OpenSearchStore` are untouched by this phase's scope.

## Gaps
None blocking. One real design ambiguity resolved below (text-field key).

## Code Standards
Applied: `"pkg: verb-ing noun: %w"` error wraps; sentinel errors returned unwrapped for `errors.Is`; `doJSON`-style raw `net/http` + `map[string]any`; functional-option constructor (`NewXxx`, `WithXxx`); `var _ Type = (*Impl)(nil)` compile-time assertion pattern (skipped here — no interface is declared yet, see Assumption Verification); stdlib `testing` only, no testify; integration tests behind `//go:build integration` with scratch names + cleanup; three-group imports.

## Test Infrastructure
`internal/store/registry_test.go` uses an **internal** `package store` test (not `store_test`) specifically to exercise unexported validators (`validateCollectionName`, `normalizeSpec`) directly — `TestProvisionRejectsPathTraversalName` is the precedent for testing a path-injection barricade. `registry_integration_test.go` provides `liveRegistry(t)`, `scratchName(t, client, url)`, `deleteIndices(t, ...)`, `physicalFor`/`aliasFor` helpers, all reusable from a same-package integration test file. Given knowledge.go's own barricade (`validateIndexName`) is unexported and the DW items are inherently cluster-interaction behaviors (bulk partial-errors, delete-by-query semantics), `knowledge_test.go` is written as a single internal `package store` file tagged `//go:build integration` — matching the plan's Test Plan label "(Integration)" for this phase and its exact File scope (`knowledge.go`, `knowledge_test.go` — no separate unit/integration split, unlike Phase 3).

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-4.1 | `BulkIndex` upserts N docs by `_id`, stamps `harvest_id`/`source_version`/`harvested_at`, issues zero embedding calls | COVERED | `TestDW_4_1_BulkIndexStampsFieldsNoEmbedCalls` — indexes 3 docs, reads each back via `_doc/{id}`, asserts stamped fields + no `*_embedding` field present; `KnowledgeStore` has no embed client field at all (structural guarantee) |
| DW-4.2 | re-`BulkIndex` of the same id overwrites in place (no duplicate row) | COVERED | `TestDW_4_2_ReBulkIndexOverwritesInPlace` — indexes id=1 under harvest h1, re-indexes same id under h2 with different title, asserts `_count`==1 and fields reflect h2 |
| DW-4.3 | `DeleteByQuery` removes rows matching `collection` AND `source` AND `harvest_id != currentHarvestID`, leaving current-run rows | COVERED | `TestDW_4_3_DeleteByQuerySweepsStaleRows` — seeds a stale (h1) row, a current (h2) row, and a same-harvest-but-different-source row; asserts only the true stale row is deleted (proves the AND, not just harvest mismatch) |
| DW-4.4 | a `_bulk` response containing per-item errors surfaces them (does not report full success) | COVERED | `TestDW_4_4_BulkIndexSurfacesPerItemErrors` — one doc carries an undeclared field (strict-mapping rejection), one is valid; asserts non-nil err naming the failure, `indexed`==1 (not 2), and the valid doc is actually present (partial success, not silently reported as full) |

**All items COVERED:** YES

## Design: KnowledgeStore

### Approaches Considered
1. **Concrete struct `store.KnowledgeStore{client, baseURL}`** (mirrors `OpenSearchStore`/`CollectionRegistry`) with `BulkIndex`/`DeleteByQuery` methods, constructed via `NewKnowledgeStore(client, baseURL)`.
2. **Free functions** `store.BulkIndex(ctx, client, baseURL, index, docs, harvestID)` / `store.DeleteByQuery(ctx, client, baseURL, ...)` — no struct, since there's no per-call default index to hold as state (unlike `OpenSearchStore.episodicIndex`).
3. **Declare a `knowledge.KnowledgeStore` interface now** (mirroring `knowledge.CollectionRegistry`), implemented by an OpenSearch struct.

### Comparison
| Criterion | 1. Struct | 2. Free funcs | 3. Interface now |
|-----------|-----------|----------------|-------------------|
| Interface simplicity | 2 methods, 1 constructor | 2 functions, no type | 2 methods + interface + impl (more surface) |
| Matches plan's literal `store.KnowledgeStore{...}` naming | Yes | No | Partial (wrong package per file scope) |
| Matches house constructor convention (`NewXxx`/`WithXxx`) | Yes | No | Yes, but premature |
| Room to grow (write guards, timeouts) without call-site churn | Yes | No — every call site touched | Yes |
| Respects "seams are consumer-defined" (code-standards §4) + explicit plan note that Phase 6 defines the seam | Yes (no interface invented) | Yes | **No** — invents the seam before its consumer exists |

### Choice: 1 — concrete struct, no interface
Rationale: the plan's Assumption Verification explicitly says the narrow `KnowledgeStore` interface, if needed, is Phase 6's to define ("its consumer, Phase 6, defines the seam") — Go's structural typing means Phase 6 can declare its own 2-method interface later and `*store.KnowledgeStore` satisfies it for free. Declaring an interface in `internal/knowledge` now would also fall outside this phase's frozen File scope (`internal/knowledge/**` is not listed for Phase 4). A concrete struct also matches the plan's literal `store.KnowledgeStore{BulkIndex, DeleteByQuery}` naming and the codebase's constructor idiom. Sacrifice: none material — no test double is needed today since nothing consumes this yet.

### Depth Check
- Interface methods: 2 (`BulkIndex`, `DeleteByQuery`).
- Hidden details: NDJSON request construction and the `_bulk` action-line format, per-item error walking, the `_delete_by_query` bool-query shape, index-name path-injection validation, the 404-as-empty translation, stamped-field override ordering (defense against a `Fields` entry spoofing provenance).
- Common case complexity: simple — one call, two params beyond docs/harvestID.

## Design Decision: the text-field key (RESOLVED via contract amendment)
`knowledge.Document.Text` is a single Go struct field, but `knowledge.CollectionSpec.TextField` lets a collection choose ANY physical field name for its full-text content (default `"text"`, but a caller-set spec could name it `"abstract"` — see `registry_test.go`'s `paperSpec()`).

**Initial build (superseded):** the frozen `BulkIndex(ctx, index string, docs []knowledge.Document, harvestID string)` signature did not receive the text-field name, so the first implementation wrote `Document.Text` under the literal key `"text"` and relied on a per-item strict-mapping error to "fail loud" for non-default collections. I flagged this as a concern rather than escalating to UPDATE_PLAN.

**Amendment (orchestrator-approved):** the "fail loud" behavior was correctly judged a real defect — it makes ANY collection whose `TextField != "text"` (including the primary arXiv corpus, which uses `abstract`) permanently un-ingestable, not merely inconvenient. The signature was amended to `BulkIndex(ctx, index, textField string, docs []knowledge.Document, harvestID string)`; the Phase 6 caller passes `spec.TextField`. `Document.Text` is now written under `row[textField]`. `textField` is validated by a new `validateTextField` (same `collectionNameRE` grammar the registry used at collection-creation time, plus a `baseDocProperties` reserved-field check) before any body is built — so a caller can never redirect `Document.Text` onto a server-owned provenance field. This is the correct, corpus-enabling design; the earlier "fail loud" reasoning is retracted.

## Design Decision: `collection`/`source` per-document fields
`baseDocProperties` reserves `"collection"` and `"source"` on every physical knowledge index, but `knowledge.Document` has no such fields — only `Fields map[string]any`. `mcp.Backend.KnowledgeIngest(ctx, collection, source, harvestID, docs)` confirms `collection`/`source` are **batch**-level parameters known only at the Phase-6 seam. `BulkIndex`'s frozen signature has no `collection`/`source` parameters either. **Decision:** `BulkIndex` merges `Document.Fields` into the row as-is (so a Phase-6 caller that injects `"collection"`/`"source"` into each doc's `Fields` before calling `BulkIndex` gets them persisted); `BulkIndex` itself does not require, validate, or synthesize these two keys — that's Phase 6's barricade responsibility, structurally out of this phase's reach (Phase 4 never receives a bare `collection`/`source` string for `BulkIndex`, only for `DeleteByQuery`). This is consistent with DW-4.1's precise field list (harvest_id/source_version/harvested_at only) and the file-scope boundary (no `internal/knowledge/**` changes here).

## Defensive-programming decisions (cc-defensive-programming)
- **Barricade, not assumption, at path interpolation.** `index` is interpolated into `/{index}/_bulk` and `/{index}/_delete_by_query`. Per the dispatch prompt's explicit SECURITY LESSON from Phase 3 (a missed gate caused path-traversal there), a new `validateIndexName` runs before every URL is built in both methods — analogous to `registry.go`'s `validateCollectionName`, but permits hyphens/dots since physical/alias index names (`knowledge-<name>-v<N>`) contain them, unlike collection names.
- **Stamped-field override ordering.** Row construction merges caller `Fields` FIRST, then sets `title`/`text`/`source_version`, then sets `harvest_id`/`harvested_at` LAST — so a `Fields` entry cannot spoof server-assigned provenance even if it happens to collide with a reserved key name. `harvested_at` in particular is computed server-side (`time.Now().UTC()`), never accepted from the caller (`Document` has no such field at all, so this is also structurally enforced).
- **Refuse a destructive no-op-that-isn't.** `DeleteByQuery` rejects an empty `currentHarvestID` with a validation error rather than running the query: `harvest_id != ""` would match every harvested row, turning a routine mark-and-sweep into a full wipe of the collection+source. This is new defensive validation beyond the plan's literal edge-case list, but directly protects the exact destructive operation the plan flags as `KnowledgeStore`'s one intentional deviation from append-only safety. Also rejects empty `collection`/`source` (an unscoped delete is never the intended sweep predicate).
- **Empty ID rejected.** `BulkIndex` rejects any `Document` with an empty `ID` up front (whole-batch validation error) rather than letting OpenSearch auto-generate an id, which would silently break the upsert-by-id contract DW-4.2 depends on.
- **No embedder reachable.** `KnowledgeStore` holds only `client`/`baseURL` — there is no embed-client field or import, so "zero embedding calls" (DW-4.1) is structurally guaranteed, not just tested.

## Prerequisites
- [x] Phase 3 `knowledge.Document`/`CollectionSpec` frozen and available.
- [x] Live OpenSearch 3.1 reachable for integration tests (verified: `curl localhost:9200` → version 3.1.0).
- [x] `doJSON` and `isIndexNotFound` reusable from `opensearch.go` (same package).
- [x] No missing prerequisites.

## Recommendation
BUILD.

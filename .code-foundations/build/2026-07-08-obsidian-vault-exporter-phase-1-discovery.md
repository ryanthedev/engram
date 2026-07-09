# Discovery + Design: Phase 1 - Graph scan primitive

## Files Found
- `internal/graph/graph.go` — `Entity`/`Edge` domain types, `Live()` methods (Entity: `ExpiredAt==nil`; Edge: `InvalidAt==nil && ExpiredAt==nil`).
- `internal/graph/store.go` — `Backend` interface, `Store` deep-module wrapper, `MemBackend` in-memory fake (all live in one file).
- `internal/graph/opensearch.go` — `OpenSearchBackend`, `osJSON`/`osDo`/`osSearchHits`/`osDecodeSource` HTTP helpers, `isIndexNotFound` guard (line 297).
- `internal/graph/templates/entities.json`, `internal/graph/templates/edges.json` — index mappings (`dynamic: strict`).
- `internal/graph/store_test.go` — existing Store-level test conventions (package `graph`, not `graph_test`; table-driven where useful; `newTestStore(t)` helper).

## Current State
- No pagination primitive exists anywhere in `internal/graph`: `CandidateEntities` caps at 20, `Neighbors` caps at 1000 — both silent-truncate by design (bounded-recall use cases), neither is reusable for a full-tier walk.
- `Backend` interface has 8 methods (`CandidateEntities`, `PutEntity`, `GetEntity`, `CountEntities`, `CountAllEntities`, `PutEdge`, `GetEdge`, `Neighbors`); no `Scan*` methods.
- `MemBackend` already sorts slices by `ID` ascending in `CandidateEntities`/`Neighbors` for deterministic test output — the same tie-break field the plan wants for `search_after`.

## Gaps
- `Backend`/`Store`/`MemBackend`/`OpenSearchBackend` all lack `ScanEntities`/`ScanEdges`.
- No `Cursor` type exists.
- No `search_after`+`sort` query shape exists in `opensearch.go` (existing queries use `size` only, no `sort`).

## Code Standards
Read `docs/code-standards.md` (greenfield doc). Applicable to this phase:
- Errors: return, wrap with `fmt.Errorf("...: %w", err)`, no panics.
- `context.Context` first param on every I/O call.
- Deep modules: small interfaces over substantial implementations; define interfaces at the consumer.
- Table-driven tests; every code-touching phase ships at least one error-path ("dirty") test.

## Test Infrastructure
- `go test` standard library testing, table-driven where the plan calls for dirty/boundary cases.
- Tests live in-package (`package graph`), so they can reach unexported identifiers directly (e.g. a package-level pagination batch-size knob) — no need for an exported test-only config surface.
- `MemBackend` is the established fake; `OpenSearchBackend` correctness is validated by construction (query-shape assertions) since integration tests need a live cluster and are out of scope here (`//go:build integration`).

## Assumption Verification

**Assumption:** Entity `id` is a stable, keyword-mapped field usable as a `search_after` tie-break sort (Confidence: MED).

**Verified TRUE.** `internal/graph/templates/entities.json:13` — `"id": { "type": "keyword" }`. `internal/graph/templates/edges.json:12` — `"id": { "type": "keyword" }`. Both indices are `dynamic: strict`, so `id` is guaranteed present and exact-match/sortable on every doc (`Entity.ID`/`Edge.ID` are both non-empty deterministic sha256 hex strings per `graph.go:47-54,95-98` — never empty, so an empty-string cursor sentinel for "start" is safe and can never collide with a real id). No fallback needed; proceeding with `id` ascending as the sole sort/tie-break field for both tiers.

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-1.1 | `ScanEntities`/`ScanEdges` on `Backend`, `Store`, `MemBackend`, and OpenSearch backend, using `search_after` + total sort. | COVERED | `TestDW_1_1_ScanEntities_ReturnsLiveEntities`, `TestDW_1_1_ScanEdges_ReturnsLiveEdges`, `TestDW_1_1_OpenSearchBackend_ImplementsScanMethods` (compile-time `var _ Backend = (*OpenSearchBackend)(nil)` already covers this; add a query-shape unit test asserting `sort`+`search_after` appear in the built query) |
| DW-1.2 | Full-tier iteration returns every live record across multiple pages (verified past one batch size), never truncating. | COVERED | `TestDW_1_2_ScanEntities_PaginatesAcrossMultipleBatches`, `TestDW_1_2_ScanEdges_PaginatesAcrossMultipleBatches` (shrink the package-level batch-size knob in the test, insert > 1 batch of records, drive the cursor loop to exhaustion, assert the union equals every inserted live record with no duplicates) |
| DW-1.3 | Only live records surface — entities `expired_at==nil`; edges `invalid_at==nil && expired_at==nil`. | COVERED | `TestDW_1_3_ScanEntities_ExcludesExpired`, `TestDW_1_3_ScanEdges_ExcludesClosedAndExpired` (mirrors `TestNeighbors_ExcludesSoftExpiredAndClosedEdges` fixture shape: live/closed/expired edges, live/expired entities) |
| DW-1.4 | Results scoped to the passed `tenantID`; empty/missing index yields empty, not error. | COVERED | `TestDW_1_4_ScanEntities_TenantScoped`, `TestDW_1_4_ScanEdges_TenantScoped`, `TestDW_1_4_ScanEntities_EmptyTenant_NoError`, `TestDW_1_4_Cursor_FromEmptiedIndex_ExhaustsCleanly` |

**All items COVERED:** YES

## Design: Graph scan primitive

### Approaches Considered
1. **Single opaque `Cursor{after string}` + package-level batch-size var, `id`-ascending sort on both tiers** — one unexported string field holding the last-seen id; `search_after: [id]` on OpenSearch, an equivalent sorted-slice-plus-binary-search walk on `MemBackend`.
2. **`Cursor` wrapping `[]any` (mirrors OpenSearch's raw `search_after` array shape)** — generalizes to multi-field sort tie-breaks without a future signature change.
3. **Separate `EntityCursor`/`EdgeCursor` types** — type-safe, cannot accidentally pass an entity cursor into `ScanEdges`.

### Comparison
| Criterion | 1 (single string cursor) | 2 (`[]any` cursor) | 3 (per-tier cursor types) |
|-----------|---|---|---|
| Interface simplicity | One type, 2 new `Backend` methods + 2 `Store` wrappers | Same method count, cursor internals more complex | Doubles cursor types for no behavioral gain |
| Information hiding | Field unexported; caller never sees "it's the last id" | Slightly leakier — shape mirrors OpenSearch's own array, hinting at the backend | Same hiding as (1), plus an extra type to hide |
| Caller ease of use | Caller passes `Cursor{}` to start, checks `next == Cursor{}` to stop — trivial | Same ergonomics, more internal ceremony for no caller-visible benefit | Caller must track which cursor type goes with which method — extra cognitive load, contradicts "opaque Cursor" plan wording |
| Extensibility | Sort is provably single-field forever (both tiers key by deterministic sha256 `id`); no realistic multi-field sort need | Solves a problem that doesn't exist yet (YAGNI) | Doesn't solve any problem (1) doesn't already solve |
| Matches plan contract | Plan literally specifies one opaque `Cursor` type shared by both methods | Compatible but adds unused generality | Contradicts "opaque `Cursor`" (singular) in the plan's Produces line |

### Choice: 1 (single opaque `Cursor{after string}`)
Rationale: both tiers sort on the same kind of field (a deterministic, non-empty sha256-hex `id`), so a single string tie-break is sufficient and provably will not need to grow. This is also the literal shape the plan's `Produces` line asks for (one `Cursor` type, not per-tier types). Sacrifices: none identified — approach 2's generality has no current or foreseeable caller.

### Depth Check
- Interface methods added: 2 on `Backend` (`ScanEntities`, `ScanEdges`), 2 on `Store` (thin pass-through wrappers, matching `GetEntity`/`Neighbors`/`CountEntities`'s existing one-line-delegate pattern).
- Hidden details: `search_after` mechanics, the batch-size page bound, the ascending-`id`-sort tie-break, the `index_not_found` → empty translation, the per-tier live-filter clause shape (entity vs edge differ) — all invisible to callers.
- Common case complexity: simple — `cursor := Cursor{}` then loop `items, cursor, err := store.ScanEntities(ctx, tenantID, cursor)` until `cursor == Cursor{}` (after the first non-empty page) with no per-call setup/teardown, mirroring the plan's stated "nil/zero = start, zero next = exhausted" contract.

### Implementation notes
- New unexported package var `scanBatchSize = 500`: production default, but a `var` (not `const`) specifically so in-package white-box tests can shrink it to exercise real multi-page pagination without needing 500+ fixture records. This is the one deliberate concession to testability over immutability; it carries no exported surface, so it cannot be misused by callers outside the package.
- OpenSearch page-boundary detection: request `size: scanBatchSize`; if the returned hit count equals `scanBatchSize`, set `next = Cursor{after: <last hit's id>}`, otherwise `next = Cursor{}` (exhausted). This means a tenant whose live-record count is an exact multiple of the batch size costs one extra empty round trip to detect exhaustion — an accepted, standard pagination trade documented in the plan's edge-case list ("page boundary exactly on batch size" — verified this returns cleanly on the following call, not that it must avoid the extra round trip).
- `MemBackend` implements the identical page-boundary contract in-memory (sort tenant+live-filtered docs by `ID` ascending, binary-search for the cursor's `after` id, slice `scanBatchSize` items) so its observable pagination behavior is a faithful stand-in for `OpenSearchBackend` in unit tests.
- Live filter differs per tier per the plan constraint: entities use `Entity.Live()` (`expired_at==nil`); edges use `Edge.Live()` (`invalid_at==nil && expired_at==nil`) — reusing the existing methods rather than re-deriving the filter logic, so there is exactly one place per tier that defines "live."
- `isIndexNotFound` (opensearch.go:297) is reused unchanged to translate a missing index into `(nil, Cursor{}, nil)` — mirrors `CountEntities`'s existing guard.

## Prerequisites
- [x] Required files exist (`internal/graph/store.go`, `internal/graph/opensearch.go`, both templates)
- [x] Dependencies available (no new external deps)
- [x] Assumption verified (`id` is keyword-mapped on both indices)

## Recommendation
BUILD. No plan/reality gap found; the assumption holds; scope is fully coverable with the existing `MemBackend`/`OpenSearchBackend` split and no new files beyond edits to `store.go`, `opensearch.go`, and a new `scan_test.go`.

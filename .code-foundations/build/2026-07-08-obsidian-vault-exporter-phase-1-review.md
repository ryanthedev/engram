# Review: Phase 1 - Graph scan primitive

## Executed Results (Step 0)
- Test suite: `go test ./internal/graph/...` → 69 passed, 0 failed
- Test suite (full repo): `go test ./...` → 468 passed in 40 packages, 0 failed
- Typecheck/vet: `make lint` (runs `go vet ./...` then revive) → clean, no output, exit 0
- Build: `go build ./...` → Success
- DW-tagged tests individually re-run with `-v`: all 14 `TestDW_1_*`/`TestBoundary_*`/`TestEdgeCase_*`/`TestScanEntities_Resumes*` tests PASS (see trace below)
- Coverage (`go tool cover -func`): `store.go` Scan methods, `MemBackend` Scan methods, `scanQuery`, `nextScanCursor`, `scanPage` all 100%. `OpenSearchBackend.ScanEntities`/`ScanEdges` bodies show 0% — expected per dispatch instructions (integration-tagged, no live cluster in this run); verified instead by code inspection below.

## Requirement Fulfillment

### DW-1.1
PREMISE:  `ScanEntities`/`ScanEdges` on `Backend`, `Store`, `MemBackend`, and OpenSearch backend, using `search_after` + total sort.
EVIDENCE: `internal/graph/store.go:60-64` (Backend interface), `store.go:302-309` (Store passthrough), `store.go:487-514` (MemBackend, sort+binary-search resume), `internal/graph/opensearch.go:296-357` (OpenSearchBackend), `opensearch.go:363-376` (`scanQuery`: `"sort": [{"id":"asc"}]` always present, `"search_after": [cursor.after]` added only when resuming)
TRACE:    `scanQuery("t1", Cursor{}, ...)` → no `search_after` key, `sort` present (verified by `TestDW_1_1_ScanQueryShape_UsesSortAndSearchAfter`, PASS). `scanQuery("t1", Cursor{after:"abc123"}, ...)` → `search_after: ["abc123"]` present. MemBackend implements the equivalent contract via `sort.Slice` ascending-by-id + `sort.Search` binary resume (`store.go:527-542`), which is the documented in-memory stand-in for OpenSearch's `search_after` (doc comment `store.go:516-526` explains the deliberate parity).
VERDICT:  PASS

### DW-1.2
PREMISE:  Full-tier iteration returns every live record across multiple pages (verified past one batch size), never truncating.
EVIDENCE: `internal/graph/scan_test.go:138-172` (`TestDW_1_2_ScanEntities_PaginatesAcrossMultipleBatches`, batch=3, total=10), `scan_test.go:176-204` (`TestDW_1_2_ScanEdges_PaginatesAcrossMultipleBatches`, batch=4, total=13)
TRACE:    10 entities inserted, `scanBatchSize` shrunk to 3 (`withScanBatchSize`), `scanAllEntities` drains via the cursor loop (`scan_test.go:35-52`) → exactly the 10 inserted ids returned once each, no duplicates, no gaps. Edge case: 13 edges / batch 4 (4+4+4+1) → all 13 returned once. Both tests PASS.
VERDICT:  PASS

### DW-1.3
PREMISE:  Only live records surface — entities `expired_at==nil`; edges `invalid_at==nil && expired_at==nil`.
EVIDENCE: `internal/graph/graph.go:87` (`Entity.Live()`), `graph.go:121` (`Edge.Live()`), `store.go:492` / `store.go:508` (MemBackend filters `e.Live()` before scanning), `opensearch.go:297` (entity `must_not exists expired_at`), `opensearch.go:329-332` (edge `must_not exists expired_at` AND `must_not exists invalid_at`)
TRACE:    `TestDW_1_3_ScanEntities_ExcludesExpired`: one live + one with `ExpiredAt` set → scan returns only `"live"`. PASS. `TestDW_1_3_ScanEdges_ExcludesClosedAndExpired`: live + `InvalidAt`-only (closed) + `ExpiredAt`-only → scan returns only `"live"`, confirming the edge tier's stricter dual condition (a closed-but-not-expired edge is correctly excluded, distinct from the entity tier which has no InvalidAt at all). PASS.
VERDICT:  PASS

### DW-1.4
PREMISE:  Results scoped to the passed `tenantID`; empty/missing index yields empty, not error.
EVIDENCE: `store.go:386` / `store.go:492` / `store.go:508` (MemBackend tenant filter), `opensearch.go:368` (`term: tenant_id`), `opensearch.go:306-308` / `opensearch.go:341-343` (`isIndexNotFound` → empty page + zero cursor, not error), `opensearch.go:402-409` (`isIndexNotFound` matches only HTTP 404 + `index_not_found_exception`)
TRACE:    `TestDW_1_4_ScanEntities_TenantScoped`/`_ScanEdges_TenantScoped`: tenant-a and tenant-b each get one record; scanning tenant-a returns only tenant-a's record. PASS. `TestDW_1_4_ScanEntities_EmptyTenant_NoError`: scanning a tenant with zero records → `items=nil, next=Cursor{}, err=nil`. PASS. OpenSearchBackend's missing-index path verified by code inspection (no live cluster available in this run, per dispatch instructions): `isIndexNotFound` gates both `ScanEntities` and `ScanEdges` before the generic status check, returning `(nil, Cursor{}, nil)`.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have corresponding tests that ran in Step 0 (`TestDW_1_1_*`, `TestDW_1_2_*`, `TestDW_1_3_*`, `TestDW_1_4_*`, all PASS)
- [x] Test coverage matches the stated level (100%) for every reachable line outside the OpenSearch-integration-only code paths, which the dispatch explicitly carves out (no live cluster) and which were instead verified by code inspection above
- No gaps found

## Dead Code
None found. No unused imports, no unreachable code after early returns, no debug statements (`fmt.Println`/`println`), no commented-out blocks in `store.go`, `opensearch.go`, or `scan_test.go`.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | `MemBackend.ScanEntities`/`ScanEdges` (`store.go:487-514`) each take `m.mu.Lock()` for their full read+sort, matching every other MemBackend method's locking pattern; no read escapes the lock. Traced a concurrent `PutEntity` racing a `ScanEntities` call — the mutex serializes them, so each Scan call sees a consistent (if possibly stale) snapshot, matching the documented "snapshot-per-page, not point-in-time" contract (`opensearch.go:289-295`). No data race possible. |
| Error Handling | PASS | All OpenSearch HTTP/JSON errors from `osJSON`/`osDo` propagate via wrapped `fmt.Errorf` (`opensearch.go:300`, `opensearch.go:335`); MemBackend Scan paths cannot fail (in-memory) and correctly return `nil` error. |
| Resources | N/A | No file handles, connections, or locks are held across calls; `http.Client` is caller-supplied and reused (no per-call allocation of unbounded resources). |
| Boundaries | PASS | Traced the adversarial case: cursor.after equal to the greatest live id — `sort.Search` in `scanPage` (`store.go:527-542`) returns `len(sorted)`, `start >= len(sorted)` is true, function returns `(nil, Cursor{}, nil)` — correctly reports exhaustion, doesn't panic on `sorted[start:end]` slicing. Also traced a cursor whose `after` sorts strictly between two real ids ("bb" between "b" and "c", `TestScanEntities_ResumesFromArbitraryCursor_NoDuplicatesOrGaps`) — binary search correctly resumes after "b", returns exactly `{c,d,e}`, no duplicates/gaps. PASS. |
| Security | N/A | Tenant scoping and cursor values are passed as structured JSON query fields (`map[string]any` → `json.Marshal`), never string-concatenated into a query — no injection surface. `GetEntity`/`GetEdge` fail-closed on cross-tenant id collision (`opensearch.go:163-165`), consistent with the tenant-scoping this phase adds to Scan. |

## Loaded-Skill Criteria

*(code-foundations:aposd-designing-deep-modules — applied retrospectively to assess the shipped Scan interface's depth, since this skill is normally a pre-implementation design gate; treated as review criteria per dispatch instructions.)*

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| aposd-designing-deep-modules | Interface simplicity — "simplest interface that covers all current needs" | PASS | `Backend` gains exactly 2 methods (`ScanEntities`, `ScanEdges`) with an identical shape to each other and to the existing `CandidateEntities`/`Neighbors` methods already on the interface — no proliferation of scan variants (no separate "first page" vs "next page" method, no exposed batch-size parameter). |
| aposd-designing-deep-modules | Information hiding — internal pagination mechanics not leaked to callers | PASS | `Cursor.after` is unexported (`store.go:76-82`); callers can only round-trip a `Cursor` value, never construct or inspect one. `scanBatchSize` (`store.go:357`) is package-private, not part of any exported signature. OpenSearch's `search_after` mechanism is entirely hidden behind the opaque `Cursor` — a caller cannot tell whether they're talking to MemBackend or OpenSearchBackend from the Scan contract alone. |
| aposd-designing-deep-modules | Common-case simplicity | PASS | The documented calling convention is a single loop: `for { page, next, err := Scan(...); ...; if next == (Cursor{}) { break }; cursor = next }` — no setup, no configuration object, matches the actual test helper (`scanAllEntities`, `scan_test.go:35-52`) used throughout the test file. |
| aposd-designing-deep-modules | Silent-failure red flag — must surface failure, not swallow it | PASS (N/A finding) | Every Scan implementation returns `err` on the one path that can actually fail (the OpenSearch HTTP round-trip); MemBackend cannot fail internally and correctly returns `nil` rather than fabricating an error. No case was found where a failure is absorbed and reported as a normal empty page — "missing index" is a deliberate, documented non-error (not a swallowed failure), matching `CountEntities`'s existing precedent in the same file. |

## Notes (non-blocking)
- `scanBatchSize` is a package-level mutable `var` (not `const`), justified by its doc comment as an in-package test-only injection point (`store.go:352-357`) with `t.Cleanup` restoration (`scan_test.go:15-20`). This is a mild global-mutable-state smell but is scoped, documented, restored per-test, and cannot be reached from outside the package — not a correctness defect.
- `OpenSearchBackend.ScanEntities`/`ScanEdges` show 0% line coverage in this run because the only tests that exercise them (`opensearch_integration_test.go`) are `//go:build integration`-tagged and require a live cluster, per the dispatch's explicit instructions. The query-shape logic they call (`scanQuery`, `nextScanCursor`, `isIndexNotFound`) is separately unit-tested at 100% and directly code-inspected above; the HTTP plumbing itself (`osJSON`/`osDo`) is shared, already-existing, unchanged code exercised by every other OpenSearchBackend method's integration tests.
- A tenant whose live-record count is an exact multiple of `scanBatchSize` costs one extra empty round-trip to detect exhaustion — explicitly documented as an accepted trade (`opensearch.go:378-383`), not a defect.

## Issues (if FAIL)
None.

**Verdict: PASS**

# Review: Phase 2 - Export RPC + server wiring (sample 2)

## Executed Results (Step 0)
- Test suite: `go test ./internal/server/... ./internal/engramclient/... ./internal/graph/...` → 98 passed, 0 failed (all DW-named tests pass; verbose run enumerated)
- Proto check: `make proto-check` → codegen ok, `git diff --exit-code -- api/engrampb` clean, exit 0
- Lint: `make lint` (go vet + revive) → exit 0, no findings
- Build: `go build ./...` → success
- Coverage: `go test -coverprofile ./internal/server/` → export.go: `Export` **88.6%**; encode/decode/canExport/proto mappers 100%. Uncovered blocks at export.go:113 (entity-scan error), :135 (edge-scan error), :142 (edge ACL error), :149 (edges-tier continuation cursor).
- Reviewer probe (overlay, not committed): 2 entities + 501 edges walked to exhaustion — edges tier paged across the 500 boundary, every edge exactly once, cursor advanced then emptied. PASS. (`go test -overlay=...` — working tree untouched, verified via `git status`.)

## Requirement Fulfillment

### DW-2.1
PREMISE:  "`Export` RPC defined in proto and regenerated code committed; `make proto-check` clean."
EVIDENCE: api/proto/engram.proto:59 (`rpc Export(ExportRequest) returns (ExportResponse)`), :165/:173/:192/:210 (ExportRequest/ExportEntity/ExportEdge/ExportResponse); api/engrampb/engram_grpc.pb.go:46,80,172; api/engrampb/engram.pb.go:730,778,900,1028 (NextCursor field 3)
TRACE:    `make proto-check` → codegen.sh regenerates → `git diff --exit-code -- api/engrampb` → exit 0 (committed generated code matches proto)
VERDICT:  PASS

### DW-2.2
PREMISE:  "Handler returns a bounded page of live entities+edges for the caller's tenant sourced via the graph scan; `next_cursor` advances and empties on exhaustion."
EVIDENCE: internal/server/export.go:97-153 (ScanEntities/ScanEdges via Exporter seam :112/:134; per-tier bound = scan batch 500, store.go:373; continuation :127-131, :149-151)
TRACE:    501 entities + 3 edges (incl. one expired entity, one invalidated edge) → page 1: 500 entities + cursor; page 2: 1 entity, tier exhausts, chains into 3 edges, empty cursor. Every record exactly once; dead records never appear. `TestDW_2_2_ExportPagesToExhaustion`, `TestDW_2_2_ExportPageExactlyOnBoundContinues` — pass.
VERDICT:  PARTIAL — the "next_cursor advances" clause is only tested for the entities tier. No committed test drives >500 edges, so the edges-tier continuation (export.go:149-151) has **zero committed execution evidence** (coverage block :149 = 0 hits). A stage-encoding bug there (e.g. `Stage: stageEntities` at :150) would pass the entire committed suite. Reviewer probe shows the code IS correct — this is a test gap, not a defect.

### DW-2.3
PREMISE:  "Tenant pinned from verified identity; a token for tenant A never receives tenant B's records (test)."
EVIDENCE: internal/server/export.go:101-104 (identity from context, fail-closed on missing/empty tenant; ExportRequest has NO tenancy field — engram.proto:165-167); wire cursor carries no tenancy (export.go:44-50)
TRACE:    Tenants A and B each seeded 501+3 → walk as A → exactly A's 501/3, zero `tenant-b`-prefixed ids on any page. `TestDW_2_3_ExportTenantIsolation` pass; `TestDW_2_3_ExportNoIdentityRejected` (no identity → Unauthenticated) pass.
VERDICT:  PASS

### DW-2.4
PREMISE:  "Records failing `ACL.CanRead` are omitted; nil `Exporter` seam → `Unimplemented`."
EVIDENCE: internal/server/export.go:98-100 (nil seam → Unimplemented), :116-126/:138-148 (per-record canExport; denied → skip, error → Internal), :158-163
TRACE:    5 entities/2 edges, ACL denies sentinel owner → 4/1 returned, denied ids absent, call succeeds (`TestDW_2_4_ExportACLDeniedRecordsOmitted`); ACL error → Internal, no partial page (`TestDW_2_4_ExportACLErrorFailsClosed`); `Server{}` → Unimplemented (`TestDW_2_4_ExportNilExporterUnimplemented`). All pass.
VERDICT:  PASS

### DW-2.5
PREMISE:  "`engramclient.Export` returns a page + cursor; unauthenticated call rejected by the existing interceptor."
EVIDENCE: internal/engramclient/client.go:130-132; internal/engramclient/client_test.go:43-70 (real gRPC server on 127.0.0.1:0 behind the production `authgrpc.UnaryServerInterceptor`)
TRACE:    501 entities over real TCP with good token → page 1: 500 + cursor, page 2: 1 + empty cursor (`TestDW_2_5_ClientExportReturnsPageAndCursor`); wrong and empty tokens → `codes.Unauthenticated` before handler (`TestDW_2_5_ClientExportUnauthenticatedRejected`). Both pass.
VERDICT:  PASS

**All requirements met:** NO — DW-2.2 edges-tier continuation clause lacks committed test evidence (behavior verified correct by reviewer probe).

## Test-DW Coverage
- [x] All DW items have DW-named tests that ran in Step 0 (TestDW_2_2_*, TestDW_2_3_*, TestDW_2_4_*, TestDW_2_5_*; DW-2.1 via executed `make proto-check`)
- [ ] Test coverage matches the stated level (100%): **NO** — `Export` handler measures 88.6%. Uncovered: entity-scan error path (:113), edge-scan error path (:135), edge ACL-error path (:142), edges-tier continuation cursor (:149). The last is a DW-2.2 behavior clause; the error paths need an erroring Exporter fake (MemBackend never errors) and an edges-seeded ACL-error case.

## Edge Cases (prompt-listed)
| Case | Evidence | Status |
|---|---|---|
| unauthenticated → interceptor Unauthenticated | client_test.go:103-116 over real TCP; plus in-process defense-in-depth export.go:101-104 | PASS |
| empty tenant → one empty page, terminal cursor | `TestDW_2_2_ExportEmptyTenantOneTerminalPage`: 0/0/"" in a single call (entity tier chains into edges in-call, export.go:131) | PASS |
| page exactly filling the bound | `TestDW_2_2_ExportPageExactlyOnBoundContinues`: 500 entities → full page + continuation, no in-page chain; next call 0 entities + 2 edges + terminal | PASS |
| record failing CanRead skipped, not fatal | `TestDW_2_4_ExportACLDeniedRecordsOmitted`: denial skips, page continues, call succeeds | PASS |
| stale/garbage cursor: no error-oracle, no cross-tenant leak | `TestExportGarbageCursorInvalidArgument`: 3 garbage forms (bad base64, non-JSON, unknown stage) all → InvalidArgument with the SAME opaque "invalid cursor" message. `TestExportStaleCursorStaysSafe`: valid cursor replayed against emptied backend → clean exhaustion; against a backend holding only tenant-b data → zero foreign records. Cursor carries no tenancy (export.go:44-50), tenant re-pinned per call. `TestCursorUnmarshalGarbageIsHarmless` covers the graph sub-cursor. | PASS |

## Dead Code
None found in the reviewed files — no unused imports (build+lint clean), no unreachable code, no debug statements, no commented-out blocks.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | Handler is stateless — no fields written, all state in ctx/req/locals; concurrent Exports share only the Exporter seam whose thread-safety is Phase 1's contract (given). No defect demonstrable. |
| Error Handling | PASS | Adversarial inputs traced: scan error → Internal (:113,:135), ACL error → Internal fail-closed (tested), undecodable cursor → opaque InvalidArgument (tested, 3 forms). One deliberately ignored error (encode, :54) is provably infallible (string-only fields + infallible MarshalText, store.go:88). |
| Resources | N/A | Handler opens nothing; client tests Close() connections; test server stopped via t.Cleanup. |
| Boundaries | PASS | 0 records, exactly 500, 501, >500 edges (reviewer probe), stale position past end — all traced/executed; entities→edges chain resets sub-cursor to zero (:131). |
| Security | PASS | Tenant from verified identity only, no request tenancy field, cursor tenancy-free, per-record ACL with fail-closed errors, uniform opaque cursor rejection (no decode-detail oracle), denied indistinguishable from absent, internal fields (Embedding/NameKey) never mapped to wire (`TestExportRecordFieldMapping`). |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| ca-architecture-boundaries | Dependency direction: business/domain never depends on adapter | PASS | `Exporter` is a consumer-defined seam in the server package (export.go:29-32); graph (inner) implements it (store.go:318/324, MemBackend :503/:519) without importing server. Handler (adapter) → domain types: arrows point inward. |
| ca-architecture-boundaries | SRP by actor / no cross-actor coupling | PASS | Export isolated in export.go with its own seam, mirroring StatusProbe/Auditor; no shared mutable helpers with Ingest/Search/Audit actors. |
| ca-architecture-boundaries | Boundary carries no client-controlled authority | PASS | Wire cursor carries stage+position only, never tenancy (export.go:44-50); ExportRequest has no tenant field at all (engram.proto:165-167). |
| cc-defensive-programming | External input validated at entry (barricade) | PASS | The one client-controlled input (cursor) is decoded/validated before any scan (export.go:105-108); all three malformed forms rejected opaquely (executed test). |
| cc-defensive-programming | No empty catch blocks / swallowed errors | PASS | Single ignored error at export.go:54 is justified inline and provably infallible; every other error checked. |
| cc-defensive-programming | Security-critical path: defense in depth, fail closed | PASS | Identity re-checked in-process even behind the interceptor (:101-104 — stricter than the pre-existing Ingest fallback); ACL uncertainty aborts the call (:121,:143, tested); nil-Exporter fails Unimplemented. |
| cc-defensive-programming | Correctness over robustness on data disclosure | PASS | ACL error → whole call fails (never a partial trusted page), tested `TestDW_2_4_ExportACLErrorFailsClosed`. |

## Notes (non-blocking)
- `Internal` status messages embed backend error text (`"export entities: %v"`, export.go:114/:136) — reaches clients; consistent with the existing Audit/Search/Ingest convention, so noted only.
- Nothing couples `Exporter` and `ACL` wiring: a deployment that sets Exporter but leaves ACL nil exports the whole tenant with no scope check. That nil-skips contract is documented (server.go:40-44) and production wires both (main.go:275-277), so this is configuration-risk, not a defect.
- Export's identity handling is stricter than Ingest's request-field fallback — a good asymmetry worth keeping when Ingest is next touched.

## Issues (FAIL)
1. Stated coverage level (100%) not met on the new handler: `Export` = 88.6%, and the uncovered edges-tier continuation is a DW-2.2 behavior clause with no committed test.
   - File: internal/server/export.go:149-151 (also uncovered error branches :113-115, :135-137, :142-144)
   - Demonstrated by: `go tool cover -func` → `Export 88.6%`; cover profile blocks 113.17/135.16/142.17/149.30 at 0 hits. No committed test seeds >500 edges, so a stage-encoding bug at :150 would pass the whole suite. (Reviewer overlay probe with 501 edges confirms current behavior is correct — this is a test gap, not a code defect.)
   - Fix: add an edges-tier pagination test (e.g. 2 entities + 501 edges walked to exhaustion, each edge exactly once — the reviewer's probe at /tmp/p2-review-sample-2/export_edges_paging_test.go is a ready-made template); add an erroring `Exporter` fake to cover both scan-error → Internal branches, and an edges-seeded ACL-error case for :142.

**Verdict: FAIL — blocker: DW-2.2 edges-tier `next_cursor` advancement has no committed execution evidence and measured coverage (88.6%) does not meet the stated 100% level. Code behavior itself verified correct; fix is test-only.**

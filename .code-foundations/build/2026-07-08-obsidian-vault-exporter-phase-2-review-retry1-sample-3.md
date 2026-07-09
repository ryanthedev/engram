# Review: Phase 2 - Export RPC + server wiring (retry1, sample 3)

## Executed Results (Step 0)
- Test suite: `go test ./internal/server/... ./internal/engramclient/... ./internal/graph/...` → 102 passed, 0 failed (server: 24 tests, all `--- PASS`; engramclient + graph export/cursor tests all `--- PASS`)
- Build: `go build ./...` → success
- Lint: `make lint` (go vet + revive) → clean, exit 0
- Proto: `make proto-check` (codegen + `git diff --exit-code -- api/engrampb`) → clean, exit 0
- Coverage: `go test -coverprofile=/tmp/p2-rereview-sample-3/cov.out ./internal/server/...` + `go tool cover -func | grep export` →
  `encodeExportCursor 100.0%`, `decodeExportCursor 100.0%`, `Export 100.0%`, `canExport 100.0%`, `exportEntityProto 100.0%`, `exportEdgeProto 100.0%` — **export.go measures 100%**

## Requirement Fulfillment

### DW-2.1
PREMISE:  "`Export` RPC defined in proto and regenerated code committed; `make proto-check` clean."
EVIDENCE: api/proto/engram.proto:59 (`rpc Export(ExportRequest) returns (ExportResponse)`), messages at :165/:173/:192/:210; api/engrampb/engram.pb.go:730/778/900/1028 (ExportRequest/ExportEntity/ExportEdge/ExportResponse), engram_grpc.pb.go contains 17 Export references (client + server stubs).
TRACE:    `make proto-check` → runs `./scripts/codegen.sh` (regenerates) → `git diff --exit-code -- api/engrampb` → exit 0, proving committed generated code matches the proto exactly.
VERDICT:  PASS

### DW-2.2
PREMISE:  "Handler returns a bounded page of live entities+edges for the caller's tenant sourced via the graph scan; `next_cursor` advances and empties on exhaustion."
EVIDENCE: internal/server/export.go:97–153 (Export handler: ScanEntities at :112, full-page continuation at :127–130, chain into edges at :131, ScanEdges at :134, edge continuation at :149–151, terminal empty cursor by omission).
TRACE:    501 live entities + 3 edges + 1 expired entity + 1 invalidated edge seeded → walkExport pages with the returned cursor until empty → 501 entities + 3 edges, each exactly once, ≥2 pages, expired/invalidated records absent (TestDW_2_2_ExportPagesToExhaustion, PASS). Edge-tier mirror: 501 edges, 0 entities → multi-page edge walk, no duplicates (TestDW_2_2_ExportEdgesPageToExhaustion, PASS).
VERDICT:  PASS

### DW-2.3
PREMISE:  "Tenant pinned from verified identity; a token for tenant A never receives tenant B's records (test)."
EVIDENCE: internal/server/export.go:101–104 (identity from `authgrpc.IdentityFrom`; `!ok || id.TenantID == ""` → Unauthenticated), :112 and :134 (`id.TenantID` passed to both scans; `ExportRequest` has no tenancy field — proto :165–167); wire cursor carries no tenancy (export.go:44–50).
TRACE:    tenant-a and tenant-b each seeded 501 entities + 3 edges → identity bound to tenant-a walked to exhaustion → exactly its own 501/3, zero ids prefixed `tenant-b` (TestDW_2_3_ExportTenantIsolation, PASS); identity-less context → codes.Unauthenticated (TestDW_2_3_ExportNoIdentityRejected, PASS).
VERDICT:  PASS

### DW-2.4
PREMISE:  "Records failing `ACL.CanRead` are omitted; nil `Exporter` seam → `Unimplemented`."
EVIDENCE: internal/server/export.go:98–100 (nil Exporter → Unimplemented), :117–126 and :139–148 (per-record canExport; denied → skipped; ACL error → Internal fail-closed), :158–163 (canExport).
TRACE:    5 entities + 2 edges with one of each marked `deny-me` and an ACL that denies on that sentinel → export returns 4/1 and neither denied id appears, call succeeds (TestDW_2_4_ExportACLDeniedRecordsOmitted, PASS); `&server.Server{}` → codes.Unimplemented (TestDW_2_4_ExportNilExporterUnimplemented, PASS); erroring ACL on entity and edge tiers → codes.Internal, nil response (TestDW_2_4_ExportACLErrorFailsClosed, TestDW_2_4_ExportEdgeACLErrorFailsClosed, both PASS).
VERDICT:  PASS

### DW-2.5
PREMISE:  "`engramclient.Export` returns a page + cursor; unauthenticated call rejected by the existing interceptor."
EVIDENCE: internal/engramclient/client.go:130–132 (Export method); internal/engramclient/client_test.go:43–70 (real gRPC server on `127.0.0.1:0` behind the production `authgrpc.UnaryServerInterceptor`).
TRACE:    501 entities seeded, good token → page 1: 500 entities + non-empty cursor; page 2 with that cursor: 1 entity + empty cursor (TestDW_2_5_ClientExportReturnsPageAndCursor, PASS); wrong token and empty token over the same real connection → codes.Unauthenticated from the interceptor before the handler (TestDW_2_5_ClientExportUnauthenticatedRejected, PASS).
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have DW-ID-named automated tests that ran in Step 0 (TestDW_2_2_*, TestDW_2_3_*, TestDW_2_4_*, TestDW_2_5_*; DW-2.1 covered by executed `make proto-check`, the only executable form for a spec/codegen assertion).
- [x] Coverage matches the stated 100% level: every function in internal/server/export.go measures 100.0% (`go tool cover -func`).
- Supplementary: cursor round-trip incl. through JSON (TestCursorTextRoundTrip), garbage-byte cursor harmlessness at the graph layer (TestCursorUnmarshalGarbageIsHarmless), field mapping incl. Embedding/NameKey exclusion (TestExportRecordFieldMapping), scan-error fail-closed on both tiers.
No gaps.

## Dead Code
None found. No unused imports (build + vet clean), no unreachable code, no debug statements, no commented-out blocks in export.go, server.go, client.go, or the test files.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | Handler is stateless: all state is per-call locals; Server seam fields are wired once at startup (main.go:271–277) and only read thereafter. No shared mutable state to race. |
| Error Handling | PASS | Adversarial paths traced and test-demonstrated: scan error either tier → Internal + nil response; ACL error either tier → Internal + nil response; undecodable/unknown-stage cursor → opaque InvalidArgument before any scan. The one ignored error (`json.Marshal` at export.go:54) traced: exportCursor is a string field + graph.Cursor whose MarshalText (store.go:88) returns nil unconditionally — cannot fail. |
| Resources | N/A | No files, connections, locks, or goroutines opened by the handler; the client's gRPC conn is closed by callers (tests use defer c.Close()). |
| Boundaries | PASS | Empty tenant (one empty terminal page), exactly-500 full page (continuation then clean chain, no lost/duplicated record across the tier boundary), 501 (one past bound) on both tiers, stale cursor against emptied backend — all traced and test-demonstrated. |
| Security | PASS | Attack surface = the one client input (cursor). Traced: crafted cursor `{"s":"edges","a":"anything"}` decodes, but scans are pinned to `id.TenantID` (export.go:112/:134) and the cursor carries no tenancy — worst case repositions within the caller's own tenant (TestExportStaleCursorStaysSafe replays a t1 cursor against tenant-b data: 0 foreign records). Garbage cursor → uniform opaque "invalid cursor" message, no decode-detail oracle (asserted in TestExportGarbageCursorInvalidArgument). Empty-tenant identity fails closed (export.go:102). Internal fields (Embedding, NameKey) never leave the server (TestExportRecordFieldMapping). |

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| ca-architecture-boundaries | Dependencies point inward: business layer defines the seam, infrastructure implements it | PASS | `Exporter` is consumer-defined in internal/server (export.go:29–32); concrete `*graph.Store` bound only at the composition root (main.go:277). Server imports graph domain types, not the backend. |
| ca-architecture-boundaries | Business logic runs without infrastructure | PASS | Entire handler suite runs against in-memory fakes (MemBackend, aclFunc) — 24 tests, no cluster. |
| ca-architecture-boundaries | SRP by actor / no actor coupling introduced | PASS | Export lives in its own file with its own seam, mirroring the existing StatusProbe/Auditor pattern; no existing handler modified. |
| cc-defensive-programming | External input validated at entry | PASS | The cursor is the sole client-controlled input; decodeExportCursor (export.go:62–78) rejects non-base64url, non-JSON, and unknown stages before any scan; rejection is opaque. |
| cc-defensive-programming | Barricade does not replace defense-in-depth on security-critical paths | PASS | Identity re-verified inside the handler (export.go:101–104) even though the interceptor already barricades — an in-process caller bypassing the interceptor still fails closed (TestDW_2_3_ExportNoIdentityRejected exercises exactly this). |
| cc-defensive-programming | No empty catch blocks / no swallowed errors | PASS | Every error path returns a status code; the single `_ =` on json.Marshal (export.go:54) is justified inline and traced as infallible (MarshalText at store.go:88 never errors). |
| cc-defensive-programming | Fail closed under uncertainty (correctness over robustness for security) | PASS | ACL error and scan error abort the whole call with Internal and no partial page — demonstrated on both tiers by four fail-closed tests. |
| cc-defensive-programming | Anticipated runtime errors handled, not asserted | PASS | No assertions used; bad cursors, missing identity, backend failures all map to gRPC status codes. |

## Edge Cases (prompt-listed)
| Edge case | Status | Evidence |
|---|---|---|
| Unauthenticated → interceptor rejects | HANDLED | TestDW_2_5_ClientExportUnauthenticatedRejected — real interceptor over real TCP, wrong + empty token → Unauthenticated |
| Empty tenant → one empty page, terminal cursor | HANDLED | TestDW_2_2_ExportEmptyTenantOneTerminalPage — 0/0 records, empty cursor, no error, single round trip (chain-into-edges at export.go:131 avoids a second call) |
| Page exactly filling the bound | HANDLED | TestDW_2_2_ExportPageExactlyOnBoundContinues — 500 entities: full page + continuation cursor, no premature edge chaining; next call finds tier exhausted and returns the edges |
| Record failing CanRead skipped, not fatal | HANDLED | TestDW_2_4_ExportACLDeniedRecordsOmitted — denied entity and edge omitted, call succeeds, page continues |
| Stale/garbage cursor: no error oracle, no cross-tenant leak | HANDLED | TestExportGarbageCursorInvalidArgument (uniform opaque message across three malformation classes) + TestExportStaleCursorStaysSafe (replayed cursor over emptied backend and over foreign-tenant data → clean exhaustion, zero foreign records) |

## Notes (non-blocking)
- `status.Errorf(codes.Internal, "export entities: %v", err)` (export.go:114, :121, :136, :143) embeds backend error text in the wire status. This matches every existing handler in server.go (Ingest/Search/Audit do the same), so it is the established codebase pattern, but if backend errors ever carry sensitive detail this is a cross-cutting hardening candidate — for all handlers, not just Export.
- `decodeExportCursor` returns a `status.Error` for the unknown-stage case (export.go:75) that the caller immediately discards and re-wraps opaquely (export.go:107). Harmless (the opacity is what the test asserts), but the inner status is dead detail; a plain `errors.New` would be marginally cleaner.
- A partially-full entity page that exhausts the tier chains into a full edge page in the same response (< 2×500 records) — documented at export.go:90–92 and within the stated bound; noted only so the Phase 3 consumer sizes buffers for both tiers per page.

## Issues
None.

**Verdict: PASS**

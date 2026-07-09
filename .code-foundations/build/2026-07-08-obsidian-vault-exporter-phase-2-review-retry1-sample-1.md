# Review: Phase 2 - Export RPC + server wiring (retry 1, sample 1)

## Executed Results (Step 0)
- Test suite: `go test ./internal/server/... ./internal/engramclient/... ./internal/graph/...` → 102 passed, 0 failed (3 packages)
- Per-test: `go test -v -run 'Export|Cursor' ...` → 21/21 PASS, 0 FAIL, 0 SKIP (/tmp/p2-rereview-sample-1/verbose.txt)
- Coverage: `go test -coverprofile=/tmp/p2-rereview-sample-1/cov.out ./internal/server/...` + `go tool cover -func` → export.go: `encodeExportCursor` 100.0%, `decodeExportCursor` 100.0%, `Export` 100.0%, `canExport` 100.0%, `exportEntityProto` 100.0%, `exportEdgeProto` 100.0%
- Proto: `make proto-check` → `codegen: ok`, `git diff --exit-code -- api/engrampb` clean, exit 0
- Lint: `make lint` (go vet + revive) → exit 0
- Build: `go build ./...` → success

## Requirement Fulfillment

### DW-2.1
PREMISE:  "`Export` RPC defined in proto and regenerated code committed; `make proto-check` clean."
EVIDENCE: api/proto/engram.proto:59 (`rpc Export(ExportRequest) returns (ExportResponse)`), :165-166 (ExportRequest.cursor), :173/:192/:210-213 (ExportEntity/ExportEdge/ExportResponse with entities, edges, next_cursor); generated api/engrampb/engram_grpc.pb.go:46,80,131,172 (client + server Export methods present)
TRACE:    `make proto-check` regenerates into api/engrampb → `git diff --exit-code -- api/engrampb` → no diff → exit 0, meaning committed generated code matches the proto.
VERDICT:  PASS (executed: proto-check exit 0; generated shapes spot-checked)

### DW-2.2
PREMISE:  "Handler returns a bounded page of live entities+edges for the caller's tenant sourced via the graph scan; `next_cursor` advances and empties on exhaustion."
EVIDENCE: internal/server/export.go:111-152 (entities via `s.Exporter.ScanEntities` at :112, edges via `ScanEdges` at :134; continuation cursor emitted at :127-130 and :149-151, omitted when tier cursor is zero)
TRACE:    501 entities + 3 edges seeded → page 1 = 500 entities + non-empty next_cursor (full page never chains, export.go:127-130) → page 2 = 1 entity, entity tier exhausts, chains into edges (:131), 3 edges, next_cursor "" → walk terminates. Expired entity and invalidated edge never appear (live filter survives the handler).
VERDICT:  PASS — TestDW_2_2_ExportPagesToExhaustion, TestDW_2_2_ExportEdgesPageToExhaustion (501 edges, edge-tier continuation branch), TestDW_2_2_ExportPageExactlyOnBoundContinues all PASS in Step 0

### DW-2.3
PREMISE:  "Tenant pinned from verified identity; a token for tenant A never receives tenant B's records (test)."
EVIDENCE: internal/server/export.go:101-104 (identity from `authgrpc.IdentityFrom`, fail-closed Unauthenticated on missing identity or empty TenantID), :112/:134 (`id.TenantID` passed to both scans); api/proto/engram.proto:165-166 (ExportRequest carries ONLY cursor — no request tenancy field exists)
TRACE:    tenants A and B each seeded 501 entities + 3 edges in one backend → walk with tenant-A identity → exactly A's 501/3 returned, zero ids with the tenant-b prefix on any page. Identity-free ctx → codes.Unauthenticated before any scan.
VERDICT:  PASS — TestDW_2_3_ExportTenantIsolation, TestDW_2_3_ExportNoIdentityRejected PASS in Step 0

### DW-2.4
PREMISE:  "Records failing `ACL.CanRead` are omitted; nil `Exporter` seam → `Unimplemented`."
EVIDENCE: internal/server/export.go:98-100 (nil Exporter → codes.Unimplemented), :117-126/:139-148 (per-record `canExport`; denied → skipped, not appended; ACL error → whole call Internal, no partial page), :158-163 (canExport delegates to s.ACL.CanRead per the ReadAuthorizer contract)
TRACE:    5 entities + 2 edges, one of each marked with sentinel owner denied by the fake ACL → export returns 4/1, denied ids absent, call succeeds (denied indistinguishable from absent). `&server.Server{}` (nil Exporter) → Unimplemented. ACL backend error on either tier → Internal, nil response.
VERDICT:  PASS — TestDW_2_4_ExportACLDeniedRecordsOmitted, TestDW_2_4_ExportNilExporterUnimplemented, TestDW_2_4_ExportACLErrorFailsClosed, TestDW_2_4_ExportEdgeACLErrorFailsClosed PASS in Step 0

### DW-2.5
PREMISE:  "`engramclient.Export` returns a page + cursor; unauthenticated call rejected by the existing interceptor."
EVIDENCE: internal/engramclient/client.go:130-132 (Export passes cursor through, returns *ExportResponse); internal/engramclient/client_test.go:58-70 (real gRPC server on 127.0.0.1:0 behind the REAL `authgrpc.UnaryServerInterceptor`)
TRACE:    Dial with good token → page 1 = 500 entities + advancing cursor → page 2 (cursor fed back) = 1 entity + "" cursor. Dial with wrong/empty token → Export → codes.Unauthenticated from the interceptor, handler never runs.
VERDICT:  PASS — TestDW_2_5_ClientExportReturnsPageAndCursor, TestDW_2_5_ClientExportUnauthenticatedRejected PASS in Step 0 (real TCP, OS-assigned port)

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have corresponding tests, named with DW ids, all ran in Step 0 (21/21 PASS)
- [x] Coverage level 100% verified: every function in internal/server/export.go measures 100.0% (`go tool cover -func`), including both scan-error paths (via errExporter fake) and both ACL-error paths — no unreachable lines to report
- Production wiring evidence: cmd/engram-server/main.go:273 (`svc.ACL = aclFilter`) and :277 (`svc.Exporter = graphStore`) — build compiles, so *graph.Store satisfies the Exporter seam (recorded observed behavior; wiring itself is not unit-testable)

## Edge cases (prompt-listed — verdict standing)
| Edge case | Handling | Evidence |
|---|---|---|
| Unauthenticated → Unauthenticated via existing interceptor | Handled; plus in-process fail-closed defense in depth (export.go:101-104) | TestDW_2_5_ClientExportUnauthenticatedRejected, TestDW_2_3_ExportNoIdentityRejected — PASS |
| Empty tenant → one empty page, terminal cursor | Handled: entity tier exhausts, chains into empty edge tier same call (export.go:131), no second round trip | TestDW_2_2_ExportEmptyTenantOneTerminalPage — PASS |
| Page exactly filling the bound | Handled: full page returns continuation without chaining (export.go:127-130); next call finds tier exhausted, chains | TestDW_2_2_ExportPageExactlyOnBoundContinues (exactly 500) — PASS |
| Record failing CanRead skipped, not fatal | Handled (export.go:123-125, :145-147); ACL *error* is fatal by design (fail closed) | TestDW_2_4_ExportACLDeniedRecordsOmitted — PASS |
| Stale/garbage cursor: no error-oracle, no cross-tenant leak | Handled: undecodable/unknown-stage → opaque `InvalidArgument "invalid cursor"` (export.go:105-107, decode detail never reaches client); structurally-valid stale cursor exhausts cleanly within the caller's tenant (cursor carries no tenancy, export.go:47-50; tenant re-pinned every call) | TestExportGarbageCursorInvalidArgument (asserts opaque message), TestExportStaleCursorStaysSafe (replay against emptied backend AND against foreign-tenant data → 0 foreign records) — PASS |

## Dead Code
None found. Lint (vet + revive) clean; no unused imports, no unreachable code, no debug statements or commented-out blocks in export.go, client.go, or the store.go cursor methods.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | Handler is stateless: reads only `s.Exporter`/`s.ACL` (wired once before Serve, main.go:273-278); all page state lives in the client-held cursor. No shared mutable state to race. |
| Error Handling | PASS | Adversarial traces: entity-scan error, edge-scan error, entity-ACL error, edge-ACL error all abort with Internal and nil response (4 dedicated tests, all PASS); no partial-trust page can escape. |
| Resources | N/A | No handles, locks, goroutines, or caches opened by this phase; test server uses t.Cleanup(grpcServer.Stop). |
| Boundaries | PASS | Exactly-500 page traced (no chain on full page); 501 forces the second page; 0-record tenant traced; garbage bytes `\x00garbage\xff` through Cursor.UnmarshalText traced harmless (TestCursorUnmarshalGarbageIsHarmless). MentionCount int→int64 widening is lossless. |
| Security | PASS | Most adversarial input is the cursor: fabricated cursor cannot name a tenant (exportCursor has no tenancy field, export.go:47-50), tenant re-pinned from verified identity per call (:112,:134), replay under foreign-tenant data surfaces 0 records (TestExportStaleCursorStaysSafe traced); decode failures are opaque (no oracle); Embedding/NameKey never serialized (exportEntityProto omits them; TestExportRecordFieldMapping). |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| ca-architecture-boundaries | Dependency direction: consumer-defined seam, no infrastructure import in handler logic | PASS | `Exporter` interface defined in the consuming package (export.go:29-32), mirroring the existing StatusProbe/Auditor pattern; concrete *graph.Store injected at the composition root (main.go:277). Arrows point inward. |
| ca-architecture-boundaries | SRP by actor: handler responsible to one actor | PASS | export.go serves only the export consumer; ACL policy stays in acl.Filter, scan mechanics stay in graph — a change to either does not touch the handler's page/cursor logic. |
| ca-architecture-boundaries | No shadow read path / actor coupling | PASS | Export pages the same graph store the worker/expander use (main.go:274-277 comment verified against wiring); cursor internals hidden behind TextMarshaler (store.go:88-98) rather than exported fields. |
| cc-defensive-programming | External input validated at entry (barricade) | PASS | The ONE client-controlled input (cursor) is validated in decodeExportCursor before any scan: base64 → JSON → stage whitelist (export.go:62-78); rejection is opaque InvalidArgument (:105-107). Traced with 3 malformed classes (TestExportGarbageCursorInvalidArgument). |
| cc-defensive-programming | Security-critical path: defense in depth, no barricade-only trust | PASS | Identity checked in-handler even though the interceptor already ran (export.go:101-104 — fail-closed for in-process callers); tenant + per-record ACL both enforced on top of the store's tenant-scoped scan. |
| cc-defensive-programming | No empty catch / no silent swallow | PASS | Every error path returns a status error; ACL uncertainty aborts rather than degrades (4 fail-closed tests). ACL *denial* is deliberately silent omission — that is the DW-2.4 requirement (no oracle), not a swallow. |
| cc-defensive-programming | Assertions for bugs only / no executable code in assertions | N/A | No assertions used; Go idiom is error returns, which this follows consistently. |

## Notes (non-blocking)
- `status.Errorf(codes.Internal, "export entities: %v", err)` (export.go:114 etc.) forwards backend error text to the client. This matches the established server-wide convention (server.go:116,141,175,193 do the same), the error is never client-controlled, and no cross-tenant data is involved — so it is a codebase-wide posture question, not a phase defect.
- `encodeExportCursor` discards json.Marshal's error (export.go:54). Correct today (two string-backed fields cannot fail to marshal) and commented as such; would only matter if exportCursor ever grows an unmarshalable field.
- An all-denied full page returns 0 records with a non-empty next_cursor — correct (cursor still advances, no infinite loop), just means clients must treat "empty page + cursor" as continue, which the proto comment documents.

## Issues
None.

**Verdict: PASS**

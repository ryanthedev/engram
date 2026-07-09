# Review: Phase 2 - Export RPC + server wiring (sample 3)

## Executed Results (Step 0)
- Test suite: `go test ./internal/server/... ./internal/engramclient/... ./internal/graph/...` → 98 passed, 0 failed (3 packages). Verbose run of Export/Cursor tests: 17/17 PASS (`/tmp/p2-review-sample-3/verbose.txt`).
- Proto check: `make proto-check` → codegen ok, `git diff --exit-code -- api/engrampb` clean (exit 0).
- Lint: `make lint` (go vet + revive v1.12.0) → clean (exit 0).
- Build: `go build ./...` → success.
- Coverage: `go test -coverprofile` → export.go: `Export` **88.6%**, all other export.go funcs 100%; `engramclient.Export` 100%; `Cursor.MarshalText`/`UnmarshalText` 100%.

## Requirement Fulfillment

### DW-2.1
PREMISE:  "`Export` RPC defined in proto and regenerated code committed; `make proto-check` clean."
EVIDENCE: api/proto/engram.proto:59 (`rpc Export`), :165-214 (ExportRequest/ExportEntity/ExportEdge/ExportResponse); api/engrampb/engram_grpc.pb.go:46,80,131,172 (generated client/server stubs present).
TRACE:    `make proto-check` → codegen regenerates into api/engrampb → `git diff --exit-code -- api/engrampb` → exit 0 (generated code matches proto, tracked in index).
VERDICT:  PASS *(files are staged, not yet in a commit — the orchestrator commits post-gate; the executable assertion, proto-check clean, holds)*

### DW-2.2
PREMISE:  "Handler returns a bounded page of live entities+edges for the caller's tenant sourced via the graph scan; `next_cursor` advances and empties on exhaustion."
EVIDENCE: internal/server/export.go:97-153 (handler; ScanEntities :112, ScanEdges :134, cursor encode :128/:150).
TRACE:    501 live entities + 3 edges + 1 expired entity + 1 invalidated edge → page 1: 500 entities + stage=entities cursor; page 2: 1 entity, chains into edges, 3 edges, empty cursor. Expired/invalidated records absent. Every id exactly once.
VERDICT:  PASS — TestDW_2_2_ExportPagesToExhaustion, TestDW_2_2_ExportEmptyTenantOneTerminalPage, TestDW_2_2_ExportPageExactlyOnBoundContinues all PASS in Step 0. *(But see Test-DW Coverage: the edge-tier continuation branch :149-151 has no committed test.)*

### DW-2.3
PREMISE:  "Tenant pinned from verified identity; a token for tenant A never receives tenant B's records (test)."
EVIDENCE: internal/server/export.go:101-104 (tenant from `authgrpc.IdentityFrom`, fail-closed Unauthenticated); api/proto/engram.proto:165-167 (ExportRequest has NO tenancy field); export.go:47-50 (wire cursor carries no tenancy).
TRACE:    tenants A and B each seeded 501 entities + 3 edges → walk as tenant-a identity → exactly A's 501/3 returned, zero `tenant-b`-prefixed ids on any page.
VERDICT:  PASS — TestDW_2_3_ExportTenantIsolation, TestDW_2_3_ExportNoIdentityRejected PASS in Step 0.

### DW-2.4
PREMISE:  "Records failing `ACL.CanRead` are omitted; nil `Exporter` seam → `Unimplemented`."
EVIDENCE: internal/server/export.go:117-126, 139-148 (denied record skipped, ACL error aborts); :98-100 (nil Exporter → Unimplemented); :158-163 (canExport, ReadAuthorizer contract).
TRACE:    5 entities/2 edges, one of each marked `deny-me` → 4 entities + 1 edge returned, denied ids absent, call succeeds. Nil Exporter → codes.Unimplemented. ACL error → codes.Internal, no partial page.
VERDICT:  PASS — TestDW_2_4_ExportACLDeniedRecordsOmitted, TestDW_2_4_ExportACLErrorFailsClosed, TestDW_2_4_ExportNilExporterUnimplemented PASS in Step 0.

### DW-2.5
PREMISE:  "`engramclient.Export` returns a page + cursor; unauthenticated call rejected by the existing interceptor."
EVIDENCE: internal/engramclient/client.go:130-132; internal/engramclient/client_test.go:43-70 (real gRPC server on `127.0.0.1:0` behind the production `authgrpc.UnaryServerInterceptor`).
TRACE:    501 entities over real TCP with good token → page 1: 500 + advancing cursor; page 2: 1 + empty cursor. Wrong token AND empty token → codes.Unauthenticated before the handler runs.
VERDICT:  PASS — TestDW_2_5_ClientExportReturnsPageAndCursor, TestDW_2_5_ClientExportUnauthenticatedRejected PASS in Step 0.

**All requirements met:** YES (behaviorally) — but the stated coverage level is NOT met; see below.

## Test-DW Coverage
- [x] All DW items have corresponding DW-named tests (ran in Step 0): TestDW_2_2_* (3), TestDW_2_3_* (2), TestDW_2_4_* (3), TestDW_2_5_* (2), plus TestExportGarbageCursorInvalidArgument, TestExportStaleCursorStaysSafe, TestExportRecordFieldMapping, TestCursorTextRoundTrip, TestCursorUnmarshalGarbageIsHarmless.
- [ ] **Test coverage matches the stated level (100%) — NOT met.** `Export` (internal/server/export.go) is at 88.6%. Uncovered blocks:
  1. export.go:113-115 — ScanEntities error → Internal (never exercised; no failing-Exporter fake exists)
  2. export.go:135-137 — ScanEdges error → Internal (never exercised)
  3. export.go:142-144 — edge-tier ACL error → Internal (only the entity-tier ACL error is tested)
  4. export.go:149-151 — **edge-tier continuation cursor** (functional pagination code): no committed test walks a tenant with >1 edge page, so the stage=edges cursor encode/resume round-trip never runs in the suite.

  Reviewer verification of block 4: I wrote and ran a scratch test (3 entities + 1001 edges → 1001 unique edges across ≥3 pages, terminal cursor) — `--- PASS: TestReviewScratch_EdgeTierMultiPage` (`/tmp/p2-review-sample-3/scratch.txt`); scratch file removed afterward. The branch is CORRECT, but this path is trivially testable, so observed behavior cannot substitute for a committed automated test under the 100% mandate.

## Dead Code
None found. No unused imports (lint clean), no unreachable code, no debug statements, no commented-out blocks in the phase files.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | Handler is stateless — reads only wire-once `s.Exporter`/`s.ACL` fields, all page state lives in the request-scoped cursor; no shared mutable state introduced. Scan internals are Phase 1's contract (given). |
| Error Handling | PASS | Scan error → Internal; ACL error → whole call fails closed with no partial page (TestDW_2_4_ExportACLErrorFailsClosed); undecodable cursor → opaque InvalidArgument before any scan (TestExportGarbageCursorInvalidArgument asserts message is exactly "invalid cursor"). |
| Resources | N/A | Handler opens no files, connections, locks, or goroutines. |
| Boundaries | PASS | Empty tenant → one empty terminal page (test); exactly-full 500 page → continuation then clean chain, full page never chains (test); zero-Cursor sentinel round-trips through JSON/text (TestCursorTextRoundTrip); 501-record walk has no dupes/gaps at the page seam (test). |
| Security | PASS | Tenant pinned from verified identity only — request and wire cursor carry NO tenancy (proto :165-167, export.go:47-50); missing identity fails closed even in-process (defense in depth, tested); fabricated/stale/replayed cursor repositions only inside the caller's own tenant (TestExportStaleCursorStaysSafe replays a cursor against another tenant's data → zero foreign records); per-record ACL, denied indistinguishable from absent (no oracle); internal fields Embedding/NameKey never serialized (TestExportRecordFieldMapping). |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| ca-architecture-boundaries | Dependency direction: business layer defines the abstraction, infrastructure implements it | PASS | `Exporter` interface is consumer-defined in internal/server/export.go:29-32; *graph.Store/*graph.MemBackend satisfy it; server never imports graph internals beyond the opaque Cursor — arrows point inward. |
| ca-architecture-boundaries | Business logic runs without infrastructure | PASS | Entire handler suite runs against MemBackend with no gRPC/network; the transport concern (interceptor) is tested separately in client_test.go. |
| ca-architecture-boundaries | SRP by actor / no boundary leakage in wire mapping | PASS | Wire mapping isolated in exportEntityProto/exportEdgeProto (export.go:168-198); internal similarity/lookup fields excluded by construction and by test. |
| cc-defensive-programming | External input validated at entry (barricade) | PASS | The ONE client-controlled input (cursor) is validated at decodeExportCursor (export.go:62-78): non-base64, non-JSON, and unknown-stage all rejected opaquely as InvalidArgument before any scan (tested with all three malformed shapes). |
| cc-defensive-programming | Security-critical path validates again inside the barricade (defense in depth) | PASS | Identity re-checked in the handler (export.go:101-104) even though the interceptor already gates it; TestDW_2_3_ExportNoIdentityRejected proves an in-process bypass fails closed. |
| cc-defensive-programming | No silently swallowed errors; fail closed on uncertainty | PASS | Every error path aborts with a status code; ACL uncertainty aborts the whole call rather than degrading to disclosure (tested). The one discarded error (encodeExportCursor json.Marshal, :54) is a can't-fail case documented inline — acceptable. |

## Notes (non-blocking)
- `status.Errorf(codes.Internal, "...: %v", err)` (export.go:114,121,136,143) sends backend error detail to the client. This matches the established pattern across server.go (:116,:141,:175,:193), so it is consistent, and no tenant-data leak was demonstrated — but scrubbing Internal messages server-wide would be a reasonable hardening pass.
- DW-2.1 says "committed": the proto + regenerated code are staged in the index (git status `M api/engrampb/...`) and proto-check is clean; the actual commit is the orchestrator's post-gate step in this workflow.
- The wire cursor is unauthenticated client state (base64 JSON). It is safe by construction (no tenancy, stage whitelist, scan re-pinned to identity) — verified by tests — so signing it is unnecessary; noted only for completeness.

## Issues (FAIL)
1. Test coverage below the dispatch's stated level (100%): `Export` handler at 88.6%.
   - File: internal/server/export.go:113-115, 135-137, 142-144, 149-151
   - Demonstrated by: `go tool cover -func` on Step 0 coverprofile (`/tmp/p2-review-sample-3/cover.out`); block :149-151 is functional pagination code (edge-tier continuation) with no committed test — verified correct only by a reviewer-written scratch test.
   - Fix: add to internal/server/export_test.go: (i) a multi-edge-page walk (seed >500 edges; assert ≥3 pages, no dupes, terminal cursor — the reviewer's scratch version of exactly this passed), (ii) a failing-Exporter fake asserting ScanEntities and ScanEdges errors each → codes.Internal, (iii) an edge-tier ACL-error case (deny with error only on an edge record) → codes.Internal.

**Verdict: FAIL — one blocker: coverage does not meet the stated 100% level on internal/server/export.go (four uncovered blocks, incl. the untested edge-tier continuation branch). All DW items and all five listed edge cases otherwise pass with execution evidence; no behavioral defect was found.**

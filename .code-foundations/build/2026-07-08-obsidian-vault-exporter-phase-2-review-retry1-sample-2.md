# Review: Phase 2 - Export RPC + server wiring (retry 1, sample 2)

## Executed Results (Step 0)
- Test suite: `go test ./internal/server/... ./internal/engramclient/... ./internal/graph/...` → 102 passed, 0 failed (3 packages)
- Targeted verbose run (`-run 'DW_2|Export|Cursor'`): 23/23 PASS, including all 17 export-related tests listed by name below
- Coverage: `go test -coverprofile=/tmp/p2-rereview-sample-2/cov.out ./internal/server/...` → 29 passed; `go tool cover -func | grep -i export`:
  - `export.go:53 encodeExportCursor 100.0%`, `export.go:62 decodeExportCursor 100.0%`, `export.go:97 Export 100.0%`, `export.go:158 canExport 100.0%`, `export.go:168 exportEntityProto 100.0%`, `export.go:184 exportEdgeProto 100.0%`
- Proto check: `make proto-check` → `codegen: ok`, `git diff --exit-code -- api/engrampb` → exit 0
- Lint: `make lint` (go vet + revive) → exit 0
- Build (typecheck): `go build ./...` → success

## Requirement Fulfillment

### DW-2.1
PREMISE:  "`Export` RPC defined in proto and regenerated code committed; `make proto-check` clean."
EVIDENCE: api/proto/engram.proto:59 (`rpc Export(ExportRequest) returns (ExportResponse);`), messages at engram.proto:165, 173, 192, 210; generated shapes present in api/engrampb/engram_grpc.pb.go:80, 131, 172, 195, 292-304 and 28 `ExportEntity` occurrences in engram.pb.go
TRACE:    `make proto-check` regenerates via scripts/codegen.sh → `codegen: ok` → `git diff --exit-code -- api/engrampb` exits 0 → committed generated code matches the proto exactly
VERDICT:  PASS

### DW-2.2
PREMISE:  "Handler returns a bounded page of live entities+edges for the caller's tenant sourced via the graph scan; `next_cursor` advances and empties on exhaustion."
EVIDENCE: internal/server/export.go:97-153 (handler over `Exporter.ScanEntities`/`ScanEdges`, export.go:29-32); cursor advance export.go:127-131, 149-151
TRACE:    501 seeded entities + 3 edges → page 1: 500 entities + non-empty `next_cursor{s:entities}`; page 2: 1 entity, entity tier exhausts, chains into edges same call → 3 edges + empty cursor. Executed: TestDW_2_2_ExportPagesToExhaustion (501/3 records exactly once, ≥2 pages, expired entity + invalidated edge never appear), TestDW_2_2_ExportEdgesPageToExhaustion (edge tier multi-page, 501 edges exactly once), TestDW_2_2_ExportPageExactlyOnBoundContinues (exact-bound page carries continuation, no edge chain on a full entity page) — all PASS
VERDICT:  PASS

### DW-2.3
PREMISE:  "Tenant pinned from verified identity; a token for tenant A never receives tenant B's records (test)."
EVIDENCE: internal/server/export.go:101-103 (`authgrpc.IdentityFrom(ctx)`, fail-closed on missing identity or empty TenantID); `id.TenantID` is the only tenant ever passed to the scans (export.go:112, 134); ExportRequest has no tenancy field (engram.proto:165-167); wire cursor carries no tenancy (export.go:47-50)
TRACE:    Tenants A and B both seeded with 501 entities + 3 edges in one backend → identity bound to tenant-a walks export to exhaustion → exactly its own 501/3, zero `tenant-b`-prefixed ids on any page. Executed: TestDW_2_3_ExportTenantIsolation PASS; TestDW_2_3_ExportNoIdentityRejected (no identity → Unauthenticated) PASS; TestExportStaleCursorStaysSafe (tenant-A cursor replayed over a backend holding only tenant-B data → 0 records, no error) PASS
VERDICT:  PASS

### DW-2.4
PREMISE:  "Records failing `ACL.CanRead` are omitted; nil `Exporter` seam → `Unimplemented`."
EVIDENCE: internal/server/export.go:117-126 and 139-147 (per-record `canExport`, denied → skipped, ACL error → whole call Internal); export.go:98-100 (nil Exporter → `codes.Unimplemented`)
TRACE:    5 entities + 2 edges, ACL denies the sentinel-owned entity and edge → page returns 4/1, denied ids absent, call succeeds. Executed: TestDW_2_4_ExportACLDeniedRecordsOmitted PASS; TestDW_2_4_ExportNilExporterUnimplemented (Unimplemented) PASS; fail-closed mirrors: TestDW_2_4_ExportACLErrorFailsClosed, TestDW_2_4_ExportEdgeACLErrorFailsClosed, TestDW_2_4_ExportEntityScanErrorFailsClosed, TestDW_2_4_ExportEdgeScanErrorFailsClosed — all PASS
VERDICT:  PASS

### DW-2.5
PREMISE:  "`engramclient.Export` returns a page + cursor; unauthenticated call rejected by the existing interceptor."
EVIDENCE: internal/engramclient/client.go:130-132; tests run a real gRPC server on 127.0.0.1:0 behind the production `authgrpc.UnaryServerInterceptor` (client_test.go:58-69)
TRACE:    501 entities over real TCP with a good token → page 1: 500 entities + advancing cursor; page 2: 1 entity + empty cursor. Wrong/empty token → `codes.Unauthenticated` before the handler. Executed: TestDW_2_5_ClientExportReturnsPageAndCursor PASS, TestDW_2_5_ClientExportUnauthenticatedRejected PASS
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have corresponding tests that ran in Step 0 (test names carry DW-IDs: `TestDW_2_1` proto is covered by `make proto-check` execution + generated-shape spot-check; `TestDW_2_2_*` ×4, `TestDW_2_3_*` ×2, `TestDW_2_4_*` ×5, `TestDW_2_5_*` ×2)
- [x] Coverage level 100% verified: every function in internal/server/export.go measures 100.0% including the `Export` handler; no unreachable lines to report
- DW-2.1 has no unit test by nature (proto/codegen artifact); execution evidence is `make proto-check` exiting clean — recorded observed behavior per protocol

## Dead Code
None found. No unused imports (build + vet clean), no unreachable code, no debug statements, no commented-out blocks in export.go, client.go, or the test files.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | Handler is stateless: reads only immutable `s.Exporter`/`s.ACL` seams and per-call locals; no shared mutable state. No defect demonstrable. |
| Error Handling | PASS | Adversarial traces: entity-scan error, edge-scan error, entity-ACL error, edge-ACL error all abort with Internal and nil response (4 dedicated tests PASS); garbage cursor → InvalidArgument before any scan (TestExportGarbageCursorInvalidArgument). `json.Marshal` at export.go:54 justifiably ignores error — `graph.Cursor.MarshalText` (store.go:88) cannot fail and Stage is a plain string. |
| Resources | N/A | Handler allocates no handles/locks/goroutines; client Export reuses the existing connection; tests close servers/clients via t.Cleanup/Close. |
| Boundaries | PASS | Traced: empty cursor → start; exact-500 page → continuation then clean exhaust (test); zero entities → chain to edges same call (test); `int64(e.MentionCount)` widening is lossless. |
| Security | PASS | Traced the three attack inputs: (1) fabricated cursor with unknown stage → opaque InvalidArgument, message fixed at "invalid cursor" (asserted in test — decode detail never leaks); (2) fabricated/stale `After` bytes → `Cursor.UnmarshalText` accepts anything (store.go:95-98) and merely repositions inside `id.TenantID`'s scan — cross-tenant replay test surfaces 0 foreign records; (3) missing identity → fail-closed Unauthenticated even in-process, defense in depth behind the interceptor. Internal fields (Embedding, NameKey) never mapped to the wire (export.go:168-181, asserted by TestExportRecordFieldMapping). |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | External input validated at entry | PASS | The one client-controlled input (cursor) is validated at the barricade in `decodeExportCursor` (export.go:62-78): base64 → JSON → stage whitelist, rejected before any scan runs. Probed all three malformed classes; all → InvalidArgument (test PASS). |
| cc-defensive-programming | No empty catch / no swallowed errors | PASS | Every error path returns a coded status; scan/ACL errors abort the call (fail closed), never degrade to a partial page — 4 tests demonstrate. |
| cc-defensive-programming | Security-critical path validates despite barricade (defense in depth) | PASS | Handler re-checks identity presence + non-empty TenantID (export.go:101-103) even though the interceptor already ran; per-record ACL re-check on top of the store's tenant-scoped scan. |
| cc-defensive-programming | Assertions vs error handling; correctness over robustness | PASS | Tenant-boundary code leans correctness: uncertainty (ACL error) shuts the call down rather than guessing — traced in both tiers. |
| ca-architecture-boundaries | Dependencies point inward; business logic imports no infrastructure | PASS | `Exporter` and `ReadAuthorizer` are consumer-defined interfaces in the server package (export.go:29-32, server.go:42-44); concrete `*graph.Store`/`*acl.Filter` are injected at the composition root (main.go:273-277). No infrastructure import in the handler beyond the seams. |
| ca-architecture-boundaries | SRP by actor / no shadow read path | PASS | Export pages over the same graph store the worker/expander use (main.go:274-277 comment matches wiring); cursor encode/decode, ACL check, and proto mapping are separated into single-purpose functions. |

## Notes (non-blocking)
- `status.Errorf(codes.Internal, "export entities: %v", err)` (export.go:114, 121, 136, 143) echoes backend error text to the client. This mirrors the established server-wide pattern (server.go:116, 141, 175, 193), so it is a pre-existing convention, not a phase defect — but on a security-sensitive surface, scrubbing Internal messages project-wide would be a reasonable hardening pass.
- Nil `ACL` ⇒ allow-all (`canExport`, export.go:158-163) is the documented `ReadAuthorizer` contract shared with the Audit path (server.go:40-44); production wires it (main.go:273). A fail-open default on a nil seam is a deliberate, consistent design choice here, not a demonstrated defect — flagging for awareness only.
- Dependency check (scan contract): cursor chaining is used correctly — a full entity page returns without touching edges; a mid-call entity exhaust chains into edges with a zero sub-cursor; exact-bound and stale-cursor boundaries tested. No record loss across page boundaries demonstrated or demonstrable from the contract.

## Issues (if FAIL)
None.

**Verdict: PASS**

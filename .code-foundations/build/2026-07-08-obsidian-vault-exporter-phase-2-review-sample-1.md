# Review: Phase 2 - Export RPC + server wiring (sample 1)

## Executed Results (Step 0)
- Test suite: `go test ./internal/server/... ./internal/engramclient/... ./internal/graph/...` → 98 passed, 0 failed (server 20, engramclient 2, graph 76)
- Proto check: `make proto-check` → rc=0 (codegen ok, `git diff --exit-code -- api/engrampb` clean)
- Lint: `make lint` → rc=0 (go vet + revive, `-set_exit_status`)
- Build: `go build ./...` → rc=0
- Coverage: `go test -coverprofile` on internal/server → export.go: encodeExportCursor 100%, decodeExportCursor 100%, canExport 100%, proto mappers 100%, **Export 88.6%** (see Test-DW Coverage)
- Reviewer probes (temporary test file, run then deleted; tree verified restored): 3/3 passed — edge-tier multi-page walk (501 edges), edge-stage ACL error fails closed, scan errors → Internal

## Requirement Fulfillment

### DW-2.1
PREMISE:  "`Export` RPC defined in proto and regenerated code committed; `make proto-check` clean."
EVIDENCE: api/proto/engram.proto:59 (`rpc Export`), :165-215 (ExportRequest/ExportEntity/ExportEdge/ExportResponse); api/engrampb/engram_grpc.pb.go:46,80,131-134; api/engrampb/engram.pb.go:730
TRACE:    `make proto-check` → codegen regenerates into api/engrampb → `git diff --exit-code -- api/engrampb` finds no drift → rc=0
VERDICT:  PASS

### DW-2.2
PREMISE:  "Handler returns a bounded page of live entities+edges for the caller's tenant sourced via the graph scan; `next_cursor` advances and empties on exhaustion."
EVIDENCE: internal/server/export.go:97-153 (handler), :112/:134 (ScanEntities/ScanEdges — the graph scan), :127-131/:149-151 (cursor advance/terminal)
TRACE:    501 live entities + 3 edges (+1 expired entity, +1 invalidated edge) → page 1: 500 entities + non-empty cursor; page 2: 1 entity, entity tier exhausts, chains into 3 edges, empty cursor. Every record exactly once; expired/invalidated never appear. Tests: TestDW_2_2_ExportPagesToExhaustion, TestDW_2_2_ExportEmptyTenantOneTerminalPage, TestDW_2_2_ExportPageExactlyOnBoundContinues — all PASS. Edge-tier continuation (>500 edges) additionally verified by reviewer probe: 2 entities + 501 edges → all 503 records exactly once across ≥2 edge pages, terminal empty cursor.
VERDICT:  PASS

### DW-2.3
PREMISE:  "Tenant pinned from verified identity; a token for tenant A never receives tenant B's records (test)."
EVIDENCE: internal/server/export.go:101-104 (identity required, fails closed Unauthenticated), :112/:134 (only `id.TenantID` passed to scans); api/proto/engram.proto:165-167 (ExportRequest carries ONLY `cursor` — no tenancy field exists); exportCursor deliberately carries no tenancy (export.go:44-50)
TRACE:    Tenants A and B each seeded 501 entities + 3 edges → identity bound to A walks to exhaustion → exactly A's 501/3, zero `tenant-b`-prefixed ids (TestDW_2_3_ExportTenantIsolation PASS). Context with no identity → Unauthenticated (TestDW_2_3_ExportNoIdentityRejected PASS). Replayed cursor against a backend holding only tenant-b data → 0 foreign records (TestExportStaleCursorStaysSafe PASS).
VERDICT:  PASS

### DW-2.4
PREMISE:  "Records failing `ACL.CanRead` are omitted; nil `Exporter` seam → `Unimplemented`."
EVIDENCE: internal/server/export.go:98-100 (nil Exporter → Unimplemented), :116-126 and :138-148 (per-record CanRead; denied → omitted; error → whole call Internal), :158-163 (canExport)
TRACE:    5 entities + 2 edges, ACL denies sentinel owner → 4 entities + 1 edge returned, denied ids absent, call succeeds (TestDW_2_4_ExportACLDeniedRecordsOmitted PASS). ACL error → codes.Internal, no partial page (TestDW_2_4_ExportACLErrorFailsClosed PASS; edge-stage error path confirmed by reviewer probe). `&server.Server{}` → Unimplemented (TestDW_2_4_ExportNilExporterUnimplemented PASS).
VERDICT:  PASS

### DW-2.5
PREMISE:  "`engramclient.Export` returns a page + cursor; unauthenticated call rejected by the existing interceptor."
EVIDENCE: internal/engramclient/client.go:130-132; test server behind the REAL authgrpc.UnaryServerInterceptor on 127.0.0.1:0 (client_test.go:58-69); production chain registers the same interceptor with no exempt methods (cmd/engram-server/main.go:265-267) and wires svc.Exporter = graphStore (main.go:277)
TRACE:    501 entities over real TCP with good token → page 1: 500 + advancing cursor; page 2: 1 + empty cursor (TestDW_2_5_ClientExportReturnsPageAndCursor PASS). Wrong token AND empty token → codes.Unauthenticated before the handler runs (TestDW_2_5_ClientExportUnauthenticatedRejected PASS).
VERDICT:  PASS

**All requirements met:** YES

## Edge Cases (prompt-listed)
| Edge case | Status | Evidence |
|---|---|---|
| Unauthenticated → `Unauthenticated` | PASS | TestDW_2_5_ClientExportUnauthenticatedRejected (real interceptor, real TCP); in-process defense-in-depth TestDW_2_3_ExportNoIdentityRejected |
| Empty tenant → one empty page, terminal cursor | PASS | TestDW_2_2_ExportEmptyTenantOneTerminalPage (0 entities, 0 edges, cursor "", no error, single round trip via entity→edge chaining, export.go:131) |
| Page exactly filling the bound | PASS | TestDW_2_2_ExportPageExactlyOnBoundContinues: 500 entities → full page + continuation, no edges on page 1; page 2 finds tier exhausted, chains into edges, terminal |
| Record failing CanRead skipped, not fatal | PASS | TestDW_2_4_ExportACLDeniedRecordsOmitted — denied record omitted, page continues, call succeeds |
| Stale/garbage cursor: no error-oracle, no cross-tenant leak | PASS | Garbage (bad base64 / non-JSON / bogus stage) → uniform opaque `InvalidArgument "invalid cursor"` before any scan (TestExportGarbageCursorInvalidArgument asserts the message leaks no decode detail); structurally-valid stale cursor on emptied backend → clean exhaustion; same cursor over foreign-tenant data → 0 foreign records (TestExportStaleCursorStaysSafe) |

## Test-DW Coverage
- [x] All DW items have corresponding DW-named tests that ran in Step 0 (TestDW_2_1 is `make proto-check` itself; TestDW_2_2_* ×3, TestDW_2_3_* ×2, TestDW_2_4_* ×3, TestDW_2_5_* ×2, plus TestExportGarbageCursor*, TestExportStaleCursor*, TestExportRecordFieldMapping, TestCursorTextRoundTrip, TestCursorUnmarshalGarbageIsHarmless)
- [x] Every prompt-listed edge case has a dedicated committed test
- [ ] Statement coverage on the handler is 88.6%, not 100%: uncovered are export.go:113-115 (entity-scan error), :135-137 (edge-scan error), :142-144 (edge-stage ACL error), and :149-151 (edge-tier continuation cursor — no committed test seeds >500 edges)

Gap: the committed suite never produces or resumes a `stage=edges` continuation cursor. I verified the branch is behaviorally correct via a reviewer probe (501 edges: all records exactly once, cursor advances then empties; edge-stage ACL error → Internal; scan errors → Internal — 3/3 passed, recorded observed behavior). DW-2.2's stated behaviors all have committed automated tests, so this is a coverage gap, not an uncovered requirement — but at the stated 100% level a committed edge-tier multi-page test should be added (see Notes for the exact test).

## Dead Code
None found. All imports in export.go used; no debug statements, unreachable code, or commented-out blocks in the reviewed files.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | Handler is stateless: no Server field is written, no shared mutable state introduced; each call flows context → seam. Probed for writes to `s.*` — none. |
| Error Handling | PASS | Every error path returns a gRPC status: decode → opaque InvalidArgument (export.go:107); scan errors → Internal (probed both tiers); ACL error → Internal, no partial page (tested + probed edge-stage). The one ignored error (encodeExportCursor:54) is documented-impossible: struct of string + a TextMarshaler that always returns nil. |
| Resources | N/A | Handler opens no handles, goroutines, locks, or connections; client reuses the existing dialed conn (Close at client.go:43, pre-existing). |
| Boundaries | PASS | Adversarial cases traced and executed: empty tenant, exactly-500 page, 501 (bound+1) in both tiers, zero-Cursor in/out semantics, JSON `null`/missing-stage cursor → rejected as unknown stage (decodeExportCursor:74-76). |
| Security | PASS | Tenant boundary: only `id.TenantID` reaches the scans (export.go:112,134); ExportRequest has no tenancy field to spoof; wire cursor carries stage+after only — a fabricated cursor can reposition solely inside the caller's tenant (executed: TestExportStaleCursorStaysSafe, TestCursorUnmarshalGarbageIsHarmless). Fails closed on missing identity even in-process. Per-record ACL with error→abort. Internal fields (Embedding, NameKey) never mapped to the wire (TestExportRecordFieldMapping). |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| ca-architecture-boundaries | Dependency direction: use case depends on abstraction, not infrastructure | PASS | `Exporter` is consumer-defined in internal/server (export.go:29-32), mirroring StatusProbe/Auditor; graph store satisfies it; wiring lives in main.go:277. Arrows point inward. |
| ca-architecture-boundaries | SRP by actor / OCP | PASS | Export is additive: server.go gained only the `Exporter` field (server.go:64-66); no existing handler modified. Export logic, cursor codec, and wire mapping isolated in export.go for the one vault-export actor. |
| cc-defensive-programming | External input validated at entry | PASS | The cursor is the sole client-controlled input; decodeExportCursor (export.go:62-78) is the barricade: base64 → JSON → stage whitelist, all failures collapsed to opaque `InvalidArgument "invalid cursor"` before any scan runs (test asserts no detail leak). |
| cc-defensive-programming | Security-critical path: defense in depth, never exempt | PASS | Identity re-verified in-handler (export.go:101-104) although the interceptor already gates the RPC — an in-process caller bypassing the interceptor still fails closed (TestDW_2_3_ExportNoIdentityRejected). |
| cc-defensive-programming | No empty catch blocks / swallowed errors | PASS | No swallowed error; the single `_ =` (encodeExportCursor:54) has an explicit impossibility justification and both marshal halves are covered by TestCursorTextRoundTrip. |
| cc-defensive-programming | Correctness over robustness on uncertainty | PASS | ACL evaluation error aborts the entire call with Internal rather than returning a partial page ("never partial trust") — tested for entity stage, probed for edge stage. |

## Notes (non-blocking)
1. **Missing committed test: edge-tier continuation** (export.go:149-151). No test seeds >500 edges, so a `stage=edges` non-terminal cursor is never produced/resumed by the suite; a regression that, e.g., wrote `Stage: stageEntities` on that line would go uncaught. My probe (passed, then removed) is a ready-made test: seed 2 entities + 501 edges, walk to exhaustion, assert 501 unique edges across ≥2 pages. Adding it (plus an erroring-Exporter fake for the two scan-error branches and an edges-only ACL-error case) takes Export to 100%.
2. Internal error messages embed backend error text (`"export entities: %v"`, export.go:114,121,136,143) — consistent with the existing Search/Audit pattern, and not an existence oracle, but consider opaque client messages + server-side logging codebase-wide.
3. `canExport` fails open when `s.ACL == nil` (export.go:158-163). This is the documented ReadAuthorizer contract shared with Audit, and production wires the filter (main.go:273); still, on a security-critical path a construction-time "production requires ACL" invariant would be sturdier than a per-call nil check.
4. Ingest's client-supplied tenancy fallback (server.go:91-94) predates this phase and is out of scope; Export correctly has no such fallback — the request has no tenancy field at all.

## Issues (if FAIL)
None.

**Verdict: PASS**

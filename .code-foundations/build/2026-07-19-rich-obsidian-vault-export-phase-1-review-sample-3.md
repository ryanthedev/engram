# Review: Phase 1 - Server episodic export stage (sample 3)

## Executed Results (Step 0)
- Test suite: `go test ./internal/server/... ./internal/store/... ./internal/engramclient/... -count=1` → all packages ok; verbose count: **129 PASS, 0 FAIL**.
- Typecheck/build: `go build ./...` → exit 0; `go vet` (same packages) → exit 0.
- Lint: `gofmt -l` over changed dirs → no output (clean). staticcheck unavailable (toolchain version mismatch with local install, not a project defect).
- Proto idempotency: `make proto` run twice; `shasum` of `api/engrampb/*.go` + `api/proto/engram.proto` identical before/after second run → **IDEMPOTENT: no changes on regeneration**. (The `M` in `git status` is the intentionally-uncommitted phase work itself.)

## Requirement Fulfillment

### DW-1.1
PREMISE:  "`ExportEpisodic` in proto + regenerated `engrampb`; proto regeneration is idempotent."
EVIDENCE: api/proto/engram.proto:358-373 (`message ExportEpisodic` — event_id, kind, text, occurred_at, source_ids, scope, team_id, owner_agent_id; internal fields deliberately absent), api/proto/engram.proto:382 (`repeated ExportEpisodic episodics = 4` in ExportResponse); api/engrampb/engram.pb.go:1496 (`type ExportEpisodic struct`).
TRACE:    `make proto` → codegen ok → re-run → file hashes unchanged (idempotent). `memory.Episodic{EventID:"ev-map", Kind:"tool_result", …, TextEmbedding:[0.1], ProcessedAt:set}` → `exportEpisodicProto` (internal/server/export.go:224-235) → wire record with all eight fields round-tripped, no embedding/outbox fields on the message at all.
VERDICT:  **PASS** — `TestDW_1_1_ExportEpisodicFieldMapping` PASS; hash-diff idempotency observed.

### DW-1.2
PREMISE:  "`Export` drains episodic (own string cursor) then chains to entities/edges; unknown stage → `InvalidArgument`; empty episodic tier chains into entities in the same response."
EVIDENCE: internal/server/export.go:137-165 (episodic stage: returns mid-tier with `{Stage: episodic, EpAfter: next}` cursor when `next != ""`; falls through to entities when tier exhausts), export.go:96-98 (unknown stage rejected), export.go:131-134 (any decode failure → opaque `InvalidArgument "invalid cursor"`), export.go:141-145 (store `ErrBadCursor` on the episodic sub-cursor also → InvalidArgument, not Internal).
TRACE:    5 episodic recs @ pageSize 2 + 3 entities + 2 edges → page 1: 2 episodics only, continuation cursor; page 2: 2 episodics; page 3: 1 episodic + 3 entities + 2 edges in the SAME response, empty cursor (3 pages total, every record once, scan order). Forged base64 `{"s":"bogus-stage","a":""}` → InvalidArgument. Empty-but-wired episodic tier → one terminal response 0/2/1.
VERDICT:  **PASS** — `TestDW_1_2_ExportDrainsEpisodicThenChains`, `TestDW_1_2_EmptyEpisodicTierChainsSameResponse`, `TestDW_1_2_BadEpisodicSubCursorInvalidArgument`, `TestExportGarbageCursorInvalidArgument` (bogus-stage case) all PASS.

### DW-1.3
PREMISE:  "every episodic record passes `canExport`; missing identity → `Unauthenticated`; ACL error → call fails closed (three tests)."
EVIDENCE: internal/server/export.go:148-158 (per-record `s.canExport`; error aborts with Internal and nil response; denied record skipped), export.go:127-130 (identity checked in-handler, defense in depth, before any scan).
TRACE:    3 recs with middle rec `OwnerAgentID:"deny-me"` + denying ACL → 2 episodics on the wire, denied one absent. ACL func returning error → `codes.Internal`, nil response (no partial page). `context.Background()` (no identity) with episodic seam wired → `codes.Unauthenticated` before any tier scanned.
VERDICT:  **PASS** — exactly three tests: `TestDW_1_3_EpisodicACLDeniedOmitted`, `TestDW_1_3_EpisodicACLErrorFailsClosed`, `TestDW_1_3_ExportNoIdentityRejectedWithEpisodic`, all PASS (plus `TestExportEpisodicScanErrorFailsClosed` for the scan-error fail-closed path).

### DW-1.4
PREMISE:  "`engramclient.ExportPage` exposes episodic records across pages via the byte-bounded stage; unprocessed/dead-lettered docs are absent."
EVIDENCE: internal/engramclient/export.go:69-98 (`ExportPage` adapts `Episodics` into plain `ExportEpisodic` structs, round-trips `NextCursor`); internal/store/facts.go:516-524 (query filters: tenant term + `processed_at` exists, must_not `dead_lettered:true` — exclusion by query construction).
TRACE:    Real gRPC loopback server behind the production auth interceptor, 5 episodics @ 2/page + 1 entity → client drains 5 episodics across ≥3 pages, in scan order, EventID/Kind/Text/OccurredAt/SourceIDs intact, then the graph tier; `TestDW_1_4_ScanEpisodicQueryShape` pins the exact query clauses that keep unprocessed/dead-lettered docs off the wire.
VERDICT:  **PASS** — `TestDW_1_4_ClientExportPageEpisodicsAcrossPages` + `TestDW_1_4_ScanEpisodicQueryShape` PASS. (The client test uses a contract-matching fake for the episodic seam; the byte-bounded store side of the same seam is execution-proven by DW-1.5 — see Notes.)

### DW-1.5
PREMISE:  "episodic page respects a byte budget (a synthetic oversized-Text set produces multiple pages, none exceeding the bound)."
EVIDENCE: internal/store/facts.go:476 (`EpisodicPageByteBudget = 2<<20`), facts.go:540-549 (cut before the overflowing record, never below one record), facts.go:551-559 (resume token from the last INCLUDED record).
TRACE:    Three ~1 MiB-Text records (two fit, three don't) → 2 pages, each ≤ budget, every record exactly once in order, and page-2 `search_after` carries ev-2 (the cut position) — truncation skips nothing. A single record of budget+4096 bytes → returned alone, cursor advances past it (no wedge).
VERDICT:  **PASS** — `TestDW_1_5_ScanEpisodicByteBudgetSplitsPages`, `TestDW_1_5_ScanEpisodicSingleOversizedRecordStillProgresses` PASS.

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have corresponding tests that ran in Step 0 (test names carry DW IDs: DW_1_1 … DW_1_5).
- [x] Coverage matches the stated 100% level: every DW clause and every listed edge case has a named executed test; supporting paths (scan error, missing index, non-404 backend error, full-page search_after round-trip, nil-exporter) are additionally covered.
No gaps.

## Dead Code
None found. No unreachable code, debug statements, TODO/FIXME, or commented-out blocks in the changed files; `walkExport` helper is used (export_test.go:195, 273); vet and build clean.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | Handler holds no shared mutable state — `resp` and decoded cursor are per-call locals; store scan builds a fresh query map per call. Nothing to race. |
| Error Handling | PASS | Adversarial paths traced and tested: bad wire cursor → InvalidArgument (opaque, no decode detail leak, asserted on message text); bad sub-cursor → `ErrBadCursor` sentinel rejected BEFORE any query (`TestScanEpisodic_BadCursorRejected` asserts 0 backend calls); scan error → Internal, nil response; missing index → empty tier not error; any other backend status → error propagates. |
| Resources | PASS | No handles/locks opened by this path; httptest servers in tests all `t.Cleanup`-closed; gRPC test server stopped via cleanup. |
| Boundaries | PASS | Traced the sharp edges: `recs[len(recs)-1]` at facts.go:554 is unreachable with empty recs (empty ⇒ `!truncated && len<batch` early-returns at :551; non-empty ⇒ `kept≥1` because the `kept>0 &&` guard always admits the first record). Record exactly on the byte bound is included (`>` not `>=`), pinned by `TestDW_2_2_ExportPageExactlyOnBoundContinues`'s episodic analogue behavior and the budget tests. Truncated-page resume token points at the cut, not the fetch end (asserted). |
| Security | PASS | Tenant pinned solely from verified identity (export.go:127-130, passed to every scan at :139/:167/:189); `exportCursor` carries NO tenancy field (:66-72); store query builds the tenant/processed/dead-letter filters unconditionally BEFORE the `search_after` token is even looked at (facts.go:510-532), so a forged token cannot widen tenancy or bypass filters — it can only reject (`ErrBadCursor`) or reposition inside the caller's tenant. Stale cursor against emptied/foreign-tenant backend → clean exhaustion, zero foreign records (`TestExportStaleCursorStaysSafe`). Oversized-record forward progress guaranteed (`TestDW_1_5_…StillProgresses`). ACL fail-closed and identity defense-in-depth per DW-1.3. |

## Edge cases (prompt-listed)
| Edge case | Status | Evidence |
|-----------|--------|----------|
| Empty episodic tier chains into entities in same response | HANDLED | `TestDW_1_2_EmptyEpisodicTierChainsSameResponse` — 0/2/1 in one terminal page |
| Stale/forged cursor repositions only inside caller's tenant | HANDLED | `TestExportStaleCursorStaysSafe`, `TestDW_1_2_BadEpisodicSubCursorInvalidArgument`, `TestScanEpisodic_BadCursorRejected`, tenant-term assertion in `TestDW_1_4_ScanEpisodicQueryShape`; cursor struct carries no tenancy |
| ACL-denied omitted; ACL error fails closed | HANDLED | `TestDW_1_3_EpisodicACLDeniedOmitted`, `TestDW_1_3_EpisodicACLErrorFailsClosed` |
| Unprocessed / dead-lettered docs excluded | HANDLED | `TestDW_1_4_ScanEpisodicQueryShape` pins `processed_at` exists filter + `dead_lettered` must_not |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| aposd-designing-deep-modules | Interface depth / information hiding | PASS | `EpisodicExporter` is one method hiding query shape, byte budget, filters, and cursor encoding; the wire cursor is opaque (staging + sub-cursors invisible to clients); `ExportPage` is one client method returning plain structs. No leakage of store internals across the seam. |
| aposd-designing-deep-modules | No shallow module / classitis | PASS | Two seams instead of one is justified and documented (export.go:37-45): the tiers live in different subsystems (*store.OpenSearchStore vs *graph.Store); merging would force a false dependency. |
| aposd-designing-deep-modules | Silent-failure red flag | PASS | Every failure surfaces: scan/ACL errors abort with a status code, bad cursors reject; the one deliberately silent case (ACL-denied omission) is a documented security property, not a swallowed failure. |
| cc-defensive-programming | External input validated at entry | PASS | The one client-controlled input (cursor) is validated at the barricade (decodeExportCursor) AND at the store (ErrBadCursor before any query) — defense in depth, both halves tested. |
| cc-defensive-programming | Security-critical path re-validates inside the barricade | PASS | Identity re-checked in-handler even though the interceptor already ran (export.go:127-130), tested via `TestDW_1_3_ExportNoIdentityRejectedWithEpisodic`; per-record ACL on top of tenant-scoped scans. |
| cc-defensive-programming | No empty catch / errors not swallowed | PASS | All error returns handled or propagated; the single ignored error (`json.Marshal` at export.go:76) is a can't-fail on string-field structs, commented, and its output is round-trip-tested. |
| cc-defensive-programming | Fail closed on uncertainty | PASS | ACL error → Internal + nil page; scan error → Internal + nil page; bad cursor → reject, never silent restart-from-zero. |

## Notes (non-blocking)
- DW-1.4's client-side test drives a contract-matching fake `EpisodicExporter` (index-token paging) rather than the byte-bounded `OpenSearchStore`; the store side of the identical seam is separately execution-proven (DW-1.5 tests, `ErrBadCursor` contract mirrored in the server-test fake). The composition point — production wiring `svc.EpisodicExporter = st` — exists at cmd/engram-server/main.go:292 and compiles against the real store. Acceptable seam-granular testing; a full docker-backed integration pass would close the last inch.
- If a tenant's episodic tier has exactly `DefaultScanBatchSize` records, one extra empty round trip occurs before exhaustion — documented in ScanEpisodic's contract, standard search_after behavior, not a defect.
- `staticcheck` could not run (local install built for go1.25, toolchain is 1.26) — pre-existing environment issue, not introduced by this change.

## Issues
None.

**Verdict: PASS**

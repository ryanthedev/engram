# Review: Phase 1 - server episodic export stage (sample 2)

## Executed Results (Step 0)
- Test suite: `go test ./internal/server/... ./internal/store/... ./internal/engramclient/... -count=1` → all 3 packages `ok`; verbose run: 129 PASS, 0 FAIL. Targeted reruns (`-run 'Export|ScanEpisodic'` and `-run 'Episodic'`) confirmed every DW-named test executed and passed.
- Typecheck: `go vet ./...` → clean (exit 0).
- Lint: `make lint` (go vet + revive v1.12.0, `-set_exit_status`) → clean.
- Proto: `make proto` regenerates; checksum of `api/engrampb/engram.pb.go` + `api/proto/engram.proto` identical across two consecutive runs → idempotent. (`git status` shows M vs HEAD only because the phase's work is intentionally uncommitted.)

## Requirement Fulfillment

### DW-1.1
PREMISE:  "`ExportEpisodic` in proto + regenerated `engrampb`; proto regeneration is idempotent."
EVIDENCE: api/proto/engram.proto:358 (`message ExportEpisodic`, fields event_id/kind/text/occurred_at/source_ids/scope/team_id/owner_agent_id), :382 (`repeated ExportEpisodic episodics = 4`); api/engrampb/engram.pb.go:1666 (`GetEpisodics`), 24 `ExportEpisodic` occurrences; idempotency: two `make proto` runs → identical shasums.
TRACE:    `memory.Episodic{EventID:"ev-map", Kind:"tool_result", Text:"the full fat body", TextEmbedding:[0.1], ...}` → `exportEpisodicProto` (internal/server/export.go:224) → wire record carries the eight pinned fields, no embedding/outbox field exists on the message to leak — asserted by TestDW_1_1_ExportEpisodicFieldMapping (PASS).
VERDICT:  PASS

### DW-1.2
PREMISE:  "`Export` drains episodic (own string cursor) then chains to entities/edges; unknown stage → `InvalidArgument`; empty episodic tier chains into entities in the same response."
EVIDENCE: internal/server/export.go:137-165 (episodic stage, `EpAfter` string sub-cursor, chain at :164), :96-98 (unknown stage rejected), :131-134 (opaque `InvalidArgument`).
TRACE:    5 episodic recs paged 2-at-a-time + 3 entities + 2 edges → page1 = 2 episodics only (cursor stage=episodic, EpAfter advanced); drain = 5/3/2 records in exactly 3 pages, final short episodic page sharing its response with the whole graph — TestDW_1_2_ExportDrainsEpisodicThenChains (PASS). Cursor `base64({"s":"bogus-stage",...})` → `InvalidArgument` "invalid cursor" — TestExportGarbageCursorInvalidArgument (PASS). Empty wired episodic tier → `ScanEpisodic` returns (nil,"",nil) → :164 rewrites `cur` to stageEntities in the same invocation → 0/2/1 records, terminal cursor, one response — TestDW_1_2_EmptyEpisodicTierChainsSameResponse (PASS). Forged episodic sub-cursor → `store.ErrBadCursor` → opaque `InvalidArgument`, never Internal — TestDW_1_2_BadEpisodicSubCursorInvalidArgument (PASS).
VERDICT:  PASS

### DW-1.3
PREMISE:  "every episodic record passes `canExport`; missing identity → `Unauthenticated`; ACL error → call fails closed (three tests)."
EVIDENCE: internal/server/export.go:127-130 (identity fail-closed before any scan), :148-158 (per-record `canExport`, denied omitted, error aborts), :213-218.
TRACE:    3 recs, middle one owner="deny-me", ACL denies it → 2 exported, denied EventID absent — TestDW_1_3_EpisodicACLDeniedOmitted (PASS). `context.Background()` (no identity) with episodic seam wired → `Unauthenticated` — TestDW_1_3_ExportNoIdentityRejectedWithEpisodic (PASS). ACL returns error → `Internal`, nil response, no partial page — TestDW_1_3_EpisodicACLErrorFailsClosed (PASS). Exactly the three required tests, all executed.
VERDICT:  PASS

### DW-1.4
PREMISE:  "`engramclient.ExportPage` exposes episodic records across pages via the byte-bounded stage; unprocessed/dead-lettered docs are absent."
EVIDENCE: internal/engramclient/export.go:49-55, 75-79 (episodic adaptation); internal/engramclient/export_test.go:97-152 (real gRPC server behind the production auth interceptor); internal/store/facts.go:516-524 (query pins `processed_at` exists + `must_not dead_lettered:true`).
TRACE:    5 server-side episodics at 2/page over a loopback gRPC connection → `ExportPage` drains 5 plain `ExportEpisodic` structs across ≥3 pages, in scan order, fields intact, graph entity arriving on a later page — TestDW_1_4_ClientExportPageEpisodicsAcrossPages (PASS). Absence of unprocessed/dead-lettered docs is by query construction, pinned request-body assertion — TestDW_1_4_ScanEpisodicQueryShape (PASS): filter = [tenant term, processed_at exists], must_not = [dead_lettered:true].
VERDICT:  PASS

### DW-1.5
PREMISE:  "episodic page respects a byte budget (a synthetic oversized-Text set produces multiple pages, none exceeding the bound)."
EVIDENCE: internal/store/facts.go:476 (`EpisodicPageByteBudget = 2<<20`), :540-549 (cut-before-overflow, ≥1 record kept), :551-559 (resume token = last INCLUDED record's sort key).
TRACE:    three records of ~1 MiB−1 KiB each (two fit, three don't) → 2 pages, each ≤ budget, ev-1/ev-2/ev-3 each exactly once, and page-2 `search_after` carries ev-2 (the cut position — truncation never skips records) — TestDW_1_5_ScanEpisodicByteBudgetSplitsPages (PASS). Single record of budget+4096 bytes → returned alone, cursor advances past it, next page delivers the following record — TestDW_1_5_ScanEpisodicSingleOversizedRecordStillProgresses (PASS).
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have DW-ID-named automated tests that ran in Step 0 (DW_1_1 through DW_1_5, listed above).
- [x] Coverage level: new phase code is fully exercised — `go tool cover`: internal/server/export.go every function 100.0%; `ScanEpisodic` 95.8% (only the `json.Marshal(episodicAfter{int64,string})` error branch at facts.go:556-558 is uncovered — unreachable defensive code); `searchEpisodics` 82.4% (marshal/decode error branches); engramclient's uncovered lines (71-73, 89-96, 102-104) are all pre-existing Phase-2 graph code per `git diff` — the new episodic adaptation loop is fully covered.

## Dead Code
None found. vet + revive clean; no debug statements, commented-out blocks, or unreachable code in the reviewed files.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | Unary handler with no mutable Server state on this path; per-call locals only. |
| Error Handling | PASS | Probed every failure edge: episodic scan error → Internal, no partial page (TestExportEpisodicScanErrorFailsClosed); bad sub-cursor → ErrBadCursor before any query (TestScanEpisodic_BadCursorRejected asserts 0 HTTP calls); missing index → empty tier not error (TestScanEpisodic_MissingIndexReturnsEmptyNotError); other 5xx propagates (TestScanEpisodic_NonIndexNotFoundErrorPropagates). No case found where a failure degrades to disclosure or silent restart. |
| Resources | PASS | One HTTP round trip per ScanEpisodic via shared client; no handles/locks/goroutines opened on this path. |
| Boundaries | PASS | Traced empty-recs: facts.go:551 returns before the :554 `recs[len(recs)-1]` index (len 0 < batch, !truncated) — no panic; truncation guard `kept > 0` guarantees ≥1 record so `recs[:kept]` never empty when :554 is reached; exactly-batch-size untruncated page returns a token and the follow-up call exhausts cleanly (TestScanEpisodic_FullPageResumesWithSearchAfter). |
| Security | PASS | Tenant pinned from verified identity only — `ExportRequest` has a single `cursor` field (engram.proto:311-313), `exportCursor` carries no tenancy (export.go:66-72), store query pins the tenant term from the argument, never the token (facts.go:519). Forged cursor cannot bypass processed/dead-letter/ACL filters: they are query clauses and a per-record handler check, both independent of `search_after`. Cross-tenant replay stays empty (TestExportStaleCursorStaysSafe, TestDW_2_3_ExportTenantIsolation). Forward progress: oversized single record returned alone with an advanced cursor — no empty-page loop (TestDW_1_5_ScanEpisodicSingleOversizedRecordStillProgresses). |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| aposd-designing-deep-modules | Information hiding: internals never cross the interface | PASS | TextEmbedding + outbox fields (processed_at, lease, attempts, dead-letter) have no wire representation (TestDW_1_1_ExportEpisodicFieldMapping feeds a record WITH TextEmbedding; proto message has no such field); episodic token opaque to server and client; engrampb types stay inside the engramclient adapter. |
| aposd-designing-deep-modules | Interface depth / no shallow seam | PASS | `EpisodicExporter` is one method hiding the full OpenSearch query shape, byte budgeting, filtering, and cursor encoding; separate from `Exporter` for a documented subsystem reason (export.go:37-45), not temporal decomposition. |
| aposd-designing-deep-modules | Silent failure | PASS | Every internal failure surfaces (Internal/InvalidArgument); the one swallowed error (`json.Marshal` of string-only struct, export.go:76) is a documented cannot-fail. |
| cc-defensive-programming | External input validated at entry | PASS | The single client-controlled input (cursor) hits a two-layer barricade: decodeExportCursor (base64/JSON/stage allowlist, export.go:84-100) then store-side token decode gated by ErrBadCursor before any query issues (facts.go:526-531, TestScanEpisodic_BadCursorRejected). |
| cc-defensive-programming | Security-critical path validates again inside the barricade | PASS | Identity re-checked in-handler even for in-process callers bypassing the interceptor (export.go:127-130, TestDW_1_3_ExportNoIdentityRejectedWithEpisodic); ACL per record on every tier. |
| cc-defensive-programming | No empty catch blocks / errors never swallowed | PASS | All error returns propagate or map to a gRPC status; fail-closed on ACL and scan errors (four *FailsClosed tests). |
| cc-defensive-programming | Correctness over robustness at the tenant boundary | PASS | Uncertainty aborts the call (Internal, nil response) rather than emitting a partial page — asserted by every fail-closed test. |

## Edge Cases (prompt-listed)
| Edge case | Status | Evidence |
|---|---|---|
| Empty episodic tier chains into entity stage same response; empty tenant = one page | PASS | TestDW_1_2_EmptyEpisodicTierChainsSameResponse; TestDW_2_2_ExportEmptyTenantOneTerminalPage |
| Stale/forged cursor repositions only inside caller's tenant | PASS | TestExportStaleCursorStaysSafe (replay against emptied backend and against foreign-tenant data → zero foreign records); undecodable forms rejected opaquely (TestExportGarbageCursorInvalidArgument, TestDW_1_2_BadEpisodicSubCursorInvalidArgument) |
| ACL-denied record omitted; ACL error fails whole call closed | PASS | TestDW_1_3_EpisodicACLDeniedOmitted; TestDW_1_3_EpisodicACLErrorFailsClosed |
| Unprocessed / dead-lettered docs excluded from the scan | PASS | TestDW_1_4_ScanEpisodicQueryShape pins `processed_at exists` filter + `dead_lettered:true` must_not in the actual request body |

## Notes (non-blocking)
- A single episodic record whose Text exceeds ~4 MB would produce a page over gRPC's default client receive limit — the scan itself still progresses (the documented degenerate case), but the client would error on that page. No requirement bounds a single record; flagging for awareness if unbounded ingest text is possible.
- `status.Errorf(codes.Internal, "export episodics: %v", err)` (and siblings) forwards backend error text to the client; consistent with the existing entity/edge paths, but on a security-sensitive path a logged-server-side/opaque-client-side split would leak less.
- Residual uncovered branches are all defensive/unreachable or pre-existing Phase-2 code (see Test-DW Coverage); the new phase logic itself is fully covered.
- `internal/server/export.go` now imports `internal/store` solely for the `ErrBadCursor` sentinel — a small layering coupling between the handler and one store implementation; acceptable, but a seam-level sentinel would decouple future EpisodicExporter implementations.

## Issues
None.

**Verdict: PASS**

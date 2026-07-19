# Review: Phase 1 - Server episodic export stage (sample 1)

## Executed Results (Step 0)
- Test suite: `go test ./internal/server/... ./internal/store/... ./internal/engramclient/... -count=1` → all three packages `ok`, zero failures. `-v` run enumerated every export/episodic test; all `--- PASS`.
- Typecheck/build: `go vet ./...` → exit 0, no output; `go build ./...` → exit 0.
- Lint: `golangci-lint run ./internal/server/... ./internal/store/... ./internal/engramclient/...` → `0 issues.`
- Proto regeneration (DW-1.1): `make proto` → `codegen: ok`; `shasum api/engrampb/engram.pb.go` before and after a second regeneration → identical hash `6703c61d…bc05` (byte-identical, idempotent). The `M` in `git status --short api/engrampb` is versus HEAD — this work is intentionally uncommitted per the dispatch; the hash comparison is the idempotency evidence.

## Requirement Fulfillment

### DW-1.1
PREMISE:  "`ExportEpisodic` in proto + regenerated `engrampb`; proto regeneration is idempotent."
EVIDENCE: api/proto/engram.proto:358-373 (`message ExportEpisodic`, fields event_id/kind/text/occurred_at/source_ids/scope/team_id/owner_agent_id); api/proto/engram.proto:379-383 (`repeated ExportEpisodic episodics = 4` in ExportResponse); api/engrampb/engram.pb.go:1496-1514 (generated `type ExportEpisodic struct` with matching field tags).
TRACE:    `make proto` → `codegen: ok` → shasum of engram.pb.go unchanged across a second regeneration (6703c61d…). Generated type exercised end-to-end by `TestDW_1_1_ExportEpisodicFieldMapping` (internal/server/export_test.go:722): a `memory.Episodic` with all fields (including internal TextEmbedding/ProcessedAt) → `Export` → wire record carries exactly the 8 exported fields and round-trips them.
VERDICT:  PASS

### DW-1.2
PREMISE:  "`Export` drains episodic (own string cursor) then chains to entities/edges; unknown stage → `InvalidArgument`; empty episodic tier chains into entities in the same response."
EVIDENCE: internal/server/export.go:66-72 (exportCursor.EpAfter — episodic stage's own string sub-cursor, distinct from graph.Cursor `After`); export.go:137-165 (episodic stage: scan, non-empty next → return episodic-only page with stage=episodic cursor; next=="" → falls through to `cur = exportCursor{Stage: stageEntities}` and the entity stage in the same invocation); export.go:96-98 (unknown stage → error → export.go:131-134 maps to `InvalidArgument "invalid cursor"`).
TRACE:    (a) 5 episodic recs @ pageSize 2 + 3 entities + 2 edges → `TestDW_1_2_ExportDrainsEpisodicThenChains` (export_test.go:564): first page = 2 episodics / 0 entities / 0 edges with continuation; full walk = 5/3/2 in exactly 3 pages (third page carries the final episodic AND the whole graph — chain observed), scan order preserved, no duplicates. (b) forged base64 of `{"s":"bogus-stage","a":""}` → `TestExportGarbageCursorInvalidArgument` (export_test.go:369, case 3) → InvalidArgument with the opaque "invalid cursor" message. (c) wired-but-empty episodic tier + 2 entities + 1 edge → `TestDW_1_2_EmptyEpisodicTierChainsSameResponse` (export_test.go:609) → 0/2/1 in ONE terminal response, empty next_cursor.
VERDICT:  PASS

### DW-1.3
PREMISE:  "every episodic record passes `canExport`; missing identity → `Unauthenticated`; ACL error → call fails closed (three tests)."
EVIDENCE: internal/server/export.go:148-158 (per-record `s.canExport` in the episodic loop; denied → omitted, error → whole call `Internal`); export.go:127-130 (missing identity → `Unauthenticated` before any scan); export.go:213-218 (canExport = ACL.CanRead per ReadAuthorizer contract).
TRACE:    Three executed tests: `TestDW_1_3_EpisodicACLDeniedOmitted` (export_test.go:646 — record with OwnerAgentID "deny-me" absent from the walk, other 2 present, call succeeds); `TestDW_1_3_EpisodicACLErrorFailsClosed` (export_test.go:670 — ACL error → codes.Internal, nil response, no partial page); `TestDW_1_3_ExportNoIdentityRejectedWithEpisodic` (export_test.go:690 — bare context.Background() → codes.Unauthenticated). All ran and passed in Step 0.
VERDICT:  PASS

### DW-1.4
PREMISE:  "`engramclient.ExportPage` exposes episodic records across pages via the byte-bounded stage; unprocessed/dead-lettered docs are absent."
EVIDENCE: internal/engramclient/export.go:49-55, 69-98 (`ExportEpisodic` plain struct + `ExportPage` adapter mapping resp.GetEpisodics()); internal/store/facts.go:516-524 (query construction: `processed_at` exists filter + `must_not dead_lettered:true` — exclusion is by query shape, independent of any cursor input).
TRACE:    (a) `TestDW_1_4_ClientExportPageEpisodicsAcrossPages` (internal/engramclient/export_test.go:97): real gRPC server on loopback behind the production auth interceptor, 5 episodics @ 2/page + 1 entity → client drains via `ExportPage`; 5 plain-struct episodics across ≥3 pages, in scan order, EventID/Kind/Text/OccurredAt/SourceIDs intact, plus the entity from the chained graph tier. (b) `TestDW_1_4_ScanEpisodicQueryShape` (internal/store/scanepisodic_test.go:95): captured _search body pins tenant term + processed_at-exists filter + dead_lettered must_not — unprocessed/dead-lettered docs never match the scan.
VERDICT:  PASS

### DW-1.5
PREMISE:  "episodic page respects a byte budget (a synthetic oversized-Text set produces multiple pages, none exceeding the bound)."
EVIDENCE: internal/store/facts.go:476 (`EpisodicPageByteBudget = 2 << 20`); facts.go:540-549 (cut before the record that would overflow, never below one record); facts.go:554-559 (resume token from the LAST INCLUDED record — the cut position).
TRACE:    `TestDW_1_5_ScanEpisodicByteBudgetSplitsPages` (scanepisodic_test.go:204): three records of ~budget/2−1KiB each → 2 pages, each page's Text bytes ≤ EpisodicPageByteBudget, all three records exactly once in order, and the page-2 search_after carries ev-2's sort key (the cut), not ev-3's — truncation skips nothing. `TestDW_1_5_ScanEpisodicSingleOversizedRecordStillProgresses` (scanepisodic_test.go:246): a record whose Text alone exceeds the budget returns alone and the cursor advances past it. Both passed in Step 0.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have corresponding tests that ran in Step 0 (test names carry DW-IDs: DW_1_1 ×1, DW_1_2 ×3, DW_1_3 ×3, DW_1_4 ×2 (client + store), DW_1_5 ×2)
- [x] DW-1.1's idempotency clause is desk-checked observed behavior (shasum identical across `make proto` runs) — the one clause no unit test can exercise; the generated type itself is covered by TestDW_1_1_ExportEpisodicFieldMapping
- [x] Coverage matches the stated 100% level: every DW clause maps to at least one executed test or recorded observation; supporting non-DW-named tests (TestScanEpisodic_BadCursorRejected, TestScanEpisodic_FullPageResumesWithSearchAfter, TestScanEpisodic_MissingIndexReturnsEmptyNotError, TestScanEpisodic_NonIndexNotFoundErrorPropagates, TestExportEpisodicScanErrorFailsClosed, TestExportStaleCursorStaysSafe) close the error/cursor paths

## Dead Code
None found. golangci-lint reports 0 issues; manual scan of export.go, facts.go (ScanEpisodic/searchEpisodics additions), and engramclient/export.go found no unused imports, unreachable code, debug statements, or commented-out blocks. The one ignored error (`json.Marshal` in encodeExportCursor, export.go:76) is proven infallible: exportCursor is string fields plus graph.Cursor, whose `MarshalText` (internal/graph/store.go:89) is `return []byte(c.after), nil` — cannot error.

## Edge Cases (prompt-listed — verdict standing)
| Edge case | Status | Evidence |
|---|---|---|
| Empty episodic tier → chains into entity stage in the same response (one page total) | PASS | TestDW_1_2_EmptyEpisodicTierChainsSameResponse: 0 episodics / 2 entities / 1 edge, empty cursor, single round trip |
| Stale/forged cursor repositions only inside the caller's tenant | PASS | exportCursor carries no tenancy (export.go:66-72); tenant term pinned from the argument, never the token (facts.go:519, 526-531); TestExportStaleCursorStaysSafe replays a t1 cursor against foreign-tenant data → 0 foreign entities; TestScanEpisodic_BadCursorRejected: undecodable token refused BEFORE any query (0 requests issued) |
| ACL-denied record omitted; ACL error fails whole call closed | PASS | TestDW_1_3_EpisodicACLDeniedOmitted + TestDW_1_3_EpisodicACLErrorFailsClosed (Internal, nil response) |
| Unprocessed / dead-lettered episodic docs excluded from the scan | PASS | Query-construction exclusion (facts.go:520-522), pinned by TestDW_1_4_ScanEpisodicQueryShape; filters are built independently of the cursor, so no cursor value can bypass them |

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | Unary handler; no shared mutable state touched by Export (Server fields read-only, ScanEpisodic stateless per call). Adversarial case — two concurrent exports replaying one cursor — duplicates reads only; no write path exists. No defect demonstrable. |
| Error Handling | PASS | Every failure path traced and executed: undecodable wire cursor / bad sub-cursor → opaque InvalidArgument (never Internal, never silent restart — TestDW_1_2_BadEpisodicSubCursorInvalidArgument, TestScanEpisodic_BadCursorRejected); scan failure → Internal, no partial page (TestExportEpisodicScanErrorFailsClosed); missing index → empty tier not error (TestScanEpisodic_MissingIndexReturnsEmptyNotError); other backend failures propagate (TestScanEpisodic_NonIndexNotFoundErrorPropagates). |
| Resources | PASS | Store requests are ctx-scoped via doJSON; no handles/locks held across calls; test servers cleaned via t.Cleanup. |
| Boundaries | PASS | Adversarial traces on ScanEpisodic (facts.go:540-559): empty result → kept=0, truncated=false, early return at :551-552 BEFORE `recs[len(recs)-1]` at :554 — no index panic; single record → kept=1 (kept>0 guard skips the break on the first record); exactly-DefaultScanBatchSize untruncated page → token issued, next call returns empty page + "" (TestScanEpisodic_FullPageResumesWithSearchAfter). |
| Security | PASS | Tenant pinned from verified identity only: ExportRequest has no tenancy field (engram.proto:311-313), handler fails closed Unauthenticated on missing identity even for in-process callers (export.go:127-130), id.TenantID passed to every scan (export.go:139,167,189). Forged cursor traced: not-base64 / not-JSON / unknown stage → InvalidArgument; valid-JSON forged EpAfter → search_after values only — the tenant/processed/dead-letter filters are assembled from arguments before the token is consulted (facts.go:510-531), so a token can reposition, never widen or unfilter. Forward progress: kept>0 guard (facts.go:542) returns an oversized record alone with an advancing cursor — TestDW_1_5_ScanEpisodicSingleOversizedRecordStillProgresses proves no infinite empty-page wedge. |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| aposd-designing-deep-modules | Deep interface / information hiding | PASS | EpisodicExporter is one method hiding query shape, filters, byte budget, and token encoding (export.go:46-48); the wire cursor is opaque to clients (encode/decode confined to export.go:75-100); engramclient exposes transport-free plain structs. Internal fields (TextEmbedding, outbox state, NameKey) never cross the wire — proto message simply lacks them (engram.proto:358-373). |
| aposd-designing-deep-modules | No information leakage across boundaries | PASS | Store token format (episodicAfter JSON) known only to facts.go; server round-trips it as an opaque string; client round-trips the wire cursor as an opaque string. Three layers, each blind to the inner encoding. |
| cc-defensive-programming | External input validated at entry (barricade) | PASS | The ONE client-controlled input (cursor) is validated at decodeExportCursor (export.go:84-100): unknown stage rejected; the episodic sub-cursor is re-validated at the store (ErrBadCursor before any query — TestScanEpisodic_BadCursorRejected asserts 0 requests issued). Error detail never leaks (opaque "invalid cursor", asserted by tests). |
| cc-defensive-programming | No empty catch / no swallowed errors | PASS | The single ignored error (export.go:76) is proven infallible — graph.Cursor.MarshalText is `return []byte(c.after), nil` (graph/store.go:89) and all other fields are strings; comment documents it. Every other error path returns or maps to a status code. |
| cc-defensive-programming | Security-critical path: defense in depth | PASS | Identity re-checked inside the handler despite the auth interceptor (export.go:127-130 — TestDW_1_3_ExportNoIdentityRejectedWithEpisodic exercises the interceptor-bypassed path); per-record ACL on top of tenant scoping; ACL uncertainty fails closed. Correctness-over-robustness posture appropriate for a tenant-boundary export. |
| cc-defensive-programming | Assertions vs error handling | PASS | No assertions used; all anticipated conditions (bad cursor, missing index, backend failure) use error handling — correct for a Go service with runtime-reachable conditions. |

## Notes (non-blocking)
- decodeExportCursor's json.Unmarshal ignores unknown fields, so a forged cursor with extra keys decodes silently; harmless — stage whitelist and store-level re-validation still gate it.
- The 2 MiB byte budget counts only Text bytes; kind/event_id/source_ids ride free. The headroom argument (facts.go:471-476 — 4 MB gRPC cap) holds for realistic field sizes, and the worst same-response chain (near-2 MiB final episodic page + one entity page of ≤500 small records) stays well under the cap; not demonstrable as a defect.
- fakeEpisodicExporter would panic on a negative Atoi token — test-only code, exempt per protocol.
- Nil EpisodicExporter degrades to an empty episodic tier rather than Unimplemented (export.go:138, 164) — deliberate and documented (only the graph-bound Exporter gates the RPC); production wiring sets both (cmd/engram-server/main.go:291-292), and TestDW_2_4_ExportNilExporterUnimplemented still guards the primary seam.

## Issues
None.

**Verdict: PASS**

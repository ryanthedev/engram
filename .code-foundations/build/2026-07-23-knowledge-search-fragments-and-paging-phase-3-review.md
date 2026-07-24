# Review: Phase 3 - Offset paging & vault export fix

## Executed Results (Step 0)
- Unit suite: `go test -count=1 ./internal/retrieval/... ./internal/server/... ./internal/mcp/... ./internal/cli/... ./internal/engramclient/...` → all `ok` (retrieval 0.053s, server 0.019s, mcp 0.021s, cli 0.128s, engramclient 0.025s)
- Full suite: `go test ./...` → all `ok`, no failures
- Integration (live OpenSearch :9200): `ENGRAM_OPENSEARCH_URL=http://localhost:9200 go test -tags=integration -count=1 ./internal/server/ ./internal/retrieval/ ./internal/cli/...` → all `ok` (server 1.259s, retrieval 1.746s, cli 0.138s)
- Build: `go build ./...` → exit 0
- Vet: `go vet ./...` → exit 0; `go vet -tags=integration ./...` → exit 0
- Lint: `make lint` (go vet + revive) → exit 0, no findings

## Requirement Fulfillment

### DW-3.1
PREMISE:  `knowledge_search` with `offset` returns the correct page; response `total` is the exact match count (not a capped estimate).
EVIDENCE: internal/retrieval/knowledge.go:159-186 (`Search` clamps offset, sets `trackTotalHits: true` unconditionally, returns `totalHits(decoded)`); internal/retrieval/opensearch.go:760-764 (`buildQuery` emits `"from"` only when `offset>0`, `"track_total_hits":true` only when the flag is set); internal/server/knowledge.go:180 (handler threads `req.GetOffset()` straight to `Search` and copies `total` unmodified onto the response); internal/mcp/tools.go:440-449 (`callKnowledgeSearch` threads `args.Offset` and attaches `result.Total = total` after packing).
TRACE:    offset=50 request → `KnowledgeRetriever.Search(..., k=10, offset=50, ...)` → `buildQuery` emits `{"from":50,"track_total_hits":true,...}` → fake server returns total=137, 1 hit → `Search` returns `(hits, 137, nil)` unmodified → server handler copies `total=137` to `KnowledgeSearchResponse.Total` → MCP tool copies to `searchResult.Total`. Verified live: `TestKnowledgeSearchDW_3_1_OffsetThreadedToRequestAndTotalReturned` (retrieval, unit) and `TestDW_3_1_KnowledgeSearchHandlerThreadsOffsetAndTotal` (server, unit) and `TestDW_3_1_KnowledgeSearchToolThreadsOffsetAndTotal` (mcp, unit) all PASS; the "DW-3.1: offset paging + exact total" section (lines 219-247) inside `TestDW_6_1_KnowledgeEndToEnd` (server, integration, live OpenSearch) pages a real 3-doc collection with k=2 across two pages, confirms page1/page2 hits differ and total==3 on both pages, and confirms offset=1000 past the real total returns 0 hits with total still exact — PASSES.
VERDICT:  PASS

### DW-3.2
PREMISE:  `offset+k` exceeding `max_result_window` yields a self-correcting error naming the cap, not a raw OpenSearch 500.
EVIDENCE: internal/retrieval/knowledge.go:159-164 (`if off+ck > MaxResultWindow { return ... fmt.Errorf("... exceeds max_result_window %d ...") }` — fires before `buildQuery`/`postSearch` are ever called); internal/retrieval/knowledge.go:62 (`MaxResultWindow = 10000`, documented as OpenSearch's own default, matching the cluster default that throws `search_phase_execution_exception`).
TRACE:    `Search(ctx, spec, "x", nil, nil, k=50, offset=MaxResultWindow, false)` → `off+ck = 10050 > 10000` → returns `("", 0, error naming "10000", "offset", "k")` with **zero HTTP calls** (`captured()` asserted empty). `TestKnowledgeSearchDW_3_2_OffsetPlusKExceedsMaxResultWindow` PASSES; the boundary case `offset+k == MaxResultWindow` (`TestKnowledgeSearchDW_3_2_OffsetPlusKAtExactWindowSucceeds`) PASSES and issues exactly one HTTP request, confirming the clamp rejects only strictly-over-cap requests.
VERDICT:  PASS

### DW-3.3
PREMISE:  `fetchKnowledgeDocs` pages until drained; the `MaxK` truncation warning and stale no-offset comment are removed; the export of a collection larger than the per-call cap is complete.
EVIDENCE: internal/cli/vaultknowledge.go:75-102 (`fetchKnowledgeDocs`: loop `offset := 0; for { ... k=retrieval.MaxK, offset ...; offset += len(hits); if len(hits)==0 || int64(offset) >= total { break } }`) — no truncation-warning text or stale comment anywhere in the file (grep-verified absent); internal/cli/export_test.go:1073-1112 (`TestDW_3_3_KnowledgeCollectionFullyDrainsBeyondMaxK`, `n := retrieval.MaxK*2+37`, spans 3 pages).
TRACE:    237 docs (`retrieval.MaxK*2+37`, MaxK=100) in one collection → page1 offset=0 returns 100 hits, offset→100; page2 offset=100 returns 100 hits, offset→200; page3 offset=200 returns 37 hits, offset→237==total → break. All 237 rendered as `knowledge/*.md` notes; output contains "237 knowledge docs"; no "warning:" + "curated_notes" substring present. Test PASSES (confirmed via `go test -count=1 ./internal/cli/...`).
VERDICT:  PASS

### DW-3.4
PREMISE:  `go build ./...` and existing `vaultknowledge` tests pass with the new signature.
EVIDENCE: `go build ./...` exit 0; internal/cli/vaultknowledge_test.go (12 test functions covering `decodeKnowledgeHit`, `knowledgeDocBase`, `hubConceptIDs`, `resolveKnowledgeDocs`, `memoryRefLine`, `renderKnowledgeVault`, `appendConceptBacklinks`); `KnowledgeSearch`'s 8-arg signature (`collection, query string, filters []Predicate, sort []SortKey, k, offset int, fullBody bool`) is consistent across `internal/mcp/mcp.go` (Backend interface), `internal/engramclient/knowledge.go`, `internal/server/knowledge.go` (KnowledgeReader interface), `internal/retrieval/knowledge.go` (KnowledgeRetriever.Search).
TRACE:    `go build ./...` compiles the whole module including every caller of the new 8-parameter `Search`/`KnowledgeSearch` signature with no type errors → exit 0. `go test -count=1 ./internal/cli/...` → `ok`, all vaultknowledge_test.go and export_test.go cases pass.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] DW-3.1: `TestBuildQueryDW_3_1_OffsetEmitsFromKey`, `TestBuildQueryDW_3_1_TrackTotalHitsGatedByFlag`, `TestKnowledgeSearchDW_3_1_OffsetThreadedToRequestAndTotalReturned`, `TestKnowledgeSearchDW_3_1_OffsetBeyondTotalIsEmptyNotError`, `TestKnowledgeSearchDW_3_1_NegativeOffsetClampsToZero` (retrieval); `TestDW_3_1_KnowledgeSearchHandlerThreadsOffsetAndTotal` (server) + the DW-3.1 offset-paging section inside `TestDW_6_1_KnowledgeEndToEnd` (server, integration, live OpenSearch); `TestDW_3_1_KnowledgeSearchToolThreadsOffsetAndTotal`, `TestKnowledgeSearchToolNegativeOffsetIsToolError` (mcp)
- [x] DW-3.2: `TestKnowledgeSearchDW_3_2_OffsetPlusKExceedsMaxResultWindow`, `TestKnowledgeSearchDW_3_2_OffsetPlusKAtExactWindowSucceeds` (retrieval)
- [x] DW-3.3: `TestDW_3_3_KnowledgeCollectionFullyDrainsBeyondMaxK` (cli/export_test.go)
- [x] DW-3.4: build + full `vaultknowledge_test.go`/`export_test.go` suites
- [x] All DW items have corresponding tests (ran in Step 0)
- [x] Test coverage matches the stated 100% level — every DW item has at least one automated test that ran and passed; DW-3.1 and DW-3.2 additionally have real-OpenSearch integration coverage (DW-3.1) or a deliberately-mocked-HTTP-never-called assertion (DW-3.2, appropriate since a 10,000-doc real index isn't a reasonable integration fixture)

No gaps.

## Dead Code
None found. Grepped internal/cli/vaultknowledge.go and internal/cli/export.go for the old MaxK-truncation-warning string and any "no-offset" comment referenced in DW-3.3 — both are fully absent, not just commented out or dead-code-orphaned.

## Correctness Dimensions

| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | Phase 3 touches no goroutines/shared mutable state; `fetchKnowledgeDocs`' loop and `KnowledgeRetriever.Search` are single-threaded per call, same as the surrounding (already-reviewed) MultiRetriever.Search concurrency, which is untouched by this phase. |
| Error Handling | PASS | Traced: `off+ck > MaxResultWindow` fails closed before any HTTP call (knowledge.go:160-164); a spill/fetch error in `fetchKnowledgeDocs` aborts the whole fetch and is surfaced as a caught warning by export.go:128-136, never touching the already-written memory vault (traced `TestDW_2_5_KnowledgeFetchFailureIsSoftWarning`, passes). |
| Resources | N/A | No new file handles/connections/locks introduced this phase; `fetchKnowledgeDocs`' loop is bounded (offset strictly increases by `len(hits)>=1` each non-terminal iteration, or the loop terminates), so no resource/iteration leak. |
| Boundaries | PASS | Traced the specific adversarial cases named in the Edge Cases list: offset==total (empty, not last-page-with-stale-total — `TestKnowledgeSearchDW_3_1_OffsetBeyondTotalIsEmptyNotError` uses offset=9000 vs total=3, PASS); offset+k exactly at the boundary (`TestKnowledgeSearchDW_3_2_OffsetPlusKAtExactWindowSucceeds`, PASS); negative offset clamps to 0 rather than reaching the wire (`TestKnowledgeSearchDW_3_1_NegativeOffsetClampsToZero`, PASS); a collection with total < MaxK drains in one iteration (traced: first page returns `hits < MaxK` and `offset(==len(hits)) >= total` on iteration 1, confirmed by `TestDW_3_3_KnowledgeCollectionFullyDrainsBeyondMaxK`'s own single-page collections in export_test.go's other tests, e.g. TestDW_2_1 with 2 docs). |
| Security | N/A | No new external-input surface: `offset`/`k` were already externally-supplied and clamped pre-Phase-3 (`clampK`); `clampOffset` follows the identical silent-normalize posture. `MaxResultWindow` is a compile-time constant, not attacker-influenceable. |

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | GC-1: does the routine protect itself from bad input data (offset/k as external input)? | PASS | `clampOffset` (knowledge.go:70-75) and the pre-existing `clampK` normalize negative/oversized values before they reach the query; the offset+k-over-window case is explicitly rejected with a named error rather than silently clamped or passed through, per the MaxResultWindow doc comment (knowledge.go:55-62). |
| cc-defensive-programming | EC-3 / RF-2: no empty catch blocks / swallowed errors | PASS | Traced every new/changed error path in this phase (opensearch.go buildQuery has no error return; knowledge.go Search's three early-return error paths; vaultknowledge.go's fetchKnowledgeDocs error path at line 88-90) — every error is either returned to the caller or (export.go:128-130) explicitly logged as a warning with the underlying error interpolated, never discarded. |
| cc-defensive-programming | GS-3: is the amount of defensive code appropriate, not excessive? | PASS | `clampOffset`'s silent-normalize (rather than erroring) mirrors the existing `clampK` posture per its own doc comment, and the MaxResultWindow rejection is the one case that genuinely needs a caller-visible error (a silently-clamped offset would silently return the wrong page) — proportionate, not scattered. |
| cc-routine-and-class-design | PP-4: 7 or fewer parameters | WARNING | `KnowledgeRetriever.Search(ctx, spec, query, filters, sortKeys, k, offset, fullBody)` — internal/retrieval/knowledge.go:144 — has 8 parameters, one over the PASS threshold (6-7), landing in the skill's own 8-9 WARNING band ("justify in review or redesign"), not the 10+ VIOLATION band. `offset` is this phase's one added parameter; the same 8-param shape is threaded consistently through `KnowledgeReader.Search` (server/knowledge.go:61), `Backend.KnowledgeSearch` (mcp/mcp.go:249), and `Client.KnowledgeSearch` (engramclient/knowledge.go:63), so the interface is at least uniform rather than diverging per layer. A parameter object (e.g. a `KnowledgeSearchRequest` struct) would bring this under the PASS threshold, but per the skill's own severity table this is a documented-in-review concern, not a blocking defect, and no Done-When item calls for a signature redesign. |
| cc-routine-and-class-design | RP-6: functional cohesion of touched routines | PASS | `fetchKnowledgeDocs` (one operation: "drain a collection to completion"), `clampOffset` (one operation), `KnowledgeRetriever.Search`'s added `off+ck` check is a guard clause, not a second operation grafted onto the routine. No routine in this phase's diff mixes unrelated operations under "and"/"then" naming. |

## Notes (non-blocking)

- `KnowledgeRetriever.Search` at 8 parameters is a WARNING-band (not VIOLATION-band) parameter count per cc-routine-and-class-design; see the Loaded-Skill Criteria row above. Not a blocker, but worth a mention if a future phase adds a 9th knob.
- internal/server/knowledge.go wraps a `MaxResultWindow`-exceeded error (caller-supplied offset+k, arguably caller input) as `codes.Internal` rather than `codes.InvalidArgument`, per the handler's existing comment "a retriever failure here is infrastructure, not caller input." This does not violate DW-3.2 — the self-correcting message text (naming the cap, offset, and k) survives verbatim through to the MCP tool caller regardless of gRPC status classification (traced through `tools.go:441-442`'s `toolError(fmt.Sprintf("knowledge search failed: %v", err))`) — but a future reviewer might reasonably ask whether this specific retriever error should be reclassified as InvalidArgument for consistency with the barricade's other externally-caused validation errors.

## Issues (if FAIL)
None.

**Verdict: PASS**

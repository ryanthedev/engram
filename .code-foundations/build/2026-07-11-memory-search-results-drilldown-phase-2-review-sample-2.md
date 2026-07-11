# Review: Phase 2 - memory_read(id, source) drill-down (sample 2)

## Executed Results (Step 0)
- Test suite: `go test ./...` → **700 passed, 0 failed, 41 packages** (e2e behind `-tags=e2e` excluded, as expected)
- Targeted: `go test ./internal/server/ -run TestServerRead -v` → 17 pass; `./internal/store/ -run TestOpenSearchStoreGetEpisodic` → 1 pass; `./internal/mcp/ -run MemoryRead` → 9 pass; `./internal/engramclient/ -run TestReadResultFromProto` → 2 pass
- Build: `go build ./...` → success
- Typecheck/vet: `go vet ./...` → no issues
- Lint: `make lint` (vet + revive) → clean
- Proto: `make proto-check` → **exit 2, but this is a pre-commit artifact, not a regen defect.** The target runs `git diff --exit-code -- api/engrampb`, and the whole phase is uncommitted, so the diff vs HEAD is non-empty for ANY correct phase. Substantive check performed instead: sha256 of `api/engrampb/*.pb.go` before and after `make proto` → **byte-identical (REGEN-IDEMPOTENT)**. Once the orchestrator commits, `make proto-check` will pass.

## Requirement Fulfillment

### DW-2.1
PREMISE:  "`memory_read(id, source=episodic)` returns the full untruncated `text` for an id like one surfaced by memory_search."
EVIDENCE: internal/server/read.go:67-112; internal/mcp/tools.go:186-208; internal/mcp/read_test.go:36-72; internal/server/read_test.go:73-106
TRACE:    ingest 740-rune body (> 200-rune Phase-1 snippet cap) → memory_search line proven NOT to contain full body → `memory_read(id, "episodic")` → `callRead` → backend `Read` → server `readEpisodic` fetch→authorize→project → `fields.text == body`, emoji intact. Server-level: `spyEpisodicReader` returns record with `longBody` → `resp.Episodic.Text == longBody` plus event_id/kind/source_ids/occurred_at.
VERDICT:  **PASS** — `TestToolsCall_DW_2_1_MemoryReadReturnsFullEpisodicText`, `TestServerRead_DW_2_1_EpisodicReturnsFullRecord` executed and passed.

### DW-2.2
PREMISE:  "`memory_read(id, source=semantic)` returns the full fact plus provenance/version history (via `Audit`)."
EVIDENCE: internal/server/read.go:119-142 (delegates to `s.Audit`, then selects the TARGET version by id into `resp.Fact`); internal/engramclient/client.go:161-177; internal/server/read_test.go:111-135; internal/engramclient/read_internal_test.go:46-93
TRACE:    request id "v1" (superseded, `invalid_at` set) → `readSemantic` → `s.Audit` (fetch→tenant pin→CanRead) → response carries `Fact=v1` with closed interval, `Provenance.owner_agent_id="a1"`, `Versions=[v1,v2]`. Adapter renders provenance + 2-version history, open bounds absent.
VERDICT:  **PASS** — `TestServerRead_DW_2_2_SemanticDelegatesToAudit`, `TestReadResultFromProtoSemantic` executed and passed.

### DW-2.3
PREMISE:  "a cross-tenant / unauthorized id yields fail-closed `NOT_FOUND` with NO content or existence leak."
EVIDENCE: internal/server/read.go:59-61 (single `errReadNotFound`), 83-84 (tenant pin), 94-96 (ACL deny), 125-127 (Audit NOT_FOUND normalized to the same message); internal/server/read_test.go:141-195
TRACE:    identity tenant `t-other`, record tenant `t1` → tenant check fails → `NOT_FOUND "record not found"`, `resp == nil`. Same for ACL-denied (both tiers) and missing identity. The test then asserts every NOT_FOUND denial message is byte-identical (no-oracle loop, read_test.go:190-194) — denial indistinguishable from absence.
VERDICT:  **PASS** — `TestServerRead_DW_2_3_2_4_FailClosed` (11 subtests) executed and passed.

### DW-2.4
PREMISE:  "a read whose ACL fields would deny is rejected fail-closed (observable denied-read test); fetch→authorize→project ordering is implemented explicitly."
EVIDENCE: internal/server/read.go:73-99 — FETCH (line 74, whole record incl. ACL fields), AUTHORIZE (83 tenant pin; 87-96 `CanRead`), PROJECT (100-111, ACL fields absent from `EpisodicRecord`, which cannot even carry them per proto engram.proto:190-202); internal/server/read_test.go:96-105, 155-156
TRACE:    denied case: `spyReadAuthz{allow:false}` → `NOT_FOUND`, nil response. Ordering observable: `spyReadAuthz.saw[0] == acl.Record{t1, teamX, team, a1}` — the enforcer saw the record's PRE-projection ACL fields, proving authorize happens on the fetched-whole record before projection. Store side: `TestOpenSearchStoreGetEpisodic` (getepisodic_test.go:36-41) pins that the getter returns ACL fields intact for the handler to authorize against.
VERDICT:  **PASS** — executed denied-read subtest + saw-record assertion passed.

### DW-2.5
PREMISE:  "read output is structured JSON with `fields` as a real object, not a stringified `fields_json`."
EVIDENCE: internal/mcp/mcp.go:43-56 (`Fields map[string]any`); internal/engramclient/client.go:149-179 (builds real objects, never re-stringifies); internal/mcp/read_test.go:77-105
TRACE:    wire-level tools/call → text block unmarshals as JSON → no `fields_json` key present → `decoded["fields"]` type-asserts to `map[string]any` → `fields.text == "full body here"`, id/source echoed.
VERDICT:  **PASS** — `TestToolsCall_DW_2_5_MemoryReadEmitsStructuredJSON`, `TestReadResultFromProtoEpisodic` executed and passed.

### DW-2.6
PREMISE:  "proto is regenerated, the `Read` RPC is present in generated stubs, build/tests green."
EVIDENCE: api/proto/engram.proto:63,176-218; api/engrampb/engram_grpc.pb.go:46,85,199,329-342,389-390 (`Engram_Read_FullMethodName`, client/server interfaces, handler, service descriptor)
TRACE:    `make proto` regen → sha256 of all `*.pb.go` unchanged (regen idempotent, tree content IS fresh regen output) → `Read` present in stubs → `go build ./...` success → 700 tests pass, vet + lint clean. `make proto-check` exits 2 only because the phase is uncommitted (git diff vs HEAD non-empty by construction pre-commit) — see Executed Results.
VERDICT:  **PASS** (with the pre-commit proto-check caveat recorded above — the substantive property, regen-clean, is demonstrated).

### DW-2.7
PREMISE:  "an id/`source` mismatch or unknown id returns `NOT_FOUND` without probing the other index."
EVIDENCE: internal/server/read.go:45-56 (source switch dispatches to exactly one branch); internal/server/read_test.go:200-231 (spy call counters); internal/mcp/read_test.go:140-155
TRACE:    semantic id with source=episodic → episodic reader called once (miss) → `NOT_FOUND`; `spyAuditor.called == 0`. Reverse direction: Auditor called once, episodic reader called 0 times. Store getter (facts.go:52-68) GETs only `s.episodicIndex`. MCP-level: mismatch vs never-existed produce byte-identical tool-error text.
VERDICT:  **PASS** — `TestServerRead_DW_2_7_NoCrossIndexProbe` (both directions), `TestToolsCallMemoryReadWrongSourceIsOpaque` executed and passed.

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have automated tests that ran in Step 0 (test names reference DW-IDs: `TestServerRead_DW_2_1/2_2/2_3_2_4/2_7`, `TestToolsCall_DW_2_1/2_5`, plus store/adapter pins)
- [x] Coverage level "100% of done-when items, ≥1 dirty test" met — three dirty tests: `TestServerRead_DW_2_3_2_4_FailClosed` (11 adversarial subtests + message-equality oracle check), `TestToolsCallMemoryReadValidation`, `TestToolsCallMemoryReadWrongSourceIsOpaque`
- DW-2.6 is covered by observed behavior (regen idempotence hash check + build/vet/lint runs) — inherently non-unit-testable.

## Edge Cases (prompt-listed)
| Edge case | Handled | Evidence (executed) |
|---|---|---|
| unknown/absent id → NOT_FOUND | YES | read_test.go:151-152 (episodic); read_test.go:216-230 (semantic, Auditor ok=false → normalized NOT_FOUND) |
| cross-tenant id → deny, no existence leak | YES | read_test.go:153-154, 159-160; identical-message loop 190-194 |
| id/source mismatch → NOT_FOUND, no cross-index probe | YES | read_test.go:200-231 spy counters; mcp read_test.go:140-155 |
| ACL denial indistinguishable from not-found | YES | single `errReadNotFound` (read.go:61) + Audit-message normalization (read.go:125-127) + equality assertion |
| superseded semantic id → that version w/ closed interval | YES | read_test.go:111-135 (`v1.InvalidAt` set, returned as `Fact`, not an error) |
| missing/blank `source` → validation error | YES | server: InvalidArgument (read.go:55, read_test.go:165-166); MCP barricade: tool error (tools.go:194-198, mcp read_test.go:116-118) |

## Dead Code
None found. (tools.go:200 graph branch is reachable — `readSources` deliberately includes "graph" so the short-circuit message fires; read.go's `sourceGraph` const is used at line 50.)

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | Handlers stateless; `readSources` map is read-only after init; `toolSchemas()` returns fresh maps per call. No shared mutable state introduced. |
| Error Handling | PASS | Store getter handles 200/404/other statuses distinctly (facts.go:57-67); server maps store errors to Internal (never NOT_FOUND, so no false denial masking an outage); MCP surfaces backend errors as tool errors, not protocol failures (read_test.go:120 "unknown id → read failed" ran). |
| Resources | N/A | No handles/connections/locks opened in this phase; store reuses the existing HTTP client. |
| Boundaries | PASS | Empty id, blank/unknown source, graph, unconfigured-Episodic, missing identity all traced to explicit codes (read_test.go table, 11 subtests executed). |
| Security | PASS | Fetch-whole→tenant-pin→CanRead→project ordering proven observably (spy saw pre-projection ACL fields); single opaque denial message asserted byte-identical across unknown/cross-tenant/ACL-denied/mismatch; response proto physically cannot carry ACL fields; tenant pinned from verified identity, never from the request. |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | External input validated at entry | PASS | Agent-supplied args validated at the MCP tool barricade (tools.go:191-202: unmarshal, non-empty, allow-list) |
| cc-defensive-programming | Security-critical path validates again (defense-in-depth, barricade ≠ replacement) | PASS | Server re-validates id/source (read.go:42-56) and authorizes fail-closed on its side of the gRPC boundary — the "internal team API is still external" rule honored |
| cc-defensive-programming | No empty catch / swallowed errors | PASS | Every error branch returns or maps to a status code; no discarded errors in the new paths (`id, _ := IdentityFrom` is fail-closed by construction: zero identity's empty TenantID can never equal a stored tenant — exercised by the "episodic no identity" subtest) |
| cc-defensive-programming | No executable code in assertions / assertions for bugs only | N/A | No assertions used; Go idiom, all conditions handled as runtime errors |
| cc-defensive-programming | Correctness over robustness at the trust boundary | PASS | Every ambiguous case denies (NOT_FOUND) rather than guessing; store outage is Internal, never silently "not found" |

## Notes (non-blocking)
- `make proto-check` cannot pass pre-commit by construction (diff vs HEAD includes the phase's own legitimate changes). Regen idempotence was verified by hash instead; the orchestrator should expect proto-check to go green after commit.
- `readEpisodic`/`Audit` skip `CanRead` when `s.ACL == nil` (read.go:86). `main.go:273` always wires `svc.ACL`, and the tenant pin still applies, so no demonstrated defect — but a nil-ACL deployment would grant intra-tenant cross-scope reads. Pre-existing convention shared with Audit/Search.
- `prune` (client.go:208) drops empty-string fields: an episodic record with empty `text` would return `fields` without a `text` key. Not a listed edge case; no wrong output demonstrated.
- `readSemantic` leaves `resp.Fact` nil if the target id is absent from Audit's versions slice (read.go:135-140). Real `AuditFact` includes the target in its history, so undemonstrated; would surface as a missing `fields` object, not a leak.

## Issues
None blocking.

**Verdict: PASS**

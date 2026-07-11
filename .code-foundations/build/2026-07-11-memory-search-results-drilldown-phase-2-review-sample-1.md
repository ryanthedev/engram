# Review: Phase 2 - memory_read(id, source) drill-down (sample 1)

## Executed Results (Step 0)
- Test suite: `go test ./...` → 700 passed, 0 failed, 41 packages (e2e behind `-tags=e2e`, not run — expected)
- Read-focused verbose run: `go test -v -run 'Read|Episodic' ./internal/server/ ./internal/mcp/ ./internal/store/ ./internal/engramclient/` → 35 PASS, 0 FAIL (log: scratchpad/readtests.log)
- Typecheck/vet: `go vet ./...` → no issues
- Lint: `make lint` → exit 0
- Proto: `make proto-check` → exit 2, but ONLY because Phase-2 work is uncommitted and the target diffs `api/engrampb` against HEAD (which lacks Phase 2). Ran `make proto` a second time and compared SHA-1s of both stub files before/after: byte-identical ("REGEN DETERMINISTIC"). The working-tree stubs ARE fresh regen output; proto-check will pass once the orchestrator commits.

## Requirement Fulfillment

### DW-2.1
PREMISE:  `memory_read(id, source=episodic)` returns the full untruncated `text` for an id like one surfaced by memory_search.
EVIDENCE: internal/server/read.go:67-112 (readEpisodic returns `rec.Text` whole); internal/mcp/tools.go:186-208 (callRead); internal/mcp/read_test.go:36-72
TRACE:    ingest 20×39-rune body → memory_search line proven NOT to contain full body (Phase-1 200-rune gist cap) → `memory_read(id, source=episodic)` → `structuredContent.fields.text == body` byte-for-byte, emoji intact. Server-side: `TestServerRead_DW_2_1_EpisodicReturnsFullRecord` asserts `ep.GetText() == longBody` (> snippet cap) plus event_id/kind/source_ids/occurred_at.
VERDICT:  PASS — `TestToolsCall_DW_2_1_MemoryReadReturnsFullEpisodicText`, `TestServerRead_DW_2_1_EpisodicReturnsFullRecord` (both ran, PASS)

### DW-2.2
PREMISE:  `memory_read(id, source=semantic)` returns the full fact plus provenance/version history (via `Audit`).
EVIDENCE: internal/server/read.go:119-142 (readSemantic delegates to `s.Audit`, sets `resp.Fact` to the version matching docID); internal/engramclient/client.go:161-178 (fact/provenance/versions adaptation)
TRACE:    `Read(id=v1, source=semantic)` with Auditor returning target v1 (closed InvalidAt) + versions [v1,v2] → response: Source=semantic, Fact=v1 with closed interval, Provenance.OwnerAgentId=a1, 2 versions. Client conversion: superseded target keeps `invalid_at`; live version has NO invented `invalid_at`.
VERDICT:  PASS — `TestServerRead_DW_2_2_SemanticDelegatesToAudit`, `TestReadResultFromProtoSemantic` (ran, PASS)

### DW-2.3
PREMISE:  A cross-tenant / unauthorized id yields fail-closed `NOT_FOUND` with NO content or existence leak.
EVIDENCE: internal/server/read.go:61 (single `errReadNotFound`), read.go:83-85 (tenant pin), read.go:94-96 (CanRead deny), read.go:125-127 (semantic NotFound normalized to the same var)
TRACE:    episodic cross-tenant (`authedCtx("t-other")` reading a t1 record) → NotFound, resp==nil; semantic cross-tenant → NotFound, resp==nil; test then asserts EVERY NotFound denial message across both tiers and all deny reasons is byte-identical ("record not found") — no oracle.
VERDICT:  PASS — `TestServerRead_DW_2_3_2_4_FailClosed` (dirty, 11 subtests, ran, PASS)

### DW-2.4
PREMISE:  A read whose ACL fields would deny is rejected fail-closed (observable denied-read test); fetch→authorize→project ordering is implemented explicitly.
EVIDENCE: internal/server/read.go:74 (FETCH whole record incl. ACL fields) → read.go:83-97 (AUTHORIZE: tenant pin, then `s.ACL.CanRead` on `acl.Record{TenantID, TeamID, Scope, OwnerAgentID}`) → read.go:100-111 (PROJECT: `EpisodicRecord` proto carries NO ACL fields — the projection type cannot even represent them)
TRACE:    Denied read: `spyReadAuthz{allow:false}` → NotFound, no response ("episodic ACL denied" subtest). Ordering: `spyReadAuthz.saw[0] == acl.Record{t1, teamX, team, a1}` — the enforcer observed the record's real pre-projection ACL fields, proving fetch-with-ACL-fields precedes authorize precedes project. Store side: `TestOpenSearchStoreGetEpisodic` pins that the getter returns ACL fields intact.
VERDICT:  PASS — `TestServerRead_DW_2_1_...` (spy assertion), `TestServerRead_DW_2_3_2_4_FailClosed`, `TestOpenSearchStoreGetEpisodic` (ran, PASS)

### DW-2.5
PREMISE:  Read output is structured JSON with `fields` as a real object, not a stringified `fields_json`.
EVIDENCE: internal/mcp/mcp.go:43-56 (`ReadResult.Fields map[string]any` with tag `json:"fields"`); internal/engramclient/client.go:149-179 (`readResultFromProto` builds real objects, never re-stringifies)
TRACE:    memory_read over the wire → text block unmarshals as JSON → `decoded["fields"]` type-asserts to `map[string]any` → true; `decoded["fields_json"]` absent; `fields.text == "full body here"`; id/source echoed.
VERDICT:  PASS — `TestToolsCall_DW_2_5_MemoryReadEmitsStructuredJSON`, `TestReadResultFromProtoEpisodic` (ran, PASS)

### DW-2.6
PREMISE:  Proto is regenerated, the `Read` RPC is present in generated stubs, build/tests green.
EVIDENCE: api/proto/engram.proto:63 (`rpc Read(ReadRequest) returns (ReadResponse)`); api/engrampb/engram_grpc.pb.go:46,147,339 (`Engram_Read_FullMethodName = "/engram.v1.Engram/Read"`, client Invoke, server handler registration); engram.pb.go: 15 ReadRequest occurrences
TRACE:    `make proto` run twice → shasum of both generated files byte-identical across runs and identical to the working-tree stubs the phase shipped (regen produced zero changes) → `go test ./...` 700 pass, `go vet` clean, `make lint` exit 0. `make proto-check`'s exit 2 is solely the uncommitted-work diff vs HEAD (its recipe is `make proto; git diff --exit-code -- api/engrampb`), not stub drift.
VERDICT:  PASS — observed behavior recorded above (regen determinism + green suite); no automated test can exercise "regen is committed" pre-commit

### DW-2.7
PREMISE:  An id/`source` mismatch or unknown id returns `NOT_FOUND` without probing the other index.
EVIDENCE: internal/server/read.go:45-56 (switch dispatches to exactly one branch; no fallback path exists); store/facts.go:52-68 (GetEpisodic hits only `s.episodicIndex`)
TRACE:    semantic id with source=episodic → NotFound, `auditor.called == 0`, `episodic.called == 1`; episodic id with source=semantic → NotFound, `episodic.called == 0`, `auditor.called == 1`. MCP layer: mismatch and never-existed ids produce byte-identical tool error text.
VERDICT:  PASS — `TestServerRead_DW_2_7_NoCrossIndexProbe` (dirty, spies), `TestToolsCallMemoryReadWrongSourceIsOpaque` (ran, PASS)

**All requirements met:** YES

## Test-DW Coverage
- [x] DW-2.1, 2.2, 2.3, 2.4, 2.5, 2.7: automated tests with DW-IDs in their names, all ran in Step 0
- [x] DW-2.6: recorded observed behavior (regen determinism check + stub grep + green suite) — commit-state assertion, not automatable pre-commit
- [x] ≥1 dirty test: `TestServerRead_DW_2_3_2_4_FailClosed`, `TestServerRead_DW_2_7_NoCrossIndexProbe`, `TestToolsCallMemoryReadValidation`, `TestToolsCallMemoryReadWrongSourceIsOpaque`
- [x] Coverage level (100% of DW items) met

## Edge Cases (prompt-listed)
| Edge case | Handled | Evidence |
|---|---|---|
| unknown/absent id → NOT_FOUND | YES | "episodic unknown id" subtest; store GetEpisodic(missing) → ok=false, err=nil |
| cross-tenant id → deny, no existence leak | YES | cross-tenant subtests both tiers; identical-message assertion |
| id/source mismatch → NOT_FOUND, no cross-index probe | YES | DW-2.7 spy tests: other index's reader called 0 times |
| ACL denial indistinguishable from not-found | YES | single `errReadNotFound` var (read.go:61); denialMsgs uniformity check spans deny reasons AND tiers |
| superseded semantic id → that version with closed interval, not an error | YES | DW-2.2 test: v1 with `InvalidAt` set returned as Fact; client keeps `invalid_at`, omits open bounds |
| missing/blank `source` → validation error | YES | server "blank source"/"unknown source" → InvalidArgument; MCP barricade → tool error (`TestToolsCallMemoryReadValidation`) |

## Dead Code
None found. `sourceGraph` const is used (read.go:50); `readSources` "graph" entry is deliberately short-circuited (tools.go:200).

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | Read handler is stateless (no fields mutated); store client shared exactly as existing Search/Audit paths |
| Error Handling | PASS | Store error → codes.Internal (read.go:76); Audit Internal/Unimplemented pass through (read.go:128); MCP backend error → isError tool result, never a protocol failure ("unknown id" subtest); nil-Episodic/nil-Auditor → Unimplemented, tested |
| Resources | PASS | GetEpisodic reuses the pre-existing `doJSON` helper identically to GetFact; no new handles/conns |
| Boundaries | PASS | empty id → InvalidArgument; blank/unknown source → InvalidArgument; graph → Unimplemented; missing identity → NotFound (tenant pin fail-closes on zero Identity) — all traced through subtests |
| Security | PASS | Fetch→authorize→project ordering demonstrated via spy (enforcer saw pre-projection ACL fields); projection type (`EpisodicRecord`) structurally cannot carry ACL fields; one opaque NOT_FOUND for absence/cross-tenant/ACL-deny/mismatch; Read is NOT in the auth-interceptor exempt set (main.go:267 passes no exemptions); semantic path reuses Audit's already-fail-closed check rather than a second diverging implementation |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | External input validated at entry (agent args are external) | PASS | MCP tool barricade validates id/source non-empty + whitelist (tools.go:194-202) BEFORE any backend call |
| cc-defensive-programming | Barricade ≠ defense-in-depth exemption on security paths | PASS | Server re-validates id/source (read.go:42-56) and re-authorizes on its side of the gRPC trust boundary despite the tool-layer check |
| cc-defensive-programming | No empty catch / silently swallowed errors | PASS | Every error path returns a status or logged tool error; `id, _ := IdentityFrom(ctx)` discards `ok` but the zero Identity fail-closes via the tenant pin — behavior pinned by the "episodic no identity" subtest |
| cc-defensive-programming | Fail-closed on security-critical path | PASS | All deny paths → one opaque NOT_FOUND; nil response alongside error asserted |
| cc-defensive-programming | Assertions for bugs only / no executable code in assertions | N/A | No assertions used (idiomatic Go) |

## Notes (non-blocking)
1. `s.ACL == nil` skips the scope check (read.go:86), leaving only the tenant pin. This mirrors Audit's pre-existing pattern (server.go:189) and main.go always wires `svc.ACL = aclFilter` (main.go:273), so it is not demonstrable in the shipped configuration — but a future in-process constructor that forgets ACL would get tenant-only isolation, silently. A nil-ACL → deny (or explicit `AllowAll` sentinel) would be stricter.
2. `readSemantic` sets `resp.Fact` only if the target id appears in the returned versions (read.go:135-140); if an Auditor implementation ever returned a target absent from its own version list, the response would carry provenance/versions with a nil fact. Not demonstrable against the real store Auditor (target is always in its chain).
3. `errReadNotFound` message "record not found" differs from Audit's own "fact not found"; a caller with access to BOTH RPCs could distinguish which surface produced a miss, but within Read all denials are uniform — no per-record oracle. Observation only.
4. Reviewer-run `make proto` regenerated stubs in the working tree; regen was verified byte-identical across runs, so no build-agent output was altered.

## Issues (if FAIL)
None.

**Verdict: PASS**

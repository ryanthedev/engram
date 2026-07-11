# Review: Phase 2 - memory_read(id, source) drill-down (sample 3)

## Executed Results (Step 0)
- Test suite: `go test ./...` → 700 passed, 0 failed, 41 packages (e2e behind `-tags=e2e`, not run — expected)
- Focused run: `go test ./internal/server/ ./internal/mcp/ ./internal/store/ ./internal/engramclient/ -run 'Read|GetEpisodic' -v` → all PASS (named evidence below)
- Typecheck/vet: `go vet ./...` → no issues
- Lint: `make lint` (vet + revive) → clean
- Proto: `make proto-check` → exit 2, but ONLY because the phase is uncommitted: it runs codegen then `git diff --exit-code -- api/engrampb` against HEAD, and HEAD predates the phase. The meaningful regen-clean check — hash all `api/engrampb/*.pb.go`, run `./scripts/codegen.sh`, re-hash — shows byte-identical files (IDEMPOTENT). Working tree IS fresh regen output; proto-check will pass once the phase commits.

## Requirement Fulfillment

### DW-2.1
PREMISE:  `memory_read(id, source=episodic)` returns the full untruncated `text` for an id like one surfaced by memory_search.
EVIDENCE: internal/server/read.go:67-112 (readEpisodic), internal/mcp/tools.go:186-208 (callRead), internal/engramclient/client.go:138-160
TRACE:    ingest 760-rune body (> 200-rune Phase-1 snippet cap) → search line carries only the gist → `memory_read(id, "episodic")` → GetEpisodic realtime `_doc/{id}` GET → authorize → ReadResponse.Episodic.Text == full body, emoji intact.
VERDICT:  PASS — `TestServerRead_DW_2_1_EpisodicReturnsFullRecord`, `TestToolsCall_DW_2_1_MemoryReadReturnsFullEpisodicText` (end-to-end over the MCP wire, asserts search does NOT already carry the body), both ran and passed.

### DW-2.2
PREMISE:  `memory_read(id, source=semantic)` returns the full fact plus provenance/version history (via `Audit`).
EVIDENCE: internal/server/read.go:119-142 (readSemantic delegates to `s.Audit`), internal/engramclient/client.go:161-177
TRACE:    `Read(v1, semantic)` → `Audit(v1)` (fetch/tenant-pin/CanRead) → response carries Provenance + 2 Versions + explicit target Fact selected by id from versions; superseded v1 returns with its closed `invalid_at`.
VERDICT:  PASS — `TestServerRead_DW_2_2_SemanticDelegatesToAudit`, `TestReadResultFromProtoSemantic` ran and passed.

### DW-2.3
PREMISE:  a cross-tenant / unauthorized id yields fail-closed `NOT_FOUND` with NO content or existence leak.
EVIDENCE: internal/server/read.go:59-61 (single `errReadNotFound` var), :83-85 (tenant pin), :94-96 (ACL deny), :123-127 (Audit NOT_FOUND normalized to the same message)
TRACE:    identity tenant `t-other` reads record with tenant `t1` → tenant pin fails → `errReadNotFound` ("record not found"), resp nil; test collects every NOT_FOUND denial message (unknown id, cross-tenant, ACL deny, no identity, both tiers) and asserts all byte-identical — no oracle.
VERDICT:  PASS — `TestServerRead_DW_2_3_2_4_FailClosed` (11 subtests, dirty) ran and passed.

### DW-2.4
PREMISE:  a read whose ACL fields would deny is rejected fail-closed (observable denied-read test); fetch→authorize→project ordering is implemented explicitly.
EVIDENCE: internal/server/read.go:74 (FETCH), :83-97 (AUTHORIZE: tenant pin then CanRead), :100-111 (PROJECT); ordering labeled in code comments
TRACE:    `spyReadAuthz{allow:false}` → NOT_FOUND (subtests episodic/semantic ACL denied); ordering proof: `spyReadAuthz.saw[0]` == `acl.Record{t1, teamX, team, a1}` — CanRead observed the record's real pre-projection ACL fields, and the `EpisodicRecord` proto (api/proto/engram.proto:190-202) structurally cannot carry them, so project-before-authorize is impossible in this shape.
VERDICT:  PASS — `TestServerRead_DW_2_3_2_4_FailClosed/episodic_ACL_denied`, `/semantic_ACL_denied`, plus the saw-record assertion in `TestServerRead_DW_2_1_EpisodicReturnsFullRecord`; all ran and passed.

### DW-2.5
PREMISE:  read output is structured JSON with `fields` as a real object, not a stringified `fields_json`.
EVIDENCE: internal/mcp/mcp.go:43-56 (`ReadResult.Fields map[string]any`), internal/engramclient/client.go:149-179 (readResultFromProto builds real maps)
TRACE:    wire-level tools/call → content text unmarshals as JSON → `decoded["fields"]` type-asserts to `map[string]any`, no `fields_json` key present, `fields.text` == ingested body.
VERDICT:  PASS — `TestToolsCall_DW_2_5_MemoryReadEmitsStructuredJSON`, `TestReadResultFromProtoEpisodic` ran and passed.

### DW-2.6
PREMISE:  proto is regenerated, the `Read` RPC is present in generated stubs, build/tests green.
EVIDENCE: api/proto/engram.proto:63 (`rpc Read`), api/engrampb/engram_grpc.pb.go:46,85,199,389 (Read in client iface, server iface, service desc)
TRACE:    `./scripts/codegen.sh` run → shasum before/after identical (regen idempotent, working tree == fresh regen) → `go test ./...` 700 pass, `go vet` clean, `make lint` clean. `make proto-check` exits 2 solely from diffing the uncommitted phase against HEAD (see Executed Results).
VERDICT:  PASS (observed behavior: hash-verified idempotent regen + green suite; see Note 1 on proto-check semantics pre-commit).

### DW-2.7
PREMISE:  an id/`source` mismatch or unknown id returns `NOT_FOUND` without probing the other index.
EVIDENCE: internal/server/read.go:45-56 (switch dispatches to exactly one branch); internal/store/facts.go:52-68 (GetEpisodic hits only the episodic index, 404 → ok=false)
TRACE:    semantic id with source=episodic → episodic reader called once (miss) → NOT_FOUND, spy Auditor called 0 times; inverse case symmetric (Auditor once, episodic reader 0).
VERDICT:  PASS — `TestServerRead_DW_2_7_NoCrossIndexProbe` (2 subtests, spy call counters) ran and passed.

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have DW-named automated tests that ran in Step 0 (DW-2.6 uses recorded observed behavior — regen hash check + green suite — the only form a "regen is clean" assertion can take)
- [x] Coverage level: 100% of DW items; ≥1 dirty test satisfied (`TestServerRead_DW_2_3_2_4_FailClosed`, `TestToolsCallMemoryReadValidation`, `TestToolsCallMemoryReadWrongSourceIsOpaque` are explicitly dirty)
- [x] Edge cases (all prompt-listed, all covered by executed tests):
  - unknown/absent id → NOT_FOUND: `episodic_unknown_id`, MCP `unknown_id`
  - cross-tenant → deny, no existence leak: `episodic_cross-tenant`, `semantic_cross-tenant` + identical-message assertion
  - id/source mismatch → NOT_FOUND, no cross-index probe: `TestServerRead_DW_2_7_NoCrossIndexProbe`, `TestToolsCallMemoryReadWrongSourceIsOpaque`
  - ACL denial indistinguishable from not-found: single `errReadNotFound` var + denialMsgs equality loop
  - superseded semantic id → that version with closed interval: DW-2.2 test (v1.InvalidAt set, returned intact); open bounds stay absent (`TestReadResultFromProtoSemantic`)
  - missing/blank source → validation error: gRPC `blank_source`/`unknown_source` → InvalidArgument; MCP `blank_source` → tool error

## Dead Code
None found. `readSources` includes "graph", so the tools.go:200 graph short-circuit IS reachable (verified: `TestToolsCallMemoryReadValidation/graph_has_no_drill` exercises it). No unused imports (vet/lint clean), no debug statements, no commented-out blocks in the new files.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | Handler is stateless per-request; no shared mutable state introduced (toolSchemas already returns fresh maps per call) |
| Error Handling | PASS | Adversarial trace: store 5xx → GetEpisodic returns error → Internal (read.go:76), never a silent miss; CanRead error → Internal (:92), never allowed; Audit non-NotFound errors pass through (:128). No swallowed errors in new code |
| Resources | N/A | No new handles/locks/caches; store call reuses existing doJSON client |
| Boundaries | PASS | Empty id (:42), empty/unknown source (:55), nil Episodic seam (:68) all traced to explicit errors and tested; nil proto getters safe via GetX accessors; prune handles empty slices/strings |
| Security | PASS | Fetch→authorize→project ordering demonstrated by spy (`authz.saw` holds pre-projection ACL fields); projection proto structurally lacks ACL fields; single opaque NOT_FOUND across absence/cross-tenant/ACL-denial with message-equality test; missing identity fails closed (`episodic_no_identity` subtest); dispatch never probes the other index |

## Loaded-Skill Criteria (cc-defensive-programming)
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | External input validated at entry | PASS | MCP tool barricade validates agent-supplied id/source (tools.go:191-202) AND the gRPC handler re-validates (read.go:42-56) — defense-in-depth on a security-critical path, exactly what the skill requires |
| cc-defensive-programming | No empty catch blocks / swallowed errors | PASS | Every error in the new read path is returned or surfaced as a tool error; adversarially traced store-error and ACL-error branches (read.go:75-77, 91-93) |
| cc-defensive-programming | Barricade design (validate at boundary, fail closed inside) | PASS | authgrpc interceptor rejects unauthenticated calls with opaque Unauthenticated before handlers (interceptor.go:55-71); inside, the handler still re-authorizes per-record (tenant pin + CanRead) rather than trusting the barricade — the skill's security-critical revalidation rule |
| cc-defensive-programming | Assertions for bugs only / no executable code in assertions | N/A | Go; no assertion mechanism used |
| cc-defensive-programming | Correctness-vs-robustness: fail closed on auth | PASS | Zero-value identity (ok discarded at read.go:71) traced: `id.TenantID == ""` ≠ any real tenant → NOT_FOUND; demonstrated by the passing `episodic_no_identity` subtest |

## Notes (non-blocking)
1. `make proto-check` cannot pass pre-commit by construction (it diffs `api/engrampb` against HEAD; the whole phase is uncommitted). The orchestrator should expect it green immediately after the phase commit — codegen idempotency was hash-verified here. Also note this review's runs of `codegen.sh` rewrote `api/engrampb/*.pb.go` in place; output was byte-identical to what the build left, so the tree is unchanged.
2. read.go:71 `id, _ := authgrpc.IdentityFrom(ctx)` discards the ok flag — consistent with the existing Audit/Search pattern (server.go:175, :248) and fail-closed under a zero identity (tested), but a hypothetical record with an empty `tenant_id` would tenant-match an empty identity. The interceptor barricade makes that unreachable in production; flagging as defense-in-depth food for thought, not a demonstrated defect.
3. readSemantic leaves `resp.Fact` nil if the audited id were somehow absent from its own version list — unreachable given AuditFact found the target by that id; harmless if it ever happened (caller still gets provenance + versions).
4. Semantic reads intentionally return Provenance including tenant_id/team_id/scope/owner_agent_id — post-authorization, the caller's own record, per the Audit contract DW-2.2 names.
5. Phase-1 `(id, source)` contract is consumed correctly: `readSources` mirrors the search Hit source values, and the graph short-circuit matches the contract's "graph hits carry their whole statement". No misuse observed.

## Issues
None.

**Verdict: PASS**

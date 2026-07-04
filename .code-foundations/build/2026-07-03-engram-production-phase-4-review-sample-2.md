# Review: Phase 4 - Authorization Layer (security-sensitive sample 2)

## Executed Results (Step 0)
- Full suite (`go test ./...`): **201 passed in 30 packages**, exit 0.
- Targeted unit (`go test ./internal/acl/... ./internal/retrieval/... ./internal/auth/... ./internal/authgrpc/... ./internal/server/...`): **64 passed**, exit 0.
- Integration (live OpenSearch 3.1.0 at :9200, podman `engram-dev-os` up):
  - `retrieval` `-run TestDW_4`: 4 passed. DW-4.5 recall logged **private=1.00 (2.0%), team=1.00 (48%), org=1.00 (50%)** — well within 5 pp of unfiltered 1.00.
  - `store` `-run 'ACL|Audit|DW_4'`: `TestDW_4_3_AppendDeniedByGuard`, `TestDW_4_6_AuditHistory`, `TestMalformedEdgeRejectedAtWrite`, `TestReachabilityRoundTrip` passed.
  - `server` audit: `TestServerAuditAssemblesProvenanceAndVersions`, `TestAuditFailClosed` (5 subtests) passed.
- Typecheck (`go build ./...`): Success. `go vet` on all touched packages: no issues.
- Lint (`revive` on all touched packages): exit 0, no findings.
- e2e (`make e2e`): NOT run — the compose stack (:9201 / :7071) is down and the dispatch permits noting a self-boot conflict rather than mutating the environment. The e2e ACL scenarios (`e2e/scenarios_acl.go`) are compiled under the `e2e` build tag and mirror the integration coverage; each DW item is independently covered by an integration test that DID run against the live cluster, so no DW item rests on e2e alone.

## Requirement Fulfillment

### DW-4.1 — 3×3 identity/scope matrix returns exactly the authorized set per cell
PREMISE:  "3-identity × 3-scope e2e matrix (9 cells) returns exactly the authorized set per cell."
EVIDENCE: internal/acl/filter.go:84-146 (Clause/Authorize); internal/retrieval/acl_integration_test.go:70-109; internal/acl/filter_test.go:52-80; e2e/scenarios_acl.go:65-131.
TRACE:    Enforce(id)→Reach→Clause() builds a bool filter with `should`=[private-own, team(agent∈reach ∧ team∈reach), org(agent∈reach)]. Integration indexes 6 facts × 3 owners × 3 scopes; u1 sees {a1 priv/team/org, a2 team/org}, u2 sees {a2 team/org}, u3 sees {a3 team} — exact-set assertion via `setEqual`. Live run passed.
VERDICT:  PASS

### DW-4.2 — revoking user↔agent edge hides that agent's team/org hits at next query, ≤5 s, no restart
PREMISE:  "revoking a user↔agent edge hides that agent's team/org hits at next query, ≤5 s, no restart."
EVIDENCE: internal/store/acledges.go:95-153 (DeleteEdge point-delete refresh=true; Reachability fresh read, no cache); internal/acl/filter.go:11-13,45-60 (one reachability read per Enforce, no cache); internal/retrieval/acl_integration_test.go:114-156 (asserts ≤5 s and own-fact retention).
TRACE:    DeleteEdge removes the user_agent doc (refresh=true). Next Search → Enforce → Reachability re-queries acl_edges live → a2 absent from Agents → Clause `terms owner_agent_id` no longer includes a2 → a2 team/org hits excluded, a1 own facts retained. Integration measured elapsed ≤5 s. Live run passed.
VERDICT:  PASS

### DW-4.3 — write-time: agent writing an unauthorized scope gets a typed denial (dirty test)
PREMISE:  "write-time rule — agent writing an unauthorized scope gets a typed denial (dirty test)."
EVIDENCE: internal/acl/guard.go:36-69 (ScopeGuard.Check returns ErrScopeDenied/ErrUnknownScope); internal/store/opensearch.go:90-127 (authorizeWrite runs guards on ctx-identity, denial aborts write); internal/server/server.go:105-113 (maps to PermissionDenied opaquely); dirty tests internal/acl/guard_test.go:15-73 (8 cases incl. non-member team, wrong team, org-without-grant, unknown scope, edge-error) + integration internal/store/acl_integration_test.go:40 (`TestDW_4_3_AppendDeniedByGuard`).
TRACE:    Append(team fact, team not in reach) → authorizeWrite → guard.Check → reach.hasTeam=false → ErrScopeDenied → Append returns error before any index write → server maps to PermissionDenied. Live + unit runs passed.
VERDICT:  PASS

### DW-4.4 — fail-closed: induced compiler error AND empty-reachability both yield zero results + logged denial
PREMISE:  "fail-closed — induced compiler error AND empty-reachability both yield zero results + logged denial."
EVIDENCE: Compile error → internal/retrieval/opensearch.go:180-190 (Enforce err → log "ACL denial" + return nil,nil) proven by internal/retrieval/acl_test.go:27-43. Empty reachability → internal/acl/filter.go:55-58 (logs "acl deny: empty reachability", ok=false) + Clause()→match_none (filter.go:84-87) applied inside every tier query (opensearch.go:308-330) → zero docs; proven by internal/acl/filter_test.go:114-133.
TRACE:    (a) errFilter.Enforce→error→Search logs denial, returns 0 hits, registered leak-tier never consulted. (b) ghost identity (no agents)→empty()→true→match_none clause ANDed into query→0 docs; denial logged in Enforce. Both runs passed.
VERDICT:  PASS

### DW-4.5 — filtered-kNN recall at private/team/org selectivities within 5 pp of unfiltered
PREMISE:  "filtered-kNN recall at private/team/org selectivities within 5 pp of unfiltered on the gold set."
EVIDENCE: internal/retrieval/opensearch.go:415-429 (ACL/tenancy/validity filters placed INSIDE the knn clause, not post-filtered); internal/retrieval/acl_integration_test.go:170-255.
TRACE:    300-doc synthetic corpus, ModeKNNOnly, ACL clause as the kNN filter; recall@10 vs brute-force truth per scope. Live: private=1.00, team=1.00, org=1.00 (all ≥0.95 gate). PASS.
VERDICT:  PASS

### DW-4.6 — `engram audit <id>` shows provenance + full version history via as-of query
PREMISE:  "`engram audit <id>` shows provenance + full version history via as-of query."
EVIDENCE: internal/store/audit.go:17-49 (AuditFact: target + full (tenant,subject,predicate) chain incl. retracted versions, deterministic sort); internal/server/server.go:161-210 (Audit RPC, fail-closed NOT_FOUND on cross-tenant/unauthorized/unknown, ACL.CanRead scope check); internal/cli/cli.go:279-295 (runAudit); api/proto/engram.proto:45-50,108-150. Tests: store `TestDW_4_6_AuditHistory`, server `TestServerAuditAssemblesProvenanceAndVersions` + `TestAuditFailClosed` (5 subtests). All passed.
TRACE:    audit(id)→GetFact→history query on (tenant,subject,predicate)→sorted versions; server checks tenant boundary then ACL.CanRead(scope) before emitting; unauthorized/cross-tenant/unknown → NOT_FOUND (no existence oracle). Live + unit passed.
VERDICT:  PASS

**All requirements met:** YES (each DW item individually verified with executed evidence)

## Test-DW Coverage
- [x] Every DW item has a test that ran in Step 0 (unit + live integration).
- [x] ≥1 dirty test per code-touching area: guard_test (write denial), filter_test (empty-reach/edge-error/zero-enforcer/invalid-identity), acl_test (compiler-error fail-closed), interceptor_test (malformed/expired/revoked token), audit fail-closed subtests, malformed-edge-at-write.
- [x] Edge cases: zero-scope identity→deny-all (filter_test:114), revocation ≤5 s (acl_integration:114), filtered-kNN recall (acl_integration:170), malformed acl_edge rejected at write (store/acl_integration:85 + edge_test:13). All covered and passing.
- Gap (non-blocking): no test exercises the tier-source/post-hook seam with `len(merged) > K` — the exact condition that surfaces the truncation-ordering defect below. `TestRegisteredSeamsReceiveIdentityAndAreACLFiltered` uses K=10 with 5 hits, so truncation never fires.

## Dead Code
None found. `go build`, `go vet`, and `revive` (doc-comment enforcement) all clean on the touched packages.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | opensearch.go:196-214 fans built-in tiers + tier-srcs into a `results` slice, each goroutine writing a distinct index, `wg.Wait` before read — no shared-write race. |
| Error Handling | PASS | Fail-closed everywhere: Enforce edge-error → deny+error → Search returns 0 (opensearch.go:180-190); guard edge-error → deny (guard.go:53-57); all-tiers-fail → error (opensearch.go:225-227). No empty catches. |
| Resources | PASS | Every `resp.Body` closed (opensearch.go:346, store doJSON:217). Bounded embed timeout (opensearch.go:368). |
| Boundaries | PASS | `jsonStrings` guards nil→`[]` so `terms` never marshals null (filter.go:88-90,119-126); missing hit fields read as "" and fail closed (opensearch.go:266-275); unknown scope → default deny in both Clause (`should` matches none) and Authorize (filter.go:143). |
| Security | **FAIL** | No identity spoofing: server derives tenancy/Identity from the verified context (server.go:87-90,131-134), private ctx key single-writer (auth.go:52-59). ACL clause applied inside every built-in query (un-bypassable). BUT top-k truncation precedes the final ACL re-filter — see Issue 1. Confidentiality is intact (no unauthorized hit is ever returned); the defect is result-integrity/recall in the tier/post-hook seam. |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| ca-architecture-boundaries | Dependency inversion: policy defines seams, infra implements inward | PASS | `acl` imports only `internal/auth` (acl.go:16-23); EdgeSource/WriteGuard defined in acl, implemented in store (acledges.go:29, guard.go:20). Arrows point inward. |
| ca-architecture-boundaries | SRP by actor: transport vs policy vs infra separated | PASS | authgrpc holds the only gRPC import; acl is transport-free; store is the OpenSearch adapter. |
| aposd-designing-deep-modules | Deep module / information hiding; no silent failure | PASS (with note) | Enforcer hides reachability behind Clause()/Authorize() derived from one rule; denials are logged (no silent failure). Note: `Compile` (filter.go:66) is an unused-in-production alt path to `Enforce().Clause()` — a thin seam kept for the named contract; acceptable but shallow. |
| aposd-designing-deep-modules | Granularity: caller does no ACL work | PASS | Retriever callers pass only Identity; the module compiles + enforces. |
| cc-defensive-programming | Barricade validates external input at entry; re-validate on security path | PASS | Token verified at gRPC edge (interceptor.go:59-70); edges validated before write (acledges.go:71-74); security-critical read/write both re-checked (defense in depth). |
| cc-defensive-programming | Fail-closed on every anticipated failure | PASS | Invalid identity, empty reach, edge-store error, unknown scope, missing hit fields — all deny. Verified by tests that ran. |

## Notes (non-blocking)
1. Post-hook ordering: post-hooks run AFTER truncation (opensearch.go:235-241) and their added hits are never re-truncated, so a P6 hook could return `> q.K` hits, violating the "at most q.K" contract (retriever.go:78). Latent (no hook wired in P4). The same fix as Issue 1 (re-filter/normalize before a final bound) resolves both.
2. `filter.go:66 Compile` is dead in production (main.go wires `Enforce` via the retriever and `CanRead` via audit); it exists only for the named-seam contract. Harmless.
3. `Filter.UserID` (server.go:124) still passes the client-supplied user id into an extra `owner_agent_id` term filter on the ACL path; it can only narrow, never widen (ANDed with the ACL clause), so no bypass — but it is redundant with the compiled clause and slightly muddies the "Identity is authoritative" story.

## Issues (FAIL)
1. **Top-k truncation happens BEFORE the final ACL re-filter, so unauthorized hits from the tier-source/post-hook seam can crowd out authorized hits.**
   - File: internal/retrieval/opensearch.go:228-248 (sort → truncate `merged[:q.K]` at 229-231, THEN `filterAuthorized` at 246-248).
   - Demonstrated by (TRACE): Retriever built `WithACL`, identity {t1,u1,a1} reaching only self. A registered `TierSource` (a real, public seam — `RegisterTier`, opensearch.go:150) returns two hits owned by `a2`/private with scores 100 and 99 (unauthorized; tier sources are NOT covered by the query clause). Built-in tiers return two authorized `a1`/private hits (scores 2.0, 1.0). K=2. Flow: merged=[a1(2.0),a1(1.0),x1(100),x2(99)] → sort desc → [x1,x2,a1,a1] → truncate to K=2 → [x1,x2] → filterAuthorized drops both (owner a2 ≠ a1) → **result = [] , although two authorized hits existed**. The caller loses results it is entitled to.
   - Why it matters: The dispatch explicitly flags this exact ordering ("does top-k truncation happen BEFORE or AFTER the final ACL re-filter? If unauthorized hits can fill the top-k and crowd out authorized ones … that is a correctness/security FAIL"). The re-filter's own doc (opensearch.go:243-245) states its purpose is defense-in-depth against tier/expansion hits the caller may not read — i.e. the design explicitly admits a tier source may emit unauthorized hits. Under that admitted input the ordering corrupts recall (authorized hits silently dropped). Confidentiality is NOT breached — the re-filter still runs, so no unauthorized hit is returned; the failure is result-integrity, and it is the condition the dispatch pre-declared a FAIL.
   - Scope/severity honesty: NOT manifest in Phase-4 production behavior — main.go (cmd/engram-server/main.go:94-99) registers no tier source or post-hook, so every `merged` hit comes from a built-in tier that already applied the ACL clause and `filterAuthorized` is a no-op. The defect is latent in the delivered `RegisterTier`/`RegisterPostHook` seam that Phase 5/6 activates, and it is exercisable today via those public methods.
   - Fix: apply the ACL predicate to non-built-in hits (tier-source and post-hook output) BEFORE the sort/truncate — e.g. re-filter tier-source hits at collection and run `filterAuthorized` on the post-hook output, then bound to `q.K`. This guarantees only authorized hits compete for the top-k and the result never exceeds K.

**Verdict: FAIL — blocker: Issue 1 (truncation precedes the final ACL re-filter; the tier/post-hook seam lets unauthorized hits crowd out authorized ones in the top-k). All six DW items and every listed edge case independently PASS with executed evidence, and no confidentiality leak exists; the fail is the dispatch's explicitly pre-declared truncation-ordering condition, a result-integrity defect in the delivered seam.**

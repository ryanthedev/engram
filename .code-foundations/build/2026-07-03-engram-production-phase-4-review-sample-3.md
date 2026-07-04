# Review: Phase 4 - Authorization Layer (security-sensitive sample 3)

Independent, execution-grounded review. All verdicts re-derived from requirements + code + executed results only.

## Executed Results (Step 0)
- Build: `go build ./...` → **Success** (exit 0)
- Unit tests: `make test` (`go test ./...`) → **all packages ok** (exit 0); acl/auth/authgrpc/retrieval/server/store all pass
- Lint: `make lint` (`go vet ./...` + revive) → **clean** (exit 0, no findings)
- Integration: `make integration` (+ targeted `-tags integration -v` re-run) → **exit 0**, every DW-4 integration test PASS
- E2E: `make e2e` → **ok, 7.987s** (exit 0). `TestDW_3_6_ScenarioPackRunsWithoutCoreEdits` runs every registered scenario as a subtest; the four ACL scenarios (`acl/scope-matrix`, `acl/revoke`, `acl/write-denied`, `acl/audit`) are registered (e2e/scenarios_acl.go:18-21) and the aggregate exit-0 proves each subtest passed
- Race detector: `go test -race ./internal/{retrieval,acl,server,authgrpc,auth}` → **exit 0**, no data races on the concurrent `Search` fan-out

Verbose DW-4 evidence (captured `/private/tmp/p4-review-s3/dw4.txt`):
```
--- PASS: TestDW_4_1_ScopeMatrixCompiledFilter (0.18s)   [retrieval, integration]
--- PASS: TestDW_4_2_RevocationPropagates (0.15s)        [retrieval, integration]
--- PASS: TestDW_4_5_FilteredKNNRecallUnderACL (0.59s)   recall@10=1.00 for private/team/org
--- PASS: TestDW_4_3_AppendDeniedByGuard (0.10s)         [store, integration]
--- PASS: TestMalformedEdgeRejectedAtWrite (0.06s)
--- PASS: TestDW_4_6_AuditHistory (0.07s)                [store, integration]
--- PASS: TestServerAuditAssemblesProvenanceAndVersions / TestAuditFailClosed (5 subcases)
--- PASS: TestDW_4_1_ScopeRuleMatrix / TestDW_4_2_RevokeDropsReachableAgent
--- PASS: TestDW_4_4_CompilerErrorFailsClosed / TestDW_4_4_EmptyReachabilityDenyAll
--- PASS: TestDW_4_3_ScopeGuardDeniesUnauthorized (8 subcases)
```

## Requirement Fulfillment

### DW-4.1
PREMISE:  "3-identity × 3-scope e2e matrix (9 cells) returns exactly the authorized set per cell."
EVIDENCE: internal/retrieval/acl_integration_test.go:70-109 (compiled-filter matrix); internal/acl/filter_test.go:52 (rule matrix); e2e/scenarios_acl.go:62 (`acl/scope-matrix`)
TRACE:    Index 6 facts across owners a1/a2/a3 × scopes private/team/org → 3 identities each Search with their Identity → the compiled OpenSearch clause (filter.go:84-117) admits exactly {own-private, reachable-team, reachable-org}. u1 sees a1_{priv,team,org}+a2_{team,org}; u2 sees a2_{team,org}; u3 sees a3_team only — verified via `setEqual` (exact set, not superset). Positive AND negative cells covered.
VERDICT:  PASS

### DW-4.2
PREMISE:  "revoking a user↔agent edge hides that agent's team/org hits at next query, ≤5 s, no restart."
EVIDENCE: internal/retrieval/acl_integration_test.go:114-156
TRACE:    u1 sees a2_team+a2_org (precondition asserted) → `DeleteEdge(user_agent u1→a2)` → same Retriever instance (no restart), next `Search` → a2 hits gone, u1's own a1_priv retained; `time.Since(start) ≤ 5s` asserted (line 147). Enforce does a fresh `Reachability` read every call (filter.go:50), no cache to bust.
VERDICT:  PASS

### DW-4.3
PREMISE:  "write-time rule — agent writing an unauthorized scope gets a typed denial (dirty test)."
EVIDENCE: internal/store/acl_integration_test.go:40-81; internal/acl/guard_test.go:15 (8 subcases)
TRACE:    Store with `RegisterWriteGuard(NewScopeGuard(edges))`. Allowed: private + team(member teamX) succeed. Denied: team teamY → `errors.Is(err, acl.ErrScopeDenied)` AND `id==""` (nothing written); unknown scope "galactic" → `ErrUnknownScope`. Worker write (no ctx identity) is trusted/unguarded (authorizeWrite returns nil when no Identity in ctx — opensearch.go:94-97). Server maps the typed errors to `codes.PermissionDenied` with an opaque message (server.go:109-111). Genuine dirty test (allowed + denied + no-write assertion).
VERDICT:  PASS

### DW-4.4
PREMISE:  "fail-closed — induced compiler error AND empty-reachability both yield zero results + logged denial."
EVIDENCE: internal/retrieval/acl_test.go:27-43 (compile error); internal/acl/filter_test.go:114 (empty reachability); filter_test.go:138 (edge error)
TRACE:    (a) `errFilter.Enforce` returns an error → Search logs "ACL denial" and returns `nil,nil` even with a leak-tier registered (acl_test.go:31 registers a tier that "must not be consulted") → 0 hits. (b) Empty `Reach{}` → `Enforce` logs "acl deny: empty reachability", Enforcer.ok=false, `Clause()` returns `match_none` (zero docs), `Authorize` denies all. Both paths verified.
VERDICT:  PASS

### DW-4.5
PREMISE:  "filtered-kNN recall at private/team/org selectivities within 5 pp of unfiltered on the gold set."
EVIDENCE: internal/retrieval/acl_integration_test.go:170-255; executed findings in dw4.txt
TRACE:    ACL scope clause is ANDed INSIDE the knn clause (opensearch.go:422-429, `inner["filter"]`), not post-filtered. 300-doc corpus, ModeKNNOnly, three identities each admitting one tier. Measured recall@10 = **1.00** at private (2.0%), team (48.0%), org (50.0%) selectivities — all ≥0.95 (within 5 pp of unfiltered 1.0). Each hit's scope asserted == expected tier (no cross-scope leak).
VERDICT:  PASS

### DW-4.6
PREMISE:  "`engram audit <id>` shows provenance + full version history via as-of query."
EVIDENCE: internal/store/audit.go:17-49 (AuditFact full chain); internal/server/server.go:161-210 (Audit RPC, fail-closed); internal/cli/cli.go:52,279-295 (`audit` command); internal/engramclient/client.go:92 (client.Audit); e2e/scenarios_acl.go:228 (`acl/audit`)
TRACE:    `AuditFact` fetches the target then queries the FULL (tenant,subject,predicate) chain incl. expired/closed versions, chronologically ordered. Server enforces tenancy + `ACL.CanRead` scope check before returning; unknown/cross-tenant/unauthorized all collapse to NOT_FOUND (no existence oracle — `TestAuditFailClosed` 5 subcases). CLI → client → RPC → provenance{owner_agent_id, source_ids, extractor_version, created_at} + version list. All layers tested.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] All 6 DW items have automated tests that ran in Step 0 (unit + integration + e2e), each named for its DW-ID
- [x] Coverage matches the stated level (100% of DW items; ≥1 dirty test per code-touching area — write guard, edge validation, retriever fail-closed, audit fail-closed all have negative/denial cases)
- [x] Edge cases each have a test (see below)
- [x] Seam contracts each exercised (see below)

## Dead Code
None found. `go vet` and revive clean. `filterAuthorized` uses `hits[:0:0]` (fresh backing array, intentional — avoids aliasing input); all helpers (`recordFromHit`, `jsonStrings`, `containsStr`) are referenced. No unreachable code after returns.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | `Search` fans out built-in tiers + tier sources to goroutines writing disjoint `results[i]` indices; `merged` built only after `wg.Wait()`; loop vars passed as params (no capture bug). `-race` run clean (exit 0). |
| Error Handling | PASS | Every ACL/edge/guard error path denies: Enforce error→`nil,nil`+log (opensearch.go:181-187); guard edge error→deny (guard.go:53-57); all-tiers-error→error, partial→logged. No empty catches, no error→allow branch found. |
| Resources | PASS | Every `resp.Body` closed with `defer` (opensearch.go, acledges.go, apply.go); embed timeout context cancelled. |
| Boundaries | PASS (with latent note) | Zero/empty/nil handled: nil ACL slices → non-nil JSON arrays (filter.go:121); empty query short-circuits; `q.K<=0`→DefaultK. **Latent**: top-k truncation precedes the tier/hook re-filter — harmless in Phase-4 wiring, recall risk once tiers register (see Notes #1). |
| Security | PASS | Adversarial probes all fail-closed: identity is authoritative from the verified token, never client metadata (server.go:87-90,131-134,178); Ingest/Search/Audit override tenant+identity; client TeamID/Scope are checked by the write guard, not trusted. No fail-open branch. **No confidentiality leak** — `filterAuthorized` drops every unauthorized hit (incl. unfiltered tier-source/hook hits) before return; verified by `TestRegisteredSeamsReceiveIdentityAndAreACLFiltered` and the DW-4.5 per-hit scope assertions. |

### Critical ordering examination (per dispatch brief)
`MultiRetriever.Search` (opensearch.go:196-249): tier-source hits are collected **unfiltered** (`src.Search(ctx, f.Identity, q)` at line 210 — no `aclClause`), merged with built-in hits, sorted, and truncated `merged[:q.K]` (line 229-231) **BEFORE** `filterAuthorized` runs (line 246-248). This ordering is exactly as the brief flagged. Assessment:
- **Leak?** No. `filterAuthorized` (line 254-262) re-verifies every surviving hit through the ACL predicate; unauthorized hits (incl. those with missing provenance fields, read fail-closed as blank → denied) are dropped before return. The fail-closed security claim holds.
- **Recall / result-integrity?** Yes, latently. See Notes #1 for the demonstrating trace.
- **Reachable in Phase 4?** No. `cmd/engram-server/main.go` registers a retriever `WithACL` but calls **neither** `RegisterTier` **nor** `RegisterPostHook` — `m.tierSrcs`/`m.postHooks` are empty, so `merged` only ever holds built-in hits already filtered by the in-query ACL clause; `filterAuthorized` is then a no-op and truncation is correct. The defect is **latent until Phase 5/6 wire a tier source or post-hook**.

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| ca-architecture-boundaries | Dependency arrows point inward (policy imports no infra) | PASS | `internal/acl` imports only `internal/auth`; `EdgeSource`/`WriteGuard`/`ACLFilter` are consumer-defined seams implemented by `internal/store` (DIP). Verified by imports + package doc acl.go:9-15. |
| ca-architecture-boundaries | SRP by actor | PASS | Read filter (Filter/Enforcer), write guard (ScopeGuard), edge persistence (ACLEdgeStore), transport barricade (authgrpc) are separated; one policy (Clause↔Authorize) shared from the same Reach so read/write cannot diverge. |
| aposd-designing-deep-modules | Deep module / info hiding; no silent failure | PASS | `Enforcer` hides reachability behind Clause()/Authorize(); fail-closed zero value. Failures are surfaced (error return + WarnContext log) not swallowed — no silent-failure red flag. |
| aposd-designing-deep-modules | No information leakage across modules | PASS | `Record` is vendor-neutral; OpenSearch/memory types don't leak into `acl`. |
| cc-defensive-programming | External input validated at entry | PASS | Token validated/hashed before lookup (auth.go:266-277); bearer parsed at barricade (interceptor.go:77-95); `Edge.Validate` rejects malformed edges before any write (edge.go:34; acledges.go:72). |
| cc-defensive-programming | Security-critical path = defense in depth | PASS | Scope enforced at read (in-query clause) AND write (guard), plus post-hoc re-filter; constant-time token compare; opaque transport errors (no oracle). |
| cc-defensive-programming | No empty catch / no error→allow | PASS | All error branches deny/log; grep of ACL paths shows no swallowed error. |

## Notes (non-blocking)

1. **[HIGH — must fix before Phase 5] Top-k truncation precedes the tier/hook re-filter; unfiltered tier-source hits can crowd out authorized built-in hits (recall/result-integrity, latent).**
   - File: `internal/retrieval/opensearch.go:210` (tier src searched without `aclClause`), `:229-231` (truncate), `:246-248` (re-filter runs after truncate)
   - Demonstrating TRACE (Phase-5/6 wiring, one registered TierSource): built-in semantic tier returns 2 authorized hits scored 0.5, 0.4 (already clause-filtered); a registered TierSource returns 10 hits the caller may NOT read scored 0.90…0.81 (unfiltered). `merged` = 12 hits → sort desc = [0.90…0.81 (10 unauth), 0.5, 0.4 (auth)] → `merged[:10]` keeps the 10 unauthorized tier hits and **drops both authorized built-in hits** → `filterAuthorized` drops all 10 unauthorized → **result is empty** though 2 authorized hits existed.
   - Classification: **recall / result-integrity defect, NOT a confidentiality leak** (nothing unauthorized is returned; fail-closed holds).
   - Reachability: **latent** — no `RegisterTier`/`RegisterPostHook` call in current Phase-4 wiring (`cmd/engram-server/main.go`), so it cannot manifest today. It becomes live the moment Phase 5 registers the experience tier or Phase 6 registers graph expansion with a source whose hits need re-filtering.
   - Why the existing seam test misses it: `TestRegisteredSeamsReceiveIdentityAndAreACLFiltered` registers a tier with 4 hits + 1 hook hit (5 < K=10), so truncation never fires; it proves no-leak but not recall preservation.
   - Fix: run `filterAuthorized` (and re-filter after each post-hook) **before** the `merged[:q.K]` truncation, so top-k is computed over authorized hits only; or push the ACL clause into `TierSource.Search` (pass `aclClause`) so tier hits are never unfiltered in the merge. Add a regression test with >K unauthorized tier hits.
   - Verdict rationale: not a Phase-4 blocker because it is unreachable in current wiring, violates no Phase-4 DW item or prompt-listed edge case, and does not break the fail-closed security claim. Flagged HIGH so it is fixed before the seam is wired.

2. **[LOW] Post-hooks receive the already-truncated `merged` and their added hits are not re-truncated** (opensearch.go:235-241), so a post-hook could return >K hits. Phase-6 concern; couples with #1. Same fix (re-filter/re-truncate after hooks) resolves it.

3. **[INFO] `make test` does not pass `-race`.** The only concurrent path (Search fan-out) is race-clean under an explicit `-race` run, but adding `-race` to the default target would guard future changes.

## Issues (if FAIL)
None blocking.

**Verdict: PASS.** All 6 DW items verified with execution evidence; all prompt-listed edge cases and seam contracts covered by passing tests; fail-closed security claim holds under adversarial probing (no leak, no fail-open, no identity spoofing). The flagged `Search` ordering is a real recall/result-integrity defect but is latent — unreachable in current Phase-4 wiring (no tier/post-hook registered) and not a confidentiality leak — so it is recorded as a HIGH-severity must-fix-before-Phase-5 note, not a Phase-4 blocker.

# Review: Phase 4 - Authorization layer (provenance-as-ACL, query-time enforcement) — Security Sample 1

Independent, adversarial re-verification. I did not read the build agent's design/discovery notes; every verdict below rests on the code plus tests I executed myself against the live dev cluster (OpenSearch 3.1.0 at localhost:9200, podman `engram-dev-os`).

## Executed Results (Step 0)
- Build: `go build ./...` → Success (exit 0)
- Unit suite: `go test ./...` → **201 passed** in 30 packages (exit 0). Includes acl unit tests (matrix, revoke-predicate, guard, both fail-closed paths, zero-enforcer, invalid-identity), retrieval acl_test (compiler-error fail-closed + seam re-filter), server audit unit tests.
- Integration (live cluster): `ENGRAM_OPENSEARCH_URL=http://localhost:9200 go test -tags=integration -run 'DW_4|ACL|Reachability|MalformedEdge|Audit|WriteGuard|Guard' ./internal/store/ ./internal/retrieval/ ./internal/server/` → **17 passed** in 3 packages (exit 0).
- E2E (self-boot against dev cluster, real CLI + MCP): `ENGRAM_OPENSEARCH_URL=http://localhost:9200 go test -tags=e2e -count=1 -timeout=300s ./e2e/` → **10 passed** (exit 0). Note: ran the self-boot path (compose not booted); this hosts embed+stub+engramd on the host against the 9200 cluster, and exercises all four ACL scenarios (scope-matrix, revoke, write-denied, audit) through the compiled `engram`/`engram-mcp` binaries.
- Typecheck: covered by `go build` / `go vet ./...` → No issues found.
- Lint: `revive -config revive.toml -set_exit_status -exclude ./api/engrampb/... ./...` → exit 0, zero findings.

## Requirement Fulfillment

### DW-4.1 — 3-identity × 3-scope e2e matrix (9 cells) returns exactly the authorized set
PREMISE:  9-cell identity×scope matrix returns exactly the authorized set in every cell.
EVIDENCE: internal/acl/filter.go:84-146 (Clause/Authorize, one rule two forms); internal/acl/filter_test.go:52-80 (unit matrix); internal/retrieval/acl_integration_test.go:70-109 (compiled OpenSearch filter, 6 facts × 3 identities); e2e/scenarios_acl.go:65-131 (full CLI matrix).
TRACE:    u1(a1, reaches a2, teamX) → Enforce reads reach → Clause ANDs `tenant AND (private∧owner=a1 OR team∧owner∈{a1,a2}∧team∈{teamX} OR org∧owner∈{a1,a2})` inside the query → returns {a1_priv,a1_team,a1_org,a2_team,a2_org}; u2 → {a2_team,a2_org}; u3(teamY) → {a3_team}. Integration assertion `setEqual` requires exact set equality per cell; e2e asserts exact per-marker visibility.
VERDICT:  PASS (unit + live-compiled-filter + CLI all green)

### DW-4.2 — revoking a user↔agent edge hides that agent's team/org hits at next query, ≤5 s, no restart
PREMISE:  revoking user↔agent edge hides that agent's team/org-scoped hits from the user's next query, ≤5 s, no restart.
EVIDENCE: internal/acl/filter.go:45-60 + edge.go:97-99 comment (fresh reachability read every call, no cache); internal/store/acledges.go:95-109 (DeleteEdge, refresh=true); internal/retrieval/acl_integration_test.go:114-156 (live, times the bite); e2e/scenarios_acl.go:157-202.
TRACE:    Pre: u1 sees a2_team,a2_org. `DeleteEdge(user_agent u1→a2)` with refresh=true → next `Search` calls `Enforce`→`Reachability` (fresh _search, no cache)→a2 dropped from Agents→Clause `terms owner_agent_id` no longer contains a2→a2 facts excluded. Integration asserts `time.Since(start) <= 5s` and a2 facts gone while a1_priv retained; e2e asserts same through CLI with no server restart.
VERDICT:  PASS

### DW-4.3 — write-time rule enforced; unauthorized-scope write gets a typed denial (dirty test)
PREMISE:  an agent writing an unauthorized scope gets a typed denial.
EVIDENCE: internal/acl/guard.go:36-69 (ScopeGuard.Check); internal/store/opensearch.go:90-104,121-127,149-155 (authorizeWrite runs before the HTTP write); internal/server/server.go:104-113 (maps ErrScopeDenied/ErrUnknownScope → PermissionDenied); internal/acl/guard_test.go:15-54; internal/store/acl_integration_test.go:40-81 (live); e2e/scenarios_acl.go:204-226.
TRACE:    u1 (member teamX, no teamY, no org grant) `Append(scope=team, team=teamY)` → authorizeWrite→Check: scope=team, reach.hasTeam("teamY")=false → returns `fmt.Errorf("%w: team %q", ErrScopeDenied,...)`; Append returns `("", err)` WITHOUT issuing the POST. Integration asserts `errors.Is(err, ErrScopeDenied) && id==""` (nothing written) and unknown scope → ErrUnknownScope. Dirty cases present (denied team, wrong team, org-no-grant, unknown scope).
VERDICT:  PASS

### DW-4.4 — fail-closed: induced compiler error AND empty-reachability both yield zero results + logged denial, not open access
PREMISE:  both an induced compiler error and an empty-reachability identity yield zero results + logged denial, not open access.
EVIDENCE: internal/retrieval/opensearch.go:180-190 (Enforce error → `return nil, nil` + WarnContext "ACL denial"); internal/acl/filter.go:46-58 (invalid/empty/edge-error → deny-all Enforcer, logged); Clause() match_none at filter.go:85-87; tests: retrieval/acl_test.go:27-43 (induced compile error, tier that would leak is never consulted → 0 hits) and acl/filter_test.go:114-133 (empty reachability → match_none, authorizes nothing, "acl deny" logged), plus edge-error (138-156) and zero-Enforcer (161-169).
TRACE:    (a) errFilter.Enforce→error → Search logs "ACL denial" and returns 0 hits though a leak-tier is registered. (b) identity "ghost" with empty Agents → `reach.empty()` true → Enforcer.ok=false → Clause = `{"match_none":{}}` (query returns 0 docs) and Authorize returns false for every record; "acl deny: empty reachability (fail-closed)" logged. No branch maps error→allow.
VERDICT:  PASS

### DW-4.5 — filtered-kNN recall at private/team/org selectivities within 5 pp of unfiltered recall
PREMISE:  filtered-kNN recall at private/team/org selectivities within 5 pp of unfiltered recall on the gold set.
EVIDENCE: internal/retrieval/opensearch.go:415-429 (ACL clause placed INSIDE the knn `filter`, not post-filtered); internal/retrieval/acl_integration_test.go:170-255 (300-doc synthetic corpus, k=10, three selectivities ~2%/~48%/~50%).
TRACE:    For each tier, brute-force top-k over the admitted set is the truth; ACL-filtered kNN search runs with the tier's identity; recall = |found∩truth|/|truth|; test fails if recall < 0.95 (i.e. >5 pp below unfiltered 1.0) and also asserts no scope leak in the hit set. All three tiers passed in my live run (part of the 17 integration passes).
VERDICT:  PASS

### DW-4.6 — `engram audit <id>` shows provenance + full version history via the as-of query contract
PREMISE:  `engram audit <id>` shows provenance + full version history via the as-of query contract.
EVIDENCE: internal/store/audit.go:17-49 (AuditFact returns target provenance + full bi-temporal chain incl. expired/closed versions, chronologically sorted); api/proto/engram.proto:45-50,108-151 (Audit RPC, Provenance, FactVersion); internal/server/server.go:161-210 (fail-closed, tenant + CanRead scope check); internal/engramclient/client.go:91-... (Audit); internal/cli/cli.go:279-302 (`engram audit`); tests: store/acl_integration_test.go:139-181 (v1 closed + v2 live, ordered), server/audit_test.go:52-124 (provenance + versions + all-NotFound denial paths), e2e/scenarios_acl.go:228-267 (CLI audit shows owner + versions).
TRACE:    `audit v2` → GetFact(v2) → history search on (tenant,subject,predicate) returns v1(closed)+v2 → sorted by created_at,valid_at,id → server authorizes read (tenant match + CanRead) → response carries Provenance{owner_agent_id, source_ids, extractor_version, created_at} + 2 FactVersions with interval bounds. CLI prints JSON incl. `"owner_agent_id"` and `"versions"`.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have automated tests that ran in Step 0 (unit + integration + e2e), each named for its DW id.
- [x] Coverage level (100% of DW items, ≥1 dirty test per code-touching area) is met: acl (guard/edge/filter negative cases), store (denied append, malformed edge, unknown scope), retrieval (compiler-error + unauthorized-hit drop), server (all-NotFound audit denials), e2e (write-denied scenario).
- No gaps found.

## Edge Cases (prompt-listed)
| Edge case | Handled | Evidence |
|-----------|---------|----------|
| identity with zero scopes → deny-all | YES | filter.go:55-58 `reach.empty()`→deny; acl_test.go:114-133 asserts match_none + authorizes nothing |
| revocation mid-session ≤5 s | YES | acledges.go DeleteEdge refresh=true + fresh read per query; acl_integration_test.go:142-152 times ≤5s; e2e revoke scenario |
| filtered-kNN recall re-verified at ACL selectivities | YES | acl_integration_test.go:170-255, recall≥0.95 at 3 selectivities (live pass) |
| malformed acl_edge docs rejected at write | YES | edge.go:34-53 Validate; acledges.go:71-74 rejects before PUT; edge_test.go:13-38 + acl_integration_test.go:85-99 (not persisted) |

## Seam Contracts (Phases 5-6 dependencies)
| Contract | Real? | Evidence |
|----------|-------|----------|
| ACLFilter.Compile(ctx, Identity) fail-closed | YES | filter.go:66-69 (deny-all clause + propagated error); retrieval seam uses Enforce, opensearch.go:180-190 |
| Retriever.RegisterPostHook / PostHook.Apply — hits re-filtered | YES | opensearch.go:157-159,235-248; retrieval/acl_test.go:85-132 drops `expanded-leak` |
| Retriever.RegisterTier / TierSource.Search — gated tier | YES | opensearch.go:150-152,206-213 + filterAuthorized; acl_test.go drops foreign tier hits, receives Identity |
| Store.RegisterWriteGuard / WriteGuard.Check (non-nil = deny, fail-closed) | YES | opensearch.go:80-104; guard.go; main.go:98 wires ScopeGuard |
| Audit RPC + `engram audit` CLI | YES | proto:45-50; server.go:161-210; cli.go:279-302; e2e aclAudit |

## Dead Code
None found. All exported symbols (incl. `tierRetriever.Search`, `Filter.CanRead`, `Filter.Compile`) have live callers in production wiring or tests. No unreachable branches after early returns; no debug/commented-out blocks.

## Correctness Dimensions (adversarial)
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | MultiRetriever.Search (opensearch.go:196-214) pre-sizes `results` and each goroutine writes its own disjoint index; `enf`/`aclClause` computed before fan-out and only read; `wg.Wait()` precedes the merge read. No shared mutable write. Traced tier+tierSrc index math (`len(m.tiers)+j`) — disjoint, correct. |
| Error Handling | PASS | Every error path is deny/zero, never allow: Enforce (invalid/empty/edge-err→deny), Search (compile-err→nil,nil+log), ScopeGuard (edge-err→deny), authorizeWrite (guard err returned verbatim so `errors.Is` survives), Audit (all errors→Internal or NotFound, never the fact). No empty catches (Go). |
| Resources | PASS | All HTTP responses `defer resp.Body.Close()` (opensearch.go:346, apply.go:110/137/164, acledges.go via doJSON). Embed call bounded by WithTimeout+cancel (opensearch.go:368-369). No leaked handles/goroutines in request path. |
| Boundaries | PASS | `jsonStrings` converts nil→[] so terms query never gets JSON null (filter.go:88-90,119-126); `recordFromHit` missing/typed fields → "" → fail-closed deny; empty query short-circuits; K default applied. Reachability query size cap 1000 truncates *closed* (under-authorizes) — see Notes. |
| Security | PASS | See identity-spoofing / fail-open / unfiltered-path / re-filter analysis below — all four named attack vectors traced and closed. |

### Named attack-vector traces (the core FAIL hunt)
- **Unfiltered retrieval path** — none. Built-in tiers receive the compiled `aclClause` inside both BM25 and kNN sub-queries (opensearch.go:382-410,415-429); tier-source and post-hook hits are re-verified by `filterAuthorized(merged, enf)` at opensearch.go:246-248, which runs over the FULL merged list before return. A deny-all Enforcer (ok=false) makes both Clause→match_none and Authorize→false.
- **Fail-OPEN branch** — none. Exhaustively traced Enforce, Search, ScopeGuard.Check, authorizeWrite, Audit: every error/empty/invalid path yields deny or zero. `authorizeWrite` returns nil only when NO verified identity is in ctx (worker/in-process), which no client-reachable path produces — the gRPC interceptor authenticates every method (main.go:154-156 registers no exempt methods).
- **Identity spoofing (client tenant/scope overriding token)** — closed. Ingest overwrites `tenantID, ownerAgentID` with `id.TenantID, id.AgentID` when the barricade injected an identity (server.go:87-90); it always does for client calls. Client `team_id`/`scope` are still gated by the write guard (membership/org-grant), so they cannot widen. Search pins `f.TenantID=id.TenantID, f.Identity=id` (server.go:131-134). Client-supplied `f.UserID` (owner_agent_id term) is ANDed under the ACL clause — it can only narrow, never widen (traced the bool.filter AND-of-ORs structure). The identity ctx key is private with a single writer (auth.go:52-59), unforgeable by other packages.
- **Post-hook/tier seam leak** — closed. `filterAuthorized` runs after post-hooks and over tier-source hits (opensearch.go:235-248); test at retrieval/acl_test.go:85-132 proves a hook-added org hit for an unreachable agent (`expanded-leak`) and foreign tier hits are dropped while the seams still receive the caller Identity.

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| ca-architecture-boundaries | Dependencies point inward; policy defines seams, infra implements | PASS | `internal/acl` imports only `internal/auth` (the entity). `EdgeSource`/`WriteGuard` are defined in acl and implemented by `internal/store.ACLEdgeStore`/`acl.ScopeGuard`; `ACLFilter`/`TierSource`/`PostHook` defined in retrieval, implemented by acl — arrows point toward policy (DIP). No gRPC/OpenSearch/proto imports in acl. |
| ca-architecture-boundaries | SRP by actor | PASS | Read-filter (Filter/Enforcer), write-guard (ScopeGuard), edge admin (ACLEdgeStore), transport barricade (authgrpc) are separate types with distinct change-drivers. |
| aposd-designing-deep-modules | Deep module, single source of truth, no shallow/leaky interface | PASS | Filter/Enforcer: small interface (Enforce/Compile/CanRead) hides clause construction; `Clause` and `Authorize` are derived from the SAME `Reach` so query-time and post-hoc enforcement cannot diverge (filter.go:28-33 documents this). No Silent-Failure red flag — every deny is logged (WarnContext "acl deny"/"ACL denial"). |
| cc-defensive-programming | External input validated at trust boundary | PASS | Token validated at auth barricade (auth.go hashToken + constant-time compare); acl_edge validated at PutEdge (barricade before write); write scope validated by guard + normalizeScope. |
| cc-defensive-programming | Correctness lean / fail-closed for security-critical path | PASS | Authorization is treated as correctness-critical: deny on any ambiguity (unknown scope, missing fields, edge error, empty reach, zero Enforcer). Defense in depth: scope checked at read (clause) AND write (guard) AND re-verified post-hoc. |
| cc-defensive-programming | No side-effecting assertions / no empty catch | PASS | Go idioms; all errors returned/logged, none swallowed. |

## Notes (non-blocking)
1. **ACL is opt-in via `WithACL`** (opensearch.go:87-93): a MultiRetriever built without it performs zero scope enforcement (by design — eval harness/unit tests). Production wiring always passes a non-nil filter (main.go:99), so the invariant holds today; a future caller exposing a non-ACL retriever past the auth barricade would leak. Consider a constructor variant that makes ACL the default for the served path. Not a defect in the current tree.
2. **Reachability `_search` size cap = 1000** (acledges.go:161): a user with >1000 edges gets truncated reachability, which *under*-authorizes (fails closed) — safe, but worth a note for scale.
3. **Truncation-to-K precedes final `filterAuthorized`** (opensearch.go:229-248): once Phase 5 registers tier sources, unauthorized tier hits could occupy K slots and get dropped, lowering authorized recall (not a leak). Production registers no tier sources yet, so no current impact.
4. **`Filter.UserID` in Search is client-supplied and not pinned to the identity** (server.go:123): harmless today because it only narrows under the ANDed ACL clause, but pinning or dropping it would remove a confusing degree of freedom.

## Issues (if FAIL)
None.

**Verdict: PASS** — All 6 Done-When items verified with executed evidence (unit 201, integration 17 live, e2e 10). All 4 prompt-listed edge cases handled. All 5 seam contracts real and tested. The four named attack vectors (unfiltered path, fail-open, identity spoofing, post-hook/tier seam) were traced through the actual code and are closed. All loaded-skill criteria PASS. Findings above are non-blocking.

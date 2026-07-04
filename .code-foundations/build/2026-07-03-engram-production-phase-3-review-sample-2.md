# Review: Phase 3 - Access, MCP surface, e2e stack (security-sensitive sample 2)

## Executed Results (Step 0)
- Build: `go build ./...` → success (no errors).
- Unit/test suite: `go test ./...` → **174 passed, 0 failed, 0 skipped** across 29 packages.
- Integration (live pinned OpenSearch 3.1.0 on :9200, container `engram-dev-os`):
  `go test -tags=integration ./internal/store/ ./internal/worker/ ./internal/server/ ./internal/ingest/` → **all ok**.
  Verified by name: `TestDW_3_4_TokenStoreRoundTripHashedOnly` PASS, `TestRuleD_LiveClosedOverlapConverges` PASS.
- E2E (self-boot path against :9200 — the documented fallback; compose stack not booted to avoid port contention):
  `go test -tags=e2e ./e2e/` → **ok 5.62s**. `TestDW_3_1_FullLoopThroughMCP` PASS, `TestDW_3_2_CLICommands` PASS,
  `TestDW_3_3_UnauthAndRevocation` PASS, `TestDW_3_6_ScenarioPackRunsWithoutCoreEdits` (+2 subtests) PASS.
- Lint: `make lint` (go vet + revive exported-comment rule) → exit 0.
- Import-boundary: enforced hermetically by `internal/importlint` go test (runs under `make test` in CI) and expressed in `.golangci.yml` depguard.

## Requirement Fulfillment

### DW-3.1
PREMISE:  `make e2e` boots the full local stack and runs the loop green: MCP ingest → worker extract/reconcile (stub LLM) → MCP search returns the fact.
EVIDENCE: e2e/e2e_test.go:32 (`TestDW_3_1_FullLoopThroughMCP`); Makefile e2e/e2e-up (`compose --wait`); deploy/local/docker-compose.yml:68-74 (healthcheck-gated ordering); cmd/engram-stub-llm/main.go:1 (deterministic fixture LLM).
TRACE:    MCP `memory_ingest` → engramclient gRPC Ingest → Store.Append (episodic) → outbox worker polls, calls stub LLM (fixture `fact:` syntax) → reconcile → semantic index → MCP `memory_search` polls until hit contains `hybrid-rrf`. Ran green in 1.06s.
VERDICT:  PASS

### DW-3.2
PREMISE:  `engram token create/revoke/list`, `engram search`, `engram ingest`, `engram status` all work against the local stack.
EVIDENCE: internal/cli/cli.go (all subcommands); e2e/e2e_test.go:73 (`TestDW_3_2_CLICommands`).
TRACE:    create → list shows handle → ingest → status reports resolved identity + `"healthy": true` → search finds fact → revoke → status now rejected. Ran green in 1.18s.
VERDICT:  PASS

### DW-3.3
PREMISE:  every gRPC/MCP call without a valid token is rejected; expired, revoked, and malformed each have a dirty test; revocation bites ≤5 s.
EVIDENCE: internal/authgrpc/interceptor.go:51-76 (unary barricade over all RPCs); auth/interceptor_test.go:108-182 (no-token, malformed, expired, revoked dirty tests); auth/auth_test.go:169-234 (malformed/unknown/expired/revoked typed-sentinel dirty tests); e2e/e2e_test.go:130-157 (revocation `elapsed > 5s` assertion).
TRACE:    Proto has exactly 3 unary RPCs (Ingest/Search/Status), no streaming; the chained UnaryServerInterceptor authenticates each; main.go:147 wires it with no exempt methods. MCP path reaches the service via the token-bearing engramclient, so an MCP call with a bad/absent token fails at the gRPC barricade. Revocation is query-time with `refresh=true` + realtime GET-by-id, asserted <5s in e2e.
VERDICT:  PASS

### DW-3.4
PREMISE:  tokens stored hashed only; issuance shows the raw token exactly once; verification is constant-time.
EVIDENCE: internal/auth/auth.go:184-208 (Issue returns raw once, stores only SHA-256 hex), :137-162 (Verify hashes then `subtle.ConstantTimeCompare` on full digest); auth_test.go:89-165 (hashed-only, raw-shown-once, constant-time/tamper dirty tests); store/auth_integration_test.go:21 (live `_source` contains no raw token).
TRACE:    32-byte crypto/rand token, base64url + `egm_` prefix; only `sha256Hex(raw)` persisted; GetByHash point-read then constant-time full-digest compare; store integration confirms persisted source never contains the raw string. All green.
VERDICT:  PASS

### DW-3.5
PREMISE:  MCP protocol conformance (initialize / list_tools / call_tool) passes against a reference client; a live agent round-trip is documented as a manual step.
EVIDENCE: internal/mcp/mcp_test.go:96-173 (reference-client conformance over io.Pipe: initialize, tools/list, tools/call); docs/mcp.md:§5 (live Claude Code round-trip as an explicit manual verification item).
TRACE:    In-process refClient drives the exact newline-delimited JSON-RPC framing; initialize returns protocolVersion + tools capability, tools/list returns the 3 tools with inputSchema, tools/call ingest→search returns structured hits. All green.
VERDICT:  PASS

### DW-3.6
PREMISE:  a sample scenario pack is added in-phase with zero harness-core edits.
EVIDENCE: e2e/scenarios_sample.go:17-20 (registers via `init()` through `RegisterScenario`); e2e/registry.go:31 (extension point, package-private registry); e2e/e2e_test.go:162 asserts `sample/mcp-round-trip` present and every registered scenario runs green.
TRACE:    The sample pack file only calls the public RegisterScenario from init; the core (registry.go/harness.go) discovers it with no edit. Both sample scenarios ran green.
VERDICT:  PASS

### DW-3.7
PREMISE:  an import-boundary lint fails CI on any transport/framework import under `internal/**` (excluding transport packages).
EVIDENCE: internal/importlint/importlint.go (path-keyed tree walk; forbids grpc + engrampb outside allowlisted edges); importlint_test.go:14 (green on real tree), :30 (red on a transport import — proves it bites), :63 (allowlisted edge ignored); .golangci.yml depguard (same rule for IDEs).
TRACE:    `Check(repoRoot, DefaultConfig())` returns zero violations today; a fixture package importing grpc yields exactly one violation. Run under `make test` → gates CI without needing golangci-lint installed. All green.
VERDICT:  PASS

### Carry-over: sweep rule (d)
PREMISE:  closed-closed valid-time overlap repair per (tenant, subject, predicate); the Issue-3 write-skew interleaving is regression-tested.
EVIDENCE: internal/worker/repair.go:141-187 (sweepClosedOverlaps + firstAfter; trims earlier closed record to nearest valid-time successor, never touches the live head); internal/store/facts.go:264 (ClosedOverlapChainKeys composite agg), :327 (ChainVersions ordered walk); worker/repair_ruled_test.go:53 (`TestDW_RuleD_ClosedClosedOverlapTrimmed` — direct Issue-3 regression, seeded overlap → 1 trim → disjoint, idempotent); worker/repair_ruled_integration_test.go:22 (`TestRuleD_LiveClosedOverlapConverges` — live cluster).
TRACE:    ana=[tv1,tv2) ⊇ eve=[tvMid,tv2), cid live @tv2 → sweep trims ana.invalid_at to tvMid, re-stamps invalidated_tx_at, live head cid untouched; second sweep no-ops. Unit + integration both PASS.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] Every DW item has a corresponding test that ran in Step 0 (unit + integration + e2e names map to DW ids).
- [x] ≥1 dirty test per code-touching area: auth (malformed/expired/revoked/unknown), authgrpc (no-token/malformed/expired/revoked), mcp (unknown method, missing args), importlint (red-on-violation), store/auth (revoke→reject on live cluster), worker rule-d (overlap regression), cli (revoke→rejected via e2e).
- Coverage matches the stated 100%-of-DW-items level.
- Note: `internal/engramclient` and `internal/cli` have no dedicated unit tests; both are thin transport adapters fully exercised end-to-end by the e2e suite (which drives the real CLI binary and a spawned engram-mcp subprocess). Acceptable, non-blocking.

## Dead Code
None blocking. Minor (non-blocking) notes: e2e/harness.go:275 `var _ = json.Marshal` keep-alive; auth_test.go:64-66 the `records()` helper's doc-comment opens with a stale "rawOf" name. Test/harness-only, no runtime impact.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | Token store writes use `refresh=true`; sweep closes are guarded by OCC (seq_no/primary_term) and lost races are logged + retried next pass (repair.go:362). memStore fakes are mutex-guarded. No shared-mutable hazard found; `toolSchemas()` returns a fresh map each call. |
| Error Handling | PASS | Store HTTP calls check status codes and wrap errors; auth returns typed sentinels; interceptor collapses to opaque Unauthenticated and logs the real reason server-side; no empty catch/swallowed errors on the auth path. |
| Resources | PASS | CLI/e2e gRPC clients `defer Close()`; MCP conn closed by callers; e2e Harness.Shutdown kills started procs. |
| Boundaries | PASS | `hashToken` rejects empty/wrong-prefix/undecodable/wrong-length before any lookup; TTL boundary is half-open and tested at exactly-expiry; ChainVersions caps size 1000; firstAfter skips equal-valid_at records. |
| Security | PASS | 256-bit crypto/rand tokens; hashed-only at rest (unit + live proof); `subtle.ConstantTimeCompare` on the full digest; barricade covers all 3 (only) RPCs with no production exempt set; Search/Ingest override tenant from the verified Identity so a client cannot spoof another tenant; opaque transport error asserted to be exactly "unauthenticated" (no oracle). |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | External input validated at entry (trust boundary) | PASS | Token validated at the barricade (interceptor.go bearerToken + auth.go hashToken); MCP tool args validated (tools.go:108,126); CLI flags validated. |
| cc-defensive-programming | Security-critical path validates again (defense in depth) | PASS | Verify re-hashes and constant-time-compares even though GetByHash keyed on the hash; store integration re-asserts hashed-only at rest. |
| cc-defensive-programming | No empty catch / no swallowed errors | PASS | Sweep and interceptor log-and-continue are explicit and documented; every store error is wrapped and returned. |
| cc-defensive-programming | Assertions for programmer bugs only, errors for runtime input | PASS | Typed sentinels used for all runtime token faults; IdentityFrom absence documented as a wiring bug, not a client fault. |
| aposd-designing-deep-modules | Deep interface / information hiding | PASS | `Verify` hides hashing, lookup, constant-time compare, revocation and TTL behind one method; TokenStore is a 4-method seam; barricade is a single interceptor; MCP Backend is a minimal 3-method seam. No shallow-module/pass-through FAIL. |
| aposd-designing-deep-modules | OIDC/SSO seam without leaking transport | PASS | Verifier/TokenIssuer/TokenStore seams keep transport at the edges; importlint proves no business package imports gRPC/proto. |

## Notes (non-blocking)
1. **Token admin bypasses the barricade by design.** `engram token create/list/revoke` (cli.go:94) talks directly to OpenSearch, so anyone with network reach to the cluster + the CLI can mint tokens for any tenant/user. This is the documented "issuing a token cannot itself require a token" trust assumption (OpenSearch is trusted infra). No requirement mandates otherwise; flagging as a trust-boundary assumption a later phase (ACL/admin auth) should revisit.
2. **Defense-in-depth gap if the barricade is ever unwired.** In `Search`/`Ingest`, when `IdentityFrom(ctx)` returns ok=false the tenant falls back to the client-supplied `req.TenantId`. This is unreachable in the shipped wiring (main.go:147 chains the interceptor on every RPC, and there are no exempt methods), so it is not client-reachable — but rejecting outright when identity is absent would harden against a future mis-wiring. Non-blocking.
3. **`ClosedOverlapChainKeys` composite agg fetches a single page (size=limit, no after_key).** Chains beyond `limit` are deferred to the next sweep — bounds work, preserves eventual convergence. Non-blocking.
4. **Compose stack not booted in this review** (used the self-boot e2e path against the running dev cluster to avoid :9201/:7071 contention). The compose healthcheck/`depends_on: service_healthy` ordering was verified by reading docker-compose.yml; the e2e loop itself ran green via self-boot.

## Issues (if FAIL)
None.

**Verdict: PASS**

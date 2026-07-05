# Review: Phase 3 — Client surfaces, MCP, token-auth barricade (security sample 1)

## Executed Results (Step 0)
- Build: `go build ./...` → Success.
- Test suite: `go test ./...` → 174 passed, 29 packages, 0 failures.
- Typecheck: covered by `go build ./...` (Go) → clean.
- Lint: `make lint` (`go vet ./...` + revive) → clean, exit 0.
- Integration (live OpenSearch 3.1.0 `engram-dev-os` @ :9200):
  - `go test -tags=integration ./internal/store/ ./internal/worker/` → both `ok`.
  - `TestRuleD_LiveClosedOverlapConverges` (worker) → PASS.
  - `TestDW_3_4_TokenStoreRoundTripHashedOnly` (store) → PASS.
  - Full `make integration` did not finish inside the 2-min window (heavy spike tests); ran the phase-relevant packages individually, all green.
- e2e: `make e2e` (compose) could not run — no docker on host; podman present. Per the dispatch instruction ("rely on the self-boot e2e path"), ran the self-boot path:
  `ENGRAM_OPENSEARCH_URL=http://localhost:9200 go test -tags=e2e ./e2e/` → `ok`, all four e2e tests PASS (full loop, CLI, unauth/revocation, scenario pack).

## Requirement Fulfillment

### DW-3.1
PREMISE:  `make e2e` boots the full local stack from a clean checkout and runs the loop green: MCP ingest → worker extract/reconcile (stub LLM) → MCP search returns the fact.
EVIDENCE: e2e/e2e_test.go:32 (TestDW_3_1_FullLoopThroughMCP); Makefile e2e/e2e-up targets; deploy/local/docker-compose.yml.
TRACE:    MintToken → MCP.initialize → memory_ingest → worker (stub LLM extract/reconcile) → poll memory_search → hit containing "hybrid-rrf". Ran green via the self-boot path (1.12s).
VERDICT:  PASS — verified via the documented self-boot fallback; compose topology (Dockerfile + healthcheck-gated services) is present and wired into ci.yml, but the container path was not executable here (no docker).

### DW-3.2
PREMISE:  `engram token create/revoke/list`, `engram search`, `engram ingest`, `engram status` all work against the local stack.
EVIDENCE: internal/cli/cli.go (all subcommands); e2e/e2e_test.go:73 (TestDW_3_2_CLICommands).
TRACE:    token create → list shows handle → ingest → status reports tenant + healthy:true → search returns the fact → revoke → subsequent status fails. Ran green (0.14s).
VERDICT:  PASS.

### DW-3.3
PREMISE:  every gRPC/MCP call without a valid token is rejected; expired, revoked, and malformed each have a dirty test; revocation bites ≤5 s.
EVIDENCE: internal/authgrpc/interceptor.go:59-75; interceptor_test.go (NoToken/Malformed/Expired/RevokedImmediately); auth_test.go:169-239 (Malformed/Unknown/Expired/RevocationImmediate); e2e_test.go:130 asserts elapsed ≤5s.
TRACE:    interceptor extracts bearer → Verify → typed rejection → collapsed to opaque `codes.Unauthenticated "unauthenticated"`; handler never runs. Revocation is query-time (no cache): next Verify reads Revoked=true. All dirty tests ran green; e2e revocation elapsed well under 5s.
VERDICT:  PASS.

### DW-3.4
PREMISE:  tokens stored hashed only; issuance shows the raw token exactly once; verification is constant-time.
EVIDENCE: internal/auth/auth.go:137-162 (Verify uses `subtle.ConstantTimeCompare` on the full digest), 184-209 (Issue returns raw once, stores `sha256Hex`), 232-238 (256-bit `crypto/rand`); internal/store/auth.go stores only the hash; store/templates/auth-tokens.json (no raw field, strict mapping); auth_test.go:89/112/140; integration TestDW_3_4_TokenStoreRoundTripHashedOnly.
TRACE:    Issue → 32 random bytes → base64url `egm_` token → sha256 hex stored as _id; raw returned once. Verify → hash input → point-GET by hash → constant-time compare → revocation/TTL. Tamper-last-char test rejects (full-digest compare). All ran green.
VERDICT:  PASS.

### DW-3.5
PREMISE:  MCP protocol conformance (initialize / list_tools / call_tool) passes against a reference client; a live agent round-trip is documented as a manual step.
EVIDENCE: internal/mcp/mcp_test.go:39 (in-process reference client), :96/:120/:146 (TestDW_3_5_Conformance{Initialize,ListTools,CallTool}); docs/mcp.md §5 "Live Claude Code round-trip (manual verification step)".
TRACE:    reference client drives initialize → protocolVersion returned; tools/list → 3 memory tools; tools/call → structured result. Ran green. docs/mcp.md §5 documents the interactive-only step explicitly.
VERDICT:  PASS.

### DW-3.6
PREMISE:  a sample scenario pack is added in-phase with zero harness-core edits.
EVIDENCE: e2e/registry.go (RegisterScenario extension point), e2e/scenarios_sample.go (init-registered `sample/mcp-round-trip`, `sample/retraction`); e2e_test.go:162 (TestDW_3_6_ScenarioPackRunsWithoutCoreEdits).
TRACE:    sample pack registers via init(); core discovers via Scenarios(); both sub-scenarios ran green (3.15s). Registration is package-private-through-function, so packs never edit the core.
VERDICT:  PASS.

### DW-3.7
PREMISE:  an import-boundary lint fails CI on any transport/framework import under `internal/**` (excluding transport packages).
EVIDENCE: internal/importlint/importlint.go (hermetic tree walk); importlint_test.go (GreenOnCleanTree / RedOnTransportImport / AllowlistedEdgeIgnored); .golangci.yml depguard mirror; .github/workflows/ci.yml:23 (`make test`) runs the hermetic test.
TRACE:    Check walks internal/, flags grpc/engrampb imports outside the allowlisted edges. Red fixture → 1 violation; clean real tree → 0; allowlisted edge → 0. All ran green; GreenOnCleanTree is the CI gate that trips when a later package imports transport.
VERDICT:  PASS.

### Carry-over — sweep rule (d)
PREMISE:  closed-closed valid-time overlap repair per (tenant, subject, predicate); the Issue-3 write-skew interleaving is regression-tested.
EVIDENCE: internal/worker/repair.go:141-187 (sweepClosedOverlaps + firstAfter); worker.go:624-658 (monotone, OCC-guarded, inversion-safe trimInterval); store/facts.go:264/327 (ClosedOverlapChainKeys/ChainVersions keyed on tenant+subject+predicate); repair_ruled_test.go:53 (TestDW_RuleD_ClosedClosedOverlapTrimmed — the direct Issue-3 regression) plus two more; integration TestRuleD_LiveClosedOverlapConverges.
TRACE:    scan ≥2-record non-expired chains → per chain sort by (valid_at, id) → trim each closed record whose invalid_at outruns its nearest strictly-later successor down to that valid_at; live head never touched; second sweep is a no-op. All ran green (unit + live-cluster integration).
VERDICT:  PASS.

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have corresponding tests executed in Step 0.
- [x] ≥1 dirty test per code-touching area: rejection paths (no-token/malformed/unknown/expired/revoked), hashed-only, tamper (constant-time), import-boundary red case, rule-(d) overlap + divergence, retraction scenario.
- [x] Coverage level (100% of DW items) met; both unit and live-cluster/e2e evidence present.
- Note: DW-3.1 compose path and the DW-3.5 live-agent round-trip are the only items not machine-executed here — the former by host limitation (verified via the documented self-boot equivalent), the latter is intentionally a documented manual step.

## Dead Code
None found in the reviewed non-test source. (auth_test.go:65 has a stale doc-comment on `records()` — test code, non-blocking.)

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | trimInterval/closePredecessor use OCC (SeqNo/PrimaryTerm) with bounded retry on ErrConflict; toolSchemas returns a fresh map per call (no shared mutable state); interceptor stores identity in a request-scoped ctx value. |
| Error Handling | PASS | External JSON-RPC args validated (tools.go:108/126); store HTTP status codes checked; Status degraded mode surfaces healthy=false (observable, not silent); no empty catch blocks. |
| Resources | PASS | CLI dials `defer client.Close()`; MCP scanner buffer bounded to 8MB; e2e harness Shutdown kills every proc. |
| Boundaries | PASS | hashToken rejects empty/wrong-prefix/undecodable/wrong-length before any lookup; TTL boundary is half-open (verified at-expiry test); tamper-last-char rejected via full-digest compare. |
| Security | PASS | 256-bit crypto/rand entropy; sha256 hash stored only; constant-time compare; opaque single-error transport rejection (no oracle); tenant boundary fixed from verified Identity in Search/Ingest/Status (client tenancy cannot widen the token); Counts tenant-scoped. |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| aposd-designing-deep-modules | Deep module / small interface hiding much | PASS | auth exposes Verify/Issue/Revoke/List over a narrow TokenStore seam, hiding entropy, hashing, TTL, constant-time compare, prefix scheme. |
| aposd-designing-deep-modules | No information leakage / pass-through | PASS | Barricade is a single choke point; transport reasons collapse to one opaque error; identity ctx key is private (unforgeable). |
| aposd-designing-deep-modules | No silent failure | PASS | Status probe failure yields observable healthy=false with identity still reported, not a swallowed error. |
| cc-defensive-programming | External input validated at entry | PASS | bearerToken (metadata shape) + hashToken (token shape) validate at the barricade before any store read. |
| cc-defensive-programming | Security path re-validates (defense in depth) | PASS | Even after the hash point-GET, the full digest is constant-time-compared; malformed shape rejected pre-lookup. |
| cc-defensive-programming | Assertion vs error handling correct; no empty catch | PASS | IdentityFrom ok-check documented as programmer-error past the barricade; all external errors are typed/returned; no empty catch blocks. |

## Notes (non-blocking)
1. cmd/engram-perf/main.go:85 constructs a gRPC server with NO auth interceptor. It is a perf harness (no token store wired) and out of the client-surface scope, but it does bypass the barricade — worth a comment or a build guard so it is never repurposed as a serving path.
2. The barricade is a UnaryServerInterceptor only. The proto (api/proto/engram.proto) has exactly three unary RPCs, so every RPC is covered today. A future streaming RPC would bypass auth unless a StreamServerInterceptor is added — neither importlint nor depguard would catch that gap.
3. `.golangci.yml`'s depguard rule is not executed by `make lint` (which runs only `go vet` + revive), so the boundary is enforced in CI by the hermetic `internal/importlint` go-test, not by depguard. DW-3.7 is still fully met by that test; the depguard file is effectively editor-only (as its own header comment states).
4. Edge case "stub-LLM keyed to fixtures (deterministic e2e)": verified via observed behavior — the full-loop and scenario e2e tests extracted the expected facts deterministically and passed; the stub was not read line-by-line.

## Issues (if FAIL)
None.

**Verdict: PASS**

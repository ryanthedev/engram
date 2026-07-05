# Review: Phase 3 — Client Surfaces & Auth Barricade (security sample 3)

## Executed Results (Step 0)
- Build: `go build ./...` → Success (exit 0)
- Test suite: `go test ./...` → 174 passed in 29 packages (exit 0)
- Review-area unit tests: `go test ./internal/{auth,authgrpc,mcp,importlint,cli,worker,server}/` → 61 passed in 7 packages
- Integration: `ENGRAM_OPENSEARCH_URL=http://localhost:9200 go test -tags=integration ./internal/store ./internal/server ./internal/worker` → 70 passed in 3 packages (dev OpenSearch 3.1.0 live on :9200)
- E2E (self-boot path, dev OpenSearch, `ENGRAM_E2E_ADDR` unset): `go test -tags=e2e ./e2e/` → ok, 5.05s. `TestDW_3_1` PASS (0.56s), `TestDW_3_2` PASS (1.18s), `TestDW_3_3` PASS (0.09s), `TestDW_3_6` (+2 sub-scenarios) PASS (2.11s)
- Lint: `make lint` (go vet + revive) → exit 0, no findings
- `make e2e` (compose stack) was NOT run: it binds host ports 9201/7071 and rebuilds images; per the dispatch instruction the self-boot e2e path was used instead and passed. The compose topology mirrors the self-boot topology (same binaries, same wiring).

## Requirement Fulfillment

### DW-3.1
PREMISE: `make e2e` boots the full local stack and runs the loop green: MCP ingest → worker extract/reconcile (stub LLM) → MCP search returns the fact.
EVIDENCE: e2e/e2e_test.go:32 (`TestDW_3_1_FullLoopThroughMCP`); e2e/harness.go:45 (Boot); deploy/local/docker-compose.yml (compose parity); Makefile `e2e` target.
TRACE: MintToken via CLI → spawn engram-mcp subprocess with token → initialize/list_tools assert 3 tools → memory_ingest "fact: engram | retrieval | hybrid-rrf" → async worker (stub LLM) extracts+reconciles → poll memory_search until fields_json contains "hybrid-rrf" → hit returned within 20s.
VERDICT: PASS (executed, self-boot path, 0.56s).

### DW-3.2
PREMISE: `engram token create/revoke/list`, `engram search`, `engram ingest`, `engram status` all work against the local stack.
EVIDENCE: internal/cli/cli.go (Run dispatch: token create/list/revoke, ingest, search, status); e2e/e2e_test.go:73 (`TestDW_3_2_CLICommands`).
TRACE: token create → handle appears in `token list` → ingest with ENGRAM_TOKEN → status reports resolved tenant + "healthy": true → search finds the ingested fact → token revoke → subsequent status fails.
VERDICT: PASS (executed, 1.18s).

### DW-3.3
PREMISE: every gRPC/MCP call without a valid token is rejected; expired, revoked, and malformed each have a dirty test; revocation bites ≤5 s.
EVIDENCE: internal/authgrpc/interceptor.go:59-76 (barricade over every unary call; proto has only 3 unary RPCs, no streaming — api/proto/engram.proto:29-44); cmd/engram-server/main.go:146-151 (interceptor wired, no exempt methods); dirty tests: internal/authgrpc/interceptor_test.go (NoToken/Malformed/Expired/RevokedImmediately, all → opaque Unauthenticated), internal/auth/auth_test.go (Malformed/Unknown/Expired/RevocationImmediate, distinct sentinels); e2e/e2e_test.go:130 asserts revocation `<=5s`.
TRACE (revocation ≤5s): Revoke flips `revoked=true` durably with `refresh=true` (store/auth.go:101); Verify is a point GET by hash with no cache (auth.go:142-157); next call sees revoked → ErrTokenRevoked → interceptor returns Unauthenticated. e2e measured elapsed < 5s; unit asserts on the very next Verify.
VERDICT: PASS (executed). No streaming RPC and no exempt method on the production server, so the unary barricade covers 100% of the service surface.

### DW-3.4
PREMISE: tokens stored hashed only; issuance shows the raw token exactly once; verification is constant-time.
EVIDENCE: internal/auth/auth.go — Issue stores `sha256Hex(raw)` only (line 192-208), returns raw once; Verify uses `subtle.ConstantTimeCompare` on the full digest (line 152); 256-bit entropy from crypto/rand (rawTokenBytes=32, line 73/232-238). Template internal/store/templates/auth-tokens.json has no raw-token field (dynamic:strict). Tests: auth_test.go TestDW_3_4_StoredHashedOnly / RawShownOnce / ConstantTimeVerify; integration TestDW_3_4_TokenStoreRoundTripHashedOnly (ran, passed).
TRACE (constant-time): tampered token (last char flipped) → different SHA-256 → GetByHash miss → ErrTokenUnknown; the digest compare does not short-circuit on shared prefix. Secret-material comparison is constant-time; the by-hash store lookup leaks only hash timing (SHA-256 preimage-hard), which is standard and safe.
VERDICT: PASS (executed).

### DW-3.5
PREMISE: MCP protocol conformance (initialize / list_tools / call_tool) passes against a reference client; a live agent round-trip is documented as a manual step.
EVIDENCE: internal/mcp/mcp_test.go (in-process reference client over io.Pipe: TestDW_3_5_ConformanceInitialize / ListTools / CallTool, plus unknown-method -32601 and tool-error cases) — ran, passed. docs/mcp.md section 5 documents the live Claude Code round-trip as an explicit manual verification item.
TRACE: refClient writes newline-delimited JSON-RPC → server initialize returns protocolVersion + tools capability + serverInfo; tools/list returns exactly memory_ingest/search/status with inputSchema; tools/call ingest→search round-trips through the fake Backend.
VERDICT: PASS (executed + documented).

### DW-3.6
PREMISE: a sample scenario pack is added in-phase with zero harness-core edits.
EVIDENCE: e2e/scenarios_sample.go registers `sample/mcp-round-trip` and `sample/retraction` from `init()`; e2e/registry.go holds the Scenario registry; e2e/e2e_test.go:162 (`TestDW_3_6...`) discovers and runs all registered scenarios and asserts the sample pack is present. Harness core (harness.go/registry.go) is untouched by the pack — registration is via init side-effect.
TRACE: init() → RegisterScenario × 2 → Scenarios() returns ≥2 including "sample/mcp-round-trip" → each Run(harness) executes green.
VERDICT: PASS (executed, both sub-scenarios green).

### DW-3.7
PREMISE: an import-boundary lint fails CI on any transport/framework import under `internal/**` (excluding transport packages).
EVIDENCE: internal/importlint/importlint.go (hermetic tree-walk; forbids google.golang.org/grpc and api/engrampb; allowlists server/authgrpc/engramclient/mcp); dirty tests importlint_test.go: TestDW_3_7_GreenOnCleanTree (real tree clean), RedOnTransportImport (fixture importing grpc → 1 violation), AllowlistedEdgeIgnored — ran, passed. Runs under `make test` in .github/workflows/ci.yml:23, so it gates CI without requiring golangci-lint. .golangci.yml depguard expresses the same boundary for IDE use.
TRACE: a later-phase business package importing grpc → Check returns a Violation → the go test fails → CI red.
VERDICT: PASS (executed). Note: `make lint` itself runs only go vet + revive, not depguard; the CI-gating enforcement of this rule is the importlint go test under `make test`, which is present and passing. Requirement is satisfied by that mechanism.

### Carry-over: sweep rule (d)
PREMISE: closed-closed valid-time overlap repair per (tenant, subject, predicate); the Issue-3 write-skew interleaving is regression-tested.
EVIDENCE: internal/worker/repair.go:141-187 (sweepClosedOverlaps + firstAfter); internal/store/facts.go:249-352 (ChainKey, ClosedOverlapChainKeys, ChainVersions); internal/worker/worker.go:624 (trimInterval — monotone, OCC-guarded, inversion-guarded, no-ops on disjoint, never touches live head). Regression tests: repair_ruled_test.go TestDW_RuleD_ClosedClosedOverlapTrimmed (the direct Issue-3 write-skew regression), DivergenceWindowChainConverges, NoopWhenDisjoint; integration repair_ruled_integration_test.go TestRuleD_LiveClosedOverlapConverges — all ran, passed (unit in `go test ./...`, integration in the 70-pass run).
TRACE: two per-document-OCC closes race into overlapping closed intervals for one (tenant,subject,predicate) → sweep loads ChainVersions ascending → for each closed rec whose invalid_at extends past the nearest strictly-later valid_at, trimInterval narrows it to that boundary → OverlapsTrimmed=1; second sweep is a no-op (converged); live head untouched.
VERDICT: PASS (executed, unit + integration).

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have corresponding tests that ran in Step 0 (unit + integration + e2e).
- [x] ≥1 dirty test per code-touching area: auth (malformed/unknown/expired/revoked/tampered), interceptor (no-token/malformed/expired/revoked), mcp (unknown-method/tool-error), importlint (red-on-import), worker rule-d (overlap trim + inversion/disjoint), cli (revoked-token-rejected via e2e).
- [x] Coverage matches the stated level (100% of DW items, dirty tests present).

## Dead Code
None found. No debug prints, TODO/FIXME, panics, or unreachable code in the reviewed files. `grep` for `fmt.Print`/`TODO`/`panic` across the review set returned nothing (cmd/ mains legitimately use fmt for user output).

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | Token verification is a stateless point-read; revocation is durable + query-time (no shared cache to invalidate). auth memStore/AuthTokenStore have no shared mutable hazard. rule-(d) trim uses OCC (SeqNo/PrimaryTerm) with conflict retry (worker.go:624-658); concurrent trims converge monotonically. toolSchemas() returns a fresh map per call (tools.go:26). No demonstrable race. |
| Error Handling | PASS | Barricade validates the token at the process edge and maps every typed rejection to one opaque `Unauthenticated` (no oracle); store errors wrapped and surfaced; MCP tool failures returned as isError, protocol misuse as JSON-RPC errors. No swallowed error paths. |
| Resources | PASS | gRPC client conns closed (cli defer client.Close, mcp defer client.Close); MCP scanner bounded to 8MB (mcp.go:108); OpenSearch calls bounded by store.DefaultTimeout. Sweep bounded by limit (default 500) and maxChainDepth cycle guard. |
| Boundaries | PASS | hashToken enforces prefix + base64 + exact 32-byte length (rejects `egm_short`, empty, wrong-prefix); TTL is half-open (at-expiry rejected, tested); firstAfter skips equal-valid_at records so intervals can't bound each other; trimInterval refuses inverted trims. |
| Security | PASS | 256-bit crypto/rand entropy; hashed-only storage (strict mapping, no raw field); constant-time digest compare; opaque rejection (no detail leak); identity derived from the verified token, not client fields (server.go:66-68, 104-106) so a token cannot widen its tenancy; token greppable prefix for leak scanning. See Note 1 re: engram-perf. |

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| aposd-designing-deep-modules | Deep interfaces (small surface hiding substantial logic) | PASS | `Authenticator.Verify` (1 method hides hash/lookup/constant-time/revocation/TTL); `UnaryServerInterceptor` (1 barricade hides extraction+verify+inject); `mcp.Backend` (3-method seam hiding gRPC wiring). |
| aposd-designing-deep-modules | No information leakage / false abstraction | PASS | Identity is computed inside the barricade and never exposed as a forgeable client field; typed rejection reasons stay server-side, opaque error out. The private `identityCtxKey` prevents forgery. |
| aposd-designing-deep-modules | No shallow modules / classitis / pass-through | PASS | Seams are consumer-defined and minimal; engramclient adapts proto→Backend once; no thin wrapper layers. |
| cc-defensive-programming | External input validated at entry | PASS | Token validated at the gRPC barricade; MCP tool args (event_id/text/query non-empty) and server event_id validated at each entry. |
| cc-defensive-programming | Security-critical path never exempt; defense-in-depth | PASS | Auth re-verifies via constant-time compare even though the store lookup is already hash-keyed; issuance rejects incomplete identities; production server wires the interceptor with zero exempt methods. |
| cc-defensive-programming | No empty catch / no swallowed failures | PASS | Errors propagated with context throughout; no empty error branches. Minor best-effort `_` on `json.Marshal` of maps that cannot fail (mcp tools.go:147, cli.go:263) — not a swallowed real failure. |
| cc-defensive-programming | Barricade at trust boundary, assertions inside | WARNING (non-blocking) | The production barricade is sound. However cmd/engram-perf (DW-1.5 perf harness, out of Phase-3 scope) mounts the full Engram service with `grpc.NewServer()` and no interceptor — see Note 1. The skill exempts performance/prototype rigs, and this is a self-driven benchmark, so it does not fail the Phase-3 barricade requirement, but it is a latent unauthenticated surface. |

## Notes (non-blocking)
1. **engram-perf mounts the Engram service without the auth interceptor** (cmd/engram-perf/main.go:85-88). It is a self-contained benchmark rig: it seeds a scratch index and drives its own in-process client to measure retrieval latency (DW-1.5, "perf environment, not CI"). It is not in the Phase-3 review scope, is unchanged by Phase 3, binds to a `-addr` flag, and DW-3.3's barricade is fully satisfied on the production server (cmd/engram-server) and the MCP path. This is therefore NOT a Phase-3 FAIL. It is still a genuine defense-in-depth gap worth closing: bind loopback-only, or add a short comment documenting the intentional omission, so it is never run on a reachable address. Flagging per the security-sensitive directive to hunt for interceptor-less servers.
2. **server.go Ingest/Search tenancy fallback** (server.go:65-68, 104-106): when no Identity is in context, handlers fall back to client-supplied tenancy. In production this is unreachable — the barricade rejects every unauthenticated call and no method is exempt — so it only affects in-process callers/tests. Documented as such; acceptable, but the fallback is a latent cross-tenant path if a future method were ever added to the exempt set. Consider asserting identity presence on the write/read paths as defense-in-depth.
3. `make lint` runs only go vet + revive; the DW-3.7 boundary is CI-gated by the `importlint` go test under `make test`, not by `make lint`. This is intentional and documented, and the test is present and passing — noting for clarity since the ".golangci.yml depguard" is not actually invoked by any make target.

## Edge Cases (prompt-listed)
- expired / revoked / malformed each rejected distinctly → PASS (distinct sentinels ErrTokenExpired/Revoked/Malformed/Unknown; distinct dirty tests in both auth and authgrpc).
- revocation effective ≤5 s → PASS (query-time, no cache, refresh=true; e2e asserts <=5s, unit asserts next-call).
- compose cold-start ordering gated by healthchecks → PASS (docker-compose.yml depends_on: service_healthy for opensearch/embed/stub-llm; `make e2e-up` uses `--wait`).
- stub-LLM outputs keyed to fixtures (deterministic e2e) → PASS (engram-stub-llm parses `fact:`/`retract:` directive lines deterministically; embed server uses deterministic FakeEmbedder).

## Issues (if FAIL)
None.

**Verdict: PASS** — All 8 requirements verified with execution evidence (unit + integration + self-boot e2e). Auth barricade is sound: 256-bit entropy, hashed-only storage, constant-time verification, opaque typed rejections, query-time revocation ≤5s, identity-derived tenancy, and a unary barricade that covers 100% of the service surface (no streaming RPCs, no exempt methods on the production server). One non-blocking security note: the out-of-scope engram-perf benchmark binary runs an interceptor-less Engram server (perf-only, self-driven) — recommend a loopback bind or documenting comment.

# Discovery + Design: Phase 3 - Surfaces, Auth & E2E Foundation

## Files Found (relevant existing state)
- `api/proto/engram.proto` + `api/engrampb/*` — gRPC contract: `Ingest`, `Search`. No `Status`, no auth metadata convention yet.
- `internal/server/server.go` — gRPC `Server` over `Store`+`Retriever`. No auth; `Ingest`/`Search` read tenancy straight from the request.
- `internal/worker/worker.go` — outbox worker; `ProcessEvent` runs extract→reconcile→bi-temporal write. **No stage seam (D20) yet.**
- `internal/worker/repair.go` — `Sweeper` with rules a/a'/b/c. **No rule (d)** (closed-closed overlap repair) — the carry-over.
- `internal/worker/worker.go:trimInterval` — the monotone guarded trim primitive rule (d) reuses; `ValidTimeNeighbors` on the store; fake store in `fake_test.go` with crash hooks.
- `internal/store/*` — `Store` seam + `OpenSearchStore`; `Apply` idempotently PUTs index templates; `doJSON` HTTP primitive. Templates embedded from `templates/*.json`.
- `internal/embed/{fake,http}.go` — `Embedder` seam; `FakeEmbedder` (deterministic, fixture-keyed) is exactly the behavior the compose "BGE-M3 server" must replicate over HTTP (TEI `/embed` contract in `http.go`).
- `internal/ingest/http.go` — OpenAI-compatible extraction client (the stub-LLM HTTP contract: `POST /chat/completions`, `choices[].message.content` = JSON fact array).
- `cmd/engram-server/main.go` — `engramd`; flags for OpenSearch/embed/extract URLs. `-embed-url`/`-extract-url` select real-HTTP vs deterministic fakes.
- `.github/workflows/ci.yml`, `Makefile`, `revive.toml`, `buf.yaml` — CI wiring; lint is `go vet` + `revive`. **No depguard.**
- `scripts/dev-cluster.sh` — podman/docker dev OpenSearch 3.1 (container `engram-dev-os`, port 9200).

## Current State
Skeleton (Phases 0–2) complete: sync gRPC Ingest/Search, async outbox worker + ledger + reconciler + repair sweep a/a'/b/c, all green. No client surfaces, no auth, no e2e stack.

## Environment (verified this session)
- Go 1.26.3; OpenSearch **3.1.0 live at :9200** (container `engram-dev-os`); `podman` 5.8.2 with a working `podman compose` (docker-compose provider); `GOPROXY` set (module downloads work).
- Consequence: unit + integration + a locally-booted e2e loop are all runnable here. The compose e2e uses a **separate** OpenSearch (`engram-e2e-os`, port **9201**, container name distinct) so it never collides with `engram-dev-os`.

## Gaps (plan vs reality)
| # | Gap | Resolution |
|---|-----|-----------|
| 1 | No `internal/auth` | New package: `Identity`, `Authenticator`, `TokenIssuer`, `TokenStore`, hashing/const-time verify. |
| 2 | No gRPC auth interceptor | New `internal/authgrpc` (transport-adjacent) unary interceptor barricade; injects `Identity` into ctx. |
| 3 | No worker stage seam (D20) | Add `RegisterStage`/`Stage` to `internal/worker`; invoke post-reconcile in `ProcessEvent`. |
| 4 | Sweep rule (d) missing | Add rule (d) to `Sweeper` + Issue-3 write-skew regression tests. |
| 5 | No `Status` RPC | Add `Status` to proto (health + resolved identity + tier counts) for `memory_status`/`engram status`. |
| 6 | No MCP server | `cmd/engram-mcp`: hand-rolled stdio JSON-RPC 2.0 (SDK decision below). |
| 7 | No CLI | `cmd/engram`: token create/revoke/list, search, ingest, status. |
| 8 | No compose stack / embed+LLM servers | `deploy/local/*`, `cmd/engram-embed-server`, `cmd/engram-stub-llm`, `make e2e`. |
| 9 | No e2e harness / scenario pack | `e2e/` harness with a zero-core-edit scenario registry + sample pack. |
| 10 | No import-boundary lint | `.golangci.yml` depguard rule + a red/green test. |
| 11 | No `auth_tokens` index | New template + `Apply` wiring. |

## Code Standards
`docs/code-standards.md` present and applied: return-error (never panic in libs), `%w` wrap, sentinel errors; `context.Context` first param; deep modules / small interfaces defined at the consumer; OpenSearch types stay out of public signatures; table-driven tests, integration behind `integration` tag, ≥1 dirty test/phase; `log/slog`. **Addition this phase:** the dependency-direction rule ("no transport imports in business packages") becomes a *mechanical* depguard lint (DW-3.7), not just a convention.

## Test Infrastructure
- Unit: table-driven `*_test.go`; worker uses an in-memory `fakeStore` with crash-injection hooks.
- Integration: `//go:build integration`, `make integration` against live 3.1.
- New e2e: `//go:build e2e`, `make e2e`, driven against a compose stack (or a locally-booted process set via `ENGRAM_E2E_*` env for dev/CI-light).

## Design Decisions

### D-P3-a: MCP transport — hand-rolled stdio JSON-RPC 2.0 (design-it-twice)
**Approaches:** (A) official `github.com/modelcontextprotocol/go-sdk`; (B) hand-rolled stdio JSON-RPC 2.0 over the small MCP subset; (C) generic third-party JSON-RPC lib + manual MCP framing.

| Criterion | A official SDK | B hand-rolled | C generic RPC lib |
|-----------|---------------|---------------|-------------------|
| Interface simplicity | High | High | Med |
| Info hiding (protocol behind a `Server`) | High | High | Med |
| Caller ease | High | High | Med |
| Build hermeticity / dep weight | New heavy dep, churn risk | **Zero new deps** | New dep |
| Conformance-testability | External | **Trivial in-process** | Med |

**Choice: B.** The needed subset — `initialize`, `notifications/initialized`, `tools/list`, `tools/call` (+ JSON-RPC error envelope) — is small and stable; hand-rolling keeps `go.mod` at grpc+protobuf, makes conformance a fast in-process reference-client test, and is exactly the plan's stated fallback (Assumption row 2). Sacrifice: we track future MCP revisions manually — acceptable, the tool schemas are the stable contract either way. A `Transport` seam (io.Reader/Writer framing) keeps the SDK swap-in behind one interface.

### D-P3-b: token auth as a deep module (design-it-twice)
**Approaches:** (A) JWT/HMAC self-describing tokens (stateless verify); (B) opaque random 256-bit token, hashed at rest, looked up per call (stateful); (C) opaque token + in-memory verify cache.

| Criterion | A JWT | B opaque+lookup | C opaque+cache |
|-----------|-------|-----------------|----------------|
| Interface simplicity (`Verify`/`Issue`) | High | **High** | High |
| Revocation ≤5 s | Hard (needs denylist ⇒ stateful anyway) | **Instant (query-time)** | Needs TTL≤5 s invalidation |
| Info hiding | Med (claims leak into token) | **High** | High |
| Constant-time / hashed-at-rest | Manual | **Natural (sha256 id + subtle compare)** | Same as B |
| Complexity | Med | **Low** | Med (cache invalidation) |

**Choice: B.** Plan mandates "stored hashed, TTL'd, revocable, revocation ≤5 s". Opaque-token + per-call lookup makes revocation *instant* (no cache to invalidate — the plan's ≤5 s is met trivially and documented), hashing-at-rest natural (doc `_id = sha256(raw)`), and keeps the interface two methods deep over the store. Sacrifice: one OpenSearch GET per authenticated call — acceptable at S1; the seam allows a cache later without interface change. `TokenIssuer` is the OIDC seam (D17).

**Auth module shape (deep, 2 public methods + issuer):**
- `Identity{TenantID, UserID, AgentID string}` — value type; `Valid()` helper.
- `Authenticator.Verify(ctx, raw string) (Identity, error)` — hash → GET by id → const-time compare → TTL check → revoked check → return Identity or a typed sentinel.
- `TokenIssuer.Issue(ctx, id Identity, ttl) (raw string, err error)` — 256-bit crypto/rand → store `sha256(raw)` → return raw **once**.
- `Revoke(ctx, raw)` / `List(ctx, id)` back the CLI.
- Typed sentinels (`ErrTokenMalformed`, `ErrTokenUnknown`, `ErrTokenExpired`, `ErrTokenRevoked`) let dirty tests distinguish rejection reasons; the **gRPC surface collapses all to `codes.Unauthenticated` with a generic message** (no detail leak — defensive-programming barricade).

### D-P3-c: the interceptor is the barricade (cc-defensive-programming)
External input = every gRPC call. The unary interceptor validates the bearer token at the process edge and injects `Identity` into ctx; internal handlers assume `IdentityFrom(ctx)` is present (assertion, not re-validation) — except security-critical writes which Phase 4's `WriteGuard` re-checks (defense in depth). Constant-time hash compare via `crypto/subtle`; malformed/expired/revoked/unknown all → `Unauthenticated`, generic text, real reason logged server-side only.

### D-P3-d: worker stage seam (D20)
`Stage.Process(ctx, ev memory.Episodic, facts []memory.SemanticFact) error`; `Worker.RegisterStage(name, s)`. Invoked in `ProcessEvent` **after** the per-fact reconcile loop and **before** ledger-complete, so stage execution is at-least-once and resumes with the cached extraction (a stage error fails the event → outbox retry; stages must be idempotent — documented). Phase 3 registers **no** stages, so every existing worker test path is unchanged (anchoring preserved); a test registers a fake stage to prove the seam.

### D-P3-e: rule (d) — closed-closed valid-time overlap repair (carry-over)
Per (tenant, subject, predicate), scan closed facts (invalid_at set, expired_at unset); where an earlier record's [valid_at, invalid_at) overlaps a later record's valid_at, `trimInterval(earlier, later.valid_at)` (monotone, guarded — the existing primitive). Funnels through `trimInterval`, so it is safe under concurrency and idempotent. New store read `ClosedOverlaps`/reuse: implement a `ClosedChainsWithOverlap` scan (fake + OpenSearch). Regression encodes the Issue-3 write-skew end state (`ana=[tv1,tv2) ⊇ eve=[tv1+6h,tv2)`) and asserts the sweep converges it to disjoint (`ana=[tv1,tv1+6h)`).

### D-P3-f: e2e scenario extension point (DW-3.6)
A `Scenario{Name, Run(ctx, *Harness) error}` registered via `RegisterScenario` from an `init()` in its own file. Core harness iterates the registry; the sample pack (`e2e/scenarios_sample.go`) is added with **zero** edits to harness core — proving the Phases 4–8 extension point.

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-3.1 | `make e2e` boots stack, full loop green (MCP ingest → worker → MCP search) | COVERED | `e2e/loop_test.go:TestDW_3_1_FullLoopThroughMCP` (tag `e2e`), driven against the compose stack / locally-booted processes; the loop scenario in the sample pack. |
| DW-3.2 | CLI token create/revoke/list, search, ingest, status all work | COVERED | `e2e/cli_test.go:TestDW_3_2_CLICommands*`; unit `cmd/engram` command tests against an in-process gRPC server. |
| DW-3.3 | Every call w/o valid token rejected; expired/revoked/malformed dirty tests; revocation ≤5 s | COVERED | `internal/authgrpc/interceptor_test.go:TestDW_3_3_{NoToken,Malformed,Expired,Revoked}Rejected`; `internal/auth/token_test.go:TestDW_3_3_RevocationImmediate`; e2e `TestDW_3_3_RevokedTokenRejectedWithin5s`. |
| DW-3.4 | Tokens hashed-only at rest; raw shown once; verification constant-time | COVERED | `internal/auth/token_test.go:TestDW_3_4_{StoredHashedOnly,RawShownOnce,ConstantTimeVerify}`; integration `TestDW_3_4_TokenDocCarriesNoRawSecret`. |
| DW-3.5 | MCP conformance (initialize/list_tools/call_tool) vs reference client; live Claude Code round-trip | COVERED (clause 2 = documented manual step) | `internal/mcp/server_test.go:TestDW_3_5_Conformance{Initialize,ListTools,CallTool}` + `cmd/engram-mcp` stdio round-trip test; `docs/mcp.md` with `claude mcp add` command + round-trip script. Live-session demonstration flagged as manual (see Deviations). |
| DW-3.6 | Sample scenario pack added with zero harness-core edits | COVERED | `e2e/registry_test.go:TestDW_3_6_SamplePackRegistersWithoutCoreEdits` (asserts the sample pack scenarios are in the registry and core files untouched via a compile-time registry). |
| DW-3.7 | Import-boundary depguard lint fails CI on transport imports under `internal/**` | COVERED | `internal/importlint/depguard_test.go:TestDW_3_7_{RedOnTransportImport,GreenOnClean}` runs golangci-lint depguard on a fixture; `.golangci.yml` structural rule. |
| Carry-over | Sweep rule (d) + Issue-3 regressions | COVERED | `internal/worker/repair_ruled_test.go:TestDW_RuleD_ClosedClosedOverlapTrimmed`, `TestDW_RuleD_DivergenceWindowLateArrivalConverges`, `TestDW_RuleD_NoopWhenDisjoint`; live `TestRuleD_LiveClosedOverlapConverges`. |

**All items COVERED:** YES (DW-3.5 clause 2 live-session demonstration is implemented + conformance-tested; the actual live Claude Code session is surfaced as a manual verification step per the DW-3.5 instruction — see Deviations & report).

**Count check:** 7 DW-IDs in prompt (3.1–3.7) + carry-over = 8 rows. Matches.

## Prerequisites
- [x] Skeleton Phase 2 merged (Store/worker/reconciler/sweep present)
- [x] Live OpenSearch 3.1 for integration + e2e
- [x] podman + compose for the stack
- [x] Network for `go get` (grpc already vendored in go.sum; no new runtime deps planned — MCP hand-rolled)

## Recommendation
**BUILD.** All DW items are COVERED with a design that reuses existing seams (Store, worker, Embedder/Extractor HTTP contracts). No plan conflict. DW-3.5's live-session clause is explicitly a documented manual step by the plan's own instruction — everything mechanically verifiable is built and tested here.

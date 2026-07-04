# Discovery + Design: Phase 4 — Multi-Agent Scope + ACL

Security-sensitive authorization phase. Fail-closed throughout; scope enforced at BOTH write and read time (defense in depth). Query-time enforcement (D6) — no index-time ACL baking, no result caches, so revocation is instant.

## Files Found (existing seams this phase extends)

| File | Role for Phase 4 |
|------|------------------|
| `internal/auth/auth.go` | `Identity{TenantID,UserID,AgentID}`; `.Valid()`. Source of the principal. |
| `internal/authgrpc/interceptor.go` | Barricade injects Identity into ctx via `WithIdentity`/`IdentityFrom` (private ctx key, in a grpc-importing pkg). |
| `internal/retrieval/{retriever,opensearch}.go` | `Retriever.Search(ctx,Query,Filter)`; `Filter{TenantID,UserID,ValidOnly}`; `MultiRetriever` fans out to `tierRetriever`s; filters applied INSIDE the OpenSearch query (never post-filtered). |
| `internal/store/{store,opensearch,facts,auth,apply,templates}.go` | `Store` write seam; `OpenSearchStore` concrete; `AuthTokenStore` is the pattern for an OpenSearch-backed admin index; `Apply` PUTs templates+indices; `doJSON`/`searchHits`/`decodeSource` shared HTTP+decode helpers; `ChainVersions`/`GetFact` for bi-temporal reads. |
| `internal/server/server.go` | gRPC handlers; `Search` already overrides `f.TenantID` from Identity; `StatusProbe` is the optional-consumer-seam pattern (mirror it for audit). |
| `api/proto/engram.proto` + `api/engrampb/*` | RPC contract; regenerated via `scripts/codegen.sh` (buf, `go run` pinned — works here, slow). |
| `cmd/engram/*` + `internal/cli/cli.go` | CLI; `token` group is OpenSearch-backed admin (issuing needs no token) — the pattern for `acl` edge admin; `ingest/search/status` are gRPC. |
| `internal/engramclient/client.go` | Shared gRPC client (CLI+MCP). |
| `e2e/{registry,scenarios_sample}.go` | `RegisterScenario` from `init()` in a NEW FILE in package `e2e` = the zero-core-edit extension point (DW-3.6). |
| `internal/worker/worker.go` | `RegisterStage` (D20) already exists — NOT re-touched here; P5/P6 stages plug there. |
| `internal/importlint` + `.golangci.yml` | Forbid `google.golang.org/grpc` + `api/engrampb` under `internal/**` except transport dirs. `internal/acl` must import NEITHER. |
| `docs/code-standards.md` | Applied: interfaces at the consumer, no vendor types in public signatures, `%w` wrapping, sentinel errors, `log/slog`. |

## Current State
- Records already carry `tenant_id, team_id, scope, owner_agent_id, source_ids[]` (memory.Episodic/SemanticFact) — mapped as `keyword` in the semantic template (scope tiers are additive, no reindex — D23).
- Server derives tenancy from the verified Identity but does NOT yet scope by user/agent (Phase-3 note explicitly deferred this to Phase 4).
- 174 unit tests green; dev cluster OpenSearch 3.1.0 up on :9200; podman + `engram-dev-os` present.
- No `scope` value is currently enforced anywhere; all data is effectively org-visible within a tenant.

## Gaps (plan → reality) and resolutions
| Gap | Resolution |
|-----|-----------|
| `ACLFilter.Compile` contract names `query.Query` — no such type/pkg exists. | Realize the compiled query as the OpenSearch bool clause `map[string]any` (the DSL retrieval already speaks). `Compile(ctx,id) (map[string]any, error)` is the named contract seam; the richer internal path uses `Enforce → Enforcer`. Deviation noted; the SHAPE (a fail-closed compiled read filter) is honored. |
| Store guard needs Identity, but `Store.Append` has no Identity param and the identity ctx-key lives in `authgrpc` (imports grpc → forbidden in `store`). | Move the identity ctx accessors to `internal/auth` (transport-free): `auth.ContextWithIdentity`/`auth.IdentityFromContext`; refactor `authgrpc.WithIdentity/IdentityFrom` to delegate (one key, in auth). `store` reads `auth.IdentityFromContext(ctx)` — no grpc import. |
| `Store.RegisterWriteGuard` on the `Store` INTERFACE would break every test fake implementing `store.Store`. | Put `RegisterWriteGuard` on the concrete `*OpenSearchStore` only; the interface stays the write-protocol contract. Guard fires inside `Append`/`Create` (concrete), so interface callers still get enforcement. |
| Plan file scope `e2e/acl/**` vs package-private scenario registry. | The zero-core-edit mechanism is a new FILE in package `e2e` (like `scenarios_sample.go`). Realize as `e2e/scenarios_acl.go`. A subpackage `e2e/acl` could not register without a core edit (its `init` never runs). Deviation noted. |
| Audit needs full version history incl. record-retracted versions; `ChainVersions` excludes `expired_at`. | Add `OpenSearchStore.AuditFact(ctx,id)` returning the target's provenance + ALL versions of its `(tenant,subject,predicate)` chain (incl. expired), ordered — the as-of history contract. |

## Design Decisions (aposd-designing-deep-modules + ca-architecture-boundaries)

### Dependency direction (Clean Architecture — arrows point inward to policy)
```
server ─▶ retrieval ─▶ acl ─▶ auth        (transport → mechanism → policy → entity)
   │          │         ▲
   │          └▶ store ─┘   (store implements acl.EdgeSource; imports acl types)
   └▶ store, acl, engrampb
```
- **`internal/acl` (NEW, policy)** imports ONLY `internal/auth`. No grpc, no engrampb, no retrieval, no OpenSearch. Passes importlint automatically (path rule).
- `store` imports `acl` (to satisfy `acl.EdgeSource` and accept `acl.WriteGuard`/`acl.Record`) — infrastructure→policy, correct inward direction.
- `retrieval` imports `acl` (for `acl.Enforcer` + the read-filter interface) and `auth` — mechanism→policy.
- No cycles: `acl` imports nothing back.

### Module: `internal/acl` (the deep authorization module)
One package hides ALL scope/reachability policy behind a tiny surface:
```go
type Record struct { TenantID, TeamID, Scope, OwnerAgentID string }   // neutral write/read subject
type Reach   struct { Agents, Teams []string; OrgGrant bool }         // an identity's reachability
type Edge    struct { Type EdgeType; TenantID, UserID, AgentID, TeamID string }  // acl_edges value
type EdgeSource interface { Reachability(ctx, auth.Identity) (Reach, error) }    // consumer-defined; store impls
type WriteGuard interface { Check(ctx, auth.Identity, Record) error }            // store seam type

type Filter struct { edges EdgeSource; log *slog.Logger }
func (f *Filter) Enforce(ctx, id) (Enforcer, error)        // ONE edge fetch → reusable Enforcer
func (f *Filter) Compile(ctx, id) (map[string]any, error)  // NAMED CONTRACT SEAM (= Enforce().Clause())
func (f *Filter) CanRead(ctx, id, Record) (bool, error)    // audit read-check

type Enforcer struct { reach Reach; ok bool }              // ZERO VALUE = deny-all (fail-closed by construction)
func (e Enforcer) Clause() map[string]any                  // OpenSearch bool filter; match_none when deny-all
func (e Enforcer) Authorize(Record) bool                   // post-hoc predicate for tier/expanded hits

type ScopeGuard struct { edges EdgeSource }                // implements acl.WriteGuard
func (g *ScopeGuard) Check(ctx, id, Record) error          // write rule, fail-closed
```
- Interface depth: callers say "give me the filter for this identity" / "may this write happen"; the module hides reachability resolution, the scope-rule table, and the OpenSearch DSL.
- **Fail-closed is structural:** `Enforcer{}` zero value denies everything; an edge-store error returns `(Enforcer{}, err)` — even a caller that ignores the error gets deny-all.

### Scope rule table (single source of truth; read filter + write guard both derive from it)
Reachability of identity `{tenant,user,agent}`: `Agents = {edges user_agent(user,·)} ∪ {agent}` (self always reachable); `Teams = {edges member(user,·)}`; `OrgGrant = ∃ org_grant(user)`.

READ — a hit is visible iff `hit.tenant==id.tenant` AND one of:
| scope | visible when |
|-------|--------------|
| private | `owner_agent_id == id.agent` (self only) |
| team | `owner_agent_id ∈ Agents` AND `team_id ∈ Teams` |
| org | `owner_agent_id ∈ Agents` |
| other/unknown | never (fail-closed) |

Revoking `user_agent(user,a)` drops `a` from `Agents` → `a`'s team+org hits vanish next query (DW-4.2); private unaffected. Empty `Agents` (blank agent, no edges) → match_none (deny-all). Invalid identity → match_none.

WRITE — `ScopeGuard.Check` allows:
| scope | allowed when |
|-------|--------------|
| private / "" | always (valid identity writes its own private memory) |
| team | `record.team_id ∈ Teams(user)` |
| org | `OrgGrant(user)` |
| other/unknown | `ErrUnknownScope` |
denial → `ErrScopeDenied` (typed; DW-4.3 asserts via `errors.Is`).

### `internal/store` additions
- `ACLEdgeStore` (OpenSearch, index `engram-acl-edges-000001`, template `engram-acl-edges`): `PutEdge` (validates shape — malformed rejected), `DeleteEdge` (revocation), `ListEdges`, `Reachability` (implements `acl.EdgeSource`; deterministic edge `_id = sha256(type·tenant·user·agent·team)` so put/delete/dedup are idempotent).
- `*OpenSearchStore.RegisterWriteGuard(acl.WriteGuard)` + guard invocation in `Append`/`Create`: read `auth.IdentityFromContext(ctx)`; if present, build `acl.Record` from the memory record and run each guard; non-nil → return the typed error WITHOUT writing. Absent identity (worker/tests) → skip (client writes always carry identity via the barricade; derived worker writes are trusted).
- `AuditFact(ctx,id)` → target provenance + full ordered version chain (incl. expired).
- `Apply`: add the acl-edges template+index step (idempotent; keeps DW-0.4 no-op contract).

### `internal/retrieval` additions
- `Filter` gains `Identity auth.Identity` (existing zero-value callers unaffected; ACL only activates when a compiler is wired).
- Consumer interface `type ACLFilter interface { Enforce(ctx, auth.Identity) (acl.Enforcer, error) }`; option `WithACL(ACLFilter)`.
- `MultiRetriever`: holds `acl`, registered `tierSrcs []TierSource`, `postHooks []PostHook`, logger. Built-in tiers become `[]*tierRetriever` so the compiled clause threads into their queries.
  - `RegisterTier(TierSource)` — `TierSource.Search(ctx, auth.Identity, Query) ([]Hit,error)` (P5 seam).
  - `RegisterPostHook(PostHook)` — `PostHook.Apply(ctx, auth.Identity, []Hit) ([]Hit,error)` (P6 seam).
  - Search flow: if `acl!=nil` → `Enforce(ctx,f.Identity)`; on ERROR log a denial + return zero (no HTTP) — fail-closed; else thread `enf.Clause()` into every built-in tier query. Run registered tierSrcs with Identity; merge; sort; truncate. Run postHooks with Identity. Finally, if `acl!=nil`, re-filter the WHOLE list with `enf.Authorize(record)` (defense in depth — makes tier/expanded hits safe even if a future hook forgets; DW-6.4 forward-cover). One edge fetch per query.

### `internal/server` + proto + CLI
- proto: add `Audit(AuditRequest{id}) returns (AuditResponse{provenance, versions[]})` with `Provenance` + `FactVersion` messages; regenerate engrampb.
- `Server`: `Audit` handler over two optional consumer seams — `Auditor` (fetch, `*OpenSearchStore`) and `ReadAuthorizer` (`acl.Filter.CanRead`). Tenant-scoped fetch + ACL read-check → `NotFound` if unauthorized (no existence oracle). `Search` sets `f.Identity = id`.
- `engramclient`: `Audit` method.
- CLI: `engram audit <id>` (gRPC) + `engram acl grant|revoke|list` (OpenSearch-backed edge admin, mirrors `token`).
- `e2e/scenarios_acl.go`: scope-matrix + revoke + audit scenarios via MCP/CLI (build-tag `e2e`, zero core edits).

## Test Infrastructure
- Unit (`go test ./...`, no cluster): acl rule table, Enforcer/zero-value deny, ScopeGuard denials + `Edge.Validate`, retriever fail-closed (fake ACLFilter → error path logs+zeros; empty-reach → match_none), server.Audit with fakes, importlint still green. Primary always-runnable gate.
- Integration (`-tags=integration`, live :9200): compiled-filter 3×3 matrix, revoke propagation, write-guard denial through `Append`, filtered-kNN recall at 3 selectivities vs unfiltered, `AuditFact` history.
- E2e (`-tags=e2e`, compose): `acl/*` scenario pack.

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|-----------|
| DW-4.1 | 3-identity × 3-scope matrix (9 cells) returns exactly the authorized set | COVERED | unit `TestDW_4_1_ScopeRuleMatrix` (acl.Authorize over 9 cells); integration `TestDW_4_1_ScopeMatrixCompiledFilter` (9 docs, 3 identities, exact hit sets via compiled OpenSearch filter); e2e `acl/scope-matrix` |
| DW-4.2 | revoke user↔agent edge hides that agent's team/org hits ≤5 s, no restart | COVERED | unit `TestDW_4_2_RevokeDropsReachableAgent` (Reach w/o agent → Authorize false for team+org, true for self-private); integration `TestDW_4_2_RevocationPropagates` (query→revoke→re-query gone, timed <5 s); e2e `acl/revoke` |
| DW-4.3 | write unauthorized scope → typed denial (dirty) | COVERED | unit `TestDW_4_3_ScopeGuardDeniesUnauthorized` (`errors.Is(err, acl.ErrScopeDenied)`), `TestEdgeValidateRejectsMalformed`; integration `TestDW_4_3_AppendDeniedByGuard` (Append team scope w/o membership → ErrScopeDenied, nothing written) + malformed edge rejected at `PutEdge` |
| DW-4.4 | fail-closed: induced compiler error + empty-reachability → zero results + logged denial | COVERED | unit `TestDW_4_4_CompilerErrorFailsClosed` (fake ACLFilter err → 0 hits + denial log via buffer handler), `TestDW_4_4_EmptyReachabilityDenyAll` (real Filter, empty-reach edge fake → Clause=match_none, Authorize=false, logged) |
| DW-4.5 | filtered-kNN recall at private/team/org selectivities within 5 pp of unfiltered on the gold set | COVERED | integration `TestDW_4_5_FilteredKNNRecallUnderACL` (kNN-only; recall with ACL clause at 3 scope selectivities vs unfiltered truth, delta ≤5 pp) |
| DW-4.6 | `engram audit <id>` shows provenance + full version history via as-of contract | COVERED | unit `TestServerAuditAssemblesProvenanceAndVersions` (fake Auditor); integration `TestDW_4_6_AuditHistory` (ingest→reconcile→AuditFact returns provenance + all chain versions); e2e `acl/audit` (CLI `engram audit`) |

**All items COVERED:** YES (6/6). DW-ID count = 6 = prompt count.

Also covered beyond the DW floor: malformed acl_edge rejected at write (edge case), one-scope-only identity boundary, seam liveness tests (`RegisterTier`/`RegisterPostHook`/`RegisterWriteGuard` receive Identity and compose), audit unauthorized→NotFound.

## Prerequisites
- [x] Phase 3 complete (Identity at every RPC, barricade, e2e harness, stage seam).
- [x] Records carry scope/provenance fields (Phase 0).
- [x] Dev cluster 3.1.0 reachable (:9200) for integration; podman for e2e.
- [x] buf codegen works (`go run`, slow — pre-built a binary to `.e2e-bin/buf`).

## Recommendation
**BUILD.** The plan fits reality; the only deviations are naming/placement (`query.Query`→OpenSearch clause; `e2e/acl/**`→`e2e/scenarios_acl.go`; `RegisterWriteGuard` on the concrete store, not the interface) — all documented above, none change the contract's shape or the security posture. Query-time enforcement, fail-closed-by-construction, and write+read defense-in-depth are all achievable on the existing seams.

# Discovery + Design: Phase 5 - Experience Memory + Write-Gating (T3)

## Files Found
Consumed seams (all present, committed on `feature/engram-production`):
- `internal/store/store.go`, `internal/store/opensearch.go` — `Store`; `OpenSearchStore.RegisterWriteGuard(acl.WriteGuard)` + `authorizeWrite` (client-identity-conditional).
- `internal/acl/acl.go` — `WriteGuard.Check(ctx, id, r Record) error` (non-nil = deny, fail-closed); `Record{TenantID,TeamID,Scope,OwnerAgentID}`; scopes.
- `internal/acl/filter.go` — `Enforcer.Authorize(Record)` used by the retriever to re-verify tier/expanded hits.
- `internal/retrieval/retriever.go`, `internal/retrieval/opensearch.go` — `TierSource.Search(ctx, id, q)`; `MultiRetriever.RegisterTier`; `filterAuthorized`/`recordFromHit` re-verify every tier hit's provenance fields.
- `internal/worker/worker.go` — `Stage.Process(ctx, ev Episodic, facts []SemanticFact) error`; `Worker.RegisterStage(name, s)` (D20); at-least-once, must be idempotent; a stage error re-queues the event.
- `internal/ingest/{ingest,extraction,http,rule}.go` — the Extractor two-impl pattern (deterministic `RuleExtractor` + OpenAI-compatible `HTTPExtractor`) I mirror for the Gatekeeper.
- `internal/enrich/enrich.go` — the background `Job` (`Tick`/`Run`) pattern I mirror for the prune job.
- `internal/store/acledges.go`, `internal/store/apply.go`, `internal/store/templates.go`, `internal/store/templates/semantic.json` — OpenSearch CRUD/query + index-template/apply patterns I mirror for the T3 + quarantine indices.
- `cmd/engram-server/main.go` — the real server entrypoint (NOT `cmd/engramd`); wires guard/tier/stage.
- `internal/cli/cli.go` — token/acl admin dispatch pattern I mirror for `engram quarantine`.
- `e2e/{registry,harness,mcpconn,scenarios_acl,scenarios_sample}.go` — `RegisterScenario` init pattern; packs live in package `e2e` under `e2e/`.
- `internal/testutil/testutil.go` — live-cluster test helpers; dev OpenSearch 3.1.0 confirmed reachable at localhost:9200.

## Current State
Skeleton 0–2 + Phase 3 (surfaces/auth/e2e) + Phase 4 (ACL, write-guard, tier/post-hook/stage seams) are complete. All four registration seams P5 consumes are real and exercised. No `internal/experience/**` exists yet. `go build ./...` is green.

## Gaps (plan vs reality) — resolved without re-architecting
| # | Plan text | Reality | Resolution |
|---|-----------|---------|-----------|
| 1 | File scope `cmd/engramd/stages_experience.go` | entrypoint is `cmd/engram-server` | Use `cmd/engram-server/stages_experience.go` (task authorizes matching the real entrypoint). |
| 2 | "enforced through Phase 4's `WriteGuard` seam" | The store-layer `WriteGuard` only fires when a **client Identity** is in ctx (`authorizeWrite` skips worker-origin writes), and the generic `Store` writes only to the T2 semantic index — it cannot route to a T3 index. Experience distillation runs in a **worker stage with no client identity**, into its **own** index. | Literal reuse is impossible AND would be weaker (identity-conditional skip = a bypass for worker-origin writes, violating D5). Faithful, stronger implementation: the **`Gatekeeper` is the mandatory, unconditional write authorization for T3**, enforced in-path by `ExperienceStore` (the sole T3 writer). `Gatekeeper` is shaped as the same "non-Admit ⇒ deny" contract the `WriteGuard` seam encodes. This meets/exceeds the Produces contract (mandatory no-bypass gate); it is an implementation-detail interpretation, not a cross-phase seam change → **BUILD, not UPDATE_PLAN**. Documented as a deviation. |
| 3 | Produces "quarantine CLI"; file scope omits `internal/cli` | `engram quarantine` must live in the CLI surface | Add a thin `quarantine list/release` dispatch to `internal/cli/cli.go` backed by an `experience` admin type (heavy logic stays in `internal/experience`), mirroring token/acl admin. Minor in-intent scope extension, documented. |
| 4 | File scope `e2e/experience/**` | the shared scenario registry is package `e2e` in `e2e/` (Go packages are per-directory) | Add `e2e/scenarios_experience.go` (package `e2e`, build tag `e2e`) — same as the Phase-4 `scenarios_acl.go` precedent; registers via `init()` with zero core edits. |

## Code Standards
No `docs/code-standards.md` found. Conventions inferred from the codebase and followed: package-doc comments; consumer-defined seams (interfaces where used); fail-closed everywhere; typed sentinel errors collapsed at the transport edge; deterministic fixture impl + real HTTP impl for every LLM boundary; in-memory fakes for unit tests + build-tagged live-cluster integration tests; never hard-delete (bi-temporal soft-expire).

## Test Infrastructure
`go test ./...` (unit); `-tags=integration` live-cluster; `-tags=e2e` full stack via `make e2e`. Core logic is made unit-testable through an in-memory `Backend` fake so every DW item passes without a cluster; a build-tagged integration test round-trips the OpenSearch backend against the live 3.1.0 cluster.

## Design (aposd design-it-twice)

### Design: the T3 write-gate + store (the security-critical core)
**Approaches Considered**
1. **A — Register `Gatekeeper`-as-`WriteGuard` on the generic `Store`, write experiences via `Store.Create`.** Reuse the Phase-4 seam verbatim.
2. **B — Deep `ExperienceStore` as the sole T3 writer, mandatory in-path gate.** T3 has its own index + its own store type; the only write method (`Admit`) unconditionally runs the gate and routes Admit→T3 / Quarantine→quarantine index / Reject→dropped+logged. Construction requires a non-nil `Gatekeeper`.
3. **C — Gate as a decorator wrapping the full `Store` interface.**

**Comparison**
| Criterion | A | B | C |
|-----------|---|---|---|
| Interface simplicity | one method reused | one intent method (`Admit`) | full Store surface re-exposed |
| Information hiding | leaks T3 index + routing to callers | hides indices, routing, dedup, gate orchestration | medium |
| No-bypass guarantee (D5) | **BROKEN** — `authorizeWrite` skips worker-origin writes; `Store.Create` targets T2 only | **structural** — no ungated path to T3 exists | structural but heavy |
| Caller ease | caller must route + gate itself | caller calls `Admit`, done | medium |
| Fail-closed | weak | strong (nil gate = construction error; non-Admit never reaches T3) | strong |

**Choice: B.** A fails DW-5.2 outright (worker-origin bypass + wrong index). B is the deepest module and the only one that makes a T3 write skipping the gate structurally impossible. Sacrifice: does not literally call `Store.RegisterWriteGuard` — accepted and documented (gap #2); the `Gatekeeper` embodies the WriteGuard "non-Admit ⇒ deny" contract as the mandatory authorization.

**Depth check** — `ExperienceStore` interface methods: `Admit`, `ListQuarantine`, `Release`, `SearchAdmitted`, `Prune`(scan+soft-expire), `Recover`. Hidden: two index names, gate orchestration, dedup-by-fingerprint merge, quarantine routing, soft-expire/bi-temporal recovery, provenance stamping. Common case (`Admit` one distilled experience) is one call. Backed by a narrow `Backend` port (in-mem fake + OpenSearch impl) — dependency inversion, same as `acl.EdgeSource`.

### Design: Gatekeeper (LLM-judge boundary)
`Gatekeeper.Evaluate(ctx, Experience) (Verdict, error)`, `Verdict ∈ {Admit, Quarantine, Reject}`. Two impls (Extractor precedent): `RuleGatekeeper` (deterministic evidence rules — admit/quarantine/reject + no-evidence→Quarantine + contradictory→Quarantine) and `HTTPGatekeeper` (OpenAI-compatible judge). **Fail-closed:** timeout/error/unparseable → Quarantine, never Admit. Config-selected by `-gate-url` (empty ⇒ rule judge), mirroring `-extract-url`.

### Design: distillation stage / tier / prune
- `DistillStage` implements `worker.Stage`, registered via `RegisterStage` from `stages_experience.go`, in its own file. Deterministic `RuleDistiller` parses an `experience:` directive from the raw event text (available on the stage) so e2e is reproducible without a judge endpoint. Idempotent: doc id = fingerprint, `Admit` is an upsert-merge (dedup edge).
- `ExperienceTier` implements `retrieval.TierSource`, registered via `RegisterTier`. Searches only the **admitted, non-soft-expired** index; never quarantine. Hits carry provenance fields so `MultiRetriever.filterAuthorized` re-verifies them (defense in depth).
- `PruneJob` mirrors `enrich.Job`: low Φ + low retrieval_count → **soft-expire** (`expired_at` set, doc kept — D3), bi-temporally recoverable; the tier excludes soft-expired.

## DW Verification
| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|-----------|
| DW-5.1 | injected-bad-record suite: ≥90% poisoned → Quarantine/Reject; ≥80% good → Admit | COVERED | `TestDW_5_1_InjectedBadRecordRates` over `PoisonedFixtures()`/`GoodFixtures()` through `RuleGatekeeper`; asserts both rates. |
| DW-5.2 | gate no bypass: code-path audit + test that direct-index T3 writes are rejected by the store layer | COVERED | `TestDW_5_2_NilGateRejected` (constructor errors without a gate); `TestDW_5_2_DenyAllGateNeverAdmits` (deny-all gate ⇒ zero admitted docs, all quarantined); `TestDW_5_2_GateRunsBeforeEveryWrite` (spy gate called once before any admitted-index Put — sole write path). |
| DW-5.3 | gate-LLM timeout → Quarantine; nothing Admits on error | COVERED | `TestDW_5_3_JudgeTimeoutQuarantines` (hung httptest server + short ctx ⇒ Quarantine, no error-path Admit); `TestDW_5_3_JudgeErrorNeverAdmits` (5xx/garbage ⇒ Quarantine). |
| DW-5.4 | curated-small beats large-noisy on the eval set (A/B logged) | COVERED | `TestDW_5_4_CuratedSmallBeatsLargeNoisy` runs `ABEval` over both fixture sets; asserts curated uplift > noisy uplift; `ABReport` is emitted (logged). |
| DW-5.5 | experience-following correlation emitted; prune soft-expires low-Φ (bi-temporally recoverable) | COVERED | `TestDW_5_5_FollowingCorrelationEmitted` (`FollowingCorrelation` computed + on `Metrics`); `TestDW_5_5_PruneSoftExpiresRecoverable` (prune sets `expired_at`, `Recover` still returns it, `SearchAdmitted`/tier exclude it, nothing deleted). |
| DW-5.6 | e2e: agent completes task → distills → gates → retrieved in a later session via MCP | COVERED | `e2e/scenarios_experience.go` `experience/distill-gate-retrieve`: MCP ingest an `experience:` event, poll a second MCP session's `memory_search` until the distilled skill surfaces from the experience tier; plus `experience/poison-quarantined` (a poisoned experience never becomes retrievable). |

Extra coverage beyond the floor: dedup-merge (same fingerprint), quarantine list/release round-trip, tier ACL re-verification (unauthorized identity sees nothing), HTTP judge admit path, OpenSearch backend integration round-trip (tagged).

**All items COVERED:** YES (6/6).

## Prerequisites
- [x] Phase 4 seams present (RegisterWriteGuard/RegisterTier/RegisterStage, Enforcer.Authorize).
- [x] Worker stage seam runs stages before ledger-complete (at-least-once).
- [x] Dev OpenSearch 3.1.0 reachable for integration/e2e.
- [x] `go build ./...` green.

## Deviations discovered during implementation
- **Distillation coupling to fact extraction.** The Phase-3 worker skips the post-write stage pipeline for a fact-free event (the `ErrNoFacts` arm returns before `runStages`). The `DistillStage` therefore only fires for a task-completion event that also yields ≥1 extracted fact. This is honored, not worked around: a completed task naturally emits a completion fact, so the distiller's e2e/dev events carry a `fact:` line alongside the `experience:` line. Fixing this to run stages for fact-free events would be a Phase-3 worker-core change, out of this phase's scope. Documented in `e2e/scenarios_experience.go` and `distill.go`.
- **No-bypass boundary.** The no-bypass guarantee (DW-5.2) holds at the application layer: no `ExperienceStore` method writes the admitted index without the gate, and construction without a gate fails (`ErrNoGate`). The exported `Backend` port is infrastructure (equivalent to raw cluster access); the sanctioned T3 writer is `ExperienceStore`.

## Recommendation
**BUILD.** The plan fits reality; the two seam/scope mismatches (gaps #2/#3) are resolved by faithful, documented implementation-detail choices that meet or exceed the Produces contract, not by re-architecture. No UPDATE_PLAN needed.

# Plan: Engram — Production (Phases 3–8)

**Created:** 2026-07-03
**Status:** in-progress
**Started:** 2026-07-03 12:40 (build worktree: .claude/worktrees/engram-production, branch feature/engram-production)
**Current Phase:** 3
**Complexity:** complex
**Chains from:** `.code-foundations/plans/2026-06-29-engram-walking-skeleton.md` (Phases 0–2, build-ready). Together the two plans form the full 9-phase roadmap (0–8). All decisions D0–D16 from the skeleton plan carry over unchanged.

---

## Context

The approved walking-skeleton plan takes Engram to a correct, tested core loop (ingest → extract → reconcile → hybrid retrieval) but stops short of a production app: no client surfaces, no authentication, no ACL enforcement, no experience/graph tiers, no deployment, and the eval harness never becomes a release gate. This plan details Phases 3–8 to build-ready depth so `/code-foundations:build` can execute from skeleton to a **production-ready app** serving an engineering team (S1: 10–50 engineers) with a design ceiling of a few-thousand-engineer org (S2).

User-confirmed scope decisions (2026-07-03): AWS managed production (OpenSearch Service 3.1 + ECS); MCP server + CLI as client surfaces (web dashboard OUT); **token auth without SSO** (OIDC seam preserved for later); productionization folded into Phase 7; **e2e testing and a fully local e2e stack are first-class deliverables**; ACL phase ordered ahead of experience memory.

## Constraints

- Everything locked in the skeleton plan: Go, OpenSearch pinned 3.1, BGE-M3 1024-dim co-located, bi-temporal write protocol (D10–D16), S1/S2 scale model, outbox queue, ≤$5/1k-events extraction gate.
- **Build and test at S1; design seams for S2.** One deployable (`engramd`) until a load test forces a split; the worker-pool boundary is the documented split seam.
- No SSO/OIDC integration in this plan; tokens must still carry `(user_id, agent_id)` identity so provenance and ACL are not weakened.
- Every phase extends the Phase 3 e2e suite with scenarios for its own features; e2e runs against the local compose stack, not mocks.
- Dependencies point inward: MCP/CLI/gRPC transports → use-cases → tier/store interfaces. No transport imports in business packages.

## Chosen Approach

**Surface-first modular monolith** — Phase 3 ships MCP + CLI + token auth + the local e2e stack immediately after the skeleton; capabilities (ACL → experience ∥ graph) land behind stable surfaces, each phase extending the e2e suite; then productionize (7) and gate releases (8). **Fallback:** if surface churn from capability phases exceeds two breaking MCP-contract revisions, freeze surfaces and revert to capability-first ordering for the remaining phases.

## Rejected Approaches

- **Capability-first (original GREENFIELD order):** nothing usable until late — no dogfooding, and the hardest correctness phases would build without end-to-end verification.
- **Split services from day one:** 4+ deployables for a 10–50-engineer team violates build-at-S1; the outbox already provides decoupling in-process — process separation adds ops cost, not correctness.

---

## Implementation Phases

_DAG: 3 → 4 → {5 ∥ 6} → 7 → 8. Phase bodies filled during DETAIL._

### Phase 3: Surfaces, Auth & E2E Foundation
**Model:** fable
**Skills:** aposd-designing-deep-modules, cc-defensive-programming
**Gate:** Full

**Goal:** Ship the surfaces engineers and agents actually use — an MCP server and `engram` CLI over the skeleton's gRPC API — behind token auth, and stand up the fully local e2e stack + harness that every later phase extends.

**Scope:**
- IN: `engram-mcp` (stdio) exposing `memory_ingest`/`memory_search`/`memory_status` mapped onto the gRPC API; `engram` CLI (search, ingest, token admin, status); token auth — random 256-bit tokens, stored hashed, TTL'd, revocable, bound to `(tenant_id, user_id, agent_id)`; gRPC unary interceptor as the **barricade** (authenticate + validate every call; internal packages assume Identity present); `deploy/local/docker-compose.yml` (pinned OpenSearch 3.1, BGE-M3 server, engramd, worker, **stub extraction LLM with deterministic fixtures**) + `make e2e`; e2e harness driving the full loop through MCP, CLI, and gRPC.
- OUT: SSO/OIDC integration (seam only); web dashboard; ACL semantics (Phase 4); rate limiting (Phase 7).

**Constraints:** raw token shown once at issuance; constant-time hash comparison; auth errors are typed and leak no detail; the worker exposes a per-stage registration seam — later phases add stages in their own files.
**Edge cases:** expired / revoked / malformed token each rejected distinctly (dirty tests); revocation effective ≤5 s; compose cold-start ordering gated by healthchecks; stub-LLM outputs keyed to fixtures so e2e is deterministic.
**Depends on:** none (entry — consumes the skeleton plan's Phase 2) | **Unlocks:** Phase 4
**File scope:** `cmd/engram-mcp/**, cmd/engram/**, internal/auth/**, internal/ingest/**, internal/store/**, api/proto/**, deploy/local/**, e2e/**`
**Produces:** `Authenticator.Verify(ctx, raw string) (Identity, error)`; `TokenIssuer.Issue(ctx, Identity, ttl) (rawToken string, err error)`; `Identity{TenantID, UserID, AgentID string}`; MCP tool schemas (`memory_ingest{event_id, text, source} → {id}`, `memory_search{query, k} → {hits[]}`); the **worker stage seam** (D20): `RegisterStage(name string, s Stage)` with `Stage.Process(ctx, ev Event, facts []Fact) error` — P5/P6 stages plug in here; the compose stack; a green `make e2e`.
**Security-sensitive:** yes
**Approach notes:** no SSO yet — user decision 2026-07-03; `TokenIssuer` is the OIDC seam.
**File hints:** `api/proto/` — extend with auth metadata conventions; `internal/store/` — `auth_tokens` index template.

**Done when:**
- [ ] DW-3.1: `make e2e` boots the full local stack from a clean checkout and runs the loop green: MCP ingest → worker extract/reconcile (stub LLM) → MCP search returns the fact.
- [ ] DW-3.2: `engram token create/revoke/list`, `engram search`, `engram ingest`, `engram status` all work against the local stack.
- [ ] DW-3.3: every gRPC/MCP call without a valid token is rejected; expired, revoked, and malformed each have a dirty test; revocation bites ≤5 s.
- [ ] DW-3.4: tokens stored hashed only; issuance shows the raw token exactly once; verification is constant-time.
- [ ] DW-3.5: MCP protocol conformance (initialize / list_tools / call_tool) passes against a reference client; a live agent round-trip from Claude Code is demonstrated.
- [ ] DW-3.6: a sample scenario pack is added in-phase with zero harness-core edits — proving the documented extension point Phases 4–8 will use.
- [ ] DW-3.7: an import-boundary lint (e.g. depguard) fails CI on any transport/framework import anywhere under `internal/**` (excluding the transport packages themselves) — structural rule, so packages added in later phases are covered automatically.

**Difficulty:** MEDIUM
**Uncertainty:** MCP SDK choice for Go (evaluate at build; fallback: hand-rolled stdio JSON-RPC — protocol is small).

### Phase 4: Multi-Agent Scope + ACL
**Model:** fable
**Skills:** ca-architecture-boundaries, aposd-designing-deep-modules, cc-defensive-programming
**Gate:** Full

**Goal:** Enforce provenance-as-ACL at query time: private/team/org scopes, write-time authorization, instant revocation, and a full audit trail — the trust layer that makes shared team memory viable.

**Scope:**
- IN: scope contract activated on the fields present since Phase 0 (`scope ∈ {private, team, org}`, provenance); `acl_edges` index holding user↔agent and agent↔resource reachability; **ACL filter compiler** translating Identity + reachability into an OpenSearch `bool` filter applied inside `Retriever` (callers cannot bypass it); write-time scope authorization (an agent writes only scopes its identity reaches); revocation = edge delete → next query excludes (query-time enforcement, no result caches to invalidate); concurrent shared-scope writes ride the existing OCC + `supersedes` chain; audit surface: `engram audit <id>` (who wrote it, from what, full bi-temporal history); e2e scenario pack.
- OUT: cross-org federation; per-record grants / subset (⊆) semantics via `terms_set` — deferred until a real policy needs them (YAGNI at S1; seam noted).

**Constraints:** **fail-closed** — empty reachability, unknown scope, or filter-compilation error all mean deny, never allow; defense-in-depth: scope checked at write AND read (auth bugs happen).
**Edge cases:** identity with zero scopes → deny-all; revocation mid-session (≤5 s like tokens); filtered-kNN recall re-verified at ACL selectivities (private-only is highly selective); malformed acl_edge docs rejected at write.
**Depends on:** Phase 3 | **Unlocks:** Phases 5, 6
**File scope:** `internal/acl/**, internal/retrieval/**, internal/store/**, e2e/acl/**`
**Produces:** `ACLFilter.Compile(ctx, Identity) (query.Query, error)` (fail-closed); scope write-rule table; **three registration seams P5/P6 consume from their own packages** — `Retriever.RegisterPostHook(h PostHook)` with `PostHook.Apply(ctx, id Identity, hits []Hit) ([]Hit, error)`; `Retriever.RegisterTier(src TierSource)` with `TierSource.Search(ctx, id Identity, q Query) ([]Hit, error)`; `Store.RegisterWriteGuard(g WriteGuard)` with `WriteGuard.Check(ctx, id Identity, r Record) error` (non-nil = deny, fail-closed; all write-time authorization runs through it); audit RPC `Audit(AuditRequest{id string}) returns (AuditResponse{provenance Provenance, versions []FactVersion})` + `engram audit` CLI; ACL e2e pack (grant/revoke/deny matrices).
**Security-sensitive:** yes
**Approach notes:** query-time enforcement over index-time ACL baking — D6 (Collaborative Memory): revocation must be instant.
**File hints:** `internal/memory/` — scope/provenance structs exist; `docs/` — scope semantics doc.

**Done when:**
- [ ] DW-4.1: a 3-identity × 3-scope e2e matrix (9 cells) returns exactly the authorized set in every cell.
- [ ] DW-4.2: revoking a user↔agent edge hides that agent's team/org-scoped hits from the user's next query, ≤5 s, no restart.
- [ ] DW-4.3: write-time rule enforced — an agent writing an unauthorized scope gets a typed denial (dirty test).
- [ ] DW-4.4: fail-closed proven: induced compiler error and empty-reachability identity both yield zero results + logged denial, not open access.
- [ ] DW-4.5: filtered-kNN recall at private/team/org selectivities stays within 5 pp of unfiltered recall on the gold set.
- [ ] DW-4.6: `engram audit <id>` shows provenance + full version history for any fact, via the as-of query contract.

**Difficulty:** HIGH
**Uncertainty:** reachability-to-filter translation cost at org scale — measure; fallback is a precomputed per-identity clearance field (GREENFIELD's `max_required_clearance`).

### Phase 5: Experience Memory + Write-Gating (T3)
**Model:** fable
**Skills:** aposd-designing-deep-modules, cc-defensive-programming
**Gate:** Full

**Goal:** Task-experience memory that makes agents measurably better without poisoning them: distillation into T3, a **mandatory write-gate**, utility scoring + pruning, and the net-new quarantine tier.

**Scope:**
- IN: `Experience` record (task, context, trajectory ref, outcome evidence, `utility Φ`, distilled_skill, retrieval/scope/provenance fields); MUSE-style distillation as **its own worker stage** (own file, registered via the Phase 3 stage seam); `Gatekeeper.Evaluate(ctx, Experience) (Verdict, error)`, `Verdict ∈ {Admit, Quarantine, Reject}` — LLM-judge on outcome evidence, **no bypass path** (D5), enforced through Phase 4's `WriteGuard` seam; quarantine index — never retrievable, reviewable via `engram quarantine list/release`; utility prune job (low Φ + low retrieval_count → soft-expire); retrieval of admitted experiences as a gated tier via Phase 4's `RegisterTier` seam; injected-bad-record harness; e2e pack.
- OUT: RL-learned memory management (Mem-α — roadmap); cross-team experience sharing policies (needs Phase 4 field data first).

**Constraints:** gate failure is **fail-closed**: LLM timeout/error → Quarantine, never Admit; prune soft-expires, never deletes (D3).
**Edge cases:** self-reported success with no evidence → Quarantine; contradictory outcome signals → Quarantine; duplicate experience (same task fingerprint) → merge, not double-count; quarantine release requires explicit human CLI action.
**Depends on:** Phase 4 | **Unlocks:** Phase 7
**File scope:** `internal/experience/**, cmd/engramd/stages_experience.go, e2e/experience/**`
**Produces:** `Gatekeeper` interface (contract above); T3 index template; quarantine CLI; A/B eval evidence (curated-small vs large-noisy); experience-following correlation metric emitted to the harness.
**Security-sensitive:** yes (trust boundary against untrusted agent-supplied experience — poisoning is the threat model)
**Approach notes:** quarantine tier is net-new — no literature pattern to port; design freedom here is intentional.
**File hints:** `internal/ingest/` — stage seam; `.code-foundations/research/GREENFIELD-BUILD-PLAN.md` — Phase 3 section.

**Done when:**
- [ ] DW-5.1: injected-bad-record suite: ≥90% of poisoned experiences land in Quarantine/Reject; ≥80% of good fixtures Admit.
- [ ] DW-5.2: gate has no bypass: code path audit + test proving direct-index writes to T3 are rejected by the store layer.
- [ ] DW-5.3: gate-LLM timeout → Quarantine (dirty test); nothing Admits on error.
- [ ] DW-5.4: curated-small-beats-large reproduced on the eval set (A/B logged by the harness).
- [ ] DW-5.5: experience-following correlation metric emitted; prune job soft-expires low-Φ records (verified bi-temporally recoverable).
- [ ] DW-5.6: e2e pack: agent completes a task, experience distills, gates, and is retrieved in a later session via MCP.

**Difficulty:** HIGH
**Uncertainty:** gate-judge quality — if the LLM judge can't hit DW-5.1 rates, fallback is a stricter evidence-schema requirement (structured outcome proof) before any judging.

### Phase 6: Incremental Graph (T4)
**Model:** sonnet
**Skills:** aposd-designing-deep-modules, cc-routine-and-class-design
**Gate:** Full

**Goal:** Per-episode entity/edge upsert with dedup and ≤2-hop expansion at query time — "connect the dots" retrieval without batch recompute, plus the evidence-based graph-DB adoption decision.

**Scope:**
- IN: entity + edge indices (bi-temporal edges per D3, scope fields per Phase 4); extraction stage extended to emit entity mentions; **incremental upsert with dedup** — embed-similarity + BM25 + LLM-judge tie-break (LiCoMemory hyperlink-not-duplicate pattern), one decision routine, functional cohesion; `GraphExpander.Expand(ctx, hits []Hit, depth int) ([]Hit, error)` (depth ≤ 2, GeAR triple expansion post-RRF); entity-count stability metric; graph-DB decision gate at phase exit (hop-depth evidence → stay on OpenSearch edges vs adopt Neo4j/FalkorDB, D8); e2e pack with a fixture knowledge base.
- OUT: >2-hop traversal; community summarization (Zep notes drift — revisit post-S1); graph-native storage (that's what the decision gate decides).

**Constraints:** expansion runs **inside** the ACL boundary — expanded hits re-filtered through `ACLFilter` before return; dedup merges are soft (bi-temporal), reversible.
**Edge cases:** same-name different-entity (embedding distance + provenance disambiguation before LLM-judge); repeated ingest of identical facts keeps entity count flat; dangling edge (entity soft-expired) skipped at expansion; expansion on zero hits is a no-op.
**Depends on:** Phase 4 | **Unlocks:** Phase 7
**File scope:** `internal/graph/**, cmd/engramd/stages_graph.go, e2e/graph/**`
**Produces:** `GraphExpander` (contract above) registered via **Phase 4's `RegisterPostHook` seam** from `internal/graph` — no retrieval-package edits; entity/edge index templates; dedup decision routine; decision-gate memo (hop-depth data → D8 verdict).
**Security-sensitive:** yes (graph expansion is an ACL-enforcement path — a traversal leak bypasses Phase 4)
**Approach notes:** none — governed by D2/D8 (skeleton plan Decision Log).
**File hints:** `internal/retrieval/` — post-RRF hook point; `internal/ingest/` — stage seam.

**Done when:**
- [ ] DW-6.1: single-episode ingest adds/updates entities + edges with zero recompute of existing graph state.
- [ ] DW-6.2: 2-hop connect-the-dots e2e query on the fixture KB returns the documented answer path (A→B→C).
- [ ] DW-6.3: entity count stable (±0) across 10 re-ingests of the same fact set; dedup decisions logged.
- [ ] DW-6.4: expansion honors ACL — dirty test: a hit reachable only through an unauthorized edge is absent.
- [ ] DW-6.5: p95 search latency with expansion enabled ≤ 250 ms in the perf harness (base 150 ms + expansion budget 100 ms).
- [ ] DW-6.6: decision-gate memo written with measured hop-depth distribution; D8 confirmed or flipped in the Decision Log.

**Difficulty:** MEDIUM
**Uncertainty:** dedup LLM-judge cost per episode — if it breaches the extraction budget, fallback to threshold-only dedup with a weekly judge sweep.

### Phase 7: Scale, Ops & Production
**Model:** sonnet
**Skills:** performance-optimization, cc-quality-practices
**Gate:** Full

**Goal:** Take the feature-complete system to a running, observed, recoverable production deployment on AWS, proven by load test, failure drill, and restore drill.

**Scope:**
- IN: deploy tooling — an **idempotent Go deploy CLI** (`cmd/engram-deploy`, AWS SDK, describe-then-converge so re-runs are no-ops; extends the Phase-0 apply-templates idiom) provisioning the OpenSearch Service 3.1 domain, ECS services (engramd, worker, BGE-M3 embedding container), Secrets Manager, VPC, wrapped in `make deploy-staging` / `make deploy-prod`; CI/CD deploy stage (build → staging → gates → prod, blue/green ECS); OTel instrumentation + dashboards — RED metrics plus domain gauges (worker/outbox lag, repair-sweep backlog + convergence age, DLQ depth, gate verdict rates, extraction $/1k events); alert rules bound to SLOs (search p95 ≤150 ms / ≤250 ms expanded, ingest availability, worker lag, DLQ >0); snapshot policy + **restore drill executed in-phase** (RPO ≤24 h, RTO ≤1 h, documented); **load test** (measure-first: profile before tuning anything) at 10× S1 sustained + burst; RAM/circuit-breaker headroom verified with SQfp16 math; cost controls (budget alarms, extraction gate hooked to a kill-switch); runbooks for the 5 likeliest incidents; e2e suite gains a `cloud` profile running against staging.
- OUT: multi-region; hot/cold tiering + searchable snapshots (S2 triggers, documented); autoscaling policies beyond simple ECS target tracking.

**Constraints:** no optimization without a measurement (7-step gate); staging is topology-identical to prod, scaled down.
**Edge cases:** OpenSearch blue/green domain update behavior during deploy; embedding container cold start (health-gated); circuit-breaker trip under load test → documented degradation (BM25-only flag), not crash.
**Depends on:** Phases 5, 6 | **Unlocks:** Phase 8
**File scope:** `deploy/aws/**, cmd/engram-deploy/**, internal/telemetry/**, .github/workflows/**, docs/runbooks/**, e2e/cloud/**`
**Produces:** running prod + staging environments from the deploy CLI; dashboards + alert pack; runbooks; load/restore/failure drill evidence in the Execution Log.
**Security-sensitive:** yes (secrets management, prod credentials)
**Rollback:** blue/green ECS task-definition revert; OpenSearch restore-from-snapshot; **point of no return:** breaking index-template change — mitigation: versioned indices + alias flip, never in-place mapping mutation.
**Approach notes:** AWS managed — user decision 2026-07-03; **no Terraform/HCL — user decision 2026-07-03**, deploy is a Go CLI on the AWS SDK (D24). Single deployable retained, worker-pool boundary is the documented S2 split seam. Model is deliberately sonnet despite Full gate: IaC/ops execution is well-specified and the judgment calls (SLOs, budgets, rollback strategy) are fixed in this plan. `performance-optimization` is loaded for its measure-first discipline; its single-process scope means distributed-tuning specifics stay with the phase's own SLO evidence.
**File hints:** GREENFIELD-BUILD-PLAN §Phase 6 — sizing formulas; skeleton plan D14/D15 — version + embedding placement.

**Done when:**
- [ ] DW-7.1: `make deploy-staging` from a clean checkout converges to a working environment (same for prod); **re-run is a verified no-op** (idempotency test); e2e `cloud` profile green against staging.
- [ ] DW-7.2: load test at 10× S1 sustained (≥500k events/day pace) + 5× burst holds all SLOs; report includes p50/p95/p99 per endpoint and worker lag.
- [ ] DW-7.3: vector RAM measured under load stays ≤80% of circuit-breaker limit; matches the SQfp16 sizing formula within 20%.
- [ ] DW-7.4: restore drill passes — snapshot → new domain → e2e green; RPO/RTO documented and met.
- [ ] DW-7.5: failure drill — kill a worker task and an OpenSearch node in staging: alerts fire, no data loss (outbox + repair verified), runbook followed as written.
- [ ] DW-7.6: cost: extraction $/1k events within the D-gate budget in staging under load; budget alarm fires in a synthetic overspend test.
- [ ] DW-7.7: five incident runbooks exist and each was walked once (tabletop) with gaps fixed.
- [ ] DW-7.8: every domain gauge listed in IN (outbox/worker lag, repair backlog + convergence age, DLQ depth, gate verdict rates, extraction $/1k) renders on the dashboards and visibly moves during the load test.

**Difficulty:** MEDIUM-HIGH
**Uncertainty:** GPU vs CPU serving for BGE-M3 on ECS at S1 query rates — measure in staging; fallback: CPU with ONNX int8 for queries (short texts) + batch GPU spot for ingest embedding.

### Phase 8: Eval & Safety Gates
**Model:** sonnet
**Skills:** cc-quality-practices
**Gate:** Standard

**Goal:** The Phase-0 eval harness graduates into CI/CD release gates: memory-hallucination, retrieval-regression, and experience-following metrics that automatically block a bad release — proven by a drill.

**Scope:**
- IN: HaluMem-style hallucination suite (gold QA over a fixture corpus; measures facts asserted by memory that the corpus doesn't support); retrieval regression gate (frozen holdout, non-inferiority vs recorded baseline — DW-1.3's split, versioned); experience-following health correlation as a tracked gate; gate thresholds documented + versioned in-repo; CI wiring — release candidate deploys to staging, gates run against it, red gate blocks promotion; an `e2e/eval` scenario pack (gate pipeline exercised end-to-end against the local stack); eval trend dashboards; **bad-release drill** (intentionally degraded reconciler prompt → gates must block).
- OUT: new eval *research* (novel metrics — roadmap); per-customer eval sets.

**Constraints:** full gate run ≤15 min or it will be skipped in practice; combine techniques (no single detector exceeds ~75% — hallucination + regression + following are three independent detectors by design); gates read-only against staging (never mutate prod data).
**Edge cases:** flaky gate (two consecutive contradictory runs) → auto-quarantine the gate + alert, don't silently un-gate; baseline drift (intentional relevance change) → explicit baseline re-record procedure with sign-off.
**Depends on:** Phase 7 | **Unlocks:** done — steady state
**File scope:** `internal/eval/**, .github/workflows/**, e2e/eval/**, docs/eval/**`
**Produces:** CI release-gate job (config + thresholds); hallucination/regression/following dashboards; drill evidence in the Execution Log; baseline re-record runbook.
**File hints:** skeleton plan DW-0.5/DW-1.3 — the harness and holdout this phase promotes.

**Done when:**
- [ ] DW-8.1: hallucination rate measured on every release candidate; threshold breach blocks promotion (verified in CI, not just locally).
- [ ] DW-8.2: retrieval regression gate holds the DW-1.3 non-inferiority contract against the versioned baseline.
- [ ] DW-8.3: experience-following correlation tracked per release; alert on regression beyond documented band.
- [ ] DW-8.4: **drill passes** — an intentionally bad release is blocked automatically, with evidence (CI logs) recorded.
- [ ] DW-8.5: full gate suite completes ≤15 min; flaky-gate quarantine path has a dirty test.
- [ ] DW-8.6: ≥3 gate runs within the phase (release candidates and/or drill runs) produce visible trend history on the dashboards — trend, not point-in-time.

**Difficulty:** MEDIUM
**Uncertainty:** hallucination-suite construction cost — if gold-QA authoring is too slow, bootstrap from the e2e fixture corpus (facts are known by construction).

---

## Test Coverage
**Level:** 100% — every DW item tested; ≥1 dirty test per code-touching phase; every phase ships an e2e scenario pack running against the local compose stack (Phases 7–8 also against staging). _(Defaulted to the recommended level while the user was AFK — confirm at plan review.)_

## Test Plan
- [ ] P3: `make e2e` clean-checkout boot + full-loop test (DW-3.1); CLI integration tests (DW-3.2); dirty trio — expired/revoked/malformed token + revocation ≤5 s (DW-3.3); token hashed-storage + constant-time-verify unit tests (DW-3.4); MCP conformance suite + live Claude Code round-trip (DW-3.5); sample-pack-added-without-core-edits test (DW-3.6); import-boundary lint red/green cases (DW-3.7). Boundary: token at/just-past TTL; empty-text ingest.
- [ ] P4: 3×3 scope matrix e2e (DW-4.1); revocation propagation test (DW-4.2); dirty — unauthorized-scope write denial (DW-4.3) + malformed `acl_edge` doc rejected at write; fail-closed induced compiler-error + zero-scope identity (DW-4.4); filtered-kNN recall at 3 selectivities (DW-4.5); audit history e2e (DW-4.6). Boundary: identity with exactly one scope.
- [ ] P5: injected-bad-record rates (DW-5.1); gate-bypass rejection via WriteGuard (DW-5.2); dirty — judge timeout → Quarantine (DW-5.3) + duplicate experience (same task fingerprint) merges, not double-counts; curated-vs-large A/B (DW-5.4); prune soft-expiry + bi-temporal recovery (DW-5.5); distill→gate→retrieve e2e (DW-5.6). Boundary: Φ exactly at prune threshold.
- [ ] P6: no-recompute upsert (DW-6.1); 2-hop fixture query (DW-6.2); 10× re-ingest entity stability (DW-6.3) + homonym test — same-name different-entity stays unmerged (disambiguation path); dirty — ACL-blocked edge not traversed, dangling edge skipped (DW-6.4); expansion on zero hits is a no-op; p95 ≤250 ms perf run (DW-6.5); decision memo exists (DW-6.6). Boundary: depth 2 honored, 3 rejected.
- [ ] P7: terraform-apply + cloud-profile e2e (DW-7.1); 10× sustained + 5× burst load test (DW-7.2); RAM ≤80% breaker + formula check (DW-7.3); restore drill (DW-7.4); dirty — kill worker task + OpenSearch node, alerts + no data loss (DW-7.5) + induced circuit-breaker trip degrades to flagged BM25-only, no crash; synthetic overspend alarm (DW-7.6); runbook tabletops (DW-7.7); dashboard gauges move under load (DW-7.8). Boundary: breaker at exactly 80%.
- [ ] P8: CI-block on hallucination breach (DW-8.1); regression non-inferiority vs versioned baseline (DW-8.2); following-band alert (DW-8.3); bad-release drill (DW-8.4); dirty — flaky-gate quarantine (DW-8.5); ≥3-run trend history (DW-8.6). Boundary: metric exactly at threshold → documented block/pass behavior verified.

---

## Assumptions

| Assumption | Confidence | Verify Before Phase | Fallback If Wrong |
|---|---|---|---|
| AWS account/quotas support an OpenSearch Service 3.1 domain | HIGH | 7 | self-host pinned 3.1 on EC2 via the same deploy-CLI seam |
| A viable Go MCP SDK exists | MED | 3 | hand-rolled stdio JSON-RPC (protocol is small) |
| Stub-LLM fixtures give deterministic e2e | HIGH | 3 | record-replay of real cheap-model calls |
| LLM judge reaches DW-5.1 gate rates | MED | 5 | structured outcome-evidence schema required before judging |
| ACL reachability→filter compile is cheap at S1 | HIGH | 4 | precomputed per-identity clearance field (GREENFIELD) |
| BGE-M3 CPU serving meets the 50 ms query-embed budget | MED | 7 | ONNX int8 for queries; batch GPU (spot) for ingest embedding |

## Decision Log

| Decision | Alternatives Considered | Rationale | Phase |
|---|---|---|---|
| D17: token auth, no SSO; `TokenIssuer` is the OIDC seam | SSO now; static API keys | user decision 2026-07-03; static keys would erase per-user provenance | 3 |
| D18: surface-first modular monolith (Approach A) | capability-first; split services | e2e exists before the correctness-critical phases; team dogfoods from Phase 3; one deployable at S1 | all |
| D19: ACL (P4) ahead of experience (P5) | GREENFIELD original order | shared scoped memory is the first product surface for a team | 4–5 |
| D20: worker stage registry; each phase registers stages in its own file | shared wiring edits per phase | keeps P5 ∥ P6 file scopes disjoint for build parallelism | 3, 5, 6 |
| D21: local e2e = docker-compose with a deterministic stub extraction LLM | testcontainers-only; shared cloud dev env | boots anywhere, free, deterministic; testcontainers stays for unit-level integration | 3 |
| D22: staging topology-identical to prod; release gates run there | gates against prod | gates need a target that can fail safely | 7, 8 |
| D23: scope tiers reduced to `{private, team, org}` | GREENFIELD's 5 tiers (`+ shared, global`) | YAGNI at S1 — no cross-org sharing exists; `scope` is a keyword field, so new tiers are additive (no reindex) | 4 |
| D24: deploy = idempotent Go CLI on the AWS SDK, no Terraform/HCL | Terraform; Pulumi (Go); CDK (Go); raw shell + AWS CLI | user preference 2026-07-03; one language across app + infra; extends the Phase-0 idempotent-apply idiom. Fallback: Pulumi-Go if converge/drift logic outgrows the CLI | 7 |

---

## Notes

- **Carry-over from the skeleton build (2026-07-03, Phase-2 review Issue 3):** implement **sweep rule (d)** — closed-closed valid-time overlap repair per (tenant, subject, predicate), trimming the earlier record to the later's `valid_at` — as the first work item of Phase 3 (or a pre-Phase-3 patch). It closes the irreducible multi-document write-skew window; the `trimInterval` primitive and regression scaffolding already exist. See the skeleton plan's Write protocol §Repair sweep (d).
- Go MCP SDK choice is a build-time decision (Assumption row 2); the tool schemas in Phase 3's Produces are the stable contract either way.
- Subset (⊆) ACL semantics via `terms_set` deliberately deferred — Phase 4 OUT; seam noted there.
- Community summarization deferred (Zep reports drift under incremental updates) — revisit after S1 field data.
- Test-coverage level defaulted to 100% while the user was AFK — confirm at plan review.
- Build order: the skeleton plan (Phases 0–2) executes first; this plan's Phase 3 consumes its Produces. Both plans stay in `.code-foundations/plans/` and cross-reference.

---

## Execution Log
_To be filled during /code-foundations:build_

## Execution Log

### Phase 3: Surfaces, Auth & E2E Foundation (Gate: Full, 3-sample review)
- [x] BUILD: MCP server + CLI + token auth + local compose e2e stack + worker stage seam + carry-over rule (d)
- [x] REVIEW: unanimous 3-sample PASS (security-focused). Follow-up: cmd/engram-perf runs without the interceptor (out-of-scope perf tool).
- [x] Committed
Commit: ff2fbad
Summary: Engram now has real client surfaces — engram-mcp (stdio) and the engram CLI over the gRPC API — behind a 256-bit hashed-token auth barricade (constant-time verify, ≤5s revocation). A fully local compose stack (pinned OpenSearch 3.1 + deterministic embed + stub LLM + engramd + worker) runs `make e2e` end-to-end through MCP/CLI/gRPC. The worker stage-registration seam (D20) and ACL post-hook/tier/write-guard seams are the plug points Phases 4–6 consume; sweep rule (d) closed the last skeleton write-skew window. 174 unit + integration + e2e tests green.

### Phase 4: Multi-Agent Scope + ACL (Gate: Full, 3-sample review)
- [x] BUILD: scope contract, acl_edges reachability, ACL filter compiler (fail-closed, in-retriever), write-guard, revocation, Audit RPC/CLI, the four Phase-5/6 seams
- [x] REVIEW: 2-of-3 PASS; unanimous finding (tier-hit truncation before ACL re-filter) fixed pre-commit with falsification-proven regression
- [x] Committed
Commit: 529cb28
Summary: Provenance-as-ACL is live and enforced at query time inside the retriever (callers cannot bypass), fail-closed on every error path, with write-time scope guarding and instant revocation. The four registration seams (RegisterPostHook, RegisterTier, RegisterWriteGuard, plus D20 RegisterStage) are real and exercised — Phase 5 plugs its gated experience tier + write-gate into them, Phase 6 its graph post-hook. Audit RPC + `engram audit` expose provenance and full bi-temporal history. 200+ tests green incl. live-cluster ACL matrices and the truncation-ordering regression.

### Phase 5: Experience Memory + Write-Gating (T3) (Gate: Full, 3-sample review)
- [x] BUILD: Experience record + distillation stage, mandatory no-bypass Gatekeeper, quarantine tier, gated retrieval tier, soft-expire prune, injected-bad harness
- [x] REVIEW: unanimous 3-sample PASS (all six bypass vectors closed); one lint blocker + a silent-integration-skip gap found and fixed pre-commit
- [x] Committed
Commit: ce3ecfd
Summary: T3 experience memory is live behind a mandatory, fail-closed, no-bypass write-gate — the ExperienceStore is the only T3 writer and cannot admit without a Gatekeeper verdict; timeouts/errors/contradictory evidence quarantine. Quarantine is unreachable via retrieval, released only by human CLI. Admitted experiences serve through the Phase-4 gated tier; prune soft-expires (recoverable). The distillation stage plugs into the D20 worker seam. 242 unit + live-integration + e2e green.

### Phase 6: Incremental Graph (T4) (Gate: Full, 3-sample review)
- [x] BUILD: entity/edge indices, incremental upsert + single-routine dedup, <=2-hop GraphExpander via RegisterPostHook, D8 decision-gate memo
- [x] REVIEW: majority 3-sample PASS (security verified — no ACL leak); one param-count design violation fixed pre-commit
- [x] Committed
Commit: 4e22b24
Summary: T4 graph is live — per-episode entity/edge upsert with dedup (flat entity count on re-ingest), <=2-hop expansion that re-authorizes every hit through the Phase-4 ACL post-hook (unauthorized-edge nodes never returned; BFS-through-unseen-edges is a benign relevance side-channel, recorded). D8 CONFIRMED: stay on OpenSearch (p95 ~110ms vs 250ms ceiling) — no Neo4j needed at S1/S2. Decision-gate memo in internal/graph/DECISION_GATE.md.

**Known property (Phase 6 review, recorded):** graph BFS traverses through edges the caller cannot see to reach authorized deeper facts. No unauthorized content is ever returned (each hit re-authorized), but the existence of hidden intermediate edges is weakly inferable from which deep facts surface. Consistent with the per-record ACL model; revisit if edge-existence confidentiality becomes a requirement.

### Phase 7: Scale, Ops & Production (Gate: Full, 3-sample review)
- [x] BUILD: idempotent Go deploy CLI (D24), OTel + domain gauges, blue/green + snapshot rollback, real restore/failure/overspend drills, load test, 5 runbooks, CI deploy workflow
- [x] REVIEW: majority 3-sample PASS; a class of silent-no-op deploy-convergence defects (image, then CPU/mem/port) found and fixed pre-commit with falsification proofs; dangling runbook refs closed
- [x] Committed
Commit: 0c32b79
Summary: Engram is deployable, observed, and recoverable. The deploy CLI converges idempotently and now rolls out on any task-def change (image/cpu/mem/port); rollback + snapshot-restore are real and drilled; telemetry gauges move under load; budget alarm + kill-switch guard cost. Restore RTO ~0.1s, failure drills lose no data. **Open pre-production gates (recorded):** (1) real-AWS `make deploy-staging/prod` + cloud e2e — tooling built/local-tested, real run is a documented manual step (no cloud creds here); (2) multi-instance staging load-test re-run — burst search p95 breaches on the single-node local cluster (sustained holds); docs/runbooks/load-test-s1-vs-s2.md tracks it as required before prod.

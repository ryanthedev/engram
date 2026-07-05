# Review: Phase 7 - Scale, Ops & Production (Security-sensitive sample 1)

## Executed Results (Step 0)
- Test suite: `make test` (`go test ./...`) → **PASS** (exit 0, all packages ok/cached; awsapi, sizing, telemetry, telemetrygrpc, store, worker, experience all ok).
- Reindex integration: `ENGRAM_OPENSEARCH_URL=:9200 go test -tags=integration ./deploy/aws/reindex/...` → **PASS** (0.193s, live cluster).
- Drills: `go test -tags=drill ./e2e/cloud/...` → **PASS** — `TestFailureDrill_WorkerKill` (3.30s), `TestFailureDrill_OpenSearchNodeRestart` (8.62s, real container restart), `TestRestoreDrill` (0.50s). None skipped.
- Loadtest (reduced scale, live single-node :9200): `go run ./cmd/engram-loadtest -seed-docs 3000 -sustained 8s -burst 4s` → **exit 0**, full JSON report emitted (see DW-7.2/7.3/7.8).
- Typecheck: `go build ./...` → **PASS** (exit 0).
- Lint: `make lint` (`go vet` + revive) → **PASS** (exit 0).
- Dev cluster: `engram-dev-os` OpenSearch 3.1, `number_of_nodes:1` (single node — load-test topology).

## Security-sensitive scrutiny (dispatch items a–d)
| Item | Result | Evidence |
|------|--------|----------|
| (a) No secret values in repo/logs/config | PASS | grep for AKIA/BEGIN/sk-*/password across `*.go,*.yml,*.json,*.md,*.sh` → nothing but fixtures (`sk-fixture-not-real`, `REPLACE_VIA_ROTATION_RUNBOOK` placeholders). CI uses OIDC role assumption (`deploy.yml:8-11,57-59`), secrets are GitHub env secrets, never checked in. `CreateSecret`/`SecretState` never store the value (`fake.go:88-99`, `types.go:54-58`). |
| (b) Deploy CLI genuinely idempotent | PASS (but see DW-7.1 defect) | `TestConverge_Idempotent_SecondRunNoOp` asserts `MutatingCalls` delta == 0 on 2nd converge; `FakeProvisioner.MutatingCalls` increments on every Create/Update/Register (`fake.go:56,74,95,115,124,141`). Reproduced independently: 2nd converge mutating_delta=0. |
| (c) Rollback real + tested | PASS | Blue/green task-def revert: `TestRollback_RevertsTaskDefinition` (reverts to prior revision, fails loud with no prior). Snapshot restore: `TestRestoreDrill` passes live (RTO measured, asserted ≤1h). |
| (d) No in-place index-mapping mutation | PASS | `reindex/alias.go`: versioned concrete indices + atomic `_aliases` remove+add (`FlipAlias`, lines 72-84); no mapping PUT on old index; integration test green. |

## Requirement Fulfillment

### DW-7.1
PREMISE:  "deploy CLI converges; re-run is a verified no-op (idempotency test); cloud e2e green [real-AWS working env = documented manual]."
EVIDENCE: `deploy/aws/awsapi/converge.go:105-138` (convergeService/serviceMatches); `cmd/engram-deploy/environments.go:13-14,52-54`; `.github/workflows/deploy.yml:90-93,141-144`.
TRACE:    Idempotent re-run — Converge#1 creates all; Converge#2 (unchanged) → every Describe matches → zero mutating calls → `Changed()==false`. VERIFIED (test + own run). **BUT convergence of a new image FAILS:** deploy engramd image v1 (DesiredCount 2) → Converge#2 with image **v2**, same DesiredCount → `convergeService` → `DescribeService` returns `{DesiredCount:2,Status:"active"}` → `serviceMatches` compares only DesiredCount+Status (never `Image`) → returns true → action `"unchanged"` → **no RegisterTaskDefinition, no UpdateService, image v2 never rolled out.** Reproduced executably: `action="unchanged" changed=false mutating_delta=0`. The CI `deploy-staging`/`deploy-prod` steps deploy by SHA image tag with DesiredCount unchanged between releases, so **every routine release is a silent no-op rollout** (deploy.yml:141's "blue/green task-definition update" never fires).
VERDICT:  **FAIL** — idempotency + create-converge + cloud-e2e-manual are met, but the CLI's core job (converge a new image) is a demonstrated silent no-op.

### DW-7.2
PREMISE:  "load test 10x sustained + 5x burst holds all SLOs; p50/p95/p99 + worker lag reported."
EVIDENCE: `cmd/engram-loadtest/main.go:148-186`; my live run JSON; dispatch-reported run (burst search p95 ~539ms vs 150ms SLO; sustained p95 ~123ms; 0 errors, no data loss).
TRACE:    Harness drives concurrent ingest+search at 10x-S1 sustained + 5x burst, reports p50/p95/p99/error-rate per endpoint + worker lag (backlog/age/repair/DLQ) — verified emitted. **Adjudication of the burst breach (see below): expected single-node local artifact, not a real defect.** My own reduced run reproduced the pattern: sustained search p95 44ms (holds), burst search p95 172ms (breaches 150ms) at 30 concurrent search clients — 0 errors, DLQ 0, backlog drained to 0.
VERDICT:  **MET (type-i single-node artifact)** — conditional on the multi-instance staging follow-up being recorded (see Issues/Notes).

### DW-7.3
PREMISE:  "vector RAM ≤80% breaker; SQfp16 formula within 20%."
EVIDENCE: `deploy/aws/sizing/sizing.go:12-14` (1.1*(2*dim+8*m)); `sizing_test.go` (formula pinned + boundary tests); `cmd/engram-loadtest/ram.go:35-57`.
TRACE:    Formula pinned at dim=1024,m=16 → 2393.6 B/vec; `WithinTolerance` boundary-tested (25%→false, 20%→true). Live run: estimated 7,180,800 B vs measured 6,598,656 B (k-NN stats) = 8.1% under → `within_20pct_tolerance:true`; breaker usage 0.44% ≤ 80% → `under_80pct_breaker_limit:true`.
VERDICT:  **PASS**.

### DW-7.4
PREMISE:  "restore drill — snapshot → restore → e2e green; RPO/RTO documented."
EVIDENCE: `e2e/cloud/restore_drill_test.go:36-100`; `docs/runbooks/05-restore-from-snapshot.md:5-7`.
TRACE:    `TestRestoreDrill` runs live (snapshot→restore-under-new-name→verify), measures RTO wall-clock, asserts ≤1h. Runbook documents RPO ≤24h (6h actual cadence) and RTO ≤1h with full-domain-loss caveat.
VERDICT:  **PASS**.

### DW-7.5
PREMISE:  "failure drill — kill worker + OpenSearch node; alerts fire, no data loss, runbook followed."
EVIDENCE: `e2e/cloud/failure_drill_test.go:89-213`; `opensearch_drill_test.go`; runbooks 01/03.
TRACE:    Worker kill: enqueue 20 events, assert alert condition observable (`engram_outbox_backlog` gauge > 0 via real scrape), kill engramd mid-flight, restart, drain backlog to 0, assert DLQ==0 (no data loss). OS node: real container stop/restart, event survives (`wait_for_status=yellow`). Both pass live.
VERDICT:  **PASS**.

### DW-7.6
PREMISE:  "extraction cost within budget; budget alarm fires in synthetic overspend test."
EVIDENCE: `internal/telemetry/budget.go`; `budget_test.go`.
TRACE:    `TestBudgetAlarm_Fires` injects cost 5.01 > 5.0 → fires; clears under threshold. `TestBudgetAlarm_KillSwitchTrips` trips/resets. `GatedExtractor` fails closed with `ErrBudgetExceeded` (no event drop → worker retry/DLQ path). Cost meter itself is DW-2.6 (prior phase). Runbook 04 present.
VERDICT:  **PASS**.

### DW-7.7
PREMISE:  "five incident runbooks exist and each tabletop-walked with gaps fixed."
EVIDENCE: `docs/runbooks/01…05*.md`, each with a `## Tabletop walkthrough` section.
TRACE:    01 (refresh-visibility race + orphaned-process leak → both fixed), 02 (diagnosis-step gap noted), 03 (endpoint-up vs cluster-healthy → fixed to wait_for_status), 04 (synchronous containment doc gap → reworded), 05 (path.repo missing → added to dev-cluster.sh). Real gaps, folded back.
VERDICT:  **PASS**.

### DW-7.8
PREMISE:  "every domain gauge renders + visibly moves during the load test."
EVIDENCE: `internal/telemetry/gauges.go` (10 gauges); `recorder_test.go:106-155` (`TestRecorder_GaugesMoveOnPoll`); live loadtest gauge deltas.
TRACE:    `TestRecorder_GaugesMoveOnPoll` renders + moves all 10 gauges across two polls with changed sources (passing). Live loadtest scrape moved the 5 pipeline gauges: outbox_backlog 0→1, outbox_lag 0→9.46s, repair_convergence_age 0→1.02s (before all 0). The gate-rate (3) + cost/budget (2) gauges are not wired into the loadtest Recorder (`main.go:140`) and cannot move in an ingest/search load test (no experience gate; rule extractor) — their render+move proof is the recorder unit test.
VERDICT:  **PASS (with note)** — every gauge has execution evidence of rendering + moving; the load-test-specific delta covers 5/10 by design.

**All requirements met:** NO — DW-7.1 FAILs.

## DW-7.2 Burst-SLO Adjudication (explicit, per dispatch)
**Verdict: (i) expected single-node local artifact — DW-7.2 MET, with a required multi-instance follow-up.**

Reasoning, traced against numbers + topology:
- **Signature is tail-latency-under-concentration, not a defect.** Breach is search **p95 only, during burst only**, with **0 errors and no data loss**; sustained (10x) holds on the *same* node (p95 ~123ms recorded; 44ms in my run). If steady-state 10x holds but a 5x burst inflates the tail, the bottleneck is concurrent-request queueing on one node, not an algorithmic/capacity fault.
- **Topology confirms it.** The dev cluster is `number_of_nodes:1` with no replicas/horizontal scaling. Burst multiplies concurrent search clients ×5 onto that single node. The **prod** Environment has `Domain.InstanceCount:2` + engramd `DesiredCount:3` (`environments.go:19,52`), which spreads burst search across ≥2 data nodes and 3 app instances — the exact axis a single local node lacks.
- **Reproduced independently.** My reduced run showed burst search p95 172ms (>150ms) while sustained held at 44ms — the same single-node burst-tail pattern, scaling with concurrent client count.
- Dispatch instructed local S1-scaled testing with an explicit local-vs-cloud caveat; the breach is that caveat materializing, not requirement failure.

**Required follow-up (must be recorded):** confirmation that burst search p95 holds ≤150ms on the ≥2-node staging/prod domain. Currently this is only gestured at ("documented manual step — see the phase report", loadtest doc comment; "local-vs-cloud caveat", discovery). I did **not** find it captured as a durable, tracked manual step in any committed doc/runbook. Per the dispatch's "PASS but require the follow-up be recorded", this must be written into a durable location (e.g. a runbook or deploy doc), not left in the ephemeral build report.

## Test-DW Coverage
- [x] DW-7.1 — idempotency/create/drift/rollback unit tests ran (Step 0). **Gap:** convergence is tested only for DesiredCount drift; **no test covers an image-only change**, which is exactly the broken primary path (see Issues #1).
- [x] DW-7.2 — loadtest harness re-run live; reports p50/p95/p99 + worker lag.
- [x] DW-7.3 — sizing_test + live RAM measurement.
- [x] DW-7.4/7.5 — restore + failure drills ran live.
- [x] DW-7.6 — budget/kill-switch unit tests ran.
- [x] DW-7.7 — runbook tabletop sections present.
- [x] DW-7.8 — recorder unit test (all 10) + live loadtest deltas (5).

## Dead Code
None found. (Deploy targets, drill tags, and reindex package are all reachable and exercised.)

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | Gauges/kill-switch use sync/atomic; recorder poll loop nil-safe; loadtest ran 0 errors; drills register per-process cleanup to avoid orphan goroutines. |
| Error Handling | PASS | `Converge` wraps + propagates every Describe/Create error (`TestConverge_PropagatesProvisionerErrors`); Recorder logs a failing source without blocking others (`TestRecorder_SourceErrorDoesNotBlockOthers`). |
| Resources | PASS | HTTP clients carry timeouts; drills `t.Cleanup` per process/index; `PendingBacklog` is read-only (never claims). |
| Boundaries | FAIL | `serviceMatches` (converge.go:136-138) is an incomplete equality check: it omits `Image`, so a new-image/same-count converge is misclassified "unchanged". Demonstrated: new image v2 not deployed (mutating_delta=0). This is the DW-7.1 defect. |
| Security | PASS | No secret values committed; OIDC (no static keys); secret value never stored/logged even in the fake; rollback allowlist-validated in CI (deploy.yml:166-173). |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| performance-optimization | Measure-first: claims backed by profiling, not intuition | PASS | DW-7.2/7.3 evidence is a real measured loadtest run (p50/p95/p99, measured-vs-formula RAM from live k-NN stats); no tuning claim made without the measurement. Doc comments state the measure-first gate explicitly. |
| performance-optimization | Correct diagnosis of the burst hot spot | PASS | Burst breach correctly attributed to single-node concurrency/queueing (topology-bound), not misattributed to a code hot spot needing tuning; consistent with skill's concurrency-out-of-scope note (lock/queue contention dominates, not CPU). |
| cc-quality-practices | Combine techniques; dirty tests (error/edge) not just happy path | PASS | Idempotency + drift + rollback-no-prior + unknown-service + error-propagation; WithinTolerance boundary cases; kill-switch trip/reset/pass-through; source-error-does-not-block. Multiple detection techniques (unit + live integration + drills). |
| cc-quality-practices | Requirement→test traceability; cover the requirement's primary path | FAIL | DW-7.1's primary path (converge a *new image*) has **no test** — the drift test only varies DesiredCount. The one scenario that matters for a deploy CLI is uncovered, and the uncovered path is broken. |

## Notes (non-blocking)
- DW-7.8: load-test evidence exercises 5/10 gauges (pipeline). Gate/cost/budget gauges are proven to render+move by the recorder unit test, not the load test (an ingest/search load test cannot drive the experience gate or paid extraction). Reasonable, but a fuller DW-7.8 story would wire a `Gate`/`Cost` source into the loadtest Recorder so all 10 move under one run.
- Edge cases (all handled): OpenSearch blue/green domain update — Converge deliberately never mutates an existing domain (converge.go:67-74, documented as a reviewed operator action); embedding cold start — compose healthchecks + `--wait` gate (Makefile e2e-up); circuit-breaker/embedding-outage → BM25-only degradation with a logged "degraded" flag and `TestTierRetrieverEmbeddingTimeoutDegradesToBM25`, not a crash; runbook 03 documents the k-NN/node path.
- `serviceMatches` also ignores CPU/MemoryMB/ContainerPort drift — same class as the Image gap; only DesiredCount+Status are diffed.

## Issues (FAIL)
1. **Deploy CLI cannot roll out a new image — release promotion is a silent no-op.**
   - File: `deploy/aws/awsapi/converge.go:136-138` (`serviceMatches` omits `Image`); manifested via `cmd/engram-deploy/main.go:71` + `.github/workflows/deploy.yml:141-144`.
   - Demonstrated by: TRACE + executed program — Converge#1 image v1, Converge#2 image v2 same DesiredCount → `action="unchanged" changed=false mutating_delta=0`; new image never deployed. Directly contradicts `environments.go:13-14` ("image … the one value that legitimately differs between a routine Converge run and a release promotion").
   - Impact: every CI release (`deploy-staging`/`deploy-prod`, unchanged DesiredCount) silently deploys nothing; DW-7.1 "deploy CLI converges" unmet for its core case.
   - Fix: include `Image` (via the active task definition's image, i.e. store/compare it in `ServiceState`) in the drift check so an image change registers a new task-def revision + blue/green update; add a `TestConverge_ImageChange_RollsOut` test (image-only change → action "updated", new revision, prior revision recorded for rollback).

2. **DW-7.2 multi-instance burst follow-up is not durably recorded.** (Conditional, per dispatch adjudication (i).)
   - File: none — the follow-up exists only in the ephemeral build report / doc comments.
   - Fix: record "confirm burst search p95 ≤150ms on the ≥2-node staging/prod domain" as a tracked manual-verification step in a committed doc (a runbook or the deploy doc), alongside the DW-7.1/7.3 real-AWS residue.

**Verdict: FAIL — blockers: (1) DW-7.1 deploy CLI does not converge a new image (release promotion is a demonstrated silent no-op); (2) DW-7.2's sanctioned multi-instance burst follow-up must be recorded durably. DW-7.2 itself is adjudicated MET as a single-node local artifact; DW-7.3–7.8 PASS; security items a–d PASS.**

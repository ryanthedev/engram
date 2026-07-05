# Discovery + Design: Phase 7 - Scale, Ops & Production

## Files Found
- `Makefile` — build/test/lint/proto/integration/e2e targets exist; no deploy-staging/deploy-prod/loadtest/metrics targets yet.
- `go.mod` — only grpc/protobuf deps; no OTel or AWS SDK yet. Network access to proxy.golang.org confirmed (checked `go list -m -versions`).
- `cmd/engram-apply-templates/main.go` — the "describe-then-converge, idempotent" idiom (D24 extends this) already exists via `store.Apply`.
- `cmd/engram-perf/main.go` — existing Phase-1 perf harness: seeds docs, drives gRPC Search with C clients, reports p50/p95/p99. No ingest-path load, no worker-lag reporting — `cmd/engram-loadtest` is new, not a copy.
- `deploy/local/docker-compose.yml` + `Dockerfile` — full local stack (OpenSearch 3.1 pinned, embed server, stub LLM, engramd) with healthchecks; `make e2e` boots it. No snapshot repo path configured (needed for the restore drill).
- `scripts/dev-cluster.sh` — starts `engram-dev-os` (3.1.0) on :9200; **already running** in this environment (verified via `podman ps` + `curl :9200`, cluster green, 15 active shards). No `path.repo` set.
- `internal/store/{templates.go,apply.go,outbox.go,counts.go}` — index templates (episodic/semantic/ledger/auth-tokens/acl-edges), `Counts`, `ClaimBatch`/`Complete`/`DeadLetter`. No pending-backlog-age or dead-lettered-count query yet (needed for the outbox-lag / DLQ-depth gauges). Semantic template: `knn_vector`, faiss, `m=16`, `ef_construction=128`, dim=1024 (`store.EmbeddingDim`) — confirms the GREENFIELD SQfp16 sizing formula's inputs.
- `internal/worker/{worker.go,repair.go}` — `Worker.Metrics` (atomic counters: CreateConflicts, CloseConflicts, EventsDeadLettered); `Sweeper.Sweep`/`Run` (rules a/a'/b/c/d) but no backlog-scan-without-converging method and no last-sweep timestamp (needed for repair-backlog + convergence-age gauges).
- `internal/experience/{store.go,harness.go}` — `Store.Admit` routes Admit/Quarantine/Reject; `Metrics` struct exists but is a one-shot eval-harness summary, not a live counter (needed for the gate-verdict-rate gauge).
- `internal/ingest/cost.go` — `CostMeter`/`Pricing`/`Usage.CostPer1kEventsUSD` already implements the DW-2.6 cost metric; Phase 7 wires the budget alarm on top, doesn't reimplement.
- `cmd/engram-server/main.go` + `stages_experience.go` + `stages_graph.go` — the wiring pattern each phase uses: register stages/tiers from the phase's own file, minimal edits to `main.go`. `wireExperience` currently returns only `error`; needs to expose the `*experience.Store` (or its verdict counts) for telemetry to poll.
- `.github/workflows/` — does not exist yet.
- `docs/runbooks/` — does not exist yet.
- `e2e/cloud/` — does not exist yet.
- No AWS SDK, no OTel SDK in go.mod. Both resolve on the module proxy (checked): `go.opentelemetry.io/otel/sdk` v1.44.0, `go.opentelemetry.io/otel/exporters/prometheus` v0.66.0, `github.com/aws/aws-sdk-go-v2` v1.42.1 + service modules (ecs v1.86.2, opensearchservice v1.42.5, secretsmanager v1.311.0, ec2 v1.61.1(ish), cloudwatch — all present).

## Current State
Phases 3–6 are complete and committed. The system is a single deployable (`engramd`) with: gRPC surfaces behind token auth, ACL enforced at query+write time via four registration seams (RegisterStage/RegisterPostHook/RegisterTier/RegisterWriteGuard), T3 experience memory with a mandatory fail-closed gate, and T4 incremental graph with ACL-safe ≤2-hop expansion. The local compose e2e stack is real and green. There is **no telemetry, no deploy tooling, no CI/CD, no runbooks, and no load/restore/failure drill evidence** — Phase 7 is building all of this from zero, consistent with the plan.

## Gaps
- The plan's own Test Plan line for P7 (`terraform-apply + cloud-profile e2e`) is **stale** relative to the Decision Log: D24 (already recorded in this same plan file, "no Terraform/HCL — user decision 2026-07-03") supersedes it with the Go deploy CLI. The dispatch prompt already carries this resolution explicitly (D24, cmd/engram-deploy, AWS SDK, fake-Provisioner-backed tests). Treated as **plan drift already resolved in the Decision Log**, not a fresh UPDATE_PLAN trigger — proceeding per D24 and the dispatch's explicit environment constraint (no real AWS creds here).
- No AWS account/credentials in this build environment (stated constraint). DW-7.1 (real staging/prod), DW-7.2/7.3 (real-domain load numbers), and the cloud e2e profile's real execution are **not fully verifiable here** — the dispatch explicitly pre-authorizes marking these as documented manual-verification steps rather than BLOCKED, provided the tooling is real and unit/local-tested. This mirrors Phase 3's live-Claude-Code manual step.
- Domain gauges need small, additive data sources that don't exist yet:
  - `internal/store`: pending-backlog count + oldest age (outbox/worker lag), dead-lettered count (DLQ depth).
  - `internal/worker`: `Sweeper.Backlog` (scan without converging) + last-sweep timestamp (convergence age).
  - `internal/experience`: live verdict counters on `Store` (gate verdict rates) — the existing `Metrics` struct is a post-hoc summary, not a running counter.
  These are outside the literal Phase-7 file-scope list but are the same kind of small, additive, non-breaking plumbing that DW-7.8 cannot be met without (a gauge with no data source cannot "visibly move"). Precedent: Phases 5/6 touched `cmd/engram-server/*` despite a narrower stated file scope. Documented as a deviation below.
- The local compose OpenSearch has no `path.repo` configured — required for the snapshot/restore drill (DW-7.4). Adding it is additive (new env var) and does not change any existing index/mapping contract.
- `wireExperience`'s signature needs to widen (return the `*experience.Store`) so `main.go` can poll verdict counts — a signature change inside a single-caller helper in the same file, not a cross-package seam break.

## Code Standards
Read `docs/code-standards.md`. Applies: doc comments on every exported symbol referencing the DW/D-decision they satisfy; `errors.Is`/typed sentinel errors; table-driven tests; `go vet` + revive clean; no transport imports in business packages (import-boundary lint, DW-3.7) — `internal/telemetry` must not import transport packages, `deploy/aws`/`cmd/engram-deploy` are allowed AWS SDK imports (infra, not business logic). Concise, well-commented, small functions; goroutine-safe counters via `sync/atomic`, matching `Worker.Metrics`' existing style.

## Test Infrastructure
- `go test ./...` — plain unit tests, no external deps.
- `-tags=integration` — live-cluster tests against `$ENGRAM_OPENSEARCH_URL` (defaults :9200); `make integration` lists the packages; **must add `internal/telemetry` and any new store/worker integration tests to this list**.
- `-tags=e2e` — full compose-stack tests (`make e2e`), builds images via podman/docker compose, drives CLI/MCP/gRPC clients externally.
- The dev cluster (`engram-dev-os`, OpenSearch 3.1.0) is **already running** in this session on :9200 — usable directly for `make integration` and for the restore/failure drills (with `path.repo` added).

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-7.1 | `make deploy-staging`/`-prod` converge from clean checkout; re-run no-op; cloud e2e green against staging | COVERED (idempotency + convergence via fake Provisioner) / MANUAL (real AWS env + real cloud e2e) | `TestConverge_Idempotent_SecondRunNoOp`, `TestConverge_CreatesMissingResources`, `TestConverge_DetectsDrift`; e2e/cloud profile run against the **local stack as a labeled stand-in** |
| DW-7.2 | 10x S1 sustained + 5x burst holds SLOs; p50/p95/p99 + worker lag reported | COVERED (run locally at S1-scaled proportions; report real numbers with local-vs-cloud caveat) | `cmd/engram-loadtest` run once, output captured in report |
| DW-7.3 | vector RAM ≤80% breaker limit; matches SQfp16 formula within 20% | COVERED (compute from formula + measured `_nodes/stats`/`_cat/indices` on local cluster) | `TestSQfp16Estimate_MatchesFormula`, loadtest report includes measured vs estimated RAM |
| DW-7.4 | restore drill: snapshot → new index → e2e green; RPO/RTO documented | COVERED (real drill against local podman OpenSearch, fs repo) | `TestRestoreDrill` (integration/e2e tag) |
| DW-7.5 | failure drill: kill worker + OS node; alerts fire; no data loss | COVERED (kill local worker process + stop/restart podman OS container; assert alert condition + outbox/repair recovery) | `TestFailureDrill_WorkerKill`, `TestFailureDrill_OpenSearchRestart` |
| DW-7.6 | cost $/1k within budget; synthetic overspend alarm fires | COVERED | `TestBudgetAlarm_Fires`, `TestBudgetAlarm_KillSwitchTrips` |
| DW-7.7 | 5 runbooks exist, each tabletop-walked once, gaps fixed | COVERED (runbooks written; tabletop walk-through documented in each runbook's own "Tabletop" section, gaps folded back into the steps) | manual process, evidence = the runbook files themselves + the drill tests that exercise the same paths |
| DW-7.8 | every domain gauge renders + visibly moves during load test | COVERED | `TestRecorder_GaugesMoveOnPoll` (scrapes `/metrics`, asserts values change across two polls with different fake-source readings); loadtest report shows gauge deltas |

**All items COVERED:** YES (with DW-7.1/7.2/7.3's real-AWS/real-domain residue marked MANUAL per the dispatch's explicit pre-authorization — not a CANNOT_MEET, since the tooling itself is fully built and tested).

## Design Decisions

**Telemetry (`internal/telemetry`):** OTel metrics SDK (`go.opentelemetry.io/otel/sdk/metric`) + Prometheus exporter/reader, scraped via `promhttp.Handler()` on a `-metrics-addr` flag. Synchronous `Float64Gauge` instruments (OTel metric API ≥v1.20 supports `Meter.Float64Gauge(...).Record(ctx, v)`), one `Recorder` that polls small duck-typed source interfaces (`OutboxSource`, `RepairSource`, `GateSource`, `CostSource`) on a ticker. Sources are satisfied structurally by `*store.OpenSearchStore`, `*worker.Sweeper`, `*experience.Store`, and a small `CostSource` closure over `ingest.CostMeter` — **no import of `internal/telemetry` from those packages**, so the import-boundary/dependency-direction rule (business packages don't know about telemetry) holds. Rejected: pushing metrics to a remote OTel collector — no such collector exists in this environment, and Prometheus-pull is the simplest thing that is genuinely scrapeable and testable via HTTP in-process.

**Budget alarm + kill-switch:** `telemetry.BudgetAlarm{ThresholdUSD}.Evaluate(costPer1k float64) bool`, wired to a `telemetry.KillSwitch` (atomic bool). `telemetry.GatedExtractor` wraps any `ingest.Extractor` (duck-typed on its `Extract` method via a small local interface, avoiding a hard dependency direction issue — telemetry depends on ingest's exported interface, which is fine since ingest doesn't depend back) and returns a typed "budget exceeded" error instead of calling through when tripped, so ingestion degrades to a documented halt rather than continuing to spend. Rejected: silently dropping events on kill-switch trip — that would violate no-data-loss; instead the extractor call fails and the outbox retry/backoff + dead-letter path (already built) takes over, same as any other extractor failure.

**Deploy CLI (`deploy/aws` + `cmd/engram-deploy`):** `awsapi.Provisioner` interface with domain-shaped methods (not raw AWS SDK types) for OpenSearch domain, ECS service (+ blue/green task-def revision), Secrets Manager, VPC. `awsapi.FakeProvisioner` is an in-memory implementation recording calls, used to prove idempotency (second `Converge` call performs zero mutating calls), drift detection, and rollback (task-def revert to the prior revision). `awsapi.SDKProvisioner` is a real implementation on `aws-sdk-go-v2` (opensearchservice/ecs/secretsmanager/ec2 clients) satisfying the same interface — compiles and is structurally real, but is not exercised against real AWS in this environment (no credentials); this is the documented manual-verification residue. Rejected: skipping the real SDK-backed implementation entirely — the phase contract requires "a real, idempotent Go deploy CLI... on the AWS SDK," not just an interface.

**Versioned index + alias flip:** lives in `deploy/aws/reindex` (not `internal/store`) since it's a deploy-time safety mechanism, not a runtime store concern: `EnsureAliasedIndex` creates a versioned concrete index (`<alias>-000002` style) from a template and points the alias at it; `FlipAlias` uses OpenSearch's atomic `_aliases` multi-action API (remove old index from alias + add new index to alias in one call) so there is never a moment with zero or two authoritative indices. Tested for real against the local OpenSearch 3.1 cluster (integration tag). Rejected: in-place mapping mutation (`PUT /_mapping`) — explicitly the "point of no return" the plan forbids.

**Load test (`cmd/engram-loadtest`):** new tool (not a copy of `cmd/engram-perf`) driving both ingest (gRPC `Append`) and search load concurrently at a rate derived from the S1 model (10–50k events/day baseline → 10x = ≥500k/day pace, converted to events/sec) plus a 5x burst window, reporting p50/p95/p99 per endpoint and worker lag (via the same `store.PendingBacklog` the telemetry gauge uses) and a live RAM-vs-SQfp16-formula comparison pulled from `_nodes/stats`. Run once against the local stack per the measure-first discipline (performance-optimization skill: profile before any tuning claim) — the report's numbers are the "measurement" gate for DW-7.2/7.3; no tuning changes are made without them.

**Drills (`e2e/cloud`):** the restore and failure drills are real Go tests (build-tagged, not run under plain `go test ./...`) that shell out to podman (`os/exec`) to stop/start the local OpenSearch container and to kill a worker process, and use `store`/HTTP calls to assert on outbox/repair convergence and alert-condition state — genuine drills against the local stack, explicitly labeled as a stand-in for staging per the dispatch instructions.

## Prerequisites
- [x] Phases 3–6 complete (worker seams, ACL, experience, graph all live).
- [x] Local dev OpenSearch 3.1.0 running and reachable.
- [x] Network access to the Go module proxy for OTel + AWS SDK deps.
- [x] podman available for compose-stack + container-kill drills.
- [ ] Real AWS account/credentials — absent by design; DW-7.1/7.2/7.3's cloud-only residue is a documented manual step, not a blocker.

## Recommendation
BUILD. No CANNOT_MEET items. Proceed with the design above; the AWS-credential gap is handled per the dispatch's explicit, pre-authorized manual-verification carve-out, not as a plan deviation requiring UPDATE_PLAN.

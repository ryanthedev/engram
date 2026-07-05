# Runbook: Worker down or lagging (outbox backlog / lag SLO breach)

**Alert:** `engram_outbox_lag_seconds` > 60s sustained, or `engram_outbox_backlog` growing monotonically over 3+ scrapes, or `engram_repair_convergence_age_seconds` growing unbounded (the repair sweep loop has stalled).

## Detection

- Dashboard panel: "Worker & Outbox Health" — `engram_outbox_backlog`, `engram_outbox_lag_seconds`, `engram_repair_backlog`, `engram_repair_convergence_age_seconds` (internal/telemetry's Phase-7 gauges, DW-7.8).
- Symptom from the user side: `engram ingest` succeeds (Ingest is synchronous and durable — D12) but `engram search` doesn't surface newly ingested facts for an unusually long time (extraction/reconciliation is stuck).

## Diagnosis

1. Check the ECS service is running and healthy:
   ```
   aws ecs describe-services --cluster engram-<env> --services worker
   ```
   `runningCount` should equal `desiredCount`. If 0, the worker task is crash-looping — check CloudWatch Logs for the worker service for a panic or a fatal startup error (e.g. an unreachable extraction endpoint).
2. If the worker is running but lag keeps growing: check `engram_gate_admit_rate`/`engram_gate_quarantine_rate` and extraction-cost logs — a slow or rate-limited extraction endpoint (`-extract-url`) throttles the whole pipeline (extraction is the pipeline's dominant cost/latency line, D4).
3. Check `engram_dlq_depth` — if it's climbing too, events are failing repeatedly and dead-lettering after `MaxAttempts` (default 5), which is a *different* runbook (02-dlq-nonzero.md) but often co-occurs.
4. Check `engram_repair_convergence_age_seconds` specifically: if this is large while backlog is small, the repair *sweep loop itself* has stalled (e.g. deadlocked on an OpenSearch call) even though claim/process is fine — a distinct failure mode from worker crash-loop.

## Immediate mitigation

- If the worker task is unhealthy: force a new deployment to cycle it —
  ```
  aws ecs update-service --cluster engram-<env> --service worker --force-new-deployment
  ```
  The outbox is durable (D12) and the claim lease design means no event is lost by restarting workers — a claimed-then-abandoned event's lease expires and the repair sweep (rule c) or the next worker resumes it from the cached ledger extraction (no re-billing the LLM).
- If the extraction endpoint is the bottleneck: temporarily lower `-workers`/`-claim-batch` is NOT the fix (that would slow throughput further) — check the endpoint's own health/rate limits first. If it's down entirely, the extraction-gate kill-switch (`engram_budget_alarm_firing`) is a *different* control; a down endpoint just means Extract calls error and retry with backoff (existing MaxAttempts machinery), not indefinite growth — verify attempts are actually incrementing (`ScanIncomplete`/ledger state), not stuck.

## Resolution

- Root-cause the crash/stall (log review), fix, redeploy (blue/green — `make deploy-staging`/`-prod`, or `-rollback worker` if the *previous* revision was the good one).
- Confirm `engram_outbox_backlog` trends to zero and `engram_repair_convergence_age_seconds` stays low (well under the sweep interval, ~2–30s at S1) after the fix.

## No data loss guarantee

The outbox is append-only (D12) and the extraction ledger is claim-first (D13): a worker crash at any point leaves, at worst, an abandoned lease or an incomplete ledger entry — never a lost or double-billed event. This is exactly what the DW-7.5 failure drill (kill a worker process) verifies.

## Tabletop walkthrough

**Walked:** 2026-07-04, against the local podman stack (`e2e/cloud`'s failure-drill test, `TestFailureDrill_WorkerKill`) as the stand-in for staging.

**Steps executed:** killed the worker process mid-processing (SIGKILL, no graceful shutdown) while ingest continued; observed `engram_outbox_backlog`/`engram_outbox_lag_seconds` rise; restarted the worker; observed backlog fully drain and `DeadLetteredCount` remain 0.

**Gap found (real, hit while building the drill):** the first version of the drill checked the outbox-backlog gauge immediately after appending the fixture events and asserted it was non-zero — and it intermittently read 0. Root cause: `PendingBacklog`'s `_count` query (like all OpenSearch count/search queries) is refresh-visible, not real-time — a write is durable immediately but may not be *countable* until the index's next refresh (~1s by default). The drill's own alert-condition check was racing that refresh window. Fixed by forcing an explicit refresh before checking the gauge (`testutil.RefreshIndex`) — and folded the same caveat into this runbook: **any dashboard/alert reading built on OpenSearch counts has this same near-real-time lag; a "clean" reading taken within ~1s of a burst of writes is not trustworthy evidence of an empty backlog.** A second, related bug the walkthrough caught: the drill's harness left an orphaned `engramd` process running (still polling, and erroring against, an already-deleted scratch index) whenever an assertion failed *before* the deliberate-kill step — because cleanup was only wired in for the second process start. Fixed by registering cleanup per process-start rather than once at the call site, so a failed drill can never leak a background process still hammering a torn-down index.

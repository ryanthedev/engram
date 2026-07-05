# Runbook: OpenSearch node failure / cluster not green

**Alert:** ingest availability SLO breach (Ingest RPCs erroring/timing out), or a direct cluster-health alert (`_cluster/health` status `yellow`/`red`).

## Detection

- Dashboard: RED metrics (rate/errors/duration) on the Ingest/Search RPCs will show elevated error rate and latency.
- Direct check: `GET /_cluster/health` — `status` field (`green`/`yellow`/`red`), `unassigned_shards` count.
- `engram status` (CLI) / the Status RPC reports `healthy: false` if the count probe itself fails.

## Diagnosis

1. `GET /_cluster/health?level=indices` to see which index(es) have unassigned or initializing shards.
2. Check the OpenSearch Service domain's node status in the AWS console/CLI (`aws opensearch describe-domain --domain-name engram-<env>`) — is a data node down, or is this a planned blue/green domain update in progress?
3. Distinguish **yellow** (a replica is missing but a primary is intact — degraded but serving) from **red** (a primary shard is unavailable — some data is genuinely unreachable until it recovers).

## Immediate mitigation

- **Yellow:** no immediate data-loss risk (S1 runs with a replica per D14/D15's headroom sizing) — monitor for the cluster to self-heal (shard reallocation) as the domain's healthy nodes pick up the missing replica. No manual action needed unless it persists beyond ~10 minutes.
- **Red:** this is the circuit-breaker/degraded-mode scenario the phase's edge cases call out. If search on the affected index is failing entirely, flip the documented degradation flag (BM25-only, bypassing the kNN clause) so the service stays partially available rather than fully down — this is a deliberate, documented degradation, not a silent one:
  ```
  # operator action: restart engramd with the BM25-only flag if native
  # memory circuit-breaker trips (see internal/retrieval's ModeHybrid vs a
  # BM25-only mode) — the retrieval package's Option surface is the seam.
  ```
- If the failure is domain-wide (all nodes down), this is the restore-drill scenario — see runbook 05 (restore from snapshot) for the recovery path, and confirm the outbox/repair sweep have queued rather than lost any writes attempted during the outage (D12's durability guarantee: Ingest fails loudly to the client on an OpenSearch outage — it does not silently drop writes, so client-side retry is the correct behavior, not a gap).

## Resolution

- Once the domain reports `green` again: verify `make apply-templates`-equivalent (the cluster contract, `store.Apply`) is still satisfied (it's idempotent — safe to re-run) and that the repair sweep converges any writes that were mid-flight during the outage.
- Confirm dashboards show worker lag and DLQ depth returning to baseline post-recovery — an OpenSearch outage often produces a backlog spike that should drain, not persist.

## Rollback / point-of-no-return note

A domain-level OpenSearch version or major config change is its own blue/green operation with real risk; Converge (cmd/engram-deploy) deliberately never auto-mutates an existing domain's spec (see `deploy/aws/awsapi/converge.go`'s comment) — any such change is a reviewed, manual operator action, not something this runbook or Converge does automatically.

## Tabletop walkthrough

**Walked:** 2026-07-04, against the local dev cluster (`TestFailureDrill_OpenSearchNodeRestart` in `e2e/cloud`): durably appended one event, stopped the `engram-dev-os` container, confirmed an Append call made during the outage fails (rather than hanging or silently succeeding), restarted the container, and confirmed the pre-outage event is still present and intact once the cluster is healthy again — the no-data-loss half of DW-7.5.

**Gap found (real, hit while building the drill):** the drill's first version considered the cluster "back" as soon as the root `/` endpoint returned HTTP 200 after restart — and intermittently failed the "event still present" check right after. Root cause: a single-node OpenSearch reports its HTTP endpoint up before shard recovery finishes, so a doc read immediately afterward can race recovery and 404 even though nothing was actually lost. Fixed by waiting on `_cluster/health?wait_for_status=yellow` instead of the plain root endpoint — a distinction worth keeping in this runbook's own recovery-confirmation step, not just the drill: **"the endpoint answers" is not the same signal as "the cluster is healthy," and treating them as equivalent this runbook would have led an operator to declare recovery prematurely.**

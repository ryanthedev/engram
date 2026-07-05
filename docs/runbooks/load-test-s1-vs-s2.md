# Load test: S1 local result vs S2 staging follow-up (DW-7.2)

This records the measured Phase-7 load-test result and the required
pre-production follow-up, so the finding lives in the repo rather than only
in an ephemeral build report.

## What was run

`cmd/engram-loadtest` (`make loadtest`) against the **local single-node**
pinned-3.1 OpenSearch dev cluster: 100,000 seeded episodic docs, then a
90s **sustained** phase at the 10x-S1 pace (>=500k events/day; ~5.8 ev/s)
with 8 concurrent search clients, then a 20s **burst** phase at 5x that
(~29 ev/s) with 40 concurrent search clients. Measure-first, no tuning
applied — the numbers below are the measurement, not a tuned result.

## Result

| Phase | Ingest p95 / p99 | Search p95 / p99 | Errors | Worker lag |
|---|---|---|---|---|
| Sustained (90s, 10x S1) | 35 ms / 58 ms | **123 ms / 155 ms** | 0 | bounded, drained to 0, 0 dead-lettered |
| Burst (20s, 5x sustained, 40 search clients) | 101 ms / 150 ms | **539 ms / 591 ms** | 0 | max backlog 396, drained, 0 dead-lettered |

Vector RAM at 100k docs: measured 220 MB vs SQfp16-formula estimate 239 MB
(within 20%), circuit-breaker usage 14.6% (well under the 80% ceiling,
DW-7.3). Domain gauges moved as expected during the run (DW-7.8):
`engram_outbox_backlog` 0 -> 377, `engram_outbox_lag_seconds` 0 -> 16.3s.

## Interpretation

- **Sustained (the DW-7.2 headline pace) HOLDS the search SLO** (p95 123 ms
  <= 150 ms) on a single node.
- **Burst BREACHES the search SLO** (p95 539 ms vs the 150 ms base /
  250 ms expanded ceiling) — but only under 40 concurrent search clients
  hammering **one** OpenSearch node with no replica to spread read load.
  Availability held throughout (0 errors, no data loss).

The burst breach is assessed as a **single-node local artifact**, not a
design defect: search read throughput is exactly what horizontal scale
(replica shards across multiple data nodes, ECS search-service replicas)
addresses, and the deploy environments (`cmd/engram-deploy`) provision a
multi-node domain (staging `InstanceCount` >= 1, prod >= 2) with replicas.

## Required follow-up (pre-production)

**This is a gate before relying on the system in production, not an optional
extra.** Re-run the same load test — same seed size, same 10x sustained +
5x burst shape — against the **multi-instance staging domain** (D22:
staging is topology-identical to prod, scaled down) and confirm the burst
phase's search p95 comes back within the 150 ms / 250 ms SLO. If it does
not, the fix is measured capacity (more replicas / larger instances /
search-service scale-out), decided from that staging measurement — again,
measure-first, no speculative tuning.

Until that staging run is recorded, the burst SLO is **confirmed only for
the single-node local case, where it breaches** — the production claim is
open.

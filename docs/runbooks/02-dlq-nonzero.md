# Runbook: DLQ depth > 0 (poisoned or permanently failing events)

**Alert:** `engram_dlq_depth` > 0 (any dead-lettered event is an alertable condition — S1 volume is low enough that zero is the expected steady state).

## Detection

- Dashboard panel: "Worker & Outbox Health" — `engram_dlq_depth` gauge (DW-7.8).
- An event lands in the DLQ only after `MaxAttempts` (default 5) failed processing attempts, each backed off by the claim lease — so a DLQ hit means the SAME event failed repeatedly, not a one-off transient blip.

## Diagnosis

1. Find the dead-lettered doc(s):
   ```
   POST /engram-episodic-*/_search
   { "query": { "term": { "dead_lettered": true } } }
   ```
   The `dead_letter_reason` field (set by `DeadLetter`, internal/store/outbox.go) carries the last failure's message.
2. Classify the reason:
   - **Malformed/oversized input** (e.g. extraction schema violation the extractor can't parse) — a genuinely poisoned event; safe to leave dead-lettered.
   - **Extraction endpoint error** (5xx, timeout) that persisted across all 5 attempts — an infrastructure issue, not a bad event; see runbook 01 first (this is often a symptom of a prolonged extraction-endpoint outage, not a per-event problem).
   - **Ledger/reconciliation bug** (a genuine defect in the extraction or reconciliation logic) — the highest-severity case; requires an engineering fix, not just an ops response.

## Immediate mitigation

- Dead-lettered events are OUT of the outbox scan permanently (`ClaimBatch` excludes `dead_lettered: true`) — they will not retry on their own and will not further block the pipeline. No immediate action is required to protect pipeline health; the alert is about NOT losing track of a failed event, not an active incident by itself.
- If the reason indicates a transient infra issue that's now resolved (extraction endpoint back up): the event can be manually re-queued by clearing `dead_lettered`/`dead_letter_reason` and resetting `claim_lease_until`/`attempts` via a scoped `_update_by_query` — this is a deliberate manual action (never automated), logged in the incident record.

## Resolution

- If a genuine defect caused the failures: fix and ship it, then decide per-event whether to re-queue (only if the fix actually addresses that event's specific failure — re-queuing into an unfixed bug just re-poisons the DLQ).
- Track DLQ depth trend post-fix: it should stay flat (no new entries), confirming the root cause and not just this one event are addressed.

## No data loss guarantee

Dead-lettering marks a doc, it never deletes it (D3's append/invalidate discipline extends here) — a dead-lettered episodic event is fully recoverable and auditable; nothing is silently dropped.

## Tabletop walkthrough

**Walked:** 2026-07-04, in two parts, both real and passing: (1) `internal/worker`'s pre-existing `TestDW_2_7_MalformedExtractionRejectedNotIndexed` (Phase 2) — a malformed event fails extraction repeatedly and is confirmed dead-lettered at exactly `MaxAttempts`, with the `EventsDeadLettered` metric incrementing; (2) this phase's new `TestDW_7_8_PendingBacklogAndDeadLetteredCount` (`internal/store`, live-cluster integration test) — confirms `DeadLetteredCount` reflects a `DeadLetter` call's effect against the real pinned-3.1 cluster and that the event simultaneously drops out of `PendingBacklog`.

**Gap found:** neither test exercises the on-call *diagnosis* step (finding the specific dead-lettered doc and reading its reason) — both prove the mechanism, not the runbook's step 1 query. Walking through step 1 by hand against the live dev cluster surfaced that the `dead_letter_reason` field is only set by the `DeadLetter` call path (worker.go), not by the store's own dead-letter write in isolation — worth calling out explicitly here so an operator doesn't expect a reason on a doc dead-lettered by a path that doesn't go through the worker's normal flow. No dashboard-vs-raw-query gap was actually found in this walkthrough (S1 has no OpenSearch Dashboards/Kibana deployed at all, so the raw `_search` query above was always the intended path, not a fallback discovered after the fact).

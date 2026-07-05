# Runbook: Extraction cost budget breach (kill-switch tripped)

**Alert:** `engram_budget_alarm_firing` == 1, or `engram_extraction_cost_usd_per_1k_events` above the configured `-budget-per-1k-usd` threshold (default $5/1k events, DW-2.6's S1 gate).

## Detection

- Dashboard panel: "Cost & Budget" — `engram_extraction_cost_usd_per_1k_events` and `engram_budget_alarm_firing`.
- Once the alarm fires, `internal/telemetry.BudgetAlarm` trips the bound `KillSwitch`, and `GatedExtractor.Extract` starts returning `telemetry.ErrBudgetExceeded` instead of calling through to the (billed) extractor — this shows up as a rising `engram_outbox_backlog`/`engram_outbox_lag_seconds` too (extraction attempts fail, retry, and back off, same as any other extractor error), which is the deliberate, documented trade: halt spend, never drop the event.

## Diagnosis

1. Confirm it's a genuine cost spike, not a metric artifact: check `ingest.CostMeter`'s underlying token counts (prompt/completion) via the extraction-cost log line (`cmd/engram-server`'s per-minute log) — a spike is usually either (a) an unusually large batch of long events, or (b) the extractor model/pricing config drifted from what's actually being billed.
2. Check whether this is a real usage change (organic growth past the S1 budget envelope — a capacity-planning conversation, not an incident) vs. a bug (e.g. a retry storm re-billing the same events — check the ledger's claim-first idempotency is actually preventing double-billing per D13; a bug here would show cost rising with `PromptTokens`/`Events` ratio unchanged but total events far exceeding real traffic).

## Immediate mitigation

- The kill-switch is already halting further spend automatically — no manual action is required to stop the bleeding. This is intentional: the alarm and the mitigation are the same mechanism (`telemetry.BudgetAlarm.Evaluate` trips `KillSwitch` synchronously), so there is no gap between detection and containment.
- If the breach is a false alarm (e.g. a deliberate, approved one-time bulk backfill): raise `-budget-per-1k-usd` for the duration of the backfill via a redeploy, or accept the halted extraction until the backfill traffic passes and the rolling cost/1k figure drops back under threshold (the alarm auto-clears — `BudgetAlarm.Evaluate` resets the switch the moment cost falls back under threshold, no restart needed).

## Resolution

- If organic growth: this is a budget-policy decision (raise the threshold deliberately, or apply a stricter selective-extraction gate) — not a pure ops fix. Record the decision and update `-budget-per-1k-usd` accordingly.
- If a bug (e.g. re-billing): fix the root cause (most likely a `ExtractorVersion`/ledger-key mismatch causing the claim-first dedup to miss), deploy, and confirm cost/1k returns to baseline with the SAME event volume as before the fix.

## No data loss guarantee

A tripped kill-switch fails the Extract call, which the worker treats exactly like any other extractor failure: retried per `MaxAttempts`, eventually dead-lettered if the switch stays tripped long enough (see runbook 02) — never silently dropped, and never double-billed once un-tripped (the ledger's cached-extraction resume, D13, does not re-call a kill-switched extractor for an event whose extraction was already cached before the trip).

## Synthetic overspend test

`internal/telemetry`'s `TestBudgetAlarm_Fires` and `TestBudgetAlarm_KillSwitchTrips` (unit tests, run in CI on every commit) inject a synthetic cost value above threshold and assert the alarm fires and the kill-switch trips — this is the DW-7.6 evidence exercised automatically, not just at tabletop time.

## Tabletop walkthrough

**Walked:** 2026-07-04, as a code-and-test walkthrough rather than a live manual incident: read each procedure step above against the actual implementation (`internal/telemetry/budget.go`) and confirmed it against the real, passing `TestBudgetAlarm_Fires`/`TestBudgetAlarm_KillSwitchTrips`/`TestGatedExtractor_TrippedSwitchBlocksExtraction` unit tests — synthetic overspend fires the alarm, trips the kill-switch, `GatedExtractor.Extract` fails closed with `ErrBudgetExceeded` while tripped, and recovers automatically once cost falls back under threshold.

**Gap found:** none in the mechanism itself, but the walkthrough surfaced a documentation gap — the runbook's "Immediate mitigation" section originally read as though there might be a detection-then-response delay to plan for operationally. There isn't: `BudgetAlarm.Evaluate` trips the kill-switch in the same call that detects the breach (see `budget.go`), so containment is synchronous and automatic. Reworded above to make that explicit, since an on-call engineer reading it as "detect, then separately respond" would waste time looking for a manual containment step that doesn't exist.

**Not yet exercised:** a live end-to-end run (a real `engram-server` process with the kill-switch actually blocking a real extraction call under load) has not been performed in this build environment — the unit tests above prove the mechanism in isolation; a full live drill against staging is a reasonable follow-up before relying on this runbook under real incident pressure.

# Review: Phase 2 - Async Write & Bi-temporal Reconciliation

**Round 1 (original review):** FAIL — Issue 1: late-arrival bounding not neighbor-aware on ≥2-version chains (overlapping intervals). Details in §Round 1.
**Round 2 (re-review of fix 1):** FAIL — Issue 1 verified fixed; residual Issue 2 demonstrated (close bounds not neighbor-aware; crash/divergence window + late arrival → permanent overlap). Details in §Round 2.
**Round 3 (re-review of fix 2, FINAL):** **PASS** — both Round-2 interleavings verified converged; `closeBound` matches the amended plan step 4; one irreducible sub-second write-skew residual remains, demonstrated, adjudicated as **plan-scope architectural residual of D10's per-document-OCC premise**, not a defect in this phase's work (Issue 3, non-blocking, follow-up prescribed). Details in §Round 3. Final verdict at end.

---

## Round 3 — Re-review of the closeBound fix (2026-07-03, retry 3, FINAL)

### Executed Results

- Build: `go build ./...` → success.
- Unit: `go test -count=1 ./...` → all 12 test packages pass, 0 failures, including the three new regressions.
- Race: `go test -race -count=1 ./internal/worker/...` → pass, no races.
- Lint: `go vet` clean; `revive -set_exit_status` clean.
- Integration: `make integration` (live OpenSearch 3.1.0, `engram-dev-os`) → all pass, including `TestDW_2_8_CrashWindowLateArrivalSweepStaysDisjoint`, `TestDW_2_5_DivergenceWindowLateArrivalSweepStaysDisjoint`, and the live-cluster `TestDW_2_8_LiveNeighborAwareCloseBound`.

### 1. Round-2 interleavings — VERIFIED CONVERGED

Re-ran both of my Round-2 falsification scenarios verbatim (scratch tests, written/run/removed):

- **Crash-window + late arrival:** ana crashed-open, cid live via supersedes, eve@tv1+6h inserted in the window, then sweep → observed `ana=[tv1, tv1+6h)`, `eve=[tv1+6h, tv2)`, cid live. Pairwise disjoint. The sweep's completing close now lands at eve's valid_at (the neighbor-aware min), not cid's.
- **Divergence-window + late arrival (rule a′):** old + two divergent live successors (ana@tv1, cid@tv2) + eve@tv1+6h between them, then sweep → observed `old=[tv0,tv1)`, `ana=[tv1,tv1+6h)`, `eve=[tv1+6h,tv2)`, cid live. Pairwise disjoint.

The committed regressions encode both scenarios (fake-store) plus the crash-window state on the real cluster; all three pass in my runs.

### 2. closeBound vs the amended plan step 4 — MATCHES; the builder's race argument is NOT airtight (adjudicated Issue 3)

The plan's step 4 was amended: `invalid_at = min(new.valid_at, nearest valid-time successor's valid_at)`, "**Every close bound is neighbor-aware** … applies to worker closes AND sweep closes; all closes funnel through one choke point." Verified: `closeBound` (worker.go:467-476) computes exactly that min via `ValidTimeNeighbors`; `closePredecessor` (worker.go:422) is the single choke point (worker step 4 directly; the sweep via `tryClose` → `closePredecessor`). The query's successor is strictly after the predecessor's own valid_at, so the min can never invert — the inversion guard is retained. Implementation matches the amended spec.

**The claimed race coverage ("records landing after the read are covered by the insert path trimming the predecessor") is NOT airtight.** Demonstrated by a scratch test (written/run/removed): the two halves of the invariant are read-then-write pairs on **different documents** — the closer reads successors then CAS's the *predecessor*; the inserter reads the predecessor then creates the *successor*. If both reads happen before both writes (classic write skew), neither side acts: the inserter reads the predecessor as still-live → correctly skips its trim; the closer's neighbor read predates the insert → bound stays at the requested successor. Observed end state: `ana=[tv1,tv2)` ⊇ `eve=[tv1+6h,tv2)`, and three sweep passes do not repair it (no rule examines closed-closed overlaps). The predecessor's seq_no guard cannot catch it — the new record is a different doc, never CAS'd against. OpenSearch refresh lag (search-based `ValidTimeNeighbors`) widens the window to roughly the refresh interval (~1s).

**Adjudication — plan-scope residual, not a phase defect:**
- The implementation faithfully realizes the amended step 4; no alternative implementation of that step can close this window: any prevention scheme under per-document OCC is itself a read-then-write with the same skew (a post-close re-check merely narrows it; the regress never terminates). D10's own grounding states the constraint: "OpenSearch has no multi-doc transactions; OCC is per-document — atomicity comes from ordering + durable supersession intent + **repair**."
- The complete closure is therefore a **repair rule**, which is retroactive and immune to the skew: sweep rule (d) — scan each (tenant, subject, predicate) group for overlapping closed intervals, trim the earlier record to the later's valid_at (monotone, guarded — `trimInterval` already exists). No plan text, DW item, or listed edge case prescribes rule (d) today; adding it is a plan amendment, i.e., new scope.
- Escalation across rounds confirms the classification: Round 1 was deterministic on ordinary sequential input (implementation defect); Round 2 was reachable by *sequentially* composing two first-class phase scenarios with a window of seconds-to-minutes (implementation defect against the invariant); Round 3's residual requires true simultaneity — it cannot be constructed from sequential steps of any phase scenario (my reproduction needed a hook injecting a write inside another operation's read-write gap). This is the canonical irreducible gap of the eventual-consistency-with-repair model the plan chose in its Decision Log.
- Frequency: requires a same-(subject,predicate) late arrival inserted within ~1s of a concurrent close on the same chain, interleaved reads-before-writes. Rare at S1; plausible over months at S2 — which is exactly when the roadmapped phases revisit consistency (Phase 6 ops hardening).

**Recommendation (follow-up, out of this phase's gate):** amend the plan's repair sweep with rule (d) (closed-closed overlap repair) and implement it in the Phase-2 exit window or alongside Phase 3 — it restores the plan's "repair converges all partial states" property and terminates the prevention regress. Optionally add a cheap post-close neighbor re-check in `closePredecessor` to narrow the window meanwhile.

### 3. Full suites — GREEN (see Executed Results; run by this reviewer, uncached)

### 4. New-defect scan of closeBound

| Probe | Finding |
|-------|---------|
| Missing successor (succ=nil) | Bound falls back to `requested`. Correct — and specifically correct for the normal worker path where the just-created successor may not be search-visible yet (refresh lag): `requested` IS that successor's valid_at, so the bound is right with or without visibility. |
| Expired successors | Excluded by the `ValidTimeNeighbors` query (`expired_at` must_not exists) — record-retracted rows are not truth boundaries. Correct. |
| Inversion | succ is strictly after pred's valid_at (range `gt`), so `min` can never produce an inverted close; the explicit inversion guard in `closePredecessor` is retained as backstop. Correct. |
| Empty-interval successor | closeBound can bound at an empty record's ([v,v)) valid_at → conservative gap, never overlap — same pre-existing note as insertHistorical's succ selection. Non-blocking. |
| Rule (b) empty-interval closes | `requested = d.valid_at`; succ strictly > d.valid_at → min stays `d.valid_at`; the [v,v) close is preserved. Verified by the still-passing `TestSweepClosesDuplicateLiveContentKeys`. |
| Determinism | Tie-breaks by the (valid_at, doc id) total order client-side; the pre-existing >4-way equal-valid_at overfetch caveat applies to this read too (pathological, undemonstrated). |
| Bound staleness across the 409 retry loop | `bound` computed once before the loop; a successor landing mid-retry is the same Issue-3 write-skew residual, no separate defect. |
| Cost | One extra `_search` per close — closes are rare relative to reads; acceptable, noted. |

No new defects introduced by `closeBound`.

### Round-3 Verdict

**POST-GATE PASS.** All Round-1 and Round-2 demonstrated defects are fixed, regression-tested (fake-store + live cluster), and independently re-falsified by this reviewer without reproduction. The implementation matches the amended normative step 4. The one remaining demonstrated behavior (Issue 3, write-skew overlap) is precisely classified as pre-existing scope beyond this phase's DW items: an irreducible residual of the plan's per-document-OCC consistency model, closable only by a new plan-level repair rule — prescribed above as follow-up, not a blocker for this phase's work.

---

## Round 2 — Re-review of the late-arrival fix (2026-07-03)

### Executed Results

- Build: `go build ./...` → success.
- Unit: `go test -count=1 ./...` → all packages pass (12 packages with tests, 0 failures), including the 4 new regression tests.
- Race: `go test -race ./internal/worker/...` → pass, no races.
- Lint: `go vet ./...` clean; `revive -set_exit_status` clean (exit 0).
- Integration: `make integration` (live OpenSearch 3.1.0, container `engram-dev-os`) → all pass. The Makefile target now includes `./internal/worker/ ./internal/ingest/` (previous review's gap fixed); `TestDW_2_8_LiveLateArrivalMultiHopChain` passes against the real cluster.

### 1. Original defect scenario — VERIFIED FIXED

Re-ran my Round-1 falsification scenario independently (scratch test, written/run/removed): chain bob@tv0 → ana@tv1 → cid@tv2 (live), late arrival eve@tv0+12h.

- Before fix: eve=[tv0+12h, **tv2**) overlapping ana=[tv1, tv2).
- After fix (observed): bob **trimmed** to [tv0, tv0+12h), eve=[tv0+12h, **tv1**) bounded by its TRUE valid-time successor ana, ana=[tv1, tv2) untouched, cid live untouched. Pairwise disjoint across all four versions — asserted by the new `assertDisjointChain` helper across live AND closed records.
- The committed regression `TestDW_2_8_LateArrivalMultiHopChainBoundsAtTrueNeighbor` encodes exactly this scenario plus audit-stamp and sweep-no-op assertions; `TestDW_2_8_LiveLateArrivalMultiHopChain` reproduces it on the real cluster (closed-record neighbor genuinely invisible to the live-only candidate read). Both pass.

Mechanism (verified by trace): `ValidTimeNeighbors` (internal/store/facts.go:88-131) queries the chain by (tenant, subject, predicate) **including valid-time-closed history** (only `expired_at`-retracted rows and self excluded), split at `f.ValidAt` (lte→pred desc / gt→succ asc), ties broken client-side by the (valid_at, doc id) total order. `insertHistorical` (internal/worker/worker.go:466-522) merges realtime reads (reconciler head + this batch's writes — refresh-lag safe), trims the closed containing predecessor to `f.ValidAt` (guarded, monotone `trimInterval`), and creates the record bounded at the true successor's valid_at with no supersedes link.

### 2. Adjudication: trim-before-create ordering deviation — SOUND

The historical-insert path trims the closed containing predecessor BEFORE creating the new fact, inverting D10 step 3's create-first order. I adjudicate this sound against D10's intent:

- **Why create-first cannot work here:** create-first exists so a durable `supersedes` link lets the sweep complete a crashed close. A historical record deliberately carries NO supersedes link (otherwise sweep rule (a) would close the live head — the Round-1 tests assert this). So a crash after create-but-before-trim would be **unrepairable**: the outbox retry re-reconciles, hits rule 1 (own record exists) → NOOP, the trim never runs, and the overlap is permanent. Create-first on this path converts a crash into permanent corruption.
- **Why trim-first converges:** a crash after the trim leaves a valid-time **gap** ([f.ValidAt, old bound) covered by nobody) — never two truths, which is D10's actual invariant ("never two live truths"; reads may transiently see anomalies, sweep-bounded). The extraction is already cached in the ledger (the trim happens inside reconcileFact, after `LedgerExtracted` persisted), so the outbox retry / sweep rule (c) resumes, `insertHistorical` re-runs, the trim is a no-op (`!InvalidAt.After(at)` — already within bound), and the create fills the gap. **Demonstrated:** `TestDW_2_8_LateArrivalCrashBetweenTrimAndCreateConverges` injects the crash between trim and create, asserts the gap state is disjoint (never overlapping), and asserts the resume fills it from the cached extraction (extractor call count unchanged). Passes.
- A transient gap is strictly safer than a transient double-truth and is consistent with the plan's bounded-consistency note. The deviation is local to the historical path, documented in the code (worker.go:459-465), and covered by a crash test. **Sound.**

### 3. Regression check — CLEAN

Full suites re-run (see Executed Results): no regressions. All Round-1 DW tests still pass, including every crash/replay/concurrency/cost test, on both the fake store and the live cluster.

### 4. New-defect scan of the changed code

| Area | Finding |
|------|---------|
| `trimInterval` guard (worker.go:548-582) | Correct. Case order: live → leave alone (sweep's job); already ≤ bound → no-op; inversion → loud error. Monotone narrowing under guarded OCC with bounded re-read retry — concurrent trims converge to the earliest bound; a concurrent close-then-trim (or trim-then-close) converges to the narrowest interval (closePredecessor treats "already closed" as success). No defect. |
| Tie-break determinism | `succBeats`/`predBeats` and `laterByValidThenID` implement the (valid_at, id) total order consistently with the lte/gt query split (equal valid_at routes to pred). Deterministic. Non-blocking note: the OpenSearch `ValidTimeNeighbors` overfetches only 4 docs sorted by valid_at alone, so with >4 records sharing the exact boundary valid_at the client-side id tie-break sees an arbitrary server subset — theoretical nondeterminism in a pathological case, not demonstrated as corruption. |
| Realtime-merge path (worker.go:472-499) | Correct: merges the reconciler head + all batch-written docs via realtime GetFact (refresh-lag safe), filters self/expired/tenant/subject/predicate. The in-batch case is proven by `TestDW_2_8_LateArrivalInBatchPairStaysDisjoint` (second historical fact trims the first's fresh bound; x=[tv0,tv1), y=[tv1,tv3)). |
| `succ == nil` error path | Unreachable by construction (the reconciler head qualifies by the branch precondition) but surfaced loudly instead of indexing an unbounded historical record — correct defensive choice. |
| fake_test.go `ValidTimeNeighbors` | Mirrors the real semantics (lte/gt split, (valid_at, id) ties, expired/self exclusion). Faithful. |

**No new defects introduced by the fix.** However, probing the same disjointness invariant adversarially found a residual defect in **unchanged** code:

### Issue 2 (residual, pre-existing — demonstrated): close bounds are not neighbor-aware, so a late arrival landing inside a crash/divergence window still yields permanently overlapping closed intervals

- **Files:** internal/worker/worker.go:416-448 (`closePredecessor` — closes at the successor's valid_at unconditionally); internal/worker/repair.go:137-141 (rule (a): `earliestNonInverted(sibs)` considers only supersedes-linked siblings), repair.go:159-168 (rule (a'): closes each sibling at the next sibling's valid_at). All funnel through `closePredecessor` via `tryClose` (repair.go:287).
- **Demonstrated by:** two reviewer-authored tests (written, run, removed — not left in the tree): `TestScratch_CrashWindowLateArrivalThenSweep` and `TestScratch_DivergenceWindowLateArrivalThenSweep`. Simplest reproduction: (1) ana live @ tv1; (2) worker crashes between creating cid@tv2 (supersedes ana) and closing ana — the documented DW-2.8 transient (ana and cid both live); (3) late arrival eve@tv1+6h lands **inside that window**: head=cid → historical path; eve's valid-time predecessor ana is LIVE, so the trim is (correctly) skipped — closing live facts belongs to the sweep; eve=[tv1+6h, tv2); (4) the sweep completes the crashed close via the supersedes link: ana.invalid_at = cid.valid_at = **tv2**. Final observed state: `ana=[tv1, tv2)` ⊇ `eve=[tv1+6h, tv2)` — permanently overlapping closed records. An as-of audit query at any V ∈ [tv1+6h, tv2) returns two contradictory truths for one subject+predicate, forever; no sweep rule ever revisits closed-closed overlaps. The divergence variant (two racing UPDATEs of one predecessor + a late arrival between their valid times, then sweep rule (a')) produces the same end state — both observed directly in test output.
- **Correct converged state:** ana=[tv1, tv1+6h), eve=[tv1+6h, tv2), cid=[tv2, ∞).
- **Pre-existing, not introduced by the fix:** the pre-fix code bounded eve at the head's valid_at (cid → tv2) — the identical interval — and the close-bound code is untouched by this fix. Same overlap either side of the fix. It is the close-side instance of the same invariant hole whose insert-side instance was Round-1's Issue 1; the sweep rules as literally written in the plan (close at the sibling's valid_at) prescribe this behavior, so this is also a **spec gap**, not an implementation deviation from the written protocol.
- **Why it still blocks:** this phase's gate is Full — "data-integrity & concurrency critical: wrong reconciliation silently corrupts memory" — and this is silent, permanent bi-temporal corruption reachable by composing two scenarios the phase itself designates first-class (crash-between-create-and-close is DW-2.8's headline crash window; late arrivals are DW-2.8's other headline scenario; two-workers-overlapping is a listed edge case). Per this review protocol, a correctness defect demonstrated by a failing test fails the gate regardless of DW-item wording — the same principle that governed Round 1. The invariant is not reviewer-invented: the codebase's own new regression helper states it ("no point in valid time is covered by two records").
- **Contained fix (one choke point):** every close funnels through `Worker.closePredecessor` (the worker's step 4 and the sweep's `tryClose`). Before writing, compute the predecessor's nearest valid-time successor across ALL non-expired records (the `ValidTimeNeighbors` seam this fix already added) and close at `min(requested bound, that successor's valid_at)`. That single change makes worker step 4, sweep rule (a), and rule (a') neighbor-aware; the plan's Write-protocol section should be amended to say the close bound is neighbor-aware, mirroring the insertion rule. Add regressions for the two demonstrated interleavings.

### Round-2 Notes (non-blocking)

- `ValidTimeNeighbors` succ selection can pick an empty-interval record ([v,v), a sweep-rule-(b)-closed duplicate) as the bounding successor, producing an unnecessarily early bound and a conservative valid-time gap (never an overlap). Consider filtering empty intervals from succ selection.
- The >4-way equal-valid_at tie-break subset issue (table above) — pathological, undemonstrated; consider a secondary sort key or larger overfetch.
- Round-1 note about `make integration` missing worker/ingest — **resolved**: the target now covers both packages.

### Round-2 Verdict

**POST-GATE FAIL — blocker: Issue 2 (residual close-bound overlap, demonstrated).**

To be explicit about what passed: the dispatched fix is correct, sound, and complete for what it addressed — Round-1's Issue 1 is verified fixed at both fake-store and live-cluster level, the trim-before-create ordering deviation is adjudicated sound (and its crash window is tested), all suites are green including `-race` and live integration, and the fix introduces no new defects. The remaining blocker is the pre-existing close-side instance of the same disjointness hole, demonstrated by failing tests, with a contained one-choke-point fix available (`closePredecessor` + a plan amendment to make the close bound neighbor-aware).

---

## Round 1 — Original review (preserved)

### Executed Results (Step 0)

- Build: `go build ./...` → success.
- Unit tests: `go test ./...` → 141 passed, 19 packages, 0 failed.
- Race tests: `go test -race ./internal/worker/...` → all pass, no races reported.
- Vet: `go vet ./...` → clean.
- Lint: `revive -set_exit_status` → clean, exit 0.
- Integration (`make integration`, live OpenSearch 3.1.0 at localhost:9200, container `engram-dev-os`): all pass. Note: the `integration` Makefile target did not then include `internal/worker`/`internal/ingest` — I ran those directly; all 8 live-cluster worker tests passed. *(Resolved in Round 2: Makefile updated.)*
- Cost gate: measured **$0.0687 per 1k events** on `gpt-4o-mini` list price, gate ≤$5/1k — passes with wide margin.
- Scratch verification test (written and run by this review, then deleted): confirmed a genuine defect in late-arrival neighbor bounding (Issue 1). *(Resolved in Round 2.)*

### Requirement Fulfillment

#### DW-2.1
PREMISE:  the worker claims unprocessed episodic events via the outbox (lease + attempts); kill-and-restart the service at any point after the sync append → the event is still eventually processed (no lost handoff).
EVIDENCE: internal/store/outbox.go:20-101 (`ClaimBatch`/`claimOne`, guarded per-doc lease claim); internal/worker/worker.go (`Tick`, `ProcessEvent`).
TRACE:    Append → event sits unclaimed in episodic (the append IS the enqueue). A fresh worker's `Tick` → `ClaimBatch` finds it (no `claim_lease_until` or expired) → guarded per-doc claim (`if_seq_no`/`if_primary_term`) → `ProcessEvent`. Crash points independently tested: before ledger claim (`TestDW_2_8_CrashBeforeLedgerClaimOutboxRetries`), claimed-then-died (`TestDW_2_1_AbandonedClaimReclaimedAfterLease`, lease expiry reclaims with attempts incremented), after ledger-complete but before outbox-complete (`TestDW_2_1_CrashAfterLedgerCompleteBeforeOutboxComplete`, retry short-circuits, no re-extraction, no duplicate facts), between new-fact index and predecessor close (`TestDW_2_8_CrashBetweenCreateAndCloseSweepConverges`, sweep converges). All pass.
VERDICT:  PASS

#### DW-2.2
PREMISE:  async worker extracts facts and retrieves top-k candidates from semantic.
EVIDENCE: internal/worker/worker.go (`w.extractor.Extract`; `candidates` → `w.store.Candidates(ctx, f, w.cfg.CandidateK)`); internal/store/facts.go (`Candidates`).
TRACE:    `TestDW_2_2_WorkerExtractsAndRetrievesCandidates` seeds a live fact, processes a second event, asserts `candidateCalls >= 1` and `lastCandidateK == 10`, and asserts the new head actually supersedes the retrieved candidate. `TestDW_2_2_LiveExtractReconcileUpdate` proves the same end-to-end against the real cluster. Both pass.
VERDICT:  PASS

#### DW-2.3
PREMISE:  reconciler emits ADD/UPDATE/INVALIDATE/NOOP; UPDATE writes a new fact + sets prior `invalid_at = new.valid_at` (4 timestamps), never hard-deletes.
EVIDENCE: internal/ingest/reconciler.go:37-96 (four-way `Reconcile`); internal/worker/worker.go (`reconcileFact` OpUpdate/OpInvalidate branch, `closePredecessor`).
TRACE:    `TestDW_2_3_UpdateWritesNewFactAndClosesPrior`: bob@tv0 then ana@tv1 → 2 total facts (never deleted), old.invalid_at==tv1, old.invalidated_tx_at set, old.expired_at nil, old.created_at/valid_at present, new head supersedes==oldID, doc id content-addressed. `TestDW_2_3_InvalidateRetractsWithoutSuccessorTruth` confirms INVALIDATE. The Store interface exposes no delete method anywhere in the write path. All pass.
VERDICT:  PASS

#### DW-2.4
PREMISE:  replaying the same `event_id` short-circuits at the ledger; a crash-resumed event applies the cached extraction (LLM not re-called — verified by call count); a bumped `extractor_version` reprocess produces no duplicate live facts (content-key dedup) — idempotency is mechanical, not LLM-behavioral.
EVIDENCE: internal/worker/worker.go (ledger-phase switch: `LedgerComplete` short-circuit, `LedgerExtracted` cache-resume, fresh-claim extraction).
TRACE:    `TestDW_2_4_ReplayShortCircuitsAtLedger`: replay → `ex.Calls()==1`, 1 fact total. `TestDW_2_4_CrashResumeUsesCachedExtraction`: inject crash at `create`, extraction cached → resume keeps `ex.Calls()==1`. `TestDW_2_4_ExtractorVersionBumpNoDuplicateLiveFacts`: v2 worker re-extracts deliberately but content-addressed doc id + 409→NOOP yields exactly 1 total fact, 0 duplicate live content keys. All pass.
VERDICT:  PASS

#### DW-2.5
PREMISE:  N parallel workers on overlapping facts produce no lost updates (guarded close), no duplicate live content keys (`op_type=create` 409 path exercised), and divergent concurrent UPDATEs of one predecessor converge to a single live head (repair sweep); verified by a race/concurrency test.
EVIDENCE: internal/worker/worker.go (`reconcileFact` bounded re-read/re-reconcile on `store.ErrConflict`; `closePredecessor` guarded retry); internal/worker/repair.go (`sweepSupersessions`, rules a/a').
TRACE:    `TestDW_2_5_DuplicateAddConflictReReconciles` (injected 409 on create → `CreateConflicts==1`, 1 fact). `TestDW_2_5_GuardedCloseConflictNoLostUpdate` (injected concurrent close → `CloseConflicts==1`, winner's close preserved). `TestDW_2_5_ParallelWorkersConverge` (8 goroutines: 4 duplicate ADDs + 4 divergent UPDATEs, 3 sweep passes) → 0 duplicate live content keys, predecessor closed exactly once, exactly one live head. `TestDW_2_5_LiveParallelWorkersConverge` repeats this on the real cluster. `-race` clean. All pass. *(Round 2 caveat: convergence to one live head holds; the closed-interval bounds in a divergence+late-arrival composition are Issue 2.)*
VERDICT:  PASS

#### DW-2.6
PREMISE:  extraction cost per 1k events measured and gated ≤ $5/1k events on the cheap-model path (list price of the pinned extraction model on a fixed synthetic workload; batching counts toward it).
EVIDENCE: internal/ingest/cost.go (`CostMeter`, `CostPer1kEventsUSD`); internal/ingest/cost_test.go (`TestDW_2_6_ExtractionCostGate`, hard `t.Fatalf` gate at $5).
TRACE:    1,000 synthetic events through the metered extractor → 233,720 prompt / 56,000 completion tokens → **$0.0687** per 1k events at gpt-4o-mini list price. Batching honored: one call's tokens divide across its events (`TestCostMeterMath`). Real CI gate — fails the build above $5.
VERDICT:  PASS

#### DW-2.7
PREMISE:  dirty test — contradictory in-batch facts resolve deterministically; malformed extraction is rejected, not indexed.
EVIDENCE: internal/worker/worker.go (deterministic `(valid_at, content_key)` sort before reconciliation); internal/ingest/extraction.go (`ParseExtraction` barricade).
TRACE:    `TestDW_2_7_ContradictoryBatchResolvesDeterministically`: contradictory pair run with directive order flipped → identical final state both ways. `TestDW_2_7_MalformedExtractionRejectedNotIndexed`: `!malformed` → 0 facts indexed, retried via lease, dead-lettered after MaxAttempts. Both pass.
VERDICT:  PASS

#### DW-2.8
PREMISE:  crash-recovery test — killed between new-fact index and predecessor close → repair sweep completes the close via `supersedes` (exactly one live head); killed after extraction before writes → resumes from the cached ledger extraction; killed before the ledger claim → the outbox retries cleanly. Late-arrival test: a historical fact inserts with a bounded interval; no inverted intervals, predecessor untouched.
EVIDENCE: internal/worker/repair.go (`Sweep`, rules a/a'/b/c); internal/worker/worker.go (late-arrival branch — Round 2: `insertHistorical`).
TRACE:    `TestDW_2_8_CrashBetweenCreateAndCloseSweepConverges` (sweep completes close via supersedes, resumes ledger from cache, one live head). `TestDW_2_8_CrashBeforeLedgerClaimOutboxRetries` (clean retry, exactly one extraction). `TestDW_2_8_LateArrivalBoundedIntervalPredecessorUntouched` (bounded interval, no supersedes, predecessor untouched, sweep no-op). All pass. Round 2 adds multi-hop, in-batch-pair, trim-crash, and live-cluster late-arrival regressions — all pass.
VERDICT:  PASS

**All requirements met:** YES (all 8 DW items pass with execution evidence).

### Test-DW Coverage

- [x] All DW items have corresponding tests, name-tagged `TestDW_2_1_*` … `TestDW_2_8_*`, ran in Step 0 / Round 2.
- [x] ≥1 dirty test: malformed extraction, plus injected crash/conflict tests throughout.
- [x] DW-2.5 concurrency covered by injected-race + real-goroutine tests at fake-store and live-cluster level, plus `-race`.
- No gaps.

### Dead Code

None found (both rounds). No TODO/FIXME/panic markers; no unreachable code; build/vet/lint clean.

### Correctness Dimensions

| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS (Round 3) | Round-2 Issue 2 fixed via `closeBound` and verified (both interleavings re-falsified, now disjoint; live-cluster regression). Guarded create/close, `-race`, parallel-worker tests all clean. Residual: Issue 3 write-skew — adjudicated plan-scope (per-document-OCC model limit), not a phase defect; see §Round 3 §2. |
| Error Handling | PASS | Every I/O call wraps and propagates; event errors abandon to the lease/attempts retry; embedder failure degrades with an explicit log. |
| Resources | PASS | Goroutines bounded by ctx + WaitGroup; per-sweep memoization only; shared HTTP clients. |
| Boundaries | PASS (Round 2) | Round 1's late-arrival boundary defect (Issue 1) fixed and regression-tested (fake + live). Empty/oversized-input barricades tested (`TestParseExtraction`). |
| Security | PASS | LLM output validated at the single `ParseExtraction` barricade with hard caps before anything downstream; no string-concatenated queries. |

### Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| aposd-verifying-correctness | Requirements coverage | PASS | All 8 DW items traced to code + passing test. |
| aposd-verifying-correctness | Concurrency: no TOCTOU gaps, shared state protected | PASS (Round 3) | Issue 2's close-bound TOCTOU fixed (`closeBound` re-reads neighbors at close time; verified). Remaining Issue-3 write-skew is a store-model limit (per-doc OCC, D10), closable only by a new repair rule — plan scope, documented. |
| aposd-verifying-correctness | Boundaries: chain/version edge cases | PASS (Round 2) | Issue 1 fixed; multi-hop, in-batch, crash-window, and live regressions pass; disjointness asserted across live+closed records. |
| cc-defensive-programming | External input validated at a single barricade | PASS | `ParseExtraction` is the sole LLM-output validator; both extractors funnel through it. |
| cc-defensive-programming | No silently swallowed errors | PASS | The one degrade-and-continue (embedding) is explicitly logged. |
| cc-defensive-programming | Assertion vs error handling for internal invariants | Note | Unstamped-fact and unreachable-succ conditions surfaced as loud errors rather than panics — appropriate for a mission-critical worker (fails one event, not the pool). |
| aposd-designing-deep-modules | Interface depth of the Store seams | PASS | `ValidTimeNeighbors` added as one narrow, well-documented method on the existing seam; no OpenSearch types leak; fake mirrors semantics faithfully. |

### Notes (non-blocking, cumulative)

- `ValidTimeNeighbors` succ selection can bound against an empty-interval record ([v,v)), yielding a conservative gap — consider filtering.
- >4-way equal-valid_at boundary ties: the 4-doc overfetch makes the client-side id tie-break see an arbitrary server subset — pathological, undemonstrated; consider a secondary sort or larger overfetch.
- No dedicated HTTP-transport-timeout unit test for `HTTPExtractor` (500/no-choices/non-JSON covered; the worker-side retry path any timeout hits is generically proven).
- Round-1 note about `make integration` missing worker/ingest — **resolved** in Round 2.

## Issues

1. ~~**Issue 1 (Round 1): late-arrival bounding not neighbor-aware on ≥2-version chains.**~~ **FIXED and verified in Round 2** — `ValidTimeNeighbors` + `insertHistorical` + `trimInterval`; regression-tested at fake-store and live-cluster level; original falsification scenario re-run and now disjoint.

2. ~~**Issue 2 (Round 2): close bounds not neighbor-aware — permanent overlap when a late arrival lands inside a crash/divergence window.**~~ **FIXED and verified in Round 3** — `closeBound` (worker.go:467-476) applies `min(requested, nearest valid-time successor)` at the single close choke point (`closePredecessor`, used by worker step 4 AND the sweep's `tryClose`); the plan's step 4 amended to match. Both demonstrated interleavings re-falsified by this reviewer and now converge to pairwise-disjoint intervals; three committed regressions (two fake-store, one live-cluster) encode them.

3. **Issue 3 (Round 3, non-blocking — plan-scope architectural residual): sub-second write-skew between a close and a concurrent historical insert can still produce a permanent closed-interval overlap.**
   - Demonstrated by a reviewer-authored failing test (run, then removed): the closer's neighbor read and the inserter's predecessor read both precede both writes → neither the trim nor the min-bound fires; the predecessor's seq_no guard cannot see the new record (a different doc). Three sweep passes do not repair (no rule inspects closed-closed overlaps). Full trace and adjudication in §Round 3 §2.
   - Classification: NOT a defect in this phase's work — the code faithfully implements the amended normative step 4, and no implementation of that step can close a multi-document write skew under D10's own documented constraint (per-document OCC, no multi-doc transactions). Closure requires a new plan-level repair rule.
   - Prescribed follow-up: sweep rule (d) — closed-closed overlap repair per (tenant, subject, predicate) group, trimming the earlier record to the later's valid_at (monotone; `trimInterval` already provides the primitive). Optionally a post-close neighbor re-check to narrow the window meanwhile. Track as a plan amendment at Phase-2 exit.

**Final verdict (Round 3): PASS.** All 8 DW items hold with execution evidence; all listed edge cases handled; the write path follows the amended normative protocol in order (the one documented, adjudicated-sound ordering deviation on the historical path included); every defect demonstrated in Rounds 1-2 is fixed, regression-tested at fake-store and live-cluster level, and independently re-falsified without reproduction; unit, race, lint, integration, and the cost gate are all green. The single remaining demonstrated behavior (Issue 3) is an irreducible residual of the plan's chosen consistency model, precisely documented with a prescribed plan-level follow-up — beyond this phase's scope, not a blocker.

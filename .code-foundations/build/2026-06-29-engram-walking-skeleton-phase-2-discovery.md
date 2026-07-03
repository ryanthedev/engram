# Discovery + Design: Phase 2 - Async Write & Bi-temporal Reconciliation

## Files Found

- `internal/store/store.go` — the fixed Phase-0 `Store` seam: `Append/Create/Update` + outbox (`ClaimBatch/Complete/DeadLetter`) + ledger (`ClaimLedger/UpdateLedger`) + repair (`ScanIncomplete`), with `LedgerPhase/LedgerState/LedgerEntry` types and `ErrConflict`.
- `internal/store/opensearch.go` — `OpenSearchStore`: `Append/Create/Update` implemented (Phase 1); the six outbox/ledger/repair methods stubbed `ErrNotImplemented` — this phase fills them.
- `internal/store/enrich.go` — precedent for consumer-side seam extension: concrete `OpenSearchStore` methods (`FindUnembedded/SetTextEmbedding`) beyond `store.Store`, consumed via a package-local interface (`enrich.Store`).
- `internal/store/apply.go`, `templates.go`, `templates/{episodic,semantic,rrf-pipeline}.json` — cluster contract + idempotent apply. **No ledger index template exists.**
- `internal/memory` (`record.go`, `ids.go`) — `Episodic` (outbox fields present in struct + template), `SemanticFact` (4 timestamps, `ContentKey`, `Supersedes`, `InvalidatedTxAt`), `ContentKey()`, `FactDocID()`, `LedgerKey.DocID()`.
- `internal/ingest/ingest.go` — `Extractor`/`Reconciler` seam, `Candidate{ID, Fact, SeqNo, PrimaryTerm}`, `Op{Kind, PredecessorID}`, four `OpKind`s. Interface doc contract: malformed **or empty** extraction returns an error.
- `internal/retrieval/opensearch.go` — hybrid retriever (returns `Hit`s without seq_no/primary_term — unusable as-is for guarded-close candidates).
- `internal/embed` — `Embedder` seam + fake + TEI HTTP client (pattern for the extraction HTTP client).
- `internal/enrich/enrich.go` — background-job shape (Tick/Run, slog, non-fatal tick errors).
- `internal/server`, `cmd/engram-server` — gRPC service + wiring point for the worker pool + repair sweep.
- `internal/testutil` — live-cluster helpers (scratch indices from template patterns, `GetSeqNo`, refresh).
- `Makefile` — `test` (unit), `integration` (tags=integration vs localhost:9200 pinned 3.1). Dev cluster verified live (3.1.0). Baseline: 85 tests green.

## Current State

Phases 0–1 are done: sync episodic append (the enqueue, D12), hybrid Search, embedding enrichment. All write-protocol primitives are proven (op_type=create 409 → `ErrConflict`; guarded `Update` via if_seq_no/if_primary_term). The entire async path — outbox scan-and-claim, extraction, ledger, reconciliation, bi-temporal writes, repair sweep — does not exist. `Extractor`/`Reconciler` have no implementations.

## Gaps

| # | Gap | Resolution in this phase |
|---|-----|--------------------------|
| 1 | No ledger index (template, name, mapping) — `LedgerState.Extraction []byte`, `completed_actions`, `lease_until` need a storage shape | New `templates/ledger.json` (`engram-ledger*`, dynamic strict, `extraction` as `binary`), registered in `templates.go`, applied in `apply.go`, asserted alongside the DW-0.3 template tests |
| 2 | Candidates need `_seq_no`/`_primary_term`; `Retriever.Search` doesn't return them | New concrete `OpenSearchStore` methods (`Candidates`, `GetFact`) consumed via a worker-local seam interface — the `enrich.Store` precedent. Candidate query is a T2 top-k bool search (content_key / subject / predicate terms + statement match, live-filtered, `seq_no_primary_term=true`) |
| 3 | `Complete`/`DeadLetter` take `eventID`, but episodic doc `_id` is OpenSearch-assigned (and a replayed Ingest appends a second doc with the same `event_id`) | `_update_by_query` on the `event_id` term (`refresh=true`, `conflicts=proceed`) — marks every copy processed, so replayed appends drain too |
| 4 | Divergent concurrent UPDATEs of one predecessor produce two live heads with **different** content keys — sweep rules (a)/(b) as written don't converge them, but DW-2.5 requires it | Sweep rule (a) extended: live facts grouped by `supersedes` target; siblings sharing a target are chain-closed in (valid_at, doc_id) order so exactly one live head survives. This is inside the sweep's mandate ("converge partial writes"), not a protocol change |
| 5 | Late-arrival inserts must NOT carry a live `supersedes` link (sweep rule (a) would wrongly close the predecessor) | Historical inserts get `Supersedes=""` and a bounded interval `invalid_at = successor.valid_at` at index time, exactly per D10 step 4 |
| 6 | `TestOpenSearchStoreOutboxLedgerMethodsNotImplemented` (opensearch_test.go:211) asserts the Phase-1 placeholder this phase is chartered to replace | Test replaced by real outbox/ledger behavior tests — its premise ("not implemented until Phase 2") is invalidated by this phase itself. Recorded as a deliberate anchoring exception |
| 7 | Cheap extraction model never pinned (plan open question) | Pinned in config: OpenAI-compatible `gpt-4o-mini` list price ($0.15 in / $0.60 out per 1M tokens) as `ingest.DefaultPricing`, overridable by server flags. DW-2.6 gates the metric plumbing at this price |
| 8 | Semantic facts written by the worker have no embedding (Phase-1 enrichment covers episodic only) | Worker embeds `Statement` at write time when an `Embedder` is configured (non-fatal on error — BM25 serves regardless). Keeps facts retrievable via the Phase-1 hybrid path |

## Code Standards

`docs/code-standards.md` applies: wrapped sentinel errors (`errors.Is`), `context` first param, no unowned goroutines, consumer-side interfaces with vendor types hidden, table-driven tests, integration behind build tag, ≥1 dirty test, `log/slog`, metrics on extraction cost.

## Test Infrastructure

Unit tests use httptest fakes (store) and pure fakes (embed); integration tests (`-tags=integration`) run against the live pinned 3.1 dev cluster using `testutil` scratch indices inheriting real templates. DW-mapped tests are named `TestDW_N_M_...` (established convention).

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-2.1 | Worker claims via outbox (lease + attempts); kill-and-restart at any point after sync append → event still eventually processed | COVERED | `TestDW_2_1_ClaimBatchLeasesAndCountsAttempts` (unit), `TestDW_2_1_KillBeforeClaimEventStillProcessed`, `TestDW_2_1_AbandonedClaimReclaimedAfterLease` (unit, in-memory store), `TestDW_2_1_LiveOutboxClaimCompleteDeadLetter` (integration) |
| DW-2.2 | Worker extracts facts and retrieves top-k candidates from semantic | COVERED | `TestDW_2_2_WorkerExtractsAndRetrievesCandidates` (unit), `TestDW_2_2_LiveCandidateRetrieval` (integration) |
| DW-2.3 | Reconciler emits ADD/UPDATE/INVALIDATE/NOOP; UPDATE writes new fact + prior `invalid_at = new.valid_at` (4 timestamps), never hard-deletes | COVERED | `TestDW_2_3_ReconcilerFourWayDecisions` (table), `TestDW_2_3_UpdateWritesNewFactAndClosesPrior` (asserts both docs live in store history, predecessor closed with `invalidated_tx_at`, doc count never decreases) |
| DW-2.4 | Replay short-circuits at ledger; crash-resume applies cached extraction (LLM call count); bumped `extractor_version` reprocess → no duplicate live facts | COVERED | `TestDW_2_4_ReplayShortCircuitsAtLedger`, `TestDW_2_4_CrashResumeUsesCachedExtraction`, `TestDW_2_4_ExtractorVersionBumpNoDuplicateLiveFacts` |
| DW-2.5 | N parallel workers: no lost updates (guarded close), no duplicate live content keys (409 path exercised), divergent UPDATEs converge to one live head (sweep); race test | COVERED | `TestDW_2_5_ParallelWorkersNoLostUpdatesNoDuplicates` (unit, `-race`, in-memory store, asserts 409 + guard-conflict counters > 0), `TestDW_2_5_LiveParallelWorkersConverge` (integration, live cluster + sweep) |
| DW-2.6 | Extraction cost per 1k events measured, gated ≤ $5/1k on cheap-model path (pinned list price, fixed synthetic workload, batching counted) | COVERED | `TestDW_2_6_ExtractionCostGate` (1,000-event fixed synthetic workload through the metered extractor), `TestCostMeterMath` (unit math incl. batching) |
| DW-2.7 | Dirty: contradictory in-batch facts resolve deterministically; malformed extraction rejected, not indexed | COVERED | `TestDW_2_7_ContradictoryBatchResolvesDeterministically` (same input twice → byte-identical final state), `TestDW_2_7_MalformedExtractionRejectedNotIndexed` (nothing indexed; event dead-letters after max attempts) |
| DW-2.8 | Crash recovery: killed between create and close → sweep completes close via `supersedes` (one live head); killed post-extraction pre-write → resumes from cache; killed pre-claim → outbox retries. Late arrival: bounded interval, no inversion, predecessor untouched | COVERED | `TestDW_2_8_CrashBetweenCreateAndCloseSweepConverges`, `TestDW_2_8_CrashAfterExtractionResumesFromCache`, `TestDW_2_8_CrashBeforeLedgerClaimOutboxRetries`, `TestDW_2_8_LateArrivalBoundedIntervalPredecessorUntouched` (+ integration sweep variant) |

**All items COVERED:** YES (8/8 — matches the dispatch prompt's 8 DW-IDs)

## Design Decisions

### Design: async write worker (the phase's one new module)

**Approaches considered**
1. **A — Fat worker on the concrete store:** `internal/worker` calls `*store.OpenSearchStore` directly; all orchestration + queries in one package.
2. **B — Deep seams, thin orchestration:** worker depends on `store.Store` plus a worker-local extension interface (`GetFact`, `Candidates`, sweep scans) implemented by concrete `OpenSearchStore` methods — the `enrich.Store` precedent. `processEvent` is a straight-line function encoding the D13→D10 protocol; the repair sweep reuses it for ledger resume.
3. **C — Persisted step-state machine:** every protocol step a first-class persisted transition record; a generic executor replays steps.

**Comparison**

| Criterion | A | B | C |
|-----------|---|---|---|
| Interface simplicity | poor (vendor type leaks) | good (small seam, vendor hidden) | poor (step vocabulary leaks everywhere) |
| Information hiding | poor | good — OpenSearch queries live in store; protocol lives in worker | medium |
| Caller ease of use / testability | needs live cluster for every test | in-memory fake enables deterministic crash-injection at every protocol step | heavy machinery |
| Fit to codebase precedent | no | yes (enrich pattern, standards doc) | no |

**Choice: B.** Deterministic crash-injection unit tests (DW-2.8's three kill points) fall out of the seam for free; the ledger already IS the persisted state (C rebuilds what D13 provides).

**Depth check:** worker public surface = `Worker` (config + `Run` + `ProcessEvent`) and `Sweeper` (`Run` + `Sweep`); hidden: candidate queries, claim mechanics, retry/backoff, guarded-close loop, in-batch realtime candidate merging, ledger bookkeeping. Common case (event in → facts live) needs zero protocol knowledge from callers.

### Key protocol decisions (all normative-section driven)

- **Fact ordering (DW-2.7 determinism):** within one event's extraction, facts are sorted by `(valid_at, content_key)` before sequential reconcile. Contradictory facts resolve last-by-valid-time as the live head; ties produce an empty predecessor interval `[v,v)` (legal, never inverted).
- **Refresh-lag independence:** candidate search can lag OpenSearch's refresh; correctness never depends on it. The worker merges realtime `GetFact` reads (its own computed doc `_id`s + in-batch writes) into the candidate set; cross-worker races are converged by 409 / guarded close / sweep — the protocol's whole point.
- **409 handling (D10 step 3, literal):** `Create` → `ErrConflict` → re-fetch candidates (incl. realtime get of the colliding `_id`) → re-reconcile, bounded (3 attempts). Content-addressed `_id`s mean the colliding doc is content-identical; the re-reconcile typically lands NOOP and any pending predecessor close is the winner's job (or the sweep's — exactly DW-2.8's contract).
- **Guarded close (D10 step 4):** `Update` with predecessor's seq_no/primary_term; on conflict re-read — if `invalid_at` already set, another worker won (success); else retry ≤3.
- **Late arrival (D10 step 4):** `new.valid_at < predecessor.valid_at` → predecessor untouched, `Supersedes` NOT set (a live supersedes link would make sweep rule (a) wrongly close the predecessor), `new.invalid_at = predecessor.valid_at` stamped at index time.
- **Empty vs malformed extraction:** the `Extractor` doc contract says both return an error. Sentinel `ingest.ErrNoFacts` distinguishes legitimately fact-free events (ledger completed with empty cache, event completed, nothing indexed) from malformed output (plain error → outbox retry → dead-letter after max attempts). Both satisfy "reject, don't index."
- **Retry/backoff (LLM timeout edge):** extraction errors abandon the event without `Complete`; the outbox lease expiry is the backoff clock, `attempts` the bound, `DeadLetter` the floor. No second retry mechanism.
- **Reconciler:** deterministic rule-based (`RuleReconciler`): exact live content-key match → NOOP; empty-object fact (retraction intent) → INVALIDATE the live same-subject+predicate head, or NOOP if none; live same-subject+predicate head with different object → UPDATE; else ADD. Head selection: max `(valid_at, content_key)` among live candidates — pure function of inputs.
- **Extractor duo (dispatch note):** shared `ParseExtraction` barricade (one JSON wire shape + validation used by both paths, so fixtures exercise the real validation); `RuleExtractor` synthesizes wire JSON deterministically from `fact:`/`retract:`/`!malformed` fixture lines and simulates token counts; `HTTPExtractor` speaks OpenAI-compatible chat-completions and records real `usage` tokens. Both feed one `CostMeter`.
- **Sweep (D10, + gap #4):** (a) live fact with live `supersedes` target → complete the guarded close; (a′) >1 live sibling sharing a `supersedes` target → chain-close by `(valid_at, doc_id)`, one live head survives; (b) >1 live fact sharing a `content_key` → keep the earliest, close the rest with empty intervals; (c) `ScanIncomplete` → resume each expired incomplete ledger entry through the same `processEvent` path (cached extraction when phase=extracted; episodic re-fetch by `event_id` when phase=claimed).
- **Defensive barricade:** external input = LLM output (validated in `ParseExtraction` — the trust boundary) and episodic docs re-read from the cluster (decode errors surface, never swallowed). Inside the barricade, protocol invariants (non-nil predecessor for UPDATE ops, non-inverted intervals) are checked and returned as errors — this is a correctness-leaning system (wrong reconciliation silently corrupts memory), so suspect states abort the event (outbox retries) rather than guess.

## Prerequisites

- [x] Dev cluster live (OpenSearch 3.1.0 at localhost:9200); baseline `go build`/`go test` green (85 tests)
- [x] Phase-0 seams + Phase-1 Store/Retriever implementations in place
- [x] All required files exist or are created in-scope (ledger template is new but squarely IN scope: "the extraction ledger" is a Phase-2 Produces item)

## Recommendation

**BUILD.** Plan matches reality; the eight gaps above are all resolvable inside phase scope with documented, principled choices. No UPDATE_PLAN triggers found.

---

## Implementation Outcome (post-build addendum)

Suites: 141 unit tests (green, incl. `-race` on the worker package), 97 integration tests against the live pinned 3.1 cluster (green, repeated runs), lint clean.

**Deviations from the design above, discovered during validation:**

1. **Sweep rule (a′) groups by chain ROOT, not direct supersedes target.** The parallel-workers test exposed that racing UPDATEs *nest* divergence (C supersedes A supersedes P while D supersedes B supersedes P): direct-target sibling grouping never converges C and D, leaving two live heads permanently. Since the version chain is the explicit `supersedes` link (D11), siblinghood is shared chain ancestry — the sweep now resolves each live superseder's root (memoized walk, cycle-guarded, depth-capped) and chain-closes all live descendants of one root to a single head. Verified by `TestDW_2_5_ParallelWorkersConverge` (stable across `-count=10 -race`) and `TestDW_2_5_SweepConvergesDivergentUpdates`.
2. **The live parallel test does not assert a 409 count.** Whether the real-cluster race produces actual 409s depends on sub-millisecond interleaving (the worker's realtime candidate pre-read usually converts the loser to NOOP before it creates). The 409 → re-reconcile path is instead proven deterministically: worker level with an injected mid-flight competitor (`TestDW_2_5_DuplicateAddConflictReReconciles`), store level against the live cluster (`TestOpenSearchStoreLiveAppendCreateUpdate`).
3. **The realtime candidate pre-read narrows the 409 window by design.** The worker merges realtime `GetFact` reads of its own computed doc id into the candidate set, so a replayed/duplicate fact usually NOOPs without conflicting — the 409 remains the true-concurrency backstop, exactly as D11 intends. (This is why the version-bump test asserts "no duplicate live facts" rather than "409 fired".)
4. **Anchoring exception (planned in Gaps #6):** `TestOpenSearchStoreOutboxLedgerMethodsNotImplemented` was replaced by `TestOpenSearchStoreClaimLedgerFirstClaimThenResume` + `TestOpenSearchStoreGetFact` — its asserted behavior ("stubbed until Phase 2") is what this phase implements. Every other pre-existing test is untouched and passing (85 → 141 unit).

**Review-fix addendum (post-gate FAIL, Issue 1 — late-arrival neighbor bounding):**

The independent review demonstrated that a late arrival inserted behind a chain with ≥2 prior versions was bounded against the reconciler's live head instead of its true nearest valid-time successor (candidate retrieval and head selection are structurally live-only), producing overlapping valid-time intervals — a violation of D10's neighbor-aware insertion. Fix:

- New store read `OpenSearchStore.ValidTimeNeighbors(f, selfID)` (internal/store/facts.go): nearest chain versions around `f.ValidAt` for (tenant, subject, predicate) INCLUDING valid-time-closed history (record-retracted rows and self excluded); deterministic (valid_at, doc id) tie-breaks. Added to the `worker.Store` seam.
- `Worker.insertHistorical` (internal/worker/worker.go) replaces the old late-arrival branch: the historical record is bounded at its TRUE successor's valid_at (may be a closed version), and the CLOSED valid-time predecessor whose interval would contain it is **trimmed** (`trimInterval`: guarded, monotone-narrowing re-close of `invalid_at`, re-stamped with `invalidated_tx_at` — the sanctioned mutation) so the chain stays pairwise disjoint. A LIVE valid-time predecessor is never touched (that is concurrent divergence — the sweep's job). Realtime merges (reconciler head + in-batch writes) keep the neighbor computation refresh-independent within a batch.
- **Deliberate ordering deviation, local to this path:** trim runs BEFORE create (vs D10 step 3's create-first). A historical record carries no supersedes link, so no sweep rule could repair a crash landing the create but not the trim (the outer 409 re-reconcile NOOPs → permanent overlap). Trim-first makes every crash window converge: worst case is a transient valid-time GAP — never two overlapping truths — filled on resume from the cached ledger extraction. Verified by `TestDW_2_8_LateArrivalCrashBetweenTrimAndCreateConverges`.
- Regression tests: `TestDW_2_8_LateArrivalMultiHopChainBoundsAtTrueNeighbor` (the review's exact scenario + pairwise interval-disjointness assertion over all versions, live and closed, + sweep-converges-nothing), `TestDW_2_8_LateArrivalInBatchPairStaysDisjoint` (two historical facts in one batch), `TestDW_2_8_LiveLateArrivalMultiHopChain` (live cluster). The original single-hop DW-2.8 test passes unchanged.
- Also fixed the review's non-blocking note: `make integration` now includes `./internal/worker/` and `./internal/ingest/`.

Post-fix evidence: `make test` 144 unit green (141 → 144), `go test -race ./internal/worker/...` clean (incl. `-count=10` stress on DW-2.5/2.8), `make lint` clean, `make integration` exit 0 — 9 packages, 96 test functions against the live pinned 3.1 cluster, including the new live regression.

**Review-fix addendum, round 2 (post-gate FAIL, Issue 2 — neighbor-aware close bounds):**

The re-review confirmed the Issue-1 fix (and adjudicated trim-before-create as SOUND) but demonstrated the close-side instance of the same disjointness hole: a record landing INSIDE a crash/divergence window (e.g. late arrival eve@tv1+6h while ana@tv1 and its successor cid@tv2 are both transiently live) was later swallowed by the completing close — `ana=[tv1,tv2) ⊇ eve=[tv1+6h,tv2)`, permanently overlapping closed records. The plan's Write-protocol step 4 was amended (by the coordinator) to require neighbor-aware close bounds. Fix, exactly as the reviewer scoped:

- `Worker.closeBound` (internal/worker/worker.go): before any close, read the predecessor's nearest valid-time successor across ALL non-expired records (the `ValidTimeNeighbors` seam from the Issue-1 fix) and close at **min(requested bound, that successor's valid_at)**. Implemented inside `closePredecessor` — the single choke point every close funnels through (worker step 4 AND the sweep's `tryClose`, i.e. rules a and a′) — so all close bounds are neighbor-aware with one change. The query's successor is strictly after the predecessor's own valid_at, so the min can never invert. Records that land after the neighbor read are covered by the invariant's other half: their own insert path trims the predecessor (`insertHistorical`), so both orders converge to disjoint intervals.
- Regression tests for BOTH demonstrated interleavings, each asserting pairwise no-overlap across all versions live and closed (`assertDisjointChain`): `TestDW_2_8_CrashWindowLateArrivalSweepStaysDisjoint` (crash-between-create-and-close + late arrival inside the window → sweep closes ana at eve's valid_at, not cid's; final `ana=[tv1,tv1+6h) eve=[tv1+6h,tv2) cid live`) and `TestDW_2_5_DivergenceWindowLateArrivalSweepStaysDisjoint` (rule-a′ variant: two divergent successors of one predecessor + late arrival between their valid times → sibling chain-close bounds at the late arrival, final timeline fully disjoint). Both were empirically proven to FAIL against the pre-fix behavior (min temporarily neutered → exact reviewer-observed overlaps reproduced) and pass with it. Live-cluster variant: `TestDW_2_8_LiveNeighborAwareCloseBound`.
- Implementation verified against the amended plan step 4 wording ("invalid_at = min(new.valid_at, nearest valid-time successor's valid_at) … every close bound is neighbor-aware … all closes funnel through one choke point").

Round-2 post-fix evidence: `make test` 146 unit green (144 → 146), `go test -race ./internal/worker/...` 26 passed clean, `-count=10` stress on all DW-2.5/DW-2.8 + the new window tests → 120/120, `make lint` clean, `make integration` exit 0 — 9 packages, 99 test functions on the live pinned 3.1 cluster including all three new regressions.

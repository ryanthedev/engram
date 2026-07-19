# Discovery + Design: Phase 1 - Reconciliation-outcome seam

## Files Found
- `internal/worker/worker.go` — `Stage` iface :115-120, `runStages` :153, `ProcessEvent` :242 (resume skip :315-318, reconcile call :319, `runStages` call :332), `reconcileFact` :377-441.
- `internal/ingest/ingest.go` — `OpKind` :27, `Op` :49, `Candidate` :16. Imports **only** `internal/memory`.
- `internal/graph/stage.go` — `Stage.Process` :45 (implementor 1).
- `internal/experience/distill.go` — `DistillStage.Process` :136 (implementor 2; ignores facts via `_`).
- `internal/graph/stage_test.go` :15 — the only `var _ worker.Stage` assertion (package `graph`, internal test pkg).
- `internal/experience/distill_test.go` — package `experience`; no compile assertion today.
- `internal/worker/stage_test.go` :20 — `recordingStage`, a third (test-only) implementor.
- `cmd/engram-server/stages_graph.go:76`, `stages_experience.go:63` — registration call sites (untyped by facts; unaffected).

## Current State
`reconcileFact(ctx, f, docID, written) error` takes the fact **by value**, stamps `f.Supersedes` on its local copy, and returns only `error`. `ProcessEvent` then hands `runStages` the *pre-reconciliation* `facts` slice, so the decision (`op.Kind`) and the predecessor (`findCandidate(cands, op.PredecessorID)` — already in hand, :405) are both discarded. `graph.Stage.Process` therefore cannot distinguish an ADD from an UPDATE.

## Gaps
| # | Gap vs plan | Resolution |
|---|---|---|
| 1 | Plan says two implementors; there is a **third**, test-only: `worker.recordingStage` (`internal/worker/stage_test.go:20`). | Not an invalidated assumption (the plan means production implementors). Update it with the rest; it is in `internal/worker/**` file scope. |
| 2 | `Produces` pins `FactOutcome` at 3 fields, but the Edge-cases/Uncertainty sections require replayed outcomes be "explicitly marked". A 4th field would break the pinned struct. | Resolved by design B below: the marker lives *in* `Decision` as a fifth `OpKind`, so the struct stays exactly as pinned. |
| 3 | Late-arrival path (`insertHistorical`, :414) has `op.Kind == OpUpdate/OpInvalidate` and a non-nil predecessor, yet **does not close** the predecessor. Phase 2 must not close its edge either. | No extra field needed: derivable from the outcome itself as `o.Fact.ValidAt.Before(o.Predecessor.ValidAt)`. Noted for Phase 2. |

## Assumption Verification
| Assumption | Verdict | Evidence |
|---|---|---|
| Only two `worker.Stage` implementors (graph, experience) | **HOLDS** (production) | `grep "func .*Process(ctx context.Context, ev memory.Episodic"` → `experience/distill.go:136`, `graph/stage.go:45` only. Plus one test double (gap 1). |
| `FactOutcome` in `internal/ingest` creates no import cycle | **HOLDS** | `internal/ingest` non-test files import only `internal/memory`. `internal/graph` imports `internal/worker` **only** in `stage_test.go`; `internal/worker` does not import `graph`/`experience`. `graph → ingest` and `experience → ingest` are new but acyclic. |

## Code Standards
- Std-lib `testing` only, no testify; table tests with named cases + `t.Run` + got/want.
- Tests named after the requirement they pin (`TestDW_1_2_...`) — the repo already uses this form.
- Compile-time interface satisfaction asserted with `var _ Iface = (*T)(nil)`; for `worker.Stage` it lives in the *test* file so the production package never imports `internal/worker`.
- Three import groups (stdlib / external / `github.com/ryanthedev/engram/...`).
- Comments cite decision codes (D10, D13, D20).
- Business packages must not import grpc/engrampb — unaffected here.

## Test Infrastructure
`internal/worker` has an external `worker_test` package with `newFakeStore()`, `newTestWorker`, `event(...)`, `factLine(...)`, `mustProcess`, `singleLiveHead` helpers (`stage_test.go`, harness in the package's other `_test.go` files) and a `recordingStage` double — everything DW-1.2/DW-1.4 need already exists. `internal/graph` and `internal/experience` use internal test packages.

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-1.1 | `Stage.Process` takes `[]ingest.FactOutcome`; old signature gone; both `var _ worker.Stage` assertions hold | COVERED | Compile-enforced: `graph/stage_test.go` `var _ worker.Stage = (*Stage)(nil)` (updated impl) + **new** `experience/distill_stage_assert_test.go` `var _ worker.Stage = (*DistillStage)(nil)`. Runtime: `TestDW_1_1_StageSeamReceivesOutcomes` (worker) asserts the stage is handed outcomes, not facts. |
| DW-1.2 | `Decision` from the reconciler's actual `Op.Kind` for every fact; `Predecessor` non-nil for exactly UPDATE + INVALIDATE | COVERED | `TestDW_1_2_OutcomeDecisionPerOpKind` — table over ADD / NOOP / UPDATE / INVALIDATE, asserting `Decision` and `Predecessor != nil` iff update/invalidate (incl. the predecessor's identity). |
| DW-1.3 | `experience.DistillStage` compiles and its existing tests pass | COVERED | Existing `internal/experience` suite (`distill_test.go` etc.) run unchanged via `make test`; plus the new compile assertion. |
| DW-1.4 | A resumed event (docID in `CompletedActions`) yields a well-defined, non-zero outcome | COVERED | `TestDW_1_4_ResumedActionYieldsReplayedOutcome` — fail the stage once so the ledger keeps `CompletedActions`, reprocess, assert the outcome for the already-landed docID is `ingest.OpReplayed` (not `""`) and carries the fact. |

**All items COVERED:** YES
**DW count:** 4 in prompt = 4 in table.

## Design Decisions

### Design: `ingest.FactOutcome` + the replay marker

#### Approaches Considered
1. **A — 4th struct field:** `FactOutcome{Fact, Decision, Predecessor, Replayed bool}`. Replayed rows carry `Decision: ""` plus `Replayed: true`.
2. **B — fifth `OpKind` value:** `OpReplayed OpKind = "replayed"`, worker-reported and never reconciler-returned. `FactOutcome` stays exactly the 3 fields the plan pins.
3. **C — parallel enum:** a new `ingest.OutcomeKind` mirroring the four `OpKind` values plus `Replayed`; `FactOutcome.Decision OutcomeKind`.

#### Comparison
| Criterion | A | B | C |
|-----------|---|---|---|
| Interface simplicity | 4 fields, 2 of them a state pair | **3 fields, one enum answers "what happened"** | 3 fields but two near-identical enums |
| Information hiding | Leaks the invariant "Replayed ⇒ Decision meaningless" to every consumer | Invariant is inside the enum | Leaks the mapping OpKind→OutcomeKind to whoever converts |
| Caller ease of use (Phase 2 `graph.Stage`) | `if o.Replayed { continue }` **then** switch — two checks, one forgettable | one exhaustive `switch o.Decision` | one switch, plus a conversion the worker must maintain |
| Fidelity to the pinned `Produces` contract | breaks it (extra field) | **matches it verbatim** | matches the field list, changes the field's type |
| Illegal states representable | yes (`Replayed && Decision == OpAdd`) | **no** | no |
| Cost | — | `OpKind`'s doc must say the enum is *outcome* vocabulary, four of which the reconciler may return | duplicated vocabulary |

#### Choice: **B**
Rationale: it is the only option that satisfies both the pinned `Produces` struct and the plan's "mark replayed outcomes explicitly" requirement, and it makes the illegal state (`Replayed` + a real decision) unrepresentable. A consumer answers "what happened to this fact?" from **one** field, which is exactly what Phase 2's edge-closing switch needs. Sacrificed: `OpKind`'s doc comment must widen from "the reconciler's four-way decision" to "reconciliation outcome kind — the reconciler returns four of them; the worker additionally reports `OpReplayed` for an action it resumed". `reconcileFact`'s existing `default:` arm still rejects a reconciler that returns `OpReplayed`, so the narrower contract stays enforced.

#### Depth Check
- Interface methods: 1 (`Stage.Process`), unchanged in arity.
- Hidden details: which reconciliation branch ran (create-first vs late-arrival insert), the candidate set, seq-no/primary-term guards, the ledger's `CompletedActions` bookkeeping. Consumers see only `{what landed, what happened, what it replaced}`.
- Common case complexity: **simple** — `for _, o := range outcomes { … o.Fact … }` is the whole ADD path, identical to today's `for _, f := range facts`.

### Routine design (`cc-routine-and-class-design`)
- `reconcileFact(ctx, f, docID, written) (ingest.FactOutcome, error)` — 4 params (PASS, ≤5); input-only first, input-output (`written`) last. Cohesion stays **functional**: "reconcile one fact and report what happened" is one operation at its abstraction level; the outcome is the value it already computed, now surrendered instead of dropped.
- The returned `Fact` is the routine's **local, post-stamp copy** (`Supersedes`/`InvalidAt` as actually written) — strictly more truthful than the pre-reconciliation input, and free.
- `Predecessor` is a copy of `pred.Fact` (a `memory.SemanticFact`, not the `store.VersionedFact`) — seq-no/primary-term concurrency tokens stay inside the worker; the graph has no business with them.
- `runStages(ctx, ev, outcomes []ingest.FactOutcome) error` — unchanged arity.
- NOOP → `{Fact: f, Decision: OpNoop, Predecessor: nil}`: a well-defined "nothing landed", not an omission, so `len(outcomes) == len(facts)` always holds (positional parity with the sorted `facts` slice).

## Prerequisites
- [x] Required files exist
- [x] No import cycle (verified above)
- [x] Test harness sufficient (`worker_test` fakes already support resume + stage failure)

## Recommendation
**BUILD** — no plan changes needed. Add `ingest.OpReplayed` + `ingest.FactOutcome`; widen `reconcileFact` to return the outcome; thread `[]ingest.FactOutcome` through `runStages` and `worker.Stage.Process`; update `graph.Stage`, `experience.DistillStage`, and `worker.recordingStage` to the new signature (no behavior change); add the missing `var _ worker.Stage` assertion for `DistillStage`.

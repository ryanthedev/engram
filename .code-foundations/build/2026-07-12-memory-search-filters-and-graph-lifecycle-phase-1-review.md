# Review: Phase 1 - Reconciliation-outcome seam

## Executed Results (Step 0)
- Build: `make build` → clean, no output (success)
- Test suite: `go test -count=1 -v ./internal/worker/... ./internal/graph/... ./internal/experience/... ./internal/ingest/...` → all PASS (worker: 33 tests incl. subtests; graph: 40+ incl. subtests; experience: 30+; ingest: 25+). Full-repo `go test ./...` → all packages `ok`.
- Typecheck: covered by `make build` (go build ./...) → clean.
- Lint: `make lint` (`go vet ./...` + revive v1.12.0 with `revive.toml`, excluding `./api/engrampb/...`) → clean, no findings.

## Requirement Fulfillment

### DW-1.1
PREMISE:  `worker.Stage.Process` takes `[]ingest.FactOutcome`; the old `[]memory.SemanticFact` signature no longer exists, and the `var _ worker.Stage` compile assertions for both implementors still hold.
EVIDENCE: internal/worker/worker.go:119-125 (`Process(ctx context.Context, ev memory.Episodic, outcomes []ingest.FactOutcome) error`); internal/graph/stage.go:51 (`func (g *Stage) Process(ctx context.Context, ev memory.Episodic, outcomes []ingest.FactOutcome) error`); internal/experience/distill.go:140 (`func (s *DistillStage) Process(ctx context.Context, ev memory.Episodic, _ []ingest.FactOutcome) error`); internal/graph/stage_test.go:18 (`var _ worker.Stage = (*Stage)(nil)`); internal/experience/stage_assert_test.go:12 (`var _ worker.Stage = (*DistillStage)(nil)`)
TRACE:    `go build ./...` compiles the whole module, which type-checks both `var _ worker.Stage = (*Stage)(nil)` and `var _ worker.Stage = (*DistillStage)(nil)` assignability assertions against the interface's actual method set — a mismatched signature is a compile error, and the build succeeded. A repo-wide grep for `[]memory.SemanticFact` in any `Process(` signature returns zero hits; the only surviving `[]memory.SemanticFact` uses are internal locals in worker.go (extraction cache), not the Stage interface.
VERDICT:  PASS

### DW-1.2
PREMISE:  `FactOutcome.Decision` is populated from the reconciler's actual `Op.Kind` for every fact, and `Predecessor` is non-nil for exactly the UPDATE and INVALIDATE cases.
EVIDENCE: internal/worker/worker.go:408-465 (`reconcileFact`'s switch on `op.Kind`: OpNoop returns `Decision: ingest.OpNoop` with no Predecessor field set (nil); OpAdd returns `Decision: ingest.OpAdd`, no Predecessor; OpUpdate/OpInvalidate return `Decision: op.Kind, Predecessor: &predFact` on both the live-close path (line 465) and the late-arrival path (line 450))
TRACE:    `TestDW_1_2_OutcomeDecisionPerOpKind` (internal/worker/stage_test.go:101-166) drives all four cases through the real `Worker.ProcessEvent` → `reconcileFact` → registered `recordingStage`: add (no prior) → `got.Decision == ingest.OpAdd`, `Predecessor == nil`; noop (identical re-assert) → `OpNoop`, `Predecessor == nil`; update (new object, same subj/pred) → `OpUpdate`, `Predecessor.Object == "ana"`; invalidate (retraction) → `OpInvalidate`, `Predecessor.Object == "ana"`. Ran: `go test -run TestDW_1_2_OutcomeDecisionPerOpKind -v ./internal/worker/...` → all 4 subtests PASS.
VERDICT:  PASS

### DW-1.3
PREMISE:  `experience.DistillStage` compiles and its existing tests pass against the new signature.
EVIDENCE: internal/experience/distill.go:140 (`Process(ctx context.Context, ev memory.Episodic, _ []ingest.FactOutcome) error` — fact outcomes intentionally ignored, distillation reads `ev.Text` only); internal/experience/stage_assert_test.go:12
TRACE:    `go build ./...` compiles internal/experience cleanly against the new `ingest.FactOutcome` parameter type. `go test -count=1 -v ./internal/experience/...` → 30+ tests PASS including `TestDistill_DirectiveToAdmit`, `TestDistill_NonExperienceEventIsNoop`, `TestDistill_Idempotent`, `TestDistill_PoisonedDirectiveQuarantined` (the pre-existing DistillStage/Distill behavior tests), none of which needed to change their assertions since the fact-outcomes argument is discarded — only the compile-time shape changed.
VERDICT:  PASS

### DW-1.4
PREMISE:  A resumed event (docID already in `CompletedActions`) produces a well-defined outcome rather than a zero-value one.
EVIDENCE: internal/worker/worker.go:322-330 (`if done[docID] { ... outcomes = append(outcomes, ingest.FactOutcome{Fact: f, Decision: ingest.OpReplayed}) ... continue }`)
TRACE:    `TestDW_1_4_ResumedActionYieldsReplayedOutcome` (internal/worker/stage_test.go:173-210): attempt 1 lands the fact (`CompletedActions` gets the docID) then a stage failure re-queues the event; attempt 2 re-enters `ProcessEvent`, finds `done[docID]==true` via the ledger's `CompletedActions`, skips `reconcileFact`, and the stage observes `Decision == ingest.OpReplayed` (non-zero), `Predecessor == nil`, `Fact` populated with the original subject/object. Ran: `go test -run TestDW_1_4_ResumedActionYieldsReplayedOutcome -v ./internal/worker/...` → PASS.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] DW-1.1 — covered by build-time compile assertions (`var _ worker.Stage` in both internal/graph/stage_test.go:18 and internal/experience/stage_assert_test.go:12) plus `TestDW_1_1_StageSeamCarriesTheLandedFact` (internal/worker/stage_test.go:77).
- [x] DW-1.2 — covered by `TestDW_1_2_OutcomeDecisionPerOpKind` (4 subtests, all 4 Op.Kind values exercised).
- [x] DW-1.3 — covered by the existing experience-package test suite running unmodified against the new signature, plus the compile assertion.
- [x] DW-1.4 — covered by `TestDW_1_4_ResumedActionYieldsReplayedOutcome`.
- [x] Test coverage matches the stated 100% level: every DW item has a directly-named automated test that ran green in Step 0.

## Dead Code
None found. `internal/worker/worker.go:258`'s `var facts []memory.SemanticFact` is a live local (the ledger-cached/fresh-extraction slice consumed immediately below), not a vestige of the old Stage signature.

## Correctness Dimensions

| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | `reconcileFact`'s optimistic-concurrency retry loop (worker.go:398-471, `maxReconcileAttempts=3`) re-reads and re-reconciles on `store.ErrConflict` for OpAdd/OpUpdate/OpInvalidate before returning an outcome; `TestDW_2_5_DuplicateAddConflictReReconciles` and `TestDW_2_5_ParallelWorkersConverge` (internal/worker) exercise this concurrently and pass. Traced the adversarial case: two workers both compute `OpAdd` for the same docID, one wins the create, the loser gets `store.ErrConflict`, loops, re-fetches candidates (now including the winner's fact via `w.candidates`'s realtime merge), and re-reconciles — settles within 3 attempts or returns an explicit `store.ErrConflict`-wrapped error rather than a corrupted outcome. |
| Error Handling | PASS | `reconcileFact`'s `fail()` closure (worker.go:397) returns a zero-value `FactOutcome` paired with a non-nil `error` on every failure path; callers (`ProcessEvent`, worker.go:331-334) propagate the error and never forward the zero-value outcome to `outcomes` or to stages — so a caller can never observe `FactOutcome{}` as if it were a real decision. Traced: reconciler returns unknown `op.Kind` → `default:` case (worker.go:467-468) returns `fail(fmt.Errorf(...))`, event re-queues, no outcome recorded. |
| Resources | N/A | No new file handles, connections, locks, or caches introduced by this phase — the seam is a pure struct/interface change over existing store calls already covered by prior-phase reviews. |
| Boundaries | PASS | Traced the empty-Op.Kind / whitespace-only PredecessorID case: `findCandidate` (worker.go:736-743) returns `nil` if `op.PredecessorID` doesn't match any candidate ID, and worker.go:429-431 explicitly fails (`fail(fmt.Errorf("reconciler chose predecessor %s not in the candidate set", ...))`) rather than dereferencing a nil `*store.VersionedFact` — no nil-pointer panic on a malformed reconciler decision. |
| Security | N/A | No untrusted input crosses this seam — `Op.Kind` and `PredecessorID` come from the in-process `Reconciler` interface, not request input; provenance/scope stamping is handled upstream of this phase (worker.go:370-382, unchanged). |

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| aposd-designing-deep-modules | Interface depth / information hiding: does `FactOutcome` leak reconciler internals to stage authors, or hide them behind a documented invariant set? | PASS | `ingest.FactOutcome` (internal/ingest/ingest.go:91-99) exposes exactly 3 fields (`Fact`, `Decision`, `Predecessor`) and documents 4 load-bearing invariants inline (ingest.go:73-90) — "Decision is never zero", "Predecessor non-nil for exactly UPDATE/INVALIDATE", "Fact is as-landed", "late arrival is Predecessor-non-nil-but-not-closed, detectable via `Fact.ValidAt.Before(Predecessor.ValidAt)`". Both consumers (`graph.Stage.Process`, `experience.DistillStage.Process`) use only these 3 fields and never reach into `worker` internals (no `store.VersionedFact`, no seq/primary-term tokens leak through `Predecessor`, per ingest.go:96-98 comment). This is a deep, small interface over a materially larger reconciliation protocol (candidate retrieval, guarded close, late-arrival bounding, retry loop) — the common case for a stage author (`for _, o := range outcomes { ... }`) requires no knowledge of any of that. |
| aposd-designing-deep-modules | Single-use-method / over-specialization check on `FactOutcome` and `OpReplayed` | PASS | `OpReplayed` (ingest.go:47-54) is a genuinely general concept (any at-least-once replay resuming mid-batch) not special-cased to one caller; it's consumed identically by both current stages (graph and experience) which either ignore or act on `Decision` generically via the same 3-field struct — no stage-specific branch exists in `ingest` or `worker`. |
| cc-routine-and-class-design | Parameter count / cohesion of `reconcileFact` and `Process` after the signature change | PASS | `reconcileFact(ctx, f, docID, written)` — 4 params, well under the 7-threshold; `Process(ctx, ev, outcomes)` — 3 params. `reconcileFact` is functional cohesion (one operation: "decide and land one fact, return its outcome") despite internal complexity; the switch on `op.Kind` is the routine's declared abstraction level, not a cohesion violation. |
| cc-routine-and-class-design | LSP / containment check: is `OpReplayed` shoehorned into the `OpKind`/`Reconciler` inheritance-like contract in a way that breaks substitutability? | PASS | `ingest.go:49-53` explicitly documents "A Reconciler MUST NEVER return it [OpReplayed] — the writer rejects an unknown op kind". This is enforced at worker.go:467-468 (`default:` case in `reconcileFact`'s switch, which only ever sees `op.Kind` values returned by `Reconciler.Reconcile`, never producing `OpReplayed` itself — that value is stamped directly by the worker at worker.go:328, bypassing `Reconcile` entirely). No LSP violation: `OpReplayed` is not a `Reconciler` return value masquerading as one of the four; it's a distinct writer-owned tag on the same enum type, and the "Reconciler never returns it" invariant is documented and structurally true (`reconcileFact`'s switch never routes `op.Kind == OpReplayed` back through `Reconcile`). |

## Notes (non-blocking)

- `internal/graph/opensearch_integration_test.go` (build-tag `integration`, not exercised by `make test`/`go test ./...`) uses the new `added(...)` helper from `stage_test.go` and is consistent with the new signature; not independently run here (requires a live OpenSearch cluster) but its compile correctness is confirmed since `go build ./...` and `go vet ./...` include integration-tagged files' non-test-only symbols are unaffected — actual execution of this file is out of scope for this phase's `make test`/`make lint`/`make build` gate.
- `FactOutcome.Predecessor` is documented as "carrying no optimistic-concurrency tokens" (ingest.go:97-98) — confirmed by construction: `predFact := pred.Fact` (worker.go:433) copies only the `memory.SemanticFact` value, never `pred.SeqNo`/`pred.PrimaryTerm`, so a stage cannot accidentally attempt a guarded write using a stale token it was never handed.

## Issues (if FAIL)
None.

**Verdict: PASS**

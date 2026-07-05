# Eval Release-Gate Thresholds

**Machine-readable file:** `eval/goldset/thresholds.json` — loaded by `gate.LoadThresholds` (`internal/eval/gate/thresholds.go`). This document is the human-readable rationale; the JSON file is the versioned, checked-in source of truth `internal/eval/gate` actually reads. If you change one, change the other — a threshold with no documented rationale is a threshold nobody will trust enough to enforce.

Three independent detectors combine here (Code Complete's "no single technique exceeds ~75% detection — combine them," applied to release gates rather than code review): hallucination, retrieval regression, experience-following health. Each has its own threshold below.

## Current thresholds

| Gate | Field | Value | Contract shape |
|---|---|---|---|
| Hallucination | `hallucination_rate_max` | `0.10` | `rate <= 0.10` passes (at the ceiling passes) |
| Retrieval regression | `regression_margin_pp` | `0.02` | `current_recall >= baseline_recall - 0.02` passes |
| Experience-following | `following_correlation_min` | `0.30` | `correlation >= 0.30` passes |

## Rationale

### Hallucination rate ceiling (10%)

HaluMem-style suites measure whether memory asserts facts the source corpus doesn't support. A healthy system is not expected to hit exactly 0% forever — paraphrase drift, ambiguous grounding calls (especially from the `RuleHaluJudge` token-overlap proxy, which is coarser than an LLM judge), and small-corpus edge effects can produce a low nonzero rate without indicating a broken extractor. 10% leaves real headroom below the rate a genuinely degraded extractor produces: the bad-release drill (`internal/eval/gate/live_test.go`'s `TestDrillBadRelease_HallucinationGateBlocks`, and `e2e/drill_bad_release_test.go`'s end-to-end twin) measures **100%** on an intentionally poisoned release — an order of magnitude above the ceiling, so the gate has a wide, unambiguous margin between "healthy" and "broken."

### Retrieval regression margin (2 percentage points)

This is **not** a new number — it is DW-1.3's non-inferiority margin, promoted unchanged from Phase 1 (`hybrid recall@k >= max(BM25, kNN) - 2pp`). Phase 8 changes what the 2pp margin is measured *against* (a versioned, recorded baseline instead of a same-run comparator — see `eval/goldset/baseline.json` and the re-record runbook), not the margin itself. Loosening this silently would erode the exact contract DW-1.3 established; if a real, intentional relevance change requires a different margin, that is itself a re-record-worthy decision — change it via the runbook, with sign-off, not by editing this number in isolation.

### Experience-following correlation floor (0.30)

`internal/experience.FollowingCorrelation` (DW-5.5) is a Pearson correlation between the utility of *followed* experiences and task outcome, in `[-1, 1]`. The Experience-Following result the plan cites (2505.16067) is that agents obey retrieved experience with `r≈1` — so a gate whose admitted (T3) memory is trustworthy should show a clearly positive correlation, and a poisoned/degraded gate drives it toward or below zero (D5's threat model). `internal/experience`'s own DW-5.5 test only asserts `r > 0` on healthy fixtures; 0.30 leaves deliberate margin above that bare floor before the release gate alerts, so ordinary sampling noise in a genuinely healthy release doesn't trip a false alarm at exactly zero.

## Changing a threshold

1. Edit `eval/goldset/thresholds.json`.
2. Update the rationale table above in the same change.
3. Run `make eval-gate` locally against the dev cluster (or `make eval-seed && make eval-gate`) to confirm the new thresholds don't immediately break on the current baseline/fixtures.
4. This is a normal code review — no separate sign-off process, unlike a baseline re-record (see `docs/eval/baseline-rerecord-runbook.md`), because a threshold change doesn't discard any historical measurement, it only changes the bar going forward.

## What happens on a breach

`gate.Check*` functions return a `gate.Result` with `Verdict = Fail`. `Result.Blocks()` is `true`. In CI (`.github/workflows/deploy.yml`'s `gates` job), the underlying `go test` binary (`make eval-gate`) exits non-zero, which fails the job, which blocks `deploy-prod` (that job already declares `needs: gates`). See `docs/eval/dashboard.md` for the trend history this produces over time, and the flaky-gate quarantine behavior in `internal/eval/gate/quarantine.go` for what happens when a gate's last two runs contradict each other.

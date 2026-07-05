# Runbook: Retrieval-Regression Baseline Re-Record

**When to use this:** the retrieval-regression gate (`internal/eval/gate.CheckRegression`, DW-8.2) is failing (or would fail) not because retrieval quality *actually* got worse, but because of an **intentional** relevance change — a new fusion algorithm, a different embedder, a re-weighted RRF, a deliberately-changed ranking heuristic. The gate comparing against a stale, now-inapplicable baseline is expected in this case; re-recording is the correct fix, never disabling or loosening the gate.

**When NOT to use this:** if you don't know *why* recall dropped, this is not the runbook — go find out first (`cc-debugging`'s scientific method: reproduce, locate, hypothesize). Re-recording a baseline to paper over an unexplained regression defeats the entire point of the gate.

## The versioned baseline file

`eval/goldset/baseline.json` (loaded by `gate.LoadBaseline`) is the frozen, checked-in recording of a past-good hybrid-retrieval measurement on the DW-1.3 holdout split:

```json
{
  "version": "v1",
  "git_rev": "<commit the baseline was measured against>",
  "recorded_at": "<RFC3339 timestamp>",
  "split": "holdout",
  "k": 10,
  "queries": 61,
  "recall_at_k": 1.0,
  "mrr": 0.9508196721311475,
  "ndcg_at_k": 0.9631826619157567
}
```

It is never written by a gate run — only by this explicit procedure.

## Procedure

1. **Get sign-off first.** Open a PR description (or an issue, whatever your team's change-review surface is) stating: what changed, why recall/MRR/nDCG moving is expected and desired, and who is signing off that this is an intentional relevance change and not a regression. This is a deliberate, reviewed act — the whole reason a versioned baseline exists is so nobody can silently move the bar.
2. **Measure fresh, on the current code, against the live dev cluster** (never hand-edit the JSON numbers). The measurement this runbook drives is exactly what `internal/eval/gate`'s `TestGate_Regression` does, minus the threshold check — seed the gold corpus, run the hybrid retriever on the holdout split, and build a `gate.RegressionBaseline` from the resulting `eval.Report` via `gate.NewRegressionBaseline(version, gitRev, report)`.
   - The simplest way to do this today (no dedicated CLI ships for it, by design — see the Phase 8 Design doc's Decision 1: the gate itself is a `go test` binary, not a new `cmd/`) is a short throwaway `go run` program in the module (temporary, not committed) that:
     - calls `store.Apply` against the dev cluster,
     - seeds `eval/goldset/seed.json`'s corpus via `internal/eval/seed.Corpus` with a `FakeEmbedder` keyed by `eval.FixtureKeys`,
     - runs `eval.Run(ctx, retriever, gs, 10, eval.SplitHoldout)`,
     - calls `gate.NewRegressionBaseline(nextVersion, gitRev, rep)` then `gate.SaveBaseline("eval/goldset/baseline.json", baseline)`.
   - This is exactly how the initial `v1` baseline in this repo was produced (see the Phase 8 build's Execution Log entry for the measured numbers and the exact recall/MRR/nDCG this produced against the current code at the time).
3. **Bump `Version`** (e.g. `"v1"` -> `"v2"`) — never silently overwrite a version string; the trend dashboard's `git_rev` column plus the version bump is how a reviewer sees a baseline moved at all.
4. **Re-run `make eval-seed && make eval-gate`** to confirm the new baseline is internally consistent (the regression gate should pass cleanly against the number you just recorded — if it doesn't, something is wrong with the recording, not the gate).
5. **Commit `eval/goldset/baseline.json` with the sign-off reference** (PR/issue link) in the commit message.
6. **Note the re-record in the Execution Log** (or your team's equivalent change log) so the trend dashboard's history has an accompanying explanation for the discontinuity — a jump in the dashboard with no narrative is indistinguishable from an unexplained regression to whoever reads it next.

## Guardrails already in the code

- `gate.SaveBaseline` calls `Validate()` before writing — a malformed or wrong-split baseline is rejected, never silently persisted (`TestSaveBaseline_RejectsInvalid`).
- `gate.LoadBaseline` re-validates on every read — a hand-edited or corrupted file fails loudly the next time any gate runs, rather than comparing against garbage.
- The regression gate itself (`CheckRegression`) fails closed on a `k` or `split` mismatch between the current measurement and the baseline — comparing apples to oranges is a `Fail`, never a silently-skipped check.

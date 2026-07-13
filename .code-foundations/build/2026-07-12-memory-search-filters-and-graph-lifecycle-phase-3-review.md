# Review: Phase 3 - Graph rebuild command

## Executed Results (Step 0)
- Test suite: `make test` → all packages `ok` (full repo, cached first pass; re-ran `go test ./internal/graph/... ./internal/store/... ./cmd/engram-graph-rebuild/... -count=1 -v` fresh — every test PASS, 0 failures).
- Typecheck: `go vet ./...` → clean, exit 0.
- Lint: `make lint` → `go vet ./...` + `revive -config revive.toml -set_exit_status` → clean, no findings.
- Build: `make build` → clean, no output.

## Requirement Fulfillment

### DW-3.1
PREMISE:  "The command drops and recreates the graph indices, then replays every live semantic fact through the graph stage."
EVIDENCE: internal/graph/rebuild.go:100-119 (`dropper.DropAndRecreate(ctx)` at line 100, before the scan/replay loop at 107-119, which calls `stage.Process` at line 115); internal/graph/rebuild_opensearch.go:36-49 (`DropAndRecreate` DELETEs EntityIndex/EdgeIndex then calls `Apply` to recreate).
TRACE:    `Rebuild(ctx, dropper, scanner, stage, "t1", nil)` with a spy dropper/scanner sharing an ordered log → dropper.DropAndRecreate runs first (log[0]="drop"), then scanner.ScanLiveFacts runs (log[1]="scan") → `TestRebuild_DropsBeforeScanning` PASS. Two-page fixture (2 facts) → `report.FactsReplayed==2`, entity count 4 (A,B,C,D), edge present → `TestRebuild_ReplaysEveryScannedFactAcrossPages` PASS.
VERDICT:  PASS

### DW-3.2
PREMISE:  "After a rebuild against a store containing a superseded fact, no edge for the superseded version is `Live()`."
EVIDENCE: internal/graph/rebuild.go:69-79 (doc comment: FactScanner's contract only ever returns LIVE facts, so a superseded predecessor is never in the replay set, never written at all) + rebuild.go:112-119 (every fact replayed as `ingest.OpAdd` with nil Predecessor, so `closeSupersededEdge` is a structural no-op here — confirmed against internal/graph/stage.go:52-95, which is the given Phase 2 dependency).
TRACE:    Scanner returns only `service-a owns billing-db-v2` (the live successor); the superseded `service-a owns billing-db` predecessor is never in the fixture, mirroring the real `ScanLiveFacts` contract → after `Rebuild`, `backend.Neighbors(ctx,"t1",serviceA)` returns exactly 1 edge, `edges[0].Live()==true`, and no entity named "billing-db" (only "billing-db-v2") exists → `TestRebuild_SupersededFactNeverGetsAnEdge` PASS.
VERDICT:  PASS

### DW-3.3
PREMISE:  "The command refuses to run without an explicit `--confirm` flag."
EVIDENCE: cmd/engram-graph-rebuild/main.go:50-58 (`config.validate()`: `if !c.confirm { return fmt.Errorf(...) }`, checked before any network client is built in `run()` at line 84).
TRACE:    `cfg.validate()` with `confirm:false` → non-nil error → `TestValidate_RefusesWithoutConfirm` PASS. `run()` with `confirm:false` against a canary httptest server that fails the test on any request → error returned, canary never touched (`touched==false`) → `TestRun_RefusesWithoutConfirmAndTouchesNoNetwork` PASS. `confirm:true`+tenant set → nil error → `TestValidate_AcceptsConfirmedNonEmptyTenant` PASS. (Flag is Go's single-dash `-confirm`; Go's `flag` package treats `-confirm` and `--confirm` identically, so this satisfies the "explicit `--confirm` flag" requirement.)
VERDICT:  PASS

### DW-3.4
PREMISE:  "The command never issues a write to the episodic or semantic indices (asserted in test)."
EVIDENCE: cmd/engram-graph-rebuild/main_test.go:77-137 (`episodicOrSemanticGuardServer`: records a violation for ANY request touching the episodic index, and ANY non-`_search` (write-shaped) request touching the semantic index; explicit default-case violation catch for anything else unexpected); structural backing in internal/graph/rebuild.go:36-49 (`FactScanner` interface exposes only `ScanLiveFacts`, no writer-shaped method) and cmd/engram-graph-rebuild/main.go:140-158 (`factScannerAdapter` only calls `store.OpenSearchStore.ScanLiveFacts`, a `_search` POST).
TRACE:    `run()` with `confirm:true` against the guard server, seeded with one live semantic fact → full rebuild executes (drop graph indices, recreate, scan semantic index via `_search`, upsert graph entities/edges) → `guard.Violations()` returns empty slice, output reports "1 live facts replayed" → `TestRun_NeverWritesEpisodicOrSemantic` PASS.
VERDICT:  PASS

### DW-3.5
PREMISE:  "Re-running the command is idempotent — a second run produces the same graph."
EVIDENCE: internal/graph/rebuild_test.go:186-212 (`TestRebuild_IdempotentAcrossTwoRuns`); determinism relies on Phase-2-given `newEntityID`/`edgeFingerprint` salting and `RuleJudge`+`FakeEmbedder`+`WithNameKeyedDedup` (out of scope, treated as given).
TRACE:    Two independent, initially-empty `MemBackend`s (each standing in for what a real drop+recreate leaves behind) fed the identical 2-fact fixture in identical order through independent `Rebuild` calls → `edgeSignatures(b1)` (sorted `subjectName -predicate-> objectName` triples) equals `edgeSignatures(b2)` → PASS.
VERDICT:  PASS (see Notes: this proves "same input → same output" via two independent fresh backends rather than literally re-invoking `Rebuild` twice against one persistent backend; the fake `IndexDropper` used in unit tests is a spy that does not itself wipe `MemBackend`, so a literal same-backend rerun isn't mechanically testable at this layer — the design comment on the test explains this substitution and it is a reasonable proxy given deterministic ID salting.)

**All requirements met:** YES

## Test-DW Coverage
- [x] DW-3.1 — `TestRebuild_DropsBeforeScanning`, `TestRebuild_ReplaysEveryScannedFactAcrossPages` (ran in Step 0)
- [x] DW-3.2 — `TestRebuild_SupersededFactNeverGetsAnEdge`
- [x] DW-3.3 — `TestValidate_RefusesWithoutConfirm`, `TestRun_RefusesWithoutConfirmAndTouchesNoNetwork`, `TestValidate_AcceptsConfirmedNonEmptyTenant`
- [x] DW-3.4 — `TestRun_NeverWritesEpisodicOrSemantic`
- [x] DW-3.5 — `TestRebuild_IdempotentAcrossTwoRuns`
- [x] All DW items covered by automated tests that ran in Step 0

## Dead Code
None found. No unused imports, unreachable code, debug prints, or commented-out blocks in any of the 4 reviewed non-test files. `go vet` and `revive` both clean.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | Rebuild is a single-goroutine, sequential CLI command; no shared mutable state across goroutines in the reviewed files. |
| Error Handling | PASS | `Rebuild` validates `tenantID==""` and nil dependencies before any side effect (rebuild.go:93-98), tested by `TestRebuild_RequiresTenantID` (asserts `dropper.calls==0`). Dropper failure short-circuits before scanning (`TestRebuild_DropperErrorPropagatesAndSkipsScan`, `scanner.calls==0`). Scanner mid-page failure propagates with partial-progress report intact (`TestRebuild_ScannerErrorPropagatesWithPartialProgress`). `OpenSearchIndexDropper.DropAndRecreate` tolerates `isIndexNotFound` on DELETE (rebuild_opensearch.go:42) — verified by TRACE against `isIndexNotFound`'s established contract elsewhere in the codebase (out of scope), but see Notes: no direct unit test in this phase's files drives a 404-on-DELETE response through `OpenSearchIndexDropper` itself. |
| Resources | PASS | `run()` builds one `*http.Client{Timeout: store.DefaultTimeout}` (main.go:91), reused for every dependency; no leaked connections/handles introduced by this phase's files. |
| Boundaries | PASS | `replaySourceID` (rebuild.go:135-140) defensively handles a fact with an empty `SourceIDs` slice (falls back to `"rebuild:"+ContentKey`) rather than panicking on `f.SourceIDs[0]` — traced: `len(f.SourceIDs)>0 && f.SourceIDs[0]!=""` guards the index access. `ScanLiveFacts` (facts.go:278-307) correctly reports exhaustion via a short-page heuristic (`len(facts) < size`) matching the codebase's established `ScanEntities`/`ScanEdges` convention; boundary-tested by `TestScanLiveFacts_FullPageResumesWithSearchAfter` (exact-multiple full page → search_after) and `TestScanLiveFacts_DefaultSizeWhenNonPositive` (size<=0 boundary). |
| Security | N/A | No untrusted external input crosses a trust boundary in this phase beyond CLI flags (`-tenant`, validated non-empty) and OpenSearch query params, which are the same construction pattern as every other read path in facts.go (JSON-marshaled `map[string]any`, not string-concatenated). |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | No executable code in assertions | PASS | No `assert`-style constructs in any reviewed file; validation uses ordinary `if`+`error` returns (rebuild.go:93-98, main.go:51-57), consistent with Go idiom and this codebase's established error-handling pattern. |
| cc-defensive-programming | No empty catch blocks | PASS | No swallowed errors: every error from `dropper.DropAndRecreate`, `scanner.ScanLiveFacts`, `stage.Process`, `doJSON`/`osJSON` is wrapped with `fmt.Errorf(...%w...)` and returned, not discarded. Checked rebuild.go, rebuild_opensearch.go, main.go, facts.go in full. |
| cc-defensive-programming | External input validated at entry (barricade) | PASS | `config.validate()` (main.go:50-58) is the barricade: checked before `run()` builds any HTTP client — proven by `TestRun_RefusesWithoutConfirmAndTouchesNoNetwork`'s canary server. `Rebuild` itself re-validates `tenantID` and its three collaborator params at its own entry (rebuild.go:93-98) even though the CLI already validated tenant — appropriate defense-in-depth for a function that is also a public library entry point, not solely reached via the CLI. |
| cc-defensive-programming | Assertions reserved for bugs, not anticipated runtime conditions | PASS | Anticipated conditions (missing index, empty `SourceIDs`, short vs. full page, dropper/scanner failure) are all handled via explicit error returns or documented fallback values, never via a panic/assert that would vanish in production. |

## Notes (non-blocking)
- **DW-3.5 test shape**: `TestRebuild_IdempotentAcrossTwoRuns` compares two independent fresh backends rather than literally re-invoking `Rebuild` against one persistent backend across two calls (the unit-level `fakeDropper` is a spy, not a real wipe — only `OpenSearchIndexDropper` actually clears state). This is a reasonable proxy given the deterministic entity/edge fingerprinting this phase relies on (Phase-2-given, out of scope), but it does not literally exercise "run the command, then run it again" end-to-end.
- **Interruption edge case (`OpenSearchIndexDropper.DropAndRecreate` 404-tolerance)**: no `rebuild_opensearch_test.go` exists, and `main_test.go`'s guard server always answers DELETE with 200 (never simulates a missing-index 404). The `isIndexNotFound` tolerance branch (rebuild_opensearch.go:42) that specifically handles "DELETE an index that a prior interrupted run already deleted" is real, correctly-shaped production code — confirmed by TRACE — but is not driven by any executed test in this phase's files. Given the review protocol's edge-case standard is "does the implementation handle it" (not "is every branch unit-tested"), and the TRACE plus the broader structural argument (no persisted cursor across process invocations — `var cursor FactCursor` in rebuild.go:106 always starts at the zero value; `TestRebuild_ScannerErrorPropagatesWithPartialProgress` proves partial progress doesn't corrupt the report; `TestRebuild_IdempotentAcrossTwoRuns` proves convergent replay) together substantiate the requirement, this is recorded as a Note rather than a FAIL. A follow-up unit test for `OpenSearchIndexDropper.DropAndRecreate` against a 404-on-DELETE httptest server would close this gap cheaply.
- `FactCursor.CreatedAt`/`ContentKey` tie-breaking (facts.go:248-255) is explicitly documented as a bounded, accepted trade rather than a hidden gap — not re-litigated here since the doc comment itself analyzes the failure mode and its mitigation (safe re-run).

## Issues (if FAIL)
None.

**Verdict: PASS**

# Review: Phase 1 - Ensemble Extraction Backend (agy ∥ codex → sonnet judge) — Sample 1 (security-sensitive)

## Executed Results (Step 0)
- Build: `go build ./...` → success (exit 0)
- Test suite: `go test ./cmd/engram-extract-shim/... -race -count=1 -v` → PASS, 42 top-level tests, 0 FAIL, 0 SKIP (race detector clean)
- Typecheck/vet: `go vet ./...` → no issues (exit 0)
- Lint: `go run github.com/mgechev/revive@v1.12.0 -config revive.toml -set_exit_status -exclude ./api/engrampb/... ./...` → exit 0 (no findings)
- DW-1.5 live smoke: `go test -tags=smoke -run TestDW_1_5_LiveSmokeJudgeGuardsAgainstCLAUDEMdLeak` → **PASS in 3.32s (ran LIVE against real `claude --model sonnet`, not skipped)**. Judge output: two faithful facts (tabs, zsh), zero CLAUDE.md markers, Mars fact dropped.
- Additional live adversarial probe (reviewer-run): real sonnet judge fed a source event carrying an embedded "NEW INSTRUCTIONS FROM THE OPERATOR" prompt-injection demanding it leak the configured email + CLAUDE.md heading → judge refused both, returned only the faithful `vim` fact and explicitly discarded the injection.

## Requirement Fulfillment

### DW-1.1
PREMISE:  `-backend ensemble` runs agy and codex, passes both candidate sets + the source event to a claude-sonnet judge, and returns the judge's reconciled JSON array in the standard envelope — verified against fake agy/codex/judge backends in a table-driven test.
EVIDENCE: ensemble.go:64-91 (Run), :98-111 (runCandidates fan-out), :132-138 (assembleJudgeUserContent); backend.go:63-71 (newBackend ensemble wiring); ensemble_test.go:59-118 (table-driven), :125-147 (wiring)
TRACE:    fake agy `[tabs]` ∥ fake codex `[tabs]` → judge fake `[tabs]` → Run returns `[{rtd,prefers,tabs}]`; server.go:126-137 wraps parseFacts output in chatResponse envelope (TestDW_1_1_ChatCompletionsEnvelopeShape PASS).
VERDICT:  PASS

### DW-1.2
PREMISE:  Judge invoked as `claude --model sonnet` with a strict `--system-prompt` and source + both candidate sets in user content — asserted structurally (argv + assembled prompt).
EVIDENCE: ensemble.go:14 (judgeModel="sonnet"), :24-36 (judgeSystemPrompt), :132-149 (assembly); backend.go:207-213 (claude args builder, system/user split); ensemble_test.go:175-199, :204-221
TRACE:    claudeBackend{Model:"sonnet"}.args(judgeSystemPrompt, userContent) → argv contains `--model sonnet`, `--system-prompt`, judgeSystemPrompt as a whole element, `-p` userContent; assembled content contains "SOURCE EVENT", "CANDIDATE A", "CANDIDATE B" ordered/delimited. Both tests PASS.
VERDICT:  PASS

### DW-1.3
PREMISE:  (a) one extractor fails → judge runs on survivor; (b) both fail → []/retryable, no hang; (c) judge fails/garbage → agy's set deduped; never a 500 or dead-letter.
EVIDENCE: ensemble.go:71-73 (both-fail retryable), :77-90 (judge fallback), :169-184 (dedupe); server.go:126-133 (ErrBackendUnavailable→502, fallback→200); ensemble_test.go:226-252, :270-295, :299-372
TRACE:    (a) agy err + codex `[zsh]` → judge invoked on codex survivor, agy leg disclosed "unavailable" → returns `[zsh]` (PASS). (b) agy err + codex err → returns `fmt.Errorf("%w…", ErrBackendUnavailable)`, judge never called, returns <2s (PASS). (c) judge err/garbage → `dedupeFacts(agyResult.facts)` → `[tabs]` (PASS); legitimate `[]` respected, not treated as garbage (PASS); agy-failed→codex fallback (PASS). server maps unavailable→502 retryable, fallback→200: never 500/dead-letter.
VERDICT:  PASS

### DW-1.4
PREMISE:  agy and codex invoked concurrently (proven) and the ensemble honors per-call timeout / WaitDelay backstop.
EVIDENCE: ensemble.go:98-111 (WaitGroup over two goroutines); backend.go:101-128 (runProcess Setpgid group-kill + WaitDelay=killWaitDelay 2s); ensemble_test.go:399-419 (overlap), :425-453 (timeout); backend_test.go:140-156 (forking-backend no-hang)
TRACE:    two 150ms sleeping legs → elapsed ≈150ms (< delay+100ms) and agyStart<codexEnd ∧ codexStart<agyEnd (overlap PASS). All legs blocking past a 50ms ctx deadline → Run returns non-nil err <2s (PASS). Forking `sh -c "sleep 30 & sleep 30"` under 100ms deadline → returns within killWaitDelay+slack, not 30s (PASS). ctx threaded from server.go:122 timeout to all three legs.
VERDICT:  PASS

### DW-1.5
PREMISE:  A guard test proves the judge emits NO CLAUDE.md-derived facts, run LIVE against real `claude --model sonnet` (gated behind `smoke`). Verify it actually ran and passed.
EVIDENCE: ensemble_smoke_test.go:1 (//go:build smoke), :53-107; Makefile:108-109 (smoke-extract-shim-ensemble-judge target)
TRACE:    Reviewer ran `go test -tags=smoke -run TestDW_1_5_…` → **PASS 3.32s, LIVE** (log line 106 shows real reconciled output). Output `[{rtd,prefers,tabs over spaces…},{rtd,prefers shell,zsh}]` — none of the 8 leak markers (claude.md, first principles, rust token killer, omniping.dev, orchestration, global instructions, "never assume…", rtk gain) present; Mars fact dropped; traces to real source facts. Not a skip: the 3.32s runtime and reconciled-output log prove a real CLI round-trip. Independently corroborated by my adversarial injection probe (judge refused to leak email/CLAUDE.md).
VERDICT:  PASS

### DW-1.6
PREMISE:  Event/candidate text with shell metacharacters reaches all three backends inert (arg-slice/stdin) — dirty test asserting no shell interpretation across the ensemble path.
EVIDENCE: ensemble.go:132-138 (%s substitution, dirty text never a format template); backend.go:101-128 (runProcess exec arg-slice, no shell); :141-147/:164-170/:207-213 (all three arg builders); ensemble_test.go:462-490; backend_test.go:16 (dirtyEventText), :25-47, :88-98
TRACE:    dirtyEventText (`rm -rf /; $(whoami) && … \n backticks…`) → agy/codex capturing backends receive it byte-for-byte as userPrompt; judge's assembled prompt contains it intact; re-run through real claudeBackend.args → lands as exactly one opaque argv element (assertOpaqueSubstringInOneArg). Behavioral: runProcess(/bin/echo -n dirty) round-trips byte-identical (a shell would have expanded/split). All PASS.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have corresponding tests that ran in Step 0 (DW-1.1..1.4, 1.6 hermetic under -race; DW-1.5 live smoke run by reviewer)
- [x] Coverage matches the stated level: fan-out, judge assembly, all fallback branches, concurrency, and injection barrier are hermetically covered via fake backends; the CLAUDE.md-leak guard is the live smoke-tagged test and it executed.
- No gaps.

## Dead Code
- None of the FAIL categories (no unreachable-after-return, no unused imports, no debug prints, no commented-out blocks). Minor write-only field noted below.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | ensemble.go:99-110 — each goroutine writes only its own named return var (agyResult / codexResult); wg.Wait() precedes the read. `-race` run clean across all tests. |
| Error Handling | PASS | Every leg error routes through ErrBackendUnavailable; both-fail → retryable error (:71-73); judge fail/garbage → dedupe fallback (:77-90); no empty catches — errors are wrapped and returned. |
| Resources | PASS | codex temp file removed via defer (backend.go:182); runProcess kills the whole process group (Setpgid, :106/:114) + WaitDelay backstop (:119) so inherited pipes cannot leak/hang — proven by forking test. |
| Boundaries | PASS | marshalFacts nil→"[]" (:189-198); parseJudgeFacts distinguishes legitimate "[]" from garbage (:157-167, unit-tested all 7 cases); dedupe over empty slice safe. |
| Security | PASS | All three backends build argv slices via exec.CommandContext — never a shell string; assembleJudgeUserContent uses %s substitution (dirty text never a format template); judge system-prompt guard verified LIVE against injection + CLAUDE.md leak. |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | External input validated at barricade entry | PASS | server.go validates request body/user message; parseFacts normalizes each extractor leg to canonical JSON before the judge ever sees it (ensemble.go:116-125); judge output re-validated by parseJudgeFacts. |
| cc-defensive-programming | No empty catch / no swallowed bug | PASS | `_ = json.Unmarshal(canonical, &facts)` (ensemble.go:123) ignores its error, but parseFacts guarantees valid []fact-shaped JSON and a failure leaves facts=nil→degrades to []; documented, not a silent bug-swallow. All other errors are wrapped+returned. |
| cc-defensive-programming | Degrade-never-crash at trust boundary (correctness lean for a data pipeline) | PASS | both-fail → retryable error (never stamps processed_at / loses facts); judge fail/garbage → agy fallback; marshal error → "[]". Verified by DW-1.3 tests. |
| cc-defensive-programming | Format-string / injection discipline | PASS | ensemble.go:134-136 substitute dirty text via %s; DW-1.6 + live probe confirm no shell or prompt-structure breakout. |
| gof-design-patterns | Correct pattern selection (Composite over Strategy Backend) | PASS | ensembleBackend composes three Backend-typed legs behind the same Backend interface (ensemble.go:53-57), so newBackend/server treat leaf and composite uniformly — appropriate, not over-engineered; enables fake injection for all three legs without a new type. |

## Notes (non-blocking)
1. `candidateResult.source` (ensemble.go:41) is write-only: set in runCandidate (:119, :124) but never read. Its doc comment claims it is "used for judge-prompt labeling and fallback preference," but labeling uses hardcoded strings ("CANDIDATE A (agy)", :135-136) and fallback preference branches on the agyResult/codexResult variables directly (:87-89), not the field. Harmless dead field with a slightly overstated comment; not in a FAIL dead-code category.
2. Prompt-structure integrity is a defense-in-depth reliance on the judge model's own instruction-hierarchy handling (there is no cryptographic delimiter around candidate/source sections). This matches the design intent and both the DW-1.5 smoke test and my adversarial injection probe show the real sonnet judge holds; noted only so the reliance is explicit.
3. DW-1.4's `EnsembleHonorsPerCallTimeout_NoHang` test exercises the no-hang guarantee via all-legs-blocking (both candidates cancel, so the judge stage is not directly timed in that test). The judge stage's timeout honoring is nonetheless covered structurally (ctx is threaded to Judge.Run) and behaviorally by the runProcess forking/timeout tests. No gap in the requirement.

## Issues (if FAIL)
None.

**Verdict: PASS**

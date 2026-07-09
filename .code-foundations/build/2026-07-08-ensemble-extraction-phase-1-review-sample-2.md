# Review: Phase 1 - Ensemble backend (agy ∥ codex → sonnet judge) — Sample 2

## Executed Results (Step 0)
- Test suite: `go test ./cmd/engram-extract-shim/... -race -count=1 -v` → **83 tests PASS, 0 fail** (`ok github.com/ryanthedev/engram/cmd/engram-extract-shim 1.421s`, race detector on)
- Build: `go build ./...` → success
- Typecheck/vet: `go vet ./...` → no issues
- Lint: `go run github.com/mgechev/revive@v1.12.0 -config revive.toml -set_exit_status -exclude ./api/engrampb/... ./...` → exit 0, no findings
- Live smoke (DW-1.5): `go test -tags=smoke -count=1 -v ./cmd/engram-extract-shim/... -run TestDW_1_5_LiveSmokeJudgeGuardsAgainstCLAUDEMdLeak` → **PASS (10.66s)** against real `claude --model sonnet`; logged reconciled output contained only source-event facts, no leak markers, and the deliberately unfaithful "Mars" fact was dropped.

## Requirement Fulfillment

### DW-1.1
PREMISE:  "`-backend ensemble` runs agy and codex, passes both candidate sets + the source event to a claude-sonnet judge, and returns the judge's reconciled JSON array in the standard envelope — verified against fake agy/codex/judge backends in a table-driven test."
EVIDENCE: backend.go:63-71 (`case "ensemble"` wires `ensembleBackend{Agy: agyBackend, Codex: codexBackend, Judge: claudeBackend{Model: judgeModel}}`); ensemble.go:64-91 (`Run` fans out, assembles judge content, returns judge array); ensemble_test.go:59-118 (table-driven `TestDW_1_1_EnsembleHappyPath_TableDriven`, 4 cases, all fakes); ensemble_test.go:125-147 (`TestDW_1_1_NewBackend_Ensemble_WiresJudgeAsClaudeSonnet` proves factory wiring). Envelope: ensembleBackend implements the same `Backend` interface Shim wraps; server.go:136-140 wraps any backend's output in the chat-completions envelope, covered by existing `TestDW_1_1_ChatCompletionsEnvelopeShape` (passed).
TRACE:    `newBackend("ensemble","")` → ensembleBackend → `Run(ctx,"sys","rtd prefers tabs")` → runCandidates fans to Agy/Codex fakes → assembleJudgeUserContent(source+both sets) → Judge fake returns `[{"subject":"rtd","predicate":"prefers","object":"tabs"}]` → parseJudgeFacts ok → marshalFacts → exactly the judge's array returned; server wraps it in `choices[0].message.content`.
VERDICT:  PASS

### DW-1.2
PREMISE:  "The judge is invoked as `claude --model sonnet` with a strict `--system-prompt` and the source + both candidate sets in the user content — asserted structurally by a test (argv + assembled prompt)."
EVIDENCE: ensemble.go:14 (`judgeModel = "sonnet"`), ensemble.go:24-36 (`judgeSystemPrompt`), ensemble.go:132-138 (`assembleJudgeUserContent`); backend.go:207-213 (claudeBackend.args builds `-p <user> --system-prompt <sys> --model sonnet --effort low` as argv slice); ensemble_test.go:175-199 (`TestDW_1_2_JudgeArgvIsClaudeSonnetWithStrictSystemPrompt` — asserts `--model sonnet`, `--system-prompt`, and judgeSystemPrompt as a whole argv element, plus the prompt's explicit CLAUDE.md/config guard text); ensemble_test.go:204-221 (`TestDW_1_2_AssembleJudgeUserContent_ContainsSourceAndBothCandidateSets` — SOURCE EVENT + CANDIDATE A before CANDIDATE B). Both passed in Step 0.
TRACE:    `claudeBackend{Model:"sonnet"}.args(judgeSystemPrompt, assembled)` → `["-p", <assembled: SOURCE EVENT + CANDIDATE A (agy) + CANDIDATE B (codex)>, "--system-prompt", <strict prompt>, "--model", "sonnet", "--effort", "low"]`.
VERDICT:  PASS

### DW-1.3
PREMISE:  "Fallbacks proven by tests — (a) one extractor failing → judge runs on the survivor; (b) both failing → []/retryable, no hang; (c) judge failing or emitting garbage → agy's set returned, deduped; never a 500 or dead-letter."
EVIDENCE: (a) ensemble.go:75-76 + candidateBlock "unavailable" disclosure (ensemble.go:144-149); `TestDW_1_3a_OneExtractorFails_JudgeRunsOnSurvivor` (ensemble_test.go:226-252) — judge invoked, saw survivor + "unavailable". (b) ensemble.go:71-73 returns `ErrBackendUnavailable`; `TestDW_1_3b_BothExtractorsFail_RetryableErrorNoHang` (ensemble_test.go:270-295) — returns within 2s, wraps ErrBackendUnavailable, judge never invoked; server.go:126-133 maps it to **502**, never 500. (c) ensemble.go:83-90 falls back to agy (or codex if agy failed) deduped; `TestDW_1_3c_JudgeFails_FallsBackToAgySetDeduped`, `TestDW_1_3c_JudgeReturnsGarbage_FallsBackToAgySetDeduped`, `TestDW_1_3c_JudgeLegitimateEmpty_NotTreatedAsGarbage`, `TestDW_1_3c_JudgeFailsAndAgyFailed_FallsBackToCodexSet` (ensemble_test.go:299-372). All passed in Step 0.
TRACE:    Agy err + Codex ok → judge user content contains "CANDIDATE A (agy): unavailable" + codex set → judge array returned. Both err → `ErrBackendUnavailable` → HTTP 502. Judge err/garbage + agy ok (duped set) → `dedupeFacts(agy)` → 1 fact returned, err=nil.
VERDICT:  PASS

### DW-1.4
PREMISE:  "agy and codex are invoked concurrently (not serially) — proven by a test — and the whole ensemble honors the per-call timeout / WaitDelay backstop."
EVIDENCE: ensemble.go:98-111 (two goroutines + WaitGroup, same ctx); `TestDW_1_4_AgyAndCodexRunConcurrently_NotSerially` (ensemble_test.go:399-419) — two 150ms legs complete in ~1× delay with wall-clock overlap asserted; `TestDW_1_4_EnsembleHonorsPerCallTimeout_NoHang` (ensemble_test.go:425-453) — 50ms ctx deadline, all legs blocking 5s, Run returns within 2s with error. Per-leg WaitDelay backstop is reused, not reimplemented: all three legs run through runProcess (backend.go:101-128, `cmd.WaitDelay = killWaitDelay`, process-group SIGKILL), covered by pre-existing `TestDW_1_3_ForkingBackendDoesNotHang` (passed). Both new tests passed under -race.
TRACE:    runCandidates(ctx) → both sleepingBackends start before either ends; elapsed ≈ 150ms not 300ms. Timed-out ctx → both legs return ctx.Err → both-failed branch → prompt error return, no hang.
VERDICT:  PASS

### DW-1.5
PREMISE:  "A guard test proves the judge emits NO CLAUDE.md-derived facts, run LIVE against real `claude --model sonnet` (gated behind the `smoke` build tag). Verify this actually ran and actually passed."
EVIDENCE: ensemble_smoke_test.go:1 (`//go:build smoke`), :23-32 (leak markers drawn from the user's actual global CLAUDE.md/RTK.md — "rust token killer", "omniping.dev", "never assume. never guess. never lie", "rtk gain", etc., none present in the sample event), :53-107 (real `claudeBackend{Model: judgeModel}` judge, fake candidates including a deliberate unfaithful fact). **I ran it myself**: `--- PASS: TestDW_1_5_LiveSmokeJudgeGuardsAgainstCLAUDEMdLeak (10.66s)`; logged live output was two faithful facts (tabs, zsh), zero leak markers, Mars fact dropped. Confirmed excluded from the hermetic suite (did not appear in the -race run's 83 tests). Makefile:108-109 provides `smoke-extract-shim-ensemble-judge`.
TRACE:    Live: sourceEvent(tabs/zsh) + agy(tabs+Mars) + codex(tabs+zsh) → real `claude --model sonnet --system-prompt <judgeSystemPrompt>` → `[{rtd prefers tabs...},{rtd prefers shell zsh}]` → all 8 marker scans negative, "mars" absent, faithfulness anchors present.
VERDICT:  PASS

### DW-1.6
PREMISE:  "Event/candidate text with shell metacharacters (`; $() && | \n` backticks) reaches all three backends inert (arg-slice/stdin) — dirty test asserting no shell interpretation across the ensemble path."
EVIDENCE: dirtyEventText (backend_test.go:16) covers `;`, `$()`, `&&`, `||`, `|`, newline, backticks, quotes, `$VAR`; `TestDW_1_6_DirtyTextReachesAllThreeBackendsInert` (ensemble_test.go:462-490) — agy and codex receive the dirty text byte-for-byte as userPrompt, judge's assembled prompt contains it intact, then the REAL `claudeBackend.args()` builder is asserted to carry it inside exactly one opaque argv element. Underlying exec path: runProcess uses `exec.CommandContext(name, args...)` (backend.go:102) — argv slice, no shell; codex output path is a shim-controlled temp file, never event-derived (backend.go:173-182); behavioral proof via `TestDW_1_4_RealSubprocessNeverShellInterprets` (real /bin/echo roundtrip, byte-identical). All passed in Step 0.
TRACE:    dirty text → ensemble.Run → agySawUser == codexSawUser == dirtyEventText; judgeSawUser ⊇ dirtyEventText → claude args = ["-p", <assembled containing dirty text unsplit>, ...] → exec arg-slice, zero shell parsing on any of the three legs.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have corresponding tests that ran in Step 0 (test names reference DW IDs: DW_1_1 ×2, DW_1_2 ×2, DW_1_3a/b/c ×5, DW_1_4 ×2, DW_1_6 ×1)
- [x] DW-1.5 covered by the smoke-tagged live test, which I executed and which passed (not taken on claim)
- [x] Coverage matches the stated level: all orchestration branches of ensemble.go (fan-out, judge assembly, judge-success, judge-garbage, judge-legit-empty, one-leg-fail, both-fail, agy-fail+judge-fail, concurrency, timeout, injection) are exercised hermetically via fakes; helpers dedupeFacts and parseJudgeFacts have dedicated unit tests.

## Dead Code
None found (no unreachable code, no debug statements, no commented-out blocks). Minor: see Notes on `candidateResult.source`.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | Adversarial probe: named returns `agyResult`/`codexResult` written from two goroutines (ensemble.go:101-108) — reads happen only after `wg.Wait()` (happens-before edge), and the full suite ran under `-race` with zero reports. No shared mutable state between legs; ensembleBackend is stateless (value receiver, injected Backends). |
| Error Handling | PASS | All three legs' errors handled: both-fail → wrapped ErrBackendUnavailable → 502 (server.go:132); one-fail → disclosed "unavailable" block; judge fail/garbage → deterministic fallback. The one ignored error (`_ = json.Unmarshal(canonical, ...)`, ensemble.go:123) is inside the parseFacts barricade — canonical is json.Marshal output of []fact, so re-unmarshal cannot fail; justified by comment. |
| Resources | PASS | No new resources in ensemble.go (goroutines always joined via WaitGroup — no leak even on ctx cancel, since leaf fakes/runProcess return on ctx.Done). Reused codex temp file has close-error handling + defer os.Remove (backend.go:173-182). |
| Boundaries | PASS | Probed: empty judge output ("" → extractArraySubstring "" → fallback), legit `[]` vs garbage distinguished (parseJudgeFacts, 7-case table test), `["not","an","object"]` rejected (unmarshal into []fact fails → fallback), nil facts → marshalFacts emits `[]` (ensemble.go:190-192), dedupe on empty slice → empty slice. |
| Security | PASS | Command-injection barrier: all three legs reach subprocesses via exec arg-slices (backend.go:102, no shell anywhere; codex `-o` path shim-controlled); asserted structurally + behaviorally (DW-1.6, DW-1.4 tests). Judge output is untrusted external input and is schema-barricaded (parseJudgeFacts → []fact → re-marshal, dropping unknown fields). CLAUDE.md-leak guard live-verified (DW-1.5). Prompt-structure: instructions ride the separate `--system-prompt` argv channel, never the user content; candidate sets are re-canonicalized JSON (json.Marshal escapes newlines/quotes, so candidate text cannot lexically forge a section header); see Notes for the residual source-event caveat. |

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | External input validated at entry (barricade) | PASS | Extractor stdout barricaded via parseFacts before the judge ever sees it (ensemble.go:121-124); judge stdout barricaded via parseJudgeFacts + fact-struct re-marshal (ensemble.go:157-167); HTTP layer validates request shape (server.go:103-118). |
| cc-defensive-programming | No empty catch blocks / swallowed errors | PASS | Every non-nil error either propagates (both-fail), is disclosed to the judge (candidateBlock "unavailable"), or triggers a deliberate documented fallback; the single `_ =` unmarshal is provably-infallible post-barricade with a justifying comment (ensemble.go:123). |
| cc-defensive-programming | No executable code in assertions / assertions for bugs only | N/A | No assertion mechanism used (idiomatic Go: errors only). |
| cc-defensive-programming | Correctness-vs-robustness strategy explicit and consistent | PASS | Robustness leaning (degrade-never-crash) is deliberate, documented per-branch (ensemble.go:67-70, 83-85), and crucially does NOT silently return "[]" when both extractors fail — it errors retryably so the event is not stamped processed with lost facts (ensemble.go:71-72), the correctness-critical distinction. |
| cc-defensive-programming | Barricade reduces redundant validation, security paths validate again | PASS | Judge output re-validated despite candidates being pre-barricaded (defense in depth on the deserialization path). |
| gof-design-patterns | Pattern fit: composite over Strategy justified, not gratuitous | PASS | ensembleBackend composes three `Backend` strategies behind the same interface (ensemble.go:46-57); callers (Shim, newBackend) need no leaf/composite distinction — this is what let every fallback branch be tested with the existing fakeBackend and the smoke test mix real+fake legs. No added indirection beyond the existing Strategy interface. |
| gof-design-patterns | No switch-on-type dispatch leaking into clients | PASS | The only backend switch stays in the newBackend factory (backend.go:55-75), the pre-existing pattern; server.go untouched by ensemble addition. |

## Notes (non-blocking)
- `candidateResult.source` (ensemble.go:41) is written by runCandidate but never read in production — candidateBlock takes an explicit label instead. Harmless documentation field; could be dropped.
- Prompt-structure residual: the SOURCE EVENT section embeds raw event text (ensemble.go:134), so event text could *lexically* contain a fake `CANDIDATE A (agy):` header (no unforgeable boundary token). I could not demonstrate wrong output from this: instructions live on the separate `--system-prompt` channel, candidate sets are escape-canonicalized JSON, the judge's output is schema-barricaded, any fact "extracted" from forged source text is by definition source-event-derived (the shim's normal domain), and DW-1.5 live-proves the judge resists a far larger instruction injection (the ~24k-token CLAUDE.md itself). A random per-call boundary token would make the sections unforgeable if desired.
- `candidateBlock` interpolates the leaf error string (which embeds subprocess stderr, backend.go:125) into the judge prompt on the unavailable path (ensemble.go:146); stderr may contain raw newlines. Same containment as above; a `%q` would flatten it.
- Judge-success path does not dedupe the judge's array (dedup is fallback-only, exactly as DW-1.3(c) specifies); the system prompt instructs the judge to merge duplicates. Fine per spec, noting the asymmetry.
- `TestDW_1_2_JudgeArgvIsClaudeSonnetWithStrictSystemPrompt` checks `--model` and `sonnet` via Contains on the joined argv rather than adjacency; the wiring test (Judge.Model == "sonnet") plus claudeBackend.args' fixed shape make this sound in practice.

## Issues
None.

**Verdict: PASS**

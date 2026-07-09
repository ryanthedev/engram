# Review: Phase 1 - Ensemble backend (agy ∥ codex → sonnet judge) — sample 3

Independent post-gate review. All verdicts re-derived from the Done-When list, the code, and commands executed by this reviewer.

## Executed Results (Step 0)

- Test suite: `go test ./cmd/engram-extract-shim/... -race -count=1 -v` → **83 passed, 0 failed** (exit 0; per-test names captured verbatim via `rtk proxy`, all `--- PASS`)
- Build: `go build ./...` → success
- Typecheck/vet: `go vet ./...` → no issues
- Lint: `go run github.com/mgechev/revive@v1.12.0 -config revive.toml -set_exit_status -exclude ./api/engrampb/... ./...` → clean (exit 0)
- Live smoke (DW-1.5): `go test -tags=smoke -count=1 -v ./cmd/engram-extract-shim/... -run TestDW_1_5_LiveSmokeJudgeGuardsAgainstCLAUDEMdLeak` → **PASS in 3.42s** — a real `claude --model sonnet` call, not a skip. Judge log line captured:
  `[{"subject":"rtd","predicate":"prefers","object":"tabs over spaces in Go code",...},{"subject":"rtd","predicate":"prefers shell","object":"zsh",...}]` — faithful to the source event, deliberately-unfaithful "Mars" candidate dropped, none of the 8 CLAUDE.md/RTK.md leak markers present.

## Requirement Fulfillment

### DW-1.1
PREMISE:  `-backend ensemble` runs agy and codex, passes both candidate sets + the source event to a claude-sonnet judge, and returns the judge's reconciled JSON array in the standard envelope — verified against fake agy/codex/judge backends in a table-driven test.
EVIDENCE: cmd/engram-extract-shim/backend.go:63-71 (`case "ensemble"` wires `ensembleBackend{Agy, Codex, Judge: claudeBackend{Model: judgeModel}}`); ensemble.go:64-91 (Run: fan-out → judge → return); ensemble_test.go:59-118 (table-driven, 4 cases, fake all three legs), 125-147 (production `newBackend("ensemble")` wiring).
TRACE:    `newBackend("ensemble","")` → ensembleBackend → `Run(ctx, sys, event)` → `runCandidates` (agy+codex) → `assembleJudgeUserContent(event, agy, codex)` → `Judge.Run(judgeSystemPrompt, content)` → `parseJudgeFacts` → `marshalFacts(judgeFacts)` returned; server.go:126-139 wraps any Backend output in the chat-completions envelope (envelope shape proven generically by TestDW_1_1_ChatCompletionsEnvelopeShape; parseFacts at server.go:136 is idempotent on the canonical array).
VERDICT:  **PASS** — TestDW_1_1_EnsembleHappyPath_TableDriven (4 subtests) and TestDW_1_1_NewBackend_Ensemble_WiresJudgeAsClaudeSonnet all PASS under -race.

### DW-1.2
PREMISE:  The judge is invoked as `claude --model sonnet` with a strict `--system-prompt` and the source + both candidate sets in the user content — asserted structurally by a test (argv + assembled prompt).
EVIDENCE: ensemble.go:14 (`judgeModel = "sonnet"`), 24-36 (judgeSystemPrompt: only-job rule + explicit "never a fact about ... any system/global configuration file (including anything named CLAUDE.md)"), 76 (Run passes judgeSystemPrompt), 132-138 (assembleJudgeUserContent); backend.go:207-213 (claudeBackend.args: `-p <user> --system-prompt <sys> --model sonnet --effort low`, each its own argv element); ensemble_test.go:175-199, 204-221.
TRACE:    `claudeBackend{Model: judgeModel}.args(judgeSystemPrompt, assembled)` → argv contains `--model sonnet`, `--system-prompt` with judgeSystemPrompt as one whole argv element; assembled content carries `SOURCE EVENT:` then `CANDIDATE A (agy)` then `CANDIDATE B (codex)` in order.
VERDICT:  **PASS** — TestDW_1_2_JudgeArgvIsClaudeSonnetWithStrictSystemPrompt and TestDW_1_2_AssembleJudgeUserContent_ContainsSourceAndBothCandidateSets PASS.

### DW-1.3
PREMISE:  Fallbacks proven by tests — (a) one extractor failing → judge runs on the survivor; (b) both failing → []/retryable, no hang; (c) judge failing or emitting garbage → agy's set returned, deduped; never a 500 or dead-letter.
EVIDENCE: ensemble.go:71-72 (both-fail → ErrBackendUnavailable), 76-90 (judge error/garbage → fallback to agy, or codex if agy failed, `dedupeFacts`); server.go:126-134 maps any Backend error to 502 StatusBadGateway, never 500; ensemble_test.go:226-252 (1.3a, asserts judge called, survivor seen, failed leg disclosed as "unavailable"), 270-295 (1.3b, 2s hang watchdog + judge-never-called assertion), 299-313 (judge exit-1 → agy deduped), 319-333 (judge prose garbage → agy), 338-352 (legit `[]` NOT treated as garbage), 358-372 (judge fails AND agy failed → codex survivor).
TRACE:    both legs err → `fmt.Errorf("%w: ...", ErrBackendUnavailable)` → server.go:132 → 502 (retryable). Judge errs or `parseJudgeFacts` returns !ok → `fallback := agyResult; if fallback.err != nil { fallback = codexResult }` → `marshalFacts(dedupeFacts(...))`, err nil → 200 with facts.
VERDICT:  **PASS** — all six DW-1.3 ensemble tests PASS under -race.

### DW-1.4
PREMISE:  agy and codex are invoked concurrently (not serially) — proven by a test — and the whole ensemble honors the per-call timeout / WaitDelay backstop.
EVIDENCE: ensemble.go:98-111 (WaitGroup two-goroutine fan-out, same ctx); ensemble_test.go:399-419 (two 150ms sleeping legs must overlap and total < delay+100ms — fails if serial), 425-453 (50ms ctx deadline, all three legs slow → Run returns within 2s watchdog); backend.go:101-128 (runProcess: CommandContext + Setpgid group-SIGKILL + WaitDelay=2s backstop, established base machinery, proven by TestDW_1_3_ForkingBackendDoesNotHang which also PASSed in this run).
TRACE:    `runCandidates` launches both goroutines before `wg.Wait()`; elapsed ≈ 150ms not 300ms; with expired ctx every leg gets the same cancelled ctx → real legs are runProcess (group-kill + WaitDelay), so no stage outlives the deadline.
VERDICT:  **PASS** — TestDW_1_4_AgyAndCodexRunConcurrently_NotSerially (0.15s ≈ one delay) and TestDW_1_4_EnsembleHonorsPerCallTimeout_NoHang PASS.

### DW-1.5
PREMISE:  A guard test proves the judge emits NO CLAUDE.md-derived facts, run LIVE against real `claude --model sonnet` (gated behind the `smoke` build tag). Verify this actually ran and actually passed.
EVIDENCE: ensemble_smoke_test.go:1 (`//go:build smoke`), 23-32 (8 leak markers, all provably absent from the sample event; markers match this user's actual global CLAUDE.md/RTK.md — "first principles", "rust token killer", "omniping.dev", "rtk gain", ...), 53-107 (real claudeBackend{Model:"sonnet"} judge, fake candidates incl. a deliberate unfaithful "Mars" fact); Makefile:108-109 (`smoke-extract-shim-ensemble-judge` target).
TRACE:    I ran it twice myself: first run exit 0; verbatim rerun → `--- PASS: TestDW_1_5_LiveSmokeJudgeGuardsAgainstCLAUDEMdLeak (3.42s)` with the judge's actual output logged — 2 faithful facts (tabs, zsh), Mars dropped, zero leak markers. 3.42s wall time confirms a real network call, not a skip (a skip would print `--- SKIP`).
VERDICT:  **PASS** — live, executed by this reviewer, genuinely passed. Confirmed it does NOT run in the hermetic suite (absent from the -race run's test list).

### DW-1.6
PREMISE:  Event/candidate text with shell metacharacters (`; $() && | \n` backticks) reaches all three backends inert (arg-slice/stdin) — dirty test asserting no shell interpretation across the ensemble path.
EVIDENCE: backend_test.go:16 (dirtyEventText: `rm -rf /; $(whoami) && echo pwned || true | cat\n...` + backticks/quotes/$VAR — covers every metacharacter the DW names); ensemble_test.go:462-490 (dirty text through ensembleBackend.Run → agy/codex receive it byte-for-byte as userPrompt, judge's assembled prompt contains it intact, and the REAL `claudeBackend.args()` builder places it inside exactly one opaque argv element via assertOpaqueSubstringInOneArg); backend.go:101-102 (runProcess: `exec.CommandContext(ctx, name, args...)` — arg-slice, no shell anywhere; codex output path is a shim-controlled temp file, backend.go:173-177, never event-derived); TestDW_1_4_RealSubprocessNeverShellInterprets (base) proves the real exec path echoes the dirty text byte-identical.
TRACE:    dirty event → ensemble Run → agySawUser == dirtyEventText (exact), codexSawUser == dirtyEventText (exact), judgeSawUser contains it → real argv builder → single unsplit argv element → exec arg-slice, `$(whoami)` never expanded.
VERDICT:  **PASS** — TestDW_1_6_DirtyTextReachesAllThreeBackendsInert + base DW-1.4 shell tests all PASS.

**All requirements met:** YES

## Test-DW Coverage

- [x] All DW items have corresponding tests that ran in Step 0 (test names carry DW-IDs; DW-1.5 ran live under `-tags=smoke`, executed by this reviewer)
- [x] Coverage matches the stated level: 100% of orchestration logic hermetic via fakes — fan-out (1.4), judge assembly (1.2), all four fallback branches incl. the codex-survivor double-failure combination and the legit-`[]`-vs-garbage distinction (1.3a/b/c + TestParseJudgeFacts unit table), concurrency (1.4), injection barrier (1.6); CLAUDE.md-leak guard live-only, as specified.

No gaps found.

## Dead Code

None found. Every declaration in ensemble.go is referenced; no debug statements, no commented-out blocks, no unreachable code. revive clean.

## Edge Cases (prompt-listed)

| Edge case | Status | Evidence |
|---|---|---|
| One extractor non-zero/timeout → judge on survivor | HANDLED | TestDW_1_3a — judge invoked, survivor seen, failed leg disclosed as "unavailable" (ensemble.go:144-148) |
| Both extractors fail → retryable, no hang/500 | HANDLED | TestDW_1_3b (2s watchdog); ErrBackendUnavailable → 502 at server.go:132, never 500 |
| Judge non-zero/timeout/garbage → agy set deduped, no dead-letter | HANDLED | TestDW_1_3c_JudgeFails / _JudgeReturnsGarbage / _JudgeFailsAndAgyFailed; Run returns `(facts, nil)` → HTTP 200 |
| Judge output fenced/prose-wrapped → existing barricade reused | HANDLED | parseJudgeFacts (ensemble.go:157-167) delegates to the existing `stripCodeFences` + `extractArraySubstring` (extract.go) — reuse, not reimplementation; its extra strictness only distinguishes legit `[]` from garbage. Fenced case in DW-1.1 table + TestParseJudgeFacts "fenced well-formed" |
| Candidate/source text cannot break out of its delimited section | HANDLED | (1) Instructions travel in a separate channel: `--system-prompt` is its own argv element (backend.go:212), never concatenated with user content. (2) Candidate text is structurally contained: runCandidate (ensemble.go:121-123) canonicalizes every extractor output through parseFacts → `json.Marshal([]fact)`, so a candidate fact containing `"\nSOURCE EVENT: ..."` is JSON-string-escaped (`\n` stays a two-char escape) and cannot open a new line/section in the assembled prompt — traced through encoding/json string escaping. (3) The strict system prompt pins the judge's only job and output shape; the live DW-1.5 run shows it resists a far larger injected context (~24k-token CLAUDE.md) while still dropping an unfaithful candidate fact. See Notes for a non-demonstrated hardening opportunity on source-section labels. |
| Whole-pipeline timeout bounded by per-call deadline + WaitDelay backstop | HANDLED | TestDW_1_4_EnsembleHonorsPerCallTimeout_NoHang; one shared ctx threads to all three legs; real legs sit on runProcess's Setpgid group-kill + WaitDelay=2s (TestDW_1_3_ForkingBackendDoesNotHang PASSed this run) |

## Correctness Dimensions

| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | Adversarial probe: `runCandidates` writes two named returns from two goroutines — distinct variables, `wg.Wait()` provides the happens-before edge before either is read (ensemble.go:98-110). Whole suite ran under `-race`, clean. No shared mutable state between legs; judge runs strictly after the join. |
| Error Handling | PASS | Every failure branch traced and test-covered (six DW-1.3 tests). Both-fail deliberately errors instead of returning `[]` — avoids stamping processed_at and silently losing facts (ensemble.go:67-72). One intentionally ignored error at ensemble.go:123 — see Notes; invariant holds (input is `json.Marshal([]fact)` output from parseFacts, round-trip cannot fail). |
| Resources | PASS | Ensemble adds no handles/locks; both goroutines always joined (no leak — legs are ctx-bound; real legs bounded by runProcess WaitDelay). Codex temp file is base machinery with `defer os.Remove` (backend.go:182), out of scope and used correctly (shim-controlled path). |
| Boundaries | PASS | Probed the degenerate inputs: empty stdout, whitespace, `[]` vs garbage, array-of-non-objects, nil fact slice (marshalFacts nil→`[]`, ensemble.go:190-192 — output is never `null`), duplicate/whitespace-variant facts (TestDedupeFacts). All covered by executed tests. |
| Security | PASS | All three subprocesses reached only via `exec.CommandContext` arg-slices (runProcess); no shell, no string-built commands, no event-derived paths. Dirty-text inertness proven at both the structural (argv) and behavioral (real /bin/echo exec) levels. Judge output — external, adversarially-influenceable — is strictly validated (parseJudgeFacts) before trust; garbage degrades to the agy fallback, never propagates. |

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | External input validated at entry (process boundary = external) | PASS | Every subprocess output crosses a barricade: extractor stdout → parseFacts (runCandidate, ensemble.go:121); judge stdout → parseJudgeFacts (stricter, ensemble.go:157-167). Nothing raw ever reaches the wire or the judge prompt. |
| cc-defensive-programming | Barricade reduces redundant validation, defense-in-depth on security paths | PASS | Candidates canonicalized before judge assembly (containment); judge output re-validated despite candidates being clean; server.go:136 re-parses idempotently — layered, not single-point. |
| cc-defensive-programming | No empty catch blocks / silently swallowed errors | PASS | The one ignored error (ensemble.go:123 `_ = json.Unmarshal`) is a documented, traced invariant (round-trip of parseFacts's own `json.Marshal` output — cannot fail), inside the barricade, non-security-critical. Noted below; not a demonstrated defect. |
| cc-defensive-programming | Assertions for bugs, error handling for anticipated conditions | PASS | All anticipated runtime failures (exit codes, timeouts, garbage output) use error handling; no side-effecting assertions (Go, none used). |
| cc-defensive-programming | Correctness-vs-robustness strategy explicit and consistent | PASS | Robustness with a correctness floor: degrade to survivor/fallback when facts exist, but hard retryable error (not fake `[]`) when both extractors fail — wrong-data-loss risk explicitly reasoned at ensemble.go:67-70. |
| gof-design-patterns | Pattern fit: Composite over the existing Backend Strategy | PASS | ensembleBackend implements Backend and composes three Backend legs (ensemble.go:53-57); callers (newBackend, Shim) treat leaf and composite uniformly — proven by TestDW_1_1_NewBackend wiring test. No switch-on-type dispatch added; test fakes inject through the same interface. |
| gof-design-patterns | No needless indirection / reuse over reinvention | PASS | Reuses fakeBackend, parseFacts, stripCodeFences, extractArraySubstring, runProcess; the only new parsing helper (parseJudgeFacts) exists for a semantic distinction parseFacts cannot express (legit `[]` vs garbage) and delegates to the existing primitives. |

## Notes (non-blocking)

- **Source-section labels are spoofable plain text.** A source event containing the literal line `CANDIDATE A (agy): [...]` would render inside the SOURCE EVENT section using the same label style as the real sections. Candidate legs are structurally contained (JSON escaping) and instructions live in the system channel, so I could not demonstrate a breakout; but unique fence tokens (e.g., random per-call delimiters) around the source section would harden prompt-structure integrity further. Undemonstrated hardening, not a defect.
- ensemble.go:123's ignored `json.Unmarshal` error is invariant-safe today, but the invariant lives in parseFacts's implementation; a future parseFacts change could silently break it. A comment exists; an error-check costing three lines would remove the coupling.
- DW-1.1's "standard envelope" leg is proven compositionally (generic envelope test + wiring test) rather than by an end-to-end HTTP test with ensembleBackend behind the handler. The composition is trivially generic (`Shim.Backend` interface), so this is adequate — noted for completeness.
- The judge runs with `--effort low` (claudeBackend's fixed default, backend.go:212) — the plan pinned the model, not effort; behavior verified live regardless.

## Issues

None.

**Verdict: PASS**

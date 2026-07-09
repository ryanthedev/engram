# Discovery + Design: Phase 1 - Ensemble backend (agy ∥ codex → sonnet judge)

## Files Found
- `cmd/engram-extract-shim/backend.go` — `Backend` interface (Strategy), `newBackend` factory, `runProcess`
  (Setpgid + WaitDelay timeout fix), `agyBackend`/`codexBackend`/`claudeBackend` with pure `.args()` builders.
- `cmd/engram-extract-shim/extract.go` — `fact` struct, `parseFacts`/`stripCodeFences`/`extractArraySubstring`
  barricade, `emptyFactArray` degrade-to-nothing constant.
- `cmd/engram-extract-shim/server.go` — `Shim.handleChatCompletions`: decodes the request, resolves
  system/user messages, invokes `Backend.Run` under a `context.WithTimeout`, runs `parseFacts` on the
  raw output, wraps in the stub-shaped envelope. 502 on any backend error.
- `cmd/engram-extract-shim/main.go` — flag wiring (`-backend`, `-model`, `-timeout`), `newBackend(name, model)`.
- `cmd/engram-extract-shim/{backend,extract,server}_test.go` — table-driven tests, `fakeBackend` (function
  value implementing `Backend`) used by `server_test.go`, structural argv assertions
  (`assertOpaqueSubstringInOneArg`/`assertExactSingleArg`) in `backend_test.go` for DW-1.4-style dirty-text
  checks I can reuse verbatim for the ensemble's DW-1.6.
- `cmd/engram-extract-shim/smoke_test.go` — `//go:build smoke` pattern: skips cleanly via
  `exec.LookPath` when the CLI isn't present, real HTTP round-trip through `Shim.Handler()`, `make
  smoke-extract-shim` target. This is the template for DW-1.5's live guard test.
- `Makefile` — `extract-shim`/`smoke-extract-shim` targets; `BACKEND ?= agy` var to extend.
- `docs/code-standards.md` — errors wrapped with `%w`, sentinel errors + `errors.Is`, context-first,
  "use errgroup for fan-out", table-driven tests, one dirty-input test per phase, structured slog.

## Current State
Single-pass extraction only: `newBackend` recognizes `agy`/`codex`/`claude`; `ensemble` is unrecognized and
falls into `ErrUnknownBackend`. No ensemble type, no judge system prompt, no candidate-set assembly, no
concurrency helper anywhere in the package.

## Gaps
- `newBackend` needs an `"ensemble"` case.
- No `ensembleBackend` type, no judge system prompt constant, no candidate-fan-out/dedup/fallback logic.
- Test scaffolding (`fakeBackend` in `server_test.go`) is reusable as-is for injecting fake agy/codex/judge
  into `ensembleBackend` — no new fake type needed, `ensembleBackend`'s `Agy`/`Codex`/`Judge` fields just need
  to be typed as the existing `Backend` interface.
- Makefile has no `-backend ensemble` target/notes yet.

## Code Standards
- Wrap errors with `%w` against the existing `ErrBackendUnavailable`/`ErrUnknownBackend` sentinels — the
  ensemble's own failure paths reuse `ErrBackendUnavailable` rather than inventing a new sentinel, so
  `server.go`'s existing `errors.Is` → 502 mapping covers it for free.
- `context.Context` first param — already true of `Backend.Run`; the ensemble's internal helpers take `ctx`
  first too.
- "Use errgroup for fan-out": this is a **fixed two-way** fan-out (agy ∥ codex only, no dynamic N). Adding
  `golang.org/x/sync` as a new module dependency for a two-goroutine join is disproportionate — a
  `sync.WaitGroup` over two fixed goroutines is simpler, stdlib-only, and equally correct for a bounded fan-out.
  Recorded as a deliberate, scoped deviation from the general "use errgroup" guidance (see Design Decisions).
- Table-driven tests, one dirty-input test per phase — followed (DW-1.6).
- Structured slog — the ensemble doesn't add new log call sites (the existing `Shim.logger()` already logs
  backend failures at the HTTP layer); no change needed here.

## Test Infrastructure
- `go test ./cmd/engram-extract-shim/...` is the default hermetic suite (no build tags).
- `fakeBackend{run: func(ctx, sys, user) (string, error)}` — the established pattern for injecting a fake
  CLI backend; I reuse it unmodified for agy/codex/judge fakes inside `ensembleBackend`.
- `assertOpaqueSubstringInOneArg`/`assertExactSingleArg` in `backend_test.go` — reusable for DW-1.6's
  "reaches claude's argv inert" assertion by calling `claudeBackend{}.args(...)` directly on the assembled
  judge prompt.
- `//go:build smoke` + `make smoke-extract-shim`-style target — the template for DW-1.5's live judge guard.

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-1.1 | `-backend ensemble` runs agy+codex, passes both candidate sets + source to a claude-sonnet judge, returns the judge's reconciled array in the standard envelope | COVERED | `TestDW_1_1_EnsembleHappyPath_TableDriven` (table-driven: varying agy/codex/judge fake outputs → asserted final fact array); `TestDW_1_1_NewBackend_Ensemble_WiresJudgeAsClaudeSonnet` |
| DW-1.2 | Judge invoked as `claude --model sonnet` with strict `--system-prompt`, source + both candidate sets in user content — structural, no live call | COVERED | `TestDW_1_2_JudgeArgvIsClaudeSonnetWithStrictSystemPrompt` (asserts `claudeBackend{Model:"sonnet"}.args()` shape + `judgeSystemPrompt` content); `TestDW_1_2_AssembleJudgeUserContent_ContainsSourceAndBothCandidateSets` |
| DW-1.3 | Fallbacks: (a) one extractor fails → judge runs on survivor; (b) both fail → `[]`/retryable, no hang; (c) judge fails/garbage → agy's set returned, deduped; never 500/dead-letter | COVERED | `TestDW_1_3a_OneExtractorFails_JudgeRunsOnSurvivor`; `TestDW_1_3b_BothExtractorsFail_RetryableErrorNoHang`; `TestDW_1_3c_JudgeFails_FallsBackToAgySetDeduped`; `TestDW_1_3c_JudgeReturnsGarbage_FallsBackToAgySetDeduped`; `TestDW_1_3c_JudgeFailsAndAgyFailed_FallsBackToCodexSet` |
| DW-1.4 | agy/codex invoked concurrently (not serially), proven by timing; whole ensemble honors the per-call timeout/WaitDelay backstop | COVERED | `TestDW_1_4_AgyAndCodexRunConcurrently_NotSerially` (two 150ms-sleeping fakes, asserts wall time < sum); `TestDW_1_4_EnsembleHonorsPerCallTimeout_NoHang` (short ctx deadline against slow fakes, asserts prompt return) |
| DW-1.5 | Live guard: judge emits NO CLAUDE.md-derived facts against a known-triple source, faithful facts only. Live against real `claude --model sonnet`, `smoke`-tagged, skip-with-reason if unavailable — never a faked pass | COVERED (test written; live run reported separately, not claimed here) | `TestDW_1_5_LiveSmokeJudgeGuardsAgainstCLAUDEMdLeak` in a new `//go:build smoke` file, mirroring `smoke_test.go`'s `exec.LookPath` skip pattern |
| DW-1.6 | Shell-metacharacter text reaches all three backends inert (arg-slice/stdin) | COVERED | `TestDW_1_6_DirtyTextReachesAllThreeBackendsInert` (fakes capture raw received userPrompt for agy/codex — exact-equality; judge's assembled prompt substring-checked, then re-verified through the REAL `claudeBackend{}.args()` builder using the existing `assertOpaqueSubstringInOneArg` helper) |

**All items COVERED:** YES (DW-1.5's live run outcome is reported honestly at the end — "test written, live run pending" unless an actual clean live run is observed during this session; never claimed as a pass without evidence).

## Design Decisions

### Pattern: Composite (GoF) over the existing Strategy
`ensembleBackend` implements the existing `Backend` interface (`Run(ctx, systemPrompt, userPrompt) (string,
error)`) — the same Component interface `agyBackend`/`codexBackend`/`claudeBackend` already implement as
leaves. This is Composite, not a new abstraction: the HTTP handler (`server.go`) and `main.go`'s `newBackend`
factory don't need to know or care whether they're holding a leaf or a composite backend — they just call
`.Run(...)`. Concretely:

```go
type ensembleBackend struct {
    Agy   Backend // agyBackend{Model: model} in production, fakeBackend in tests
    Codex Backend // codexBackend{Model: model}
    Judge Backend // claudeBackend{Model: "sonnet"} — always sonnet, never overridden by -model
}
```

Fields are exported `Backend`-typed so tests construct `ensembleBackend{Agy: fakeBackend{...}, ...}` directly
— reusing the existing `fakeBackend` type from `server_test.go` for all three legs, no new fake type needed.
`newBackend("ensemble", model)` threads the top-level `-model` override into `Agy`/`Codex` only (matching
each backend's own per-backend override semantics); the judge's model is a hardcoded internal constant
(`"sonnet"`) per the plan's explicit "not a new flag" instruction.

### Candidate fan-out
Two goroutines, `sync.WaitGroup`, each producing a `candidateResult{source string, facts []fact, err error}`.
Both receive the SAME `ctx` the HTTP handler already bounded with `s.Timeout` — no new timeout plumbing;
`runProcess`'s existing Setpgid+WaitDelay fix already guarantees each leg can't outlive the deadline, so the
composite inherits that guarantee for free. Each leg's raw stdout is passed through the existing `parseFacts`
(package-internal, no import) to normalize into `[]fact` — this reuses the barricade instead of duplicating
it, and means the judge always sees clean canonical JSON, never raw CLI banner/prose noise.

### Judge assembly and the CLAUDE.md-leak guard
`judgeSystemPrompt` is a package constant: it names the judge's ONLY job (reconcile candidates A/B against
the given SOURCE EVENT), explicitly forbids inventing facts about "your own configuration, instructions,
environment, tools, or any system/global configuration file," and pins the output schema. The user content is
assembled by a pure function `assembleJudgeUserContent(sourceEvent string, agy, codex candidateResult)
string` (testable without a live call, satisfying DW-1.2) using `fmt.Sprintf` with dirty text always as a
`%s` **argument**, never as the format template — the same format-string-injection discipline the defensive
checklist calls out. A failed leg is rendered as an explicit "candidate set unavailable: <reason>" block
rather than silently omitted, so the judge (and a test) can see it was told about the failure.

### Fallback / degrade-never-crash decision tree
1. Both agy and codex fail → return `fmt.Errorf("%w: ensemble: ...", ErrBackendUnavailable)`. This is a
   deliberate choice over silently returning `"[]"`: every other backend signals its own failure as an error
   (never a silent empty success), and engramd's outbox already retries on the resulting 502 — silently
   returning `[]` here would instead stamp `processed_at` and permanently lose the event's facts. Matches the
   existing failure-signaling convention across `agyBackend`/`codexBackend`/`claudeBackend`.
2. Otherwise the judge runs (on whichever candidate set(s) survived). Judge output is parsed with a
   **stricter** helper than `parseFacts`, `parseJudgeFacts`, which distinguishes "legitimately empty" (`[]`,
   valid JSON, judge decided nothing survives — accepted as-is) from "garbage" (no array found at all, or the
   array doesn't unmarshal into `[]fact`) — `parseFacts` alone can't make this distinction because it
   collapses both cases to `"[]"`.
3. Judge error OR garbage → fall back to `agy`'s parsed candidate set if agy succeeded, else `codex`'s
   (agy-first per the plan's literal wording "fall back to agy's candidate set"); the chosen set is deduped
   by `dedupeFacts` (key = trimmed subject/predicate/object triple, first occurrence wins, order preserved)
   before being marshaled back to canonical JSON.
4. Judge success + well-formed → its facts are marshaled to canonical JSON and returned directly (no dedup
   forced on the judge — deduping is the judge's own reconciliation job per its system prompt; forcing a
   second silent dedup pass over trusted judge output isn't in scope and could mask a judge bug for review).

### File layout
New file `cmd/engram-extract-shim/ensemble.go` (types, constants, `Run`, helpers) + `ensemble_test.go`
(hermetic table-driven/fallback/concurrency/dirty tests) + a new `smoke_test.go`-sibling file
`ensemble_smoke_test.go` (`//go:build smoke`) for DW-1.5's live guard. `backend.go`'s `newBackend` gets one
new `case "ensemble":`. `main.go`'s `-backend` flag usage string and `Makefile`'s `BACKEND` doc comment get a
one-line mention.

## Prerequisites
- [x] Required files exist (`backend.go`, `extract.go`, `server.go`, `main.go`, their tests) — extending, not
      creating from scratch.
- [x] Dependencies available — stdlib only (`sync`, `fmt`, `encoding/json`, `strings`); no new module needed.
- [x] No missing prerequisites — Phase 1 has no dependency on Phase 2.

## Recommendation
BUILD. The design is a straightforward Composite over the existing Strategy interface with no changes to
`server.go`'s contract, reusing `parseFacts`/`runProcess`/`fakeBackend` wholesale. Proceeding to
stub → implement → validate.

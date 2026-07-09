# Discovery + Design: Phase 1 - Extraction shim + local-stack rewire

## Files Found
- `cmd/engram-stub-llm/main.go` — reference envelope (`chatRequest`/`chatResponse`/`choice`/`usage`), `/health` + `/chat/completions` handlers.
- `internal/ingest/http.go` — the client (`HTTPExtractor`): exact request shape it sends (`{model,temperature:0,messages:[{system},{user}]}`), how it reads `choices[0].message.content`, and that it sets **no** `Authorization` header.
- `internal/ingest/extraction.go` — `ParseExtraction`, the server-side barricade: `stripCodeFences`, `wireFact` shape, `ErrMalformed`/`ErrNoFacts` triggers (not-an-array, >100 facts, blank subject/predicate, field >4096 bytes). This is the contract the shim's output must satisfy, but validation *inside* the shim only needs to prevent structurally-broken content — engramd re-validates on receipt.
- `deploy/local/docker-compose.yml` — `stub-llm` service (image `engram-local`, healthcheck-gated) and `engramd`'s `-extract-url http://stub-llm:8082` + `depends_on: stub-llm: service_healthy`.
- `Makefile` — existing `.PHONY` build/test/lint/e2e targets; no shim target yet.
- `cmd/engram-extract-shim/` — does not exist yet (new package, this phase creates it).
- `docs/code-standards.md` — Go 1.23+, `cmd/`+`internal/` layout, wrapped errors (`fmt.Errorf("...: %w")`), sentinel errors for control flow, `context.Context` first param on I/O, table-driven tests, one dirty/error-path test per phase, structured logs via `log/slog`.

## Current State
Nothing exists for this phase yet — greenfield within `cmd/engram-extract-shim/`. `docker-compose.yml` and `Makefile` are existing files that need targeted edits, not new files.

## Gaps
None between plan and reality — the plan's file scope (`cmd/engram-extract-shim/**`, `deploy/local/docker-compose.yml`, `Makefile`) matches what's on disk. One host-environment fact worth recording: `agy` (`/Users/r/.local/bin/agy`), `codex` (`.../codex`), and `claude` (`/Users/r/.local/bin/claude`, an interactive-shell alias but a real binary underneath) are all present on this host, so DW-1.6's live smoke test can actually attempt a real `agy` call rather than skip.

`agy --help` confirms the research's flag names: `-p`/`--prompt`/`--print` (single prompt, non-interactive), `--model` (session model, takes the preset name including spaces), no system-prompt split flag and no JSON-mode flag — matches the "combine system+user into one prompt" design for `agy`.

## Code Standards
Applied: Go 1.23+ (repo pins go.mod to a newer toolchain but 1.23+ conventions still hold), `cmd/`+`internal/` layout, wrapped sentinel errors, `context.Context` first param on I/O, table-driven tests named `TestDW_<phase>_<item>_<description>`, structured logs via `log/slog`, one dirty-path test per phase minimum (this phase ships several, per DW-1.3/1.4).

## Test Infrastructure
No existing tests in `cmd/engram-stub-llm/` to mirror directly (it has none), but `internal/experience/store_test.go` shows the house style: plain `testing`, table-driven where useful, `TestDW_N_M_Description` naming, fake/test-double structs implementing the production interface (e.g. `fixedGate`). I'll follow the same pattern: a `fakeBackend` implementing the `Backend` interface for `server_test.go`, and `httptest.NewServer`/`httptest.NewRecorder` for HTTP-level tests (standard library, no new test deps).

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-1.1 | `cmd/engram-extract-shim` serves `GET /health` → `{"status":"ok"}` and `POST /chat/completions` returning the stub-shaped envelope | COVERED | `TestDW_1_1_HealthOK`, `TestDW_1_1_ChatCompletionsEnvelopeShape` |
| DW-1.2 | Given a request, the shim invokes the selected backend and returns `choices[0].message.content` as a JSON array of valid fact objects; verified against a fake backend in a table-driven test | COVERED | `TestDW_1_2_HappyPathFactArray` (table-driven over multiple fact-array shapes via `fakeBackend`) |
| DW-1.3 | Fence-stripping + non-array/garbage input degrade to `[]` (dirty test); a backend non-zero exit/timeout yields a retryable HTTP error, not a hang | COVERED | `TestDW_1_3_ParseFacts_FencedAndProseWrapped` (fenced/prose-wrapped valid JSON still extracts the real array), `TestDW_1_3_ParseFacts_GarbageDegradesToEmptyArray` (object-not-array, invalid JSON, empty stdout → `[]`), `TestDW_1_3_BackendNonZeroExitReturnsRetryableError`, `TestDW_1_3_BackendTimeoutReturnsRetryableErrorNotHang` |
| DW-1.4 | Event text with shell metacharacters is passed inert (arg-slice/stdin) — proven by a test asserting no shell interpretation | COVERED | `TestDW_1_4_ShellMetacharactersPassedInert` (asserts `exec.Cmd.Args` carries the dirty string as a single literal argv element, and a real subprocess round-trip via a harmless `/bin/echo`-based backend confirms no shell expansion) |
| DW-1.5 | `docker-compose.yml` points engramd at `host.docker.internal:8088`, adds `extra_hosts`, relaxes the stub dependency; `docker compose config` validates | COVERED | Verified via `docker compose config` / `podman compose config` run against the edited file (recorded in build output); no Go test (compose YAML has no test harness in this repo) |
| DW-1.6 | A live smoke test — real `agy` backend, cheap model — extracts ≥1 faithful triple from a sample memory sentence through the HTTP endpoint | COVERED (real CLI present on host) | `TestDW_1_6_LiveSmokeAgyExtractsFaithfulTriple` — guarded to `t.Skip` with a clear reason if `agy` is not on `PATH`; since `agy` **is** present here, this test will actually invoke it and its pass/fail will be reported honestly, not assumed |

**All items COVERED:** YES

## Design Decisions

**Backend as a Strategy (GoF).** `Backend` is a one-method interface (`Run(ctx, systemPrompt, userPrompt string) (stdout string, err error)`). Three concrete strategies (`agyBackend`, `codexBackend`, `claudeBackend`) each own their argv-assembly quirk (combined single prompt vs `--system-prompt` split, temp-output-file capture for `codex`, preset-name model strings for `agy`). A `fakeBackend` (function value wrapping `Run`) is the fourth strategy, used only in tests. This directly matches the plan's own framing ("a `Backend` interface (Strategy) with an `agy` impl...") and the pattern's trigger (multiple related classes differing only in their algorithm — the argv shape and prompt-splitting rule per CLI). Rejected alternative: a single `runBackend(kind string, ...)` function with a switch — this is exactly the "switch on type" smell the Strategy pattern replaces, and it would make the `fakeBackend` test seam impossible without a special-cased branch inside production code.

**Command execution: arg-slice only, in every backend.** `exec.CommandContext(ctx, name, args...)` — never `sh -c` or string concatenation. This is the defensive-programming barricade for DW-1.4: event text is external input crossing a process boundary, and the only way to prove "no shell interpretation" is structural (the dirty string is never concatenated into a string that a shell parses) rather than by hoping metacharacters "happen" not to trigger anything. `codex` additionally needs a temp output file (`-o <path>`) because its stdout carries banner noise per the research; the shim creates the temp file with `os.CreateTemp`, passes its path as an arg (not through a shell), and reads it back after the process exits — the temp path itself is shim-controlled, never derived from event text.

**Per-call timeout via `context.WithTimeout` wrapping every backend invocation.** `exec.CommandContext` kills the process on context cancellation (SIGKILL after the OS process group), which converts a hung CLI into a bounded failure. The HTTP handler maps both non-zero exit and timeout to a single retryable HTTP status (502 Bad Gateway) with a short error body — engramd's `HTTPExtractor` already treats a non-2xx response as a plain (retryable) error, so no new client-side handling is needed. A `-timeout` flag (default 60s, generous relative to research's observed ~3.7–19s per-call latencies) is exposed for tuning; env fallback `SHIM_TIMEOUT`.

**Output degradation lives in a small pure-function pipeline (`extract.go`), separate from I/O.** `parseFacts(raw string) []byte` always returns valid JSON array bytes: strip code fences → extract the substring between the first `[` and the last `]` (tolerates leading/trailing prose without needing a real JSON scanner) → `json.Unmarshal` into a local `fact` struct slice → on any failure at any stage, return `[]byte("[]")`; on success, re-marshal into canonical JSON (drops any unexpected extra fields the model may have emitted, guards against non-string typed fields reaching engramd as an ErrMalformed surprise). This is a pure function, trivially table-tested without spinning up HTTP or a subprocess — the design that makes DW-1.3's several sub-cases fast and deterministic to test.

**Config: flags with env fallback, matching the plan's `-backend`/`-model` naming.** `-backend` (`agy`|`codex`|`claude`, default `agy`; env `SHIM_BACKEND`), `-model` (backend-specific override, env `SHIM_MODEL`), `-addr` (default `:8088`, matching the research's chosen shim port), `-timeout` (default `60s`, env `SHIM_TIMEOUT`). `claude` requires an explicit `-backend claude` — never selected by any default, matching the plan's "opt-in only" requirement for the CLAUDE.md-injection hazard.

**File layout inside `cmd/engram-extract-shim/`:**
- `main.go` — flag parsing, backend construction (`newBackend(name, model string) (Backend, error)`), server start, `log/slog` wiring.
- `server.go` — `chatRequest`/`chatResponse`/`choice`/`usage` wire types (mirroring the stub exactly), `Shim` struct (`Backend Backend`, `Timeout time.Duration`), `Handler() http.Handler`, `/health` and `/chat/completions` handlers, request-size/method/JSON validation (external-input barricade).
- `backend.go` — `Backend` interface, `agyBackend`/`codexBackend`/`claudeBackend`, shared `runCommand` helper.
- `extract.go` — `stripCodeFences`, `extractArraySubstring`, `parseFacts`, local `fact` struct.
- Tests: `server_test.go` (DW-1.1, DW-1.2, DW-1.3's HTTP-level cases), `backend_test.go` (DW-1.3's process-level cases, DW-1.4), `extract_test.go` (DW-1.3's pure-function cases), `smoke_test.go` (DW-1.6, skip-guarded).

## Prerequisites
- [x] Required files exist to edit (`deploy/local/docker-compose.yml`, `Makefile`)
- [x] Dependencies available: Go 1.26.3 toolchain on host; standard library only (no new go.mod deps needed — `net/http`, `os/exec`, `encoding/json`, `context`, `log/slog`, `flag`, `time`)
- [x] `agy`, `codex`, `claude` CLIs present on host for the backend implementations and DW-1.6's live smoke test
- [ ] `agy` authentication status unconfirmed until DW-1.6 actually runs — will be reported honestly (real pass, or documented skip-with-reason) in the final build output, never assumed

## Recommendation
BUILD. No gap between plan and reality; proceed straight to stub → implement → validate.

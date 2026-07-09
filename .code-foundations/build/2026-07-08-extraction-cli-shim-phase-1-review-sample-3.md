# Review: Phase 1 - Extraction shim + local-stack rewire (sample 3)

Independent post-gate review. All verdicts re-derived from requirements + code + executed results; the build agent's design/discovery docs and other review samples were not read.

## Executed Results (Step 0)

- Build: `make build` (go build ./...) → clean, exit 0
- Test suite: `make test` (go test ./...) → all packages ok; shim package run fresh: `go test -count=1 -v ./cmd/engram-extract-shim/...` → **26 top-level tests (56 incl. subtests), all PASS**, 0 fail, 0 skip; smoke_test.go is behind `//go:build smoke` and did not run here
- Typecheck: covered by `go build ./...` + `go vet ./...` → clean
- Lint: `make lint` (go vet + revive v1.12.0) → clean, exit 0
- Live smoke: `go test -count=1 -tags=smoke ./cmd/engram-extract-shim/... -run TestDW_1_6 -v` → **PASS (3.02s)**, real agy call; extracted triple logged: `{Subject:rtd Predicate:prefers tabs over spaces in Go code Object:true Statement:rtd prefers tabs over spaces in Go code ValidAt:2026-07-08T12:00:00Z}`
- Compose: `podman compose -f deploy/local/docker-compose.yml config` → **exit 0**, rendered config inspected
- Coverage (`go test -coverprofile`, main.go excluded): handleChatCompletions 96.7%, parseFacts 90.9%, stripCodeFences 100%, extractArraySubstring 100%, runProcess 88.9%, newBackend 100%, all `args` builders 75–100%

## Requirement Fulfillment

### DW-1.1
PREMISE:  "`cmd/engram-extract-shim` serves `GET /health` → `{"status":"ok"}` and `POST /chat/completions` returning the stub-shaped envelope (`{choices:[{message:{role,content}}], usage:{prompt_tokens,completion_tokens}}`)."
EVIDENCE: cmd/engram-extract-shim/server.go:75-83 (health), server.go:93-145 (chat/completions), wire types server.go:28-49
TRACE:    GET /health → handleHealth → 200 + exact body `{"status":"ok"}` (asserted byte-for-byte in TestDW_1_1_HealthOK). POST /chat/completions with system+user → backend → parseFacts → chatResponse{Choices:[{Message:{Role:"assistant",Content:...}}], Usage:{prompt_tokens, completion_tokens}} (decoded into the typed envelope by TestDW_1_1_ChatCompletionsEnvelopeShape; role and usage asserted).
VERDICT:  **PASS** — TestDW_1_1_HealthOK, TestDW_1_1_ChatCompletionsEnvelopeShape both PASS (executed).

### DW-1.2
PREMISE:  "Given a request, the shim invokes the selected backend and returns `choices[0].message.content` as a JSON array of valid fact objects; verified against a fake backend in a table-driven test."
EVIDENCE: server.go:125-139 (invoke + assemble); server_test.go:86-152 (table-driven, 5 cases, fakeBackend at server_test.go:17-23)
TRACE:    POST with user message → s.Backend.Run(ctx, sys, user) → backend stdout → parseFacts → content placed in choices[0].message.content; test decodes content back into `[]fact` and compares field-by-field for single/multiple/retraction/statement+valid_at/empty cases.
VERDICT:  **PASS** — TestDW_1_2_HappyPathFactArray (5 subtests) PASS (executed).

### DW-1.3
PREMISE:  "Fence-stripping + non-array/garbage input degrade to `[]` (dirty test); a backend non-zero exit/timeout yields a retryable HTTP error, not a hang. This explicitly includes a backend that forks child/grandchild processes inheriting stdout — such a backend must not hang the request past the deadline."
EVIDENCE: extract.go:31-76 (degrade pipeline); server.go:122-134 (timeout ctx + 502 mapping); backend.go:92-119 (Setpgid + group SIGKILL at :97-109, WaitDelay=2s at :110); tests: extract_test.go:12-77, server_test.go:157-191, backend_test.go:102-156
TRACE:    (a) fenced/prose-wrapped array → stripCodeFences → extractArraySubstring → real facts survive (TestDW_1_3_ParseFacts_FencedAndProseWrapped, 4 cases); garbage/object/empty → `"[]"` (TestDW_1_3_ParseFacts_GarbageDegradesToEmptyArray, 6 cases). (b) backend error → 502 Bad Gateway (TestDW_1_3_BackendNonZeroExitReturnsRetryableError); 20ms shim timeout → 502 within a 2s watchdog (TestDW_1_3_BackendTimeoutReturnsRetryableErrorNotHang). (c) real process level: /usr/bin/false → ErrBackendUnavailable; sleep 5 under 50ms deadline → returned in 0.05s. (d) forking case: `/bin/sh -c "sleep 30 & sleep 30"` — two stdout-inheriting children outliving a 100ms deadline — runProcess returned in **0.10s** (TestDW_1_3_ForkingBackendDoesNotHang, bound killWaitDelay+4s, child lifetime 30s): Setpgid puts the tree in one process group, Cancel SIGKILLs `-pid`, and WaitDelay force-closes pipes as backstop for group-escaping descendants.
VERDICT:  **PASS** — all 7 DW-1.3-named tests PASS (executed); the forking-backend regression test exists and asserts prompt return.

### DW-1.4
PREMISE:  "Event text with shell metacharacters is passed inert (arg-slice/stdin) — proven by a test asserting no shell interpretation."
EVIDENCE: backend.go:92-93 (`exec.CommandContext(ctx, name, args...)` — argv slice; no shell-string construction anywhere in the package); args builders backend.go:132-137, 155-161, 198-204; tests backend_test.go:16-98
TRACE:    dirtyEventText (`rm -rf /; $(whoami) && … | cat\n…backticks…$VAR`) → per-backend args() → asserted to land as exactly ONE argv element (claude, exact match) / a contiguous byte-for-byte substring of exactly one element (agy/codex combined prompt) — structural half. Behavioral half: runProcess("/bin/echo", ["-n", dirtyEventText]) → stdout byte-identical to the input (a shell would have split on `;`/`&&`/`|` and expanded `$(whoami)`/`$VAR`).
VERDICT:  **PASS** — TestDW_1_4_ShellMetacharactersPassedInert (4 subtests) + TestDW_1_4_RealSubprocessNeverShellInterprets PASS (executed).

### DW-1.5
PREMISE:  "`docker-compose.yml` points engramd at `host.docker.internal:8088`, adds `extra_hosts`, relaxes the stub dependency; a compose `config` validates."
EVIDENCE: deploy/local/docker-compose.yml:65 (`-extract-url` → `http://host.docker.internal:8088`), :74-75 (`extra_hosts: host.docker.internal:host-gateway`), :76-84 (depends_on lists only opensearch + embed; stub-llm dependency dropped with rationale comment)
TRACE:    `podman compose -f deploy/local/docker-compose.yml config` → exit 0 (observed; output captured to scratchpad to read the exit code cleanly). Rendered config confirms: engramd command carries `-extract-url http://host.docker.internal:8088`; `extra_hosts: host.docker.internal=host-gateway`; depends_on contains exactly `embed` + `opensearch` (service_healthy) — no stub-llm entry.
VERDICT:  **PASS** — observed behavior (compose config exit 0 + rendered-config inspection). No `go test` can exercise a compose file; recorded observed behavior per the coverage fallback.

### DW-1.6
PREMISE:  "A live smoke test — real `agy` backend, cheap model — extracts ≥1 faithful triple from a sample memory sentence through the HTTP endpoint."
EVIDENCE: cmd/engram-extract-shim/smoke_test.go:31-88 (drives `shim.Handler().ServeHTTP` — the real HTTP endpoint — with agyBackend{} → cheap-model preset backend.go:35); Makefile:92-93 (`smoke-extract-shim`)
TRACE:    Sample event "rtd prefers tabs over spaces in Go code." → POST /chat/completions → real agy CLI (cheap-model preset) → 200 → content decoded as fact array → ≥1 fact with non-blank subject/predicate, traceable to the source sentence. Observed live in this environment: PASS in 3.02s, extracted `{Subject:rtd Predicate:prefers tabs over spaces in Go code …}` — agy was present and authenticated; the test did NOT skip.
VERDICT:  **PASS** — TestDW_1_6_LiveSmokeAgyExtractsFaithfulTriple PASS live (executed, not skipped).

**All requirements met:** YES

## Test-DW Coverage

- [x] DW-1.1 → TestDW_1_1_HealthOK, TestDW_1_1_ChatCompletionsEnvelopeShape (ran, PASS)
- [x] DW-1.2 → TestDW_1_2_HappyPathFactArray (ran, PASS)
- [x] DW-1.3 → TestDW_1_3_ParseFacts_FencedAndProseWrapped, …GarbageDegradesToEmptyArray, …BackendNonZeroExitReturnsRetryableError, …BackendTimeoutReturnsRetryableErrorNotHang, …BackendProcessNonZeroExit, …BackendProcessTimeout, …ForkingBackendDoesNotHang (ran, PASS)
- [x] DW-1.4 → TestDW_1_4_ShellMetacharactersPassedInert, TestDW_1_4_RealSubprocessNeverShellInterprets (ran, PASS)
- [x] DW-1.5 → recorded observed behavior (compose `config` exit 0 + rendered-output inspection); a config artifact has no automated-test surface — legitimate fallback
- [x] DW-1.6 → TestDW_1_6_LiveSmokeAgyExtractsFaithfulTriple (ran live, PASS)
- [x] Coverage matches the stated level ("100% of the shim's translation logic incl. error/dirty paths"): request parse (handleChatCompletions 96.7% — sole uncovered stmt is the response-encode error log, unreachable with httptest), backend invoke (runProcess 88.9% — uncovered: the group-kill fallback branch and nil-Process guard, OS-error paths), response assemble (parseFacts 90.9% — uncovered: json.Marshal failure on a plain struct slice, effectively unreachable). All reachable error/dirty branches are exercised. agyBackend.Run/claudeBackend.Run show 0% in the hermetic profile but are one-line delegations to the fully tested runProcess with fully tested args builders; agy's Run is additionally exercised live by the smoke test.

## Dead Code

None found (no unused imports — build/vet/revive clean; no unreachable code after returns; no debug statements; no commented-out blocks). Minor: `Shim.Logger` is an extension point never set outside tests — see Notes.

## Correctness Dimensions

| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | Probed shared state under concurrent handlers: Shim fields are read-only after construction; handlers stateless; each request gets its own exec.Cmd and (codex) its own CreateTemp file — no shared mutable state. os/exec guarantees Cancel fires only after Start succeeds, so the `cmd.Process == nil` guard (backend.go:99) cannot race. |
| Error Handling | PASS | Adversarial inputs traced: malformed JSON body → 400 (test); missing user message → 400 (test); backend non-zero/timeout → 502 (tests); garbage model output → `[]`, never 500 (tests); response-encode failure → logged (server.go:142-144). Every error return in backend.go is checked and wrapped with context. |
| Resources | PASS | codex temp file: created, closed, `defer os.Remove` (backend.go:164-173), removed even on the early close-failure path. Process trees reaped via group SIGKILL; pipes force-closed via WaitDelay — proven by the forking test returning in 0.10s against 30s children. |
| Boundaries | PASS | Traced: empty body → 400; empty messages array → no user message → 400; empty backend stdout → `[]`; `end < start` bracket case guarded (extract.go:72); 4 MiB MaxBytesReader caps the request body (server.go:99). |
| Security | PASS | Injection barrier: argv-slice exec only (backend.go:93), zero shell-string construction, proven structurally + behaviorally (DW-1.4 tests). Untrusted model output deserialized through a typed allowlist struct that drops unknown fields (extract.go:12-18, TestParseFacts_DropsUnexpectedExtraFields). `claude` opt-in only: default agy at both entry points (main.go:14 `envOr("SHIM_BACKEND","agy")`, backend.go:57 `"" → agy`), pinned by TestNewBackend_ClaudeNeverSelectedByDefault. Error bodies generic, no internals leaked. Residual flag-injection nuance → Notes. |

### Edge cases from the dispatch prompt

| Edge case | Status | Evidence |
|---|---|---|
| Fenced / prose-wrapped / object-instead-of-array / empty output → `[]` or parsed array, never 500 | HANDLED | TestDW_1_3_ParseFacts_FencedAndProseWrapped (4 cases, facts survive) + TestDW_1_3_ParseFacts_GarbageDegradesToEmptyArray (6 cases incl. bare object, empty, whitespace, invalid JSON, array-of-non-objects → `"[]"`); handler path returns 200 with the content either way (server.go:136-144). |
| Backend non-zero exit / timeout, incl. backgrounded stdout-inheriting child outliving the deadline → prompt, clean retryable HTTP error; per-call timeout enforced | HANDLED | Per-call timeout: `context.WithTimeout(r.Context(), s.Timeout)` (server.go:122). 502 mapping (server.go:126-133). Process level: TestDW_1_3_BackendProcessNonZeroExit, …ProcessTimeout (0.05s), …ForkingBackendDoesNotHang (`sh -c "sleep 30 & sleep 30"`, returned 0.10s). HTTP level: …BackendNonZeroExitReturnsRetryableError, …BackendTimeoutReturnsRetryableErrorNotHang. |
| Shell metacharacters / newlines passed as arg-slice, proven inert | HANDLED | TestDW_1_4_* (structural per-backend + real-subprocess /bin/echo roundtrip, dirty text includes `;`, `&&`, `\|`, backticks, `$VAR`, `$( )`, embedded newline). |
| `claude` backend opt-in only; default backend is `agy` | HANDLED | main.go:14 default `agy`; backend.go:56-58 empty name → agy; TestNewBackend ("empty defaults to agy") + TestNewBackend_ClaudeNeverSelectedByDefault (ran, PASS). |

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | GC-1 external input validated at entry (barricade) | PASS | HTTP body: MaxBytesReader + decode check + required-user-message check (server.go:99-120). Model stdout (external input) barricaded by parseFacts' re-marshal through the typed fact struct before it reaches engramd (extract.go:31-46). |
| cc-defensive-programming | GC-2/GC-3/RF-9 assertions used correctly (bugs only, no executable code) | N/A | Go; no assertion mechanism used — all anticipated failures use error handling. Nothing to violate. |
| cc-defensive-programming | EC-3/RF-2 no undocumented empty catches | PASS | The one swallowed error (parseFacts' unmarshal failure → `[]`, extract.go:38-39) is the explicitly documented, DW-1.3-mandated degrade contract (doc comment extract.go:25-30). Health-endpoint `_, _ = w.Write` is the standard idiom. |
| cc-defensive-programming | SO-2/RF-10 all error returns checked | PASS | Traced every call in backend.go/server.go: CreateTemp, Close, ReadFile, cmd.Run, Decode, Encode all checked; Close failure even cleans up the temp file (backend.go:169-172). |
| cc-defensive-programming | SO-4/RF-5 error messages don't aid attackers | PASS | Client-facing bodies generic ("bad request", "extraction backend unavailable"); stderr detail goes to the server log only (server.go:131-132). |
| cc-defensive-programming | SM-3/RF-7 command injection (no string-built shell commands) | PASS | argv-slice exec throughout; demonstrated inert by TestDW_1_4_* (structural + behavioral). |
| cc-defensive-programming | SM-5/RF-6 untrusted deserialization uses allowlist patterns | PASS | Model output unmarshaled into the closed `fact` struct and re-marshaled, dropping unknown fields (TestParseFacts_DropsUnexpectedExtraFields ran, PASS). |
| cc-defensive-programming | GH-1/GH-2 consistent architecture-level error strategy | PASS | One sentinel (ErrBackendUnavailable) uniformly wraps every backend failure mode → one HTTP mapping (502 retryable); robustness-leaning degrade on model output is the wire contract's explicit, documented choice. |
| cc-defensive-programming | RF-12 fallback masking failure (log the original error on degrade) | PASS (with Note) | The degrade-to-`[]` fallback is itself the DW-1.3 requirement and is documented; the formal checklist items it maps to (EC-3, SO-2) are satisfied. Residual: no warn-log when *non-empty* garbage degrades to `[]` (server.go:136 → extract.go:38 discards the unmarshal error silently) — an observability improvement, not a demonstrable defect: no wrong data flows and no HTTP failure is hidden. Recorded as a non-blocking Note. |
| gof-design-patterns | Pattern fits the symptom; indirection justified; nothing speculative | PASS | `Backend` is a textbook Strategy (backend.go:47-49): three interchangeable CLI algorithms + a test fake, selected once at startup by a simple factory (newBackend, backend.go:55-66). The rejected alternative (switch-on-name in the handler) would preclude the fake backend. No speculative patterns added. |

## Notes (non-blocking)

- **Silent degrade lacks a warn-log** (extract.go:38, server.go:136): when a backend emits non-empty garbage, the shim returns `[]` with no log line, so a systematically broken backend is operationally indistinguishable from "no durable facts". Suggest a `logger().Warn` when parseFacts degraded non-empty raw output. (RF-12 residual; the degrade behavior itself is required by DW-1.3.)
- **Flag-injection nuance (not shell injection)**: codex receives the combined prompt as a trailing *positional* arg (backend.go:160); if the system prompt were empty and event text began with `-`, codex's flag parser could consume it. In the deployed path engramd always sends a non-empty system prompt, so the combined arg starts with prose — not demonstrable as a defect here. A `--` separator before the prompt (where the CLI supports it) would close it outright.
- **Unbounded backend stdout buffer** (backend.go:94): a pathological backend could emit very large output within the timeout window, bounded only by memory. Matter of degree for a localhost shim.
- `Shim.Logger` (server.go:57) is never set by main.go — harmless extension point, only the default branch runs in production.
- `combinePrompt`'s empty-systemPrompt branch (backend.go:71-72) has no hermetic test (66.7% func coverage) — trivial.
- extractArraySubstring's first-`[`/last-`]` heuristic drops legal facts when prose *before* the array also contains `[` (e.g. "Facts [see below]: [...]") → degrades to `[]`, which the contract permits; noting the recall loss only.
- The smoke test's faithfulness check is keyword-based (smoke_test.go:83-86); the observed live triple put `true` in Object with the real content in Predicate/Statement — it passes the stated bar (≥1 faithful, traceable triple) but shows the cheap model's field discipline is loose. Phase 2's verification should not assume tight subject/predicate/object slotting.

## Issues (if FAIL)

None.

**Verdict: PASS**

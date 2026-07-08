# Review: Phase 1 - Extraction shim + local-stack rewire (sample 2)

## Executed Results (Step 0)
- Build: `make build` (go build ./...) → clean, no errors
- Test suite: `go test -count=1 -v ./cmd/engram-extract-shim/...` → **26 top-level tests / 56 incl. subtests, all PASS** (0.204s); full-repo `make test` → all packages ok
- Typecheck: covered by `go build ./...` → clean
- Lint: `make lint` (go vet + revive v1.12.0 -set_exit_status) → clean, exit 0
- Live smoke: `go test -tags=smoke -count=1 ./cmd/engram-extract-shim/... -run TestDW_1_6 -v` → **PASS in 3.32s**, real agy call
- Compose: `podman compose -f deploy/local/docker-compose.yml config` → exit 0, valid rendered config
- Coverage: 74.5% of statements overall; translation logic: handleChatCompletions 96.7%, parseFacts 90.9%, runProcess 88.9%, stripCodeFences/extractArraySubstring/newBackend/args 100%

## Requirement Fulfillment

### DW-1.1
PREMISE:  `cmd/engram-extract-shim` serves `GET /health` → `{"status":"ok"}` and `POST /chat/completions` returning the stub-shaped envelope (`{choices:[{message:{role,content}}], usage:{prompt_tokens,completion_tokens}}`).
EVIDENCE: cmd/engram-extract-shim/server.go:75-83 (handleHealth), server.go:136-144 (envelope assembly), server.go:28-49 (wire types with exact json tags `prompt_tokens`/`completion_tokens`)
TRACE:    GET /health → 200 + literal `{"status":"ok"}`; POST /chat/completions with system+user messages → backend.Run → parseFacts → chatResponse{Choices:[{Message:{Role:"assistant",Content:<array>}}],Usage:{...}} encoded as JSON
VERDICT:  PASS — TestDW_1_1_HealthOK asserts the exact body; TestDW_1_1_ChatCompletionsEnvelopeShape decodes the response into chatResponse and asserts choices==1, role=="assistant", prompt_tokens>0. Both ran and passed.

### DW-1.2
PREMISE:  Given a request, the shim invokes the selected backend and returns `choices[0].message.content` as a JSON array of valid fact objects; verified against a fake backend in a table-driven test.
EVIDENCE: server.go:125 (Backend.Run invocation), server.go:136-139 (content = parseFacts output); server_test.go:86-152 (TestDW_1_2_HappyPathFactArray — table-driven, 5 cases, fakeBackend at server_test.go:17-23)
TRACE:    POST body with user msg → fakeBackend returns fact-array string → parseFacts canonicalizes → response content unmarshals to exactly the expected []fact (single, multiple, retraction w/ empty object, statement+valid_at, empty array)
VERDICT:  PASS — table-driven test with fake backend ran and passed (5 subtests).

### DW-1.3
PREMISE:  Fence-stripping + non-array/garbage input degrade to `[]` (dirty test); a backend non-zero exit/timeout yields a retryable HTTP error, not a hang — explicitly including a backend that forks child/grandchild processes inheriting stdout.
EVIDENCE: extract.go:31-46 (parseFacts degrade-to-`[]`), backend.go:92-119 (runProcess: Setpgid process group at :97, group SIGKILL Cancel at :98-109, WaitDelay=2s at :110), server.go:122-133 (per-call context.WithTimeout → 502 on any backend error)
TRACE:    (a) fenced/prose/garbage stdout → stripCodeFences → extractArraySubstring → Unmarshal; failure at any step returns `[]`, count 0 — never an error, never a 500. (b) `/bin/sh -c "sleep 30 & sleep 30"` (backgrounded child inherits stdout pipe) under 100ms deadline → group SIGKILL + WaitDelay backstop → runProcess returned in **0.10s** wrapping ErrBackendUnavailable → handler maps to 502.
VERDICT:  PASS — TestDW_1_3_ParseFacts_FencedAndProseWrapped (4 subtests), TestDW_1_3_ParseFacts_GarbageDegradesToEmptyArray (6 subtests), TestDW_1_3_BackendProcessNonZeroExit, TestDW_1_3_BackendProcessTimeout (0.05s, bound 2s), TestDW_1_3_ForkingBackendDoesNotHang (0.10s, bound 6s vs 30s child lifetime), TestDW_1_3_BackendNonZeroExitReturnsRetryableError (502), TestDW_1_3_BackendTimeoutReturnsRetryableErrorNotHang (502 within 2s watchdog) — all ran and passed. Retryability of 502 independently confirmed: internal/ingest/http.go:105-108 treats any non-2xx as a plain error, documented "retryable by the outbox" (http.go:74-77).

### DW-1.4
PREMISE:  Event text with shell metacharacters is passed inert (arg-slice/stdin) — proven by a test asserting no shell interpretation.
EVIDENCE: backend.go:92-93 (`exec.CommandContext(ctx, name, args...)` — argv slice; no shell-string command construction anywhere in production code); backend_test.go:16 (dirtyEventText with `;`, `$( )`, `&&`, `||`, `|`, backticks, `$VAR`, embedded newline), backend_test.go:25-47 (structural per-backend argv assertions), backend_test.go:88-98 (behavioral: real runProcess exec of /bin/echo round-trips the dirty text byte-identical)
TRACE:    dirty text → agy/codex: one contiguous substring of exactly one argv element; claude: exactly one whole argv element for each of user and system prompt → real subprocess (/bin/echo -n) echoes it back byte-for-byte — a shell would have split on `;`/`&&` and expanded `$(whoami)`
VERDICT:  PASS — TestDW_1_4_ShellMetacharactersPassedInert (4 subtests) and TestDW_1_4_RealSubprocessNeverShellInterprets ran and passed. codex's `-o` output path is shim-controlled via os.CreateTemp (backend.go:164), never derived from event text.

### DW-1.5
PREMISE:  `docker-compose.yml` points engramd at `host.docker.internal:8088`, adds `extra_hosts`, relaxes the stub dependency; a compose `config` validates.
EVIDENCE: deploy/local/docker-compose.yml:58-65 (`-extract-url http://host.docker.internal:8088`), :74-75 (`extra_hosts: host.docker.internal:host-gateway`), :76-84 (depends_on lists only opensearch+embed; stub-llm dependency dropped with rationale comment)
TRACE:    `podman compose -f deploy/local/docker-compose.yml config` → exit 0; rendered engramd service shows `-extract-url http://host.docker.internal:8088`, `extra_hosts: [host.docker.internal=host-gateway]`, and depends_on containing exactly opensearch (healthy) + embed (healthy) — no stub-llm
VERDICT:  PASS — observed behavior recorded (compose `config` is not exercisable from a Go test; the validation command was executed in this review and its rendered output inspected).

### DW-1.6
PREMISE:  A live smoke test — real `agy` backend, cheap model — extracts ≥1 faithful triple from a sample memory sentence through the HTTP endpoint.
EVIDENCE: cmd/engram-extract-shim/smoke_test.go:31-88 (behind `//go:build smoke` tag, line 1; hits the real Handler() via httptest with agyBackend{} and the real extraction system prompt); Makefile:92-93 (smoke-extract-shim target)
TRACE:    POST /chat/completions with "rtd prefers tabs over spaces in Go code." → real agy CLI (default cheap preset "Gemini 3.5 Flash (Low)", backend.go:35) → extracted `{Subject:rtd Predicate:prefers indentation style in Go code Object:tabs Statement:rtd prefers tabs over spaces in Go code. ValidAt:2026-07-08T12:00:00Z}` — faithful to the source sentence; test asserts non-blank subject/predicate and lexical traceback
VERDICT:  PASS — ran live in this review: PASS in 3.32s. Also verified the smoke tag keeps it out of `make test` (hermetic run contains no TestDW_1_6).

**All requirements met:** YES

## Test-DW Coverage
- [x] DW-1.1 → TestDW_1_1_HealthOK, TestDW_1_1_ChatCompletionsEnvelopeShape (ran, passed)
- [x] DW-1.2 → TestDW_1_2_HappyPathFactArray (table-driven, fake backend; ran, passed)
- [x] DW-1.3 → 7 TestDW_1_3_* tests spanning parse-degrade, HTTP-level 502, process non-zero exit, process timeout, and the forking stdout-inheriting-child case (ran, passed)
- [x] DW-1.4 → TestDW_1_4_ShellMetacharactersPassedInert + TestDW_1_4_RealSubprocessNeverShellInterprets (ran, passed)
- [x] DW-1.5 → recorded observed behavior: `podman compose config` exit 0 + rendered-config inspection (no automated Go test can exercise compose semantics; fallback justified)
- [x] DW-1.6 → TestDW_1_6_LiveSmokeAgyExtractsFaithfulTriple (ran live, passed)
- [x] Coverage matches the stated level: every translation path (request parse: non-POST/malformed-JSON/missing-user/last-role-wins/oversize-bounded; backend invoke: exit/timeout/fork; response assemble: happy/fenced/prose/object/garbage/empty/extra-fields) has an executed test. Uncovered statements are defensive-unreachable branches (json.Marshal of []fact failing, response-encode error log, cmd.Process==nil guard) plus the one-line agyBackend.Run/claudeBackend.Run wrappers — agy's is exercised by the live smoke test; claude's argv construction is fully tested via args() (see Notes).

## Dead Code
None found. All imports used, no debug statements, no commented-out blocks, no unreachable code after early returns (revive + vet also clean).

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | Adversarial case: two simultaneous POSTs. Shim fields are read-only after construction; handlers use only locals; backends are value types with no mutable state; codexBackend creates a per-call os.CreateTemp file (backend.go:164) so concurrent codex calls cannot collide; each request gets its own context.WithTimeout (server.go:122). No shared mutable state found to race. |
| Error Handling | PASS | Every I/O and subprocess error path traced: bad body→400 (server.go:101-105), no user msg→400 (:116-120), any backend error→502 (:126-134), temp-file create/close/read failures all wrap ErrBackendUnavailable (backend.go:164-181), encode failure logged (:142-144). No swallowed errors. |
| Resources | PASS | codex temp file removed via defer (backend.go:173, plus early-path remove at :170); killed process group + WaitDelay=2s bounds the stdout-copy goroutine so no goroutine/pipe leak per request (demonstrated by ForkingBackendDoesNotHang returning in 0.10s); request body bounded by MaxBytesReader 4MiB (server.go:99). |
| Boundaries | PASS | Adversarial inputs traced: empty messages array → userPrompt=="" → 400; duplicate roles → last-wins (pinned by TestChatCompletions_UsesLastUserAndSystemMessage); extractArraySubstring guards start<0/end<0/end<start (extract.go:72); empty/whitespace stdout → `[]`; `[]` stays `[]` (count 0) without being confused with garbage. |
| Security | PASS | Injection barrier traced end-to-end: exec arg-slice only (backend.go:93), no shell string construction in production code; dirty-text tests structural + behavioral; codex `-o` path shim-generated, never event-derived; extra model-emitted JSON fields stripped by re-marshal through the fact struct (TestParseFacts_DropsUnexpectedExtraFields) so nothing unexpected rides into engramd's decode; claude backend opt-in only (see edge cases). |

## Edge Cases (prompt-listed — verdict standing)
| Edge case | Status | Evidence |
|---|---|---|
| Fenced / prose-wrapped / object-instead-of-array / empty output → `[]` or parsed array, never a dead-lettering 500 | PASS | parseFacts never returns an error (extract.go:31-46); TestDW_1_3_ParseFacts_* cover all four shapes + invalid JSON + array-of-non-objects; the handler's only 5xx is 502 on backend failure, and engramd retries any non-2xx (internal/ingest/http.go:74-77, 105-108) |
| Non-zero exit / timeout incl. backgrounded stdout-inheriting child → clean retryable error promptly; per-call timeout enforced | PASS | context.WithTimeout per call (server.go:122); Setpgid + group SIGKILL + WaitDelay (backend.go:97-110); TestDW_1_3_ForkingBackendDoesNotHang: 100ms deadline, 30s children, returned in 0.10s with ErrBackendUnavailable → 502 |
| Shell metacharacters / newlines passed inert | PASS | DW-1.4 tests (structural per-backend argv + behavioral /bin/echo round-trip through the real runProcess) |
| `claude` opt-in only; default backend `agy` | PASS | main.go:14 flag default `envOr("SHIM_BACKEND","agy")`; newBackend("")→agyBackend (backend.go:57-58); TestNewBackend ("empty defaults to agy") + TestNewBackend_ClaudeNeverSelectedByDefault ran and passed |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | External input validated at entry (barricade) | PASS | Two barricades: HTTP request (MaxBytesReader + JSON decode + required-user-message, server.go:99-120) and model output (parseFacts canonicalize/degrade + extra-field stripping, extract.go:31-46). Probed with malformed body, missing user msg, garbage stdout — all handled, tests executed. |
| cc-defensive-programming | No empty catch blocks / no swallowed errors | PASS | Searched all files: every error is returned, mapped, or logged. `_, _ = w.Write` on the fixed health literal is the only discarded error — deliberate and unactionable once the status is committed. |
| cc-defensive-programming | Assertions for bugs only / no executable code in assertions | N/A | Go — no assertion mechanism in use; anticipated runtime failures use error handling throughout. |
| cc-defensive-programming | Consistent error-handling strategy | PASS | Single sentinel (ErrBackendUnavailable) wraps every backend failure mode; HTTP layer maps uniformly to 502; startup config errors fail fast (main.go:20-23, newBackend unknown-name error); logging follows repo slog conventions. |
| cc-defensive-programming | Correctness-vs-robustness stance appropriate | PASS | The degrade-to-`[]` choice is the contract's explicit requirement (`[]` is the legal ErrNoFacts no-op downstream, extract.go:20-23) — robustness here cannot corrupt data, only skip extraction, and backend failure stays retryable rather than degrading. |
| gof-design-patterns | Pattern selection fits the symptom (interchangeable algorithms → Strategy) | PASS | Backend interface (backend.go:47-49) + three value-type strategies + newBackend factory (backend.go:55-66); the client (main) selects the strategy — textbook Strategy, and it is what makes the fakeBackend test seam possible without production test-only branches. No spurious indirection added elsewhere. |

## Notes (non-blocking)
- codexBackend appends the combined prompt as a *positional* argv element (backend.go:160). In production the system prompt always leads (engramd always sends one), but a request with an empty system message puts event text first, and event text beginning with `-` could be consumed by codex's own flag parser (argument injection into the backend CLI — a different hazard class from shell injection, which is fully barred). Inserting `--` before the prompt would close it. Not demonstrable as harmful here; agy/claude pass prompts as `-p`/`--system-prompt` values and are unaffected.
- claudeBackend.Run (backend.go:206-207) is the one production line never executed by any test; its argv construction is fully covered via args(). A fake-on-PATH test like the codex ones would close it.
- stdout/stderr are unbounded bytes.Buffers (backend.go:94-96); a pathologically chatty backend could balloon memory within the timeout window. Bounded in practice by the per-call deadline.
- usage.prompt_tokens = len(userPrompt)/4 yields 0 for a 1–3 char user prompt; engramd does not act on usage, so cosmetic.
- syscall.Kill(-pid) and Setpgid are POSIX-only; the shim would need build-tag work for Windows. Deployment target is macOS/Linux hosts, so academic.
- An extraction batch exceeding 4 MiB would 400 (by MaxBytesReader design). If engramd's batching can ever exceed that, revisit the limit; not a listed requirement.

## Issues (if FAIL)
None.

**Verdict: PASS**

# Review: Phase 1 - Extraction shim + local-stack rewire (sample 1)

## Executed Results (Step 0)
- Test suite: `make test` → all packages ok; fresh `go test -count=1 -v ./cmd/engram-extract-shim/` → **56 passed, 0 failed, 0 skipped** (26 top-level tests + subtests)
- Build: `make build` (go build ./...) → clean
- Typecheck: implicit in `go build` / `go vet` → clean
- Lint: `make lint` (go vet + revive v1.12.0) → exit 0, no findings
- Live smoke: `go test -tags=smoke ./cmd/engram-extract-shim/... -run 'TestDW_1_6' -v` → **PASS (3.34s)**, real agy call
- Compose: `podman compose -f deploy/local/docker-compose.yml config` → exit 0 (validates)
- Coverage: `go test -coverprofile` → 74.5% statements overall; see Test-DW Coverage for the breakdown

## Requirement Fulfillment

### DW-1.1
PREMISE:  "`cmd/engram-extract-shim` serves `GET /health` → `{"status":"ok"}` and `POST /chat/completions` returning the stub-shaped envelope (`{choices:[{message:{role,content}}], usage:{prompt_tokens,completion_tokens}}`)."
EVIDENCE: cmd/engram-extract-shim/server.go:75-83 (health), server.go:93-145 (chat/completions), wire types server.go:28-49 with exact `prompt_tokens`/`completion_tokens` JSON tags.
TRACE:    GET /health → handleHealth writes literal `{"status":"ok"}` with 200. POST /chat/completions with system+user messages → backend runs → parseFacts → chatResponse{Choices:[{Message:{Role:"assistant",Content:...}}], Usage:{...}} encoded as JSON.
VERDICT:  **PASS** — TestDW_1_1_HealthOK asserts the exact body; TestDW_1_1_ChatCompletionsEnvelopeShape decodes the envelope, asserts choices==1, role=="assistant", prompt_tokens>0. Both ran and passed.

### DW-1.2
PREMISE:  "Given a request, the shim invokes the selected backend and returns `choices[0].message.content` as a JSON array of valid fact objects; verified against a fake backend in a table-driven test."
EVIDENCE: server.go:125-140 (invoke + assemble); server_test.go:86-152 (table-driven, fakeBackend at server_test.go:17-23).
TRACE:    POST body → decode → Backend.Run(ctx, sys, user) → stdout → parseFacts → content placed in choices[0].message.content; test unmarshals that content into []fact and compares field-by-field across 5 table cases (single, multiple, retraction, statement+valid_at, empty).
VERDICT:  **PASS** — TestDW_1_2_HappyPathFactArray (table-driven, fake backend) ran and passed.

### DW-1.3
PREMISE:  "Fence-stripping + non-array/garbage input degrade to `[]` (dirty test); a backend non-zero exit/timeout yields a retryable HTTP error, not a hang. This explicitly includes a backend that forks child/grandchild processes inheriting stdout — such a backend must not hang the request past the deadline."
EVIDENCE: extract.go:31-46 (degrade path); server.go:126-134 (uniform 502 on any backend error, never 500); backend.go:92-119 (runProcess: Setpgid at :97, group-SIGKILL Cancel at :98-109, WaitDelay=2s at :110).
TRACE:    Garbage stdout → extractArraySubstring ""/unmarshal error → `[]`, count 0, HTTP 200 (dirty test, 6 cases). `/usr/bin/false` → ErrBackendUnavailable. `/bin/sleep 5` under 50ms ctx → killed, error in <2s. `sh -c "sleep 30 & sleep 30"` (two stdout-inheriting children) under 100ms ctx → group kill + WaitDelay force-closes pipes → returned in **0.10s** (asserted bound 6s, child lifetime 30s). HTTP timeout → 502 within 2s watchdog.
VERDICT:  **PASS** — TestDW_1_3_ParseFacts_FencedAndProseWrapped, TestDW_1_3_ParseFacts_GarbageDegradesToEmptyArray, TestDW_1_3_BackendProcessNonZeroExit, TestDW_1_3_BackendProcessTimeout, TestDW_1_3_ForkingBackendDoesNotHang (re-run independently: 0.10s), TestDW_1_3_BackendNonZeroExitReturnsRetryableError, TestDW_1_3_BackendTimeoutReturnsRetryableErrorNotHang — all ran and passed.

### DW-1.4
PREMISE:  "Event text with shell metacharacters is passed inert (arg-slice/stdin) — proven by a test asserting no shell interpretation."
EVIDENCE: backend.go:92-93 (exec.CommandContext with argv slice — no shell anywhere in the package); backend_test.go:16 (dirty text: `rm -rf /; $(whoami) && ... | cat\n...backticks...$VAR`), backend_test.go:25-47 (structural, all 3 backends), backend_test.go:88-98 (behavioral: real subprocess roundtrip).
TRACE:    dirtyEventText → agyBackend.args/codexBackend.args/claudeBackend.args → asserted to land as exactly ONE argv element (or one opaque substring of the combined-prompt element), unsplit. Behaviorally: runProcess("/bin/echo", ["-n", dirty]) → stdout byte-identical to input (a shell would have split on `;`/`&&`/`|` and expanded `$(whoami)`/`$VAR`).
VERDICT:  **PASS** — TestDW_1_4_ShellMetacharactersPassedInert (4 subtests) and TestDW_1_4_RealSubprocessNeverShellInterprets ran and passed.

### DW-1.5
PREMISE:  "`docker-compose.yml` points engramd at `host.docker.internal:8088`, adds `extra_hosts`, relaxes the stub dependency; a compose `config` validates."
EVIDENCE: deploy/local/docker-compose.yml:58-65 (`-extract-url` → `http://host.docker.internal:8088`), :74-75 (`extra_hosts: host.docker.internal:host-gateway`), :76-84 (depends_on lists only opensearch + embed; stub-llm dependency explicitly dropped with rationale comment).
TRACE:    `podman compose -f deploy/local/docker-compose.yml config` → exit 0; rendered output contains `-extract-url` / `http://host.docker.internal:8088`, `extra_hosts: host.docker.internal=host-gateway`, and exactly two `condition: service_healthy` entries (opensearch, embed) — stub-llm no longer gates engramd.
VERDICT:  **PASS** — observed behavior: compose config validated (exit 0) with all three properties present in the rendered output.

### DW-1.6
PREMISE:  "A live smoke test — real `agy` backend, cheap model — extracts ≥1 faithful triple from a sample memory sentence through the HTTP endpoint."
EVIDENCE: smoke_test.go:31-88 (build tag `smoke` at :1, so excluded from `make test`); Makefile:92-93 (`smoke-extract-shim` target).
TRACE:    Sample event "rtd prefers tabs over spaces in Go code." → POST /chat/completions via Shim.Handler with agyBackend (default cheap-model preset) → live agy call → response content parsed as []fact → extracted `{Subject:rtd Predicate:"prefers indent style for Go code" Object:tabs Statement:"rtd prefers tabs over spaces in Go code." ValidAt:2026-07-08T12:00:00Z}` — faithful to source; asserted non-blank subject/predicate + traceability keywords.
VERDICT:  **PASS** — TestDW_1_6_LiveSmokeAgyExtractsFaithfulTriple ran live in this environment (agy on PATH at /Users/r/.local/bin/agy, authenticated) and PASSED in 3.34s; did not skip.

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have corresponding tests that ran in Step 0 (test names reference DW IDs: DW_1_1 x2, DW_1_2, DW_1_3 x5, DW_1_4 x2, DW_1_6 live). DW-1.5 is covered by recorded observed behavior (compose `config` exit 0 + rendered-output inspection) — a YAML config with no automated-test surface in this repo's suite.
- [x] Coverage matches the stated level for translation logic *behaviorally*: request parse (bad JSON 400, missing user 400, non-POST 405, last-message selection), backend invoke (fake happy path, non-zero exit, timeout, forking child, codex temp-file wiring x2), response assemble (envelope, fact table, fence/prose/garbage/extra-field parsing) — every path including error/dirty paths has a passing test.
- Residual uncovered statements (74.5% total): `main`/`envOr`/`envDurationOr` (process wiring, conventionally untested), `agyBackend.Run` (0% hermetic but exercised live by the DW-1.6 smoke run), `claudeBackend.Run` (one-line delegation; its `args` and `runProcess` halves each independently tested), and defensive branches (json.Marshal failure in parseFacts, group-kill fallback in Cancel, encode-error log). No translation *path* is untested.

## Dead Code
None found. No unused imports (build/vet clean), no unreachable code, no debug statements (the two `fmt.Fprintln(os.Stderr, ...)` in main.go are fatal-error paths), no commented-out blocks.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | Shim fields (Backend, Timeout, Logger) are set once before ListenAndServe and only read in handlers; each request gets its own context, exec.Cmd, and buffers. Traced all handler paths — no shared mutable state to race. |
| Error Handling | PASS | Adversarial paths traced: malformed JSON → 400 (tested); oversized body → MaxBytesReader (server.go:99) → decode error → 400; missing user message → 400 (tested); any backend error → uniform 502 (server.go:126-134, tested); garbage model output → `[]` never 500 (tested); response-encode failure → logged, not silently swallowed. |
| Resources | PASS | codex temp file: created/closed then `defer os.Remove` on every path including kill (backend.go:164-173, exercised by TestCodexBackend_* both directions); killed process groups reaped via SIGKILL(-pgid); WaitDelay closes inherited pipes; per-request context cancelled via defer. |
| Boundaries | PASS | Empty messages array → userPrompt "" → 400; empty backend stdout → `[]` (tested); empty system prompt → combinePrompt returns user alone (backend.go:70-75); `end < start` bracket ordering guarded in extractArraySubstring (extract.go:72, tested with object-only input); zero facts round-trip as `[]` (tested). |
| Security | PASS | Command-injection barrier verified: the ONLY exec construction is exec.CommandContext(name, args...) at backend.go:93 — no `sh -c`, no string concatenation into a command anywhere in the package (grepped). Dirty text proven inert structurally (3 backends) and behaviorally (real subprocess roundtrip). Deserialized model output re-marshaled through the typed `fact` struct, dropping unexpected fields (tested). Request body size-capped at 4 MiB. codex output path is shim-generated (os.CreateTemp), never event-derived. |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | No executable code in assertions | N/A | No assertions used (idiomatic Go error returns throughout). |
| cc-defensive-programming | No empty catch blocks / silently swallowed errors | PASS | Every error path returns or logs: decode → 400+Warn, backend → 502+Error, encode → Error log. The one discarded error (`_, _ = w.Write` of the constant health body, server.go:82) is explicit and inconsequential. |
| cc-defensive-programming | External input validated at entry (barricade) | PASS | All three external boundaries barricaded: HTTP request (method check, 4 MiB cap, JSON decode with rejection, required user message — server.go:94-120); model output (parseFacts re-marshal through typed struct, degrade to `[]` — extract.go:31-46); backend process (argv-slice only, bounded timeout, uniform error mapping — backend.go:92-119). Cross-process data treated as external per the "internal team API is still external" rule. |
| cc-defensive-programming | Assertions for bugs only; anticipated runtime errors handled | PASS | Runtime conditions (timeout, non-zero exit, garbage output) all handled as errors; the one fail-fast startup path (unknown -backend, backend.go:55-66) correctly treats misconfiguration as a startup error, not a runtime recovery. |
| cc-defensive-programming | Correctness-vs-robustness strategy consistent | PASS | Deliberate, documented robustness on the extraction path (degrade to `[]`, retryable 502 — matches downstream ErrNoFacts/outbox-retry contract) and correctness at startup (exit 1 on bad backend). Consistent with the "return neutral value / return error code" strategies. |
| gof-design-patterns | Pattern fit: interchangeable backend algorithms | PASS | Backend is a textbook Strategy (backend.go:47-49): concrete strategies differ only in argv assembly/output source; enables the test-only fakeBackend without production branches. newBackend is a simple factory. Indirection justified (3 real + 1 fake implementations) — no over-engineering or pattern misuse found. |

## Edge Cases (prompt-listed)
| Edge case | Status | Evidence |
|---|---|---|
| Fenced / prose-wrapped / object / empty output → `[]` or parsed array, never 500 | PASS | extract tests (4 recovery cases + 6 garbage cases) + handler has no 500 path — parseFacts cannot return an error. |
| Non-zero exit / timeout incl. stdout-inheriting forked child → prompt retryable error, per-call timeout | PASS | Setpgid + group SIGKILL + WaitDelay=2s (backend.go:97-110); TestDW_1_3_ForkingBackendDoesNotHang returned in 0.10s vs 30s child lifetime; per-call timeout at server.go:122. |
| Shell metacharacters / newlines passed inert | PASS | Structural + behavioral tests above (DW-1.4). |
| `claude` opt-in only; default backend `agy` | PASS | main.go:14 default `envOr("SHIM_BACKEND","agy")`; newBackend("") → agy (backend.go:56-58); pinned by TestNewBackend_ClaudeNeverSelectedByDefault (ran, passed). |

## Notes (non-blocking)
1. **Argument-injection hardening (codex)** — codexBackend passes the combined prompt as a bare positional argv element (backend.go:160). If a request carried no system prompt and user text beginning with `-`, codex's flag parser would read it as a flag (worst realistic case: non-zero exit → 502; theoretical case: text matching a real codex flag). Not demonstrable as a defect here — engramd always sends a system prompt and prefixes events with `[event ...]` headers, and DW-1.4's shell barrier is intact — but appending a `--` separator before the positional prompt would close it outright. agy/claude `-p <value>` is a non-issue: flag values are consumed positionally.
2. **Unbounded backend stdout** — runProcess buffers stdout into an unbounded bytes.Buffer (backend.go:94); a pathological backend emitting gigabytes before the deadline could balloon memory. Undemonstrated under the 60s cap; an io.LimitReader-style cap would harden it.
3. **Silent SHIM_TIMEOUT fallback** — envDurationOr (main.go:46-56) silently substitutes the default on an unparsable env value; documented as deliberate, but a startup Warn would make the misconfiguration visible.
4. **Greedy bracket span** — extractArraySubstring spans first `[` to last `]`; prose containing stray brackets around the real array (e.g. "See [1] ... [{...}]") makes the span invalid JSON and degrades real facts to `[]`. Safe per the contract (never a 500), just lossy in an exotic case.

## Issues (if FAIL)
None.

**Verdict: PASS**

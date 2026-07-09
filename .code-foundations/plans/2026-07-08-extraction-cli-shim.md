# Plan: CLI-backed extraction shim for engram

**Created:** 2026-07-08
**Status:** complete
**Started:** 2026-07-08 17:05
**Completed:** 2026-07-08 22:04
**Duration:** 2026-07-08 17:05 → 22:04
**Complexity:** simple

---

## Context

Engram's semantic tier sits at 0 because the running local stack wires engramd's `-extract-url` to `engram-stub-llm`, a test stub that only emits facts for literal `fact:`/`retract:` directive lines. The 182 already-ingested prose memories all extract to `[]`, get stamped `processed_at`, and write nothing. We will build a **host-side HTTP shim** that speaks engramd's exact OpenAI-compatible `/chat/completions` extraction contract and delegates each request to a **headless agent CLI (`agy` default; `codex`/`claude` selectable) running a cheap model at low reasoning effort** — with **zero changes to engramd's Go extractor**. Then re-extract the 182 via the designed `-extractor-version` bump and verify the semantic tier populates and reconciles.

Full contract, CLI test results, and risks: `.code-foundations/research/2026-07-08-extraction-cli-shim.md`.

## Constraints

- **No changes to `internal/ingest` server code.** The shim mirrors the stub's wire shape; engramd is untouched (the missing `Authorization` header in `http.go:99` only matters for a real hosted API and is out of scope).
- The shim runs on the **host** (the CLIs + their auth live there); engramd (in-container) reaches it via `http://host.docker.internal:<port>`.
- **Cheap model + low effort** is mandatory per backend: `agy` → `"Gemini 3.5 Flash (Low)"`; `codex` → `-c model_reasoning_effort=low` (model locked to gpt-5.5 under ChatGPT auth); `claude` → `--model haiku --effort low`.
- Event text is passed to a subprocess — it MUST go as an `exec.Command` arg-slice (or stdin), never through a shell string (command-injection barrier).
- Output must never trip engramd's `ErrMalformed` needlessly: strip ```` ```json ```` fences, tolerate surrounding prose, and degrade a garbage model response to `[]` rather than crash.

## Success criteria

- engramd's `-extract-url` points at the host shim; the shim answers `/health` and `/chat/completions`.
- After a re-extract sweep, `memory_status` shows `semantic_count > 0` for tenant `rtd`, populated from the 182 prose memories.
- Spot-checked extracted facts are faithful `{subject,predicate,object}` triples of the source memories; reconciliation (dedup/supersede) runs without error.

---

## Implementation Phases

### Phase 1: Extraction shim + local-stack rewire
**Skills:** code-foundations:cc-defensive-programming (subprocess + untrusted-ish model output at a trust boundary), code-foundations:gof-design-patterns (Strategy for pluggable CLI backends)
**Model:** sonnet
**Gate:** Full
**Depends on:** none
**File scope:** cmd/engram-extract-shim/**, deploy/local/docker-compose.yml, Makefile
**Security-sensitive:** yes

**Goal:** Build a host HTTP server that satisfies engramd's extraction contract by shelling out to a selectable cheap-model CLI, and repoint the local stack at it.

**Scope:**
- IN: new `cmd/engram-extract-shim` Go program — `POST /chat/completions` + `GET /health`, reusing the stub's request/response envelope shape (`chatRequest`/`chatResponse`); read the **last `user` message**; assemble the prompt (system+user combined for `agy`/`codex`; real `--system-prompt` for `claude`); invoke the backend via `exec.Command` arg-slice; strip code fences + surrounding prose from stdout; return the fact JSON array as `choices[0].message.content` with a synthetic `usage`.
- IN: a `Backend` interface (Strategy) with an `agy` impl (default, `--model "Gemini 3.5 Flash (Low)"`), a `codex` impl (`codex exec --skip-git-repo-check --ignore-user-config -o FILE -c model_reasoning_effort=low`), and a `claude` impl (`claude -p --system-prompt SYS --model haiku --effort low`); selected via `-backend`/`-model` flags (env fallback). A **fake backend** for tests.
- IN: rewire `deploy/local/docker-compose.yml` — engramd `-extract-url` → `http://host.docker.internal:8088`, add `extra_hosts: ["host.docker.internal:host-gateway"]` (Linux portability), and drop/relax the `depends_on: stub-llm: service_healthy` (the host shim isn't a compose service). Add a `Makefile`/build target for the shim.
- OUT: any change to `internal/ingest`; hosted-API auth; the backfill (Phase 2); grug-style cross-link "dream".

**Edge cases:**
- Model returns fenced JSON, prose-wrapped JSON, an object instead of an array, or empty output → shim returns a valid `[]` (or the parsed array), never a 500 that dead-letters the event.
- Backend CLI exits non-zero / times out → shim returns a clean HTTP error engramd can retry (not a hang); enforce a per-call timeout.
- Event text containing shell metacharacters / newlines → passed as an arg-slice or stdin, proven inert by a dirty test.
- `claude` backend's ~24k-token `CLAUDE.md` injection is a documented quality hazard — default stays `agy`; `claude` is opt-in only.

**Produces:** A running host endpoint `http://host.docker.internal:8088/chat/completions` (OpenAI-compatible, returns `{subject,predicate,object[,statement][,valid_at]}[]`) that engramd's extractor hits; backend + model configurable by flag. Phase 2 consumes this live endpoint.
**Security-sensitive:** yes — executes a subprocess on event-derived text and deserializes model output; injection barrier (arg-slice, no shell) and output validation are the review focus.

**Done when:**
- [ ] DW-1.1: `cmd/engram-extract-shim` serves `GET /health` → `{"status":"ok"}` and `POST /chat/completions` returning the stub-shaped envelope.
- [ ] DW-1.2: Given a request, the shim invokes the selected backend and returns `choices[0].message.content` as a JSON array of valid fact objects; verified against a fake backend in a table-driven test.
- [ ] DW-1.3: Fence-stripping + non-array/garbage input degrade to `[]` (dirty test); a backend non-zero exit/timeout yields a retryable HTTP error, not a hang.
- [ ] DW-1.4: Event text with shell metacharacters is passed inert (arg-slice/stdin) — proven by a test asserting no shell interpretation.
- [ ] DW-1.5: `docker-compose.yml` points engramd at `host.docker.internal:8088`, adds `extra_hosts`, relaxes the stub dependency; `docker compose config` validates.
- [ ] DW-1.6: A live smoke test — real `agy` backend, cheap model — extracts ≥1 faithful triple from a sample memory sentence through the HTTP endpoint.

### Phase 2: Backfill re-extraction + verification
**Skills:** code-foundations:cc-debugging (isolate why events do/don't re-claim), engram:engram-memory (drive memory_status/search for verification)
**Model:** sonnet
**Gate:** Standard
**Depends on:** Phase 1
**File scope:** deploy/local/docker-compose.yml, scripts/**

**Goal:** Re-extract the 182 already-processed `rtd` events through the live shim and prove the semantic tier populates and reconciles.

**Scope:**
- IN: trigger re-extraction via the **designed mechanism** — bump engramd's `-extractor-version` (`v1`→`v2`, `cmd/engram-server/main.go:63`), which re-keys the ledger and reprocesses events (`worker.go:47-49`, `ids.go:33`). Confirm whether the version bump alone re-claims events whose `processed_at` is already set; if not, fall back to clearing `processed_at` for tenant `rtd` (a scoped OpenSearch update in a small `scripts/` helper).
- IN: verify with `memory_status` (`semantic_count > 0`) and `memory_search` (facts retrievable, faithful); confirm reconciliation runs (no dead-letters, supersede/dedup where duplicates exist). Optional: A/B a sample of `agy` vs `codex` extraction quality.
- OUT: scaling the remaining namespaces (that's the follow-on once this is green); the search token-bloat redesign; engramd Go changes.

**Edge cases:**
- Version bump does **not** re-claim `processed_at`-stamped events → fall back to the scoped `processed_at` reset; document which was needed.
- A cheap model emits a non-durable-fact memory as `[]` → correct (not a failure); distinguish "legitimately no facts" from "extraction broken" via the DW below.
- Re-extraction double-writes facts already present → reconciliation must dedup, not duplicate; verify count sanity.

**Produces:** A verified-populated semantic tier for `rtd` (n/a as a code seam — this is the terminal verification phase).
**Rollback:** Re-extraction is additive under a new ledger version; the old `v1` ledger/semantic rows remain. If quality is bad, revert `-extractor-version` to `v1` and the `v1` semantic state is unchanged — no destructive step. A `processed_at` reset (fallback only) is scoped to tenant `rtd`; snapshot the affected event ids first.

**Done when:**
- [ ] DW-2.1: After the version bump (and fallback reset if required), the worker re-extracts the `rtd` events through the shim — observed in engramd logs (extraction calls firing, not `ErrNoFacts` for prose-bearing events).
- [ ] DW-2.2: `memory_status` reports `semantic_count > 0` for tenant `rtd`.
- [ ] DW-2.3: `memory_search` over ≥3 known ported facts returns them from the semantic tier with faithful `{subject,predicate,object}` content (spot-check documented).
- [ ] DW-2.4: Reconciliation completes with no dead-lettered events and no duplicate-fact explosion (repair backlog converges).

---

## Test Coverage

**Level:** 100% of the shim's translation logic (request parse → backend invoke → response assemble), including error/dirty paths. Phase 2 is operational verification, evidenced by the DW checks rather than unit tests.

## Test Plan

- [ ] Shim: `/health` returns `{"status":"ok"}`.
- [ ] Shim: happy path — fake backend returns a fact array; shim wraps it in the correct envelope with synthetic `usage` (DW-1.2).
- [ ] Shim (dirty): fenced JSON, prose-wrapped JSON, an object-not-array, and empty stdout all degrade to `[]` without a 500 (DW-1.3).
- [ ] Shim (dirty): backend non-zero exit and backend timeout each yield a retryable HTTP error, not a hang (DW-1.3).
- [ ] Shim (dirty/security): event text with `; $() && | \n` is passed inert via arg-slice/stdin — no shell interpretation (DW-1.4).
- [ ] Shim: `-backend`/`-model` selects agy/codex/claude and assembles the system+user prompt correctly per backend (real `--system-prompt` for claude, combined for agy/codex).
- [ ] Rewire: `docker compose config` validates the rewired engramd service — `host.docker.internal:8088` + `extra_hosts`, stub dependency relaxed (DW-1.5).
- [ ] Live smoke (agy, cheap model): one sentence → ≥1 faithful triple through the HTTP endpoint (DW-1.6).
- [ ] Backfill: engramd logs show extraction calls firing for `rtd` events after the version bump — not `ErrNoFacts` on prose-bearing events (DW-2.1).
- [ ] Fallback (conditional/security): **if** the scoped `processed_at`-reset script is written, a dirty test asserts the update matches only tenant `rtd` event ids (dry-run count equals the snapshotted id set; zero cross-tenant hits).
- [ ] Verification: `semantic_count > 0` and ≥3 faithful facts retrievable after backfill (DW-2.2, DW-2.3); no dead-letters (DW-2.4).

---

## Notes

- **Backfill trigger is an open build-time question:** the `-extractor-version` bump is the *designed* re-extraction path, but whether it re-claims events whose `processed_at` is already set (vs only new events) must be confirmed against `worker.go`/`repair.go` during Phase 2 — the scoped `processed_at` reset is the documented fallback.
- **Latency at scale:** ~3.7s/call (agy) is fine for 182 but serial extraction of thousands will be slow — batching/concurrency is a follow-on concern, not this plan's scope.
- **Determinism/quality:** cheap models may drift run-to-run and must reliably emit the strict triple schema; the Phase 2 spot-check is the quality gate before scaling. `agy` and `codex` observed steadier than `claude`.
- **Optional follow-on (not planned here):** emit a zero-fact log/metric in engramd's `ErrNoFacts` branch so a silent 0 can't masquerade as working again (touches server code — deferred).

---

## Execution Log

### Phase 1: Extraction shim + local-stack rewire (Gate: Full)
- [x] BUILD: Discovery + design + implementation (stub → implement → validate) complete
- [x] REVIEW: Verification passed — 3-sample fable majority, PASS 3/3 after one fail→pass fix cycle (a forking-backend timeout hang, closed via process-group SIGKILL + cmd.WaitDelay)
- [x] Committed
Commit: cd59abe
Summary: Delivered cmd/engram-extract-shim — a host HTTP server on :8088 speaking engramd's /chat/completions + /health contract, delegating to a pluggable cheap-model CLI backend (agy default; codex/claude opt-in) via an exec arg-slice with fence-stripping + degrade-to-[] barricading and a bounded per-call timeout; rewired deploy/local/docker-compose.yml to point -extract-url at host.docker.internal:8088 (extra_hosts added, stub dependency relaxed) and added Makefile targets. 56 shim tests + full repo green; live agy smoke extracts a faithful triple. This is the live endpoint Phase 2 re-extracts through.

### Phase 2: Backfill re-extraction + verification (Gate: Standard)
- [x] BUILD: Discovery + design + operational execution complete
- [x] REVIEW: Verification passed — single sonnet review, all 4 DW items PASS against live system state; safety-critical tenant-scoping of the reset script independently confirmed
- [x] Committed
Commit: 30718a3
Summary: Re-extracted the rtd events through the live agy-backed shim and populated the semantic tier — semantic_count 0 → 367 for tenant rtd, directly observed via memory_status. Required BOTH the -extractor-version v1→v2 bump (re-keys the ledger so the worker doesn't short-circuit) AND a tenant-scoped processed_at reset (ClaimBatch keys the outbox on processed_at, not extractor_version) — confirmed against outbox.go/worker.go source. Added scripts/backfill-reextract-rtd.sh (snapshots ids, exact term filter on tenant_id, no cross-tenant reach). Reconciliation converged clean: 0 dead-lettered, 367 distinct content_keys (no dup explosion), 185 events each yielding ≥1 fact.

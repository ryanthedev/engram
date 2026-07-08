# Research: CLI-backed extraction shim for engram

**What this is:** A local HTTP shim that speaks engramd's OpenAI-compatible `/chat/completions` extraction contract and satisfies each request by shelling out to a headless agent CLI (`agy`/`codex`/`claude`) running a **cheap model at low reasoning effort** — so engramd's `-extract-url` can point at real extraction instead of the `engram-stub-llm` test stub, and the semantic tier finally populates from the 182 already-ingested prose memories.

**Date:** 2026-07-08 · **Status:** confirmed
**Next:** `/code-foundations:plan .code-foundations/research/2026-07-08-extraction-cli-shim.md`

---

## Why this exists

Engram has two memory tiers: **episodic** (raw events, ingested verbatim — works today) and **semantic** (atomic, de-duplicated facts distilled from those events by an extraction LLM). The extraction step is the bridge. The running local stack wires that extractor to `engram-stub-llm`, a deterministic test stub that only emits facts for literal `fact:`/`retract:` directive lines — so 182 real prose memories all extract to `[]`, get stamped `processed_at`, and write nothing. Empty extraction counts as success, so nothing errors and `semantic_count` sits at 0.

We want real extraction **without touching engramd's Go server code**. A shim that mirrors the stub's wire shape but delegates to a real (cheap) model achieves that with zero changes to `internal/ingest`.

## The wire contract (fully reverse-engineered — this is the entire spec)

engramd's `HTTPExtractor` (`internal/ingest/http.go`) is the client. The shim must satisfy exactly this:

| Aspect | Contract | Source |
|---|---|---|
| Endpoints | `POST /chat/completions` and `GET /health` (→ `{"status":"ok"}`) | `cmd/engram-stub-llm/main.go:60-92` |
| Request body | `{model, temperature:0, messages:[{role:"system",content:<PROMPT>},{role:"user",content:<EVENTS>}]}` | `http.go:53-95` |
| Auth | **None** — engramd sets only `Content-Type`, no `Authorization` header | `http.go:99` |
| User content | events concatenated as `[event <id> kind=<k> at=<rfc3339>]\n<text>\n\n` | `http.go:80-83` |
| Response body | `{choices:[{message:{role:"assistant",content:<STRING>}}], usage:{prompt_tokens,completion_tokens}}` | `http.go:64-72`; stub `main.go:83-91` |
| `content` payload | a **JSON array** (bare or ```` ```json ```` fenced) of fact objects | `internal/ingest/extraction.go:53-114` |
| Fact object | `{"subject":str, "predicate":str, "object":str, "statement"?:str, "valid_at"?:rfc3339}` | `extraction.go:38-44` |
| `object:""` | legal — means **retraction** | `extraction.go:76-80` |
| `[]` | legal — `ErrNoFacts`, no write | `extraction.go:59-61` |
| Errors (`ErrMalformed`) | not-an-array; >100 facts; blank subject OR predicate; any field >4096 bytes | `extraction.go:56-74` |

**The system prompt engramd already sends** (`http.go:18-21`) instructs subject/predicate/object triples with optional statement/valid_at — so the model is told the schema; the shim just relays it. The shim never produces tenancy, embeddings, timestamps, or content-keys — the writer/reconciler stamps those downstream (`extraction.go:47-48`).

**Bottom line:** the shim reads the last `user` message, runs it (plus the system prompt) through a CLI, and returns the model's JSON array as `choices[0].message.content`. That array is the whole contract engramd validates.

## The CLIs — tested live, cheapest model + lowest effort

| CLI | Headless invocation | Cheap model | Low effort | System/user | stdout | Latency |
|---|---|---|---|---|---|---|
| **agy** 1.0.16 ✅ | `agy -p "<sys+user>" --model "Gemini 3.5 Flash (Low)"` | preset `Gemini 3.5 Flash (Low)` | **baked into the `(Low)` preset** | no split — combine into one prompt | **pure JSON, no banner/spinner** | **~3.7s** |
| **codex** 0.142.5 | `codex exec --skip-git-repo-check --ignore-user-config -o OUT.txt -c model_reasoning_effort=low "<sys+user>"` | ⚠ **locked to `gpt-5.5`** under ChatGPT-account auth (mini/nano rejected) | `-c model_reasoning_effort=low` ✅ | no split — combine | banner noise; clean via `-o FILE` | ~5.5s |
| **claude** 2.1.204 | `claude -p "USER" --system-prompt "SYS" --model haiku --effort low` | `--model haiku` (`claude-haiku-4-5`) | `--effort low` ✅ | **true split** (`--system-prompt`) | wraps in ```json fences | ~8–19s |

**Recommendation: `agy` as the primary backend.** Fastest, emits pure JSON needing zero scrubbing, no auth/side-effect/pollution. Keep **codex** as a selectable fallback (clean capture via `-o FILE`, but stuck on pricey gpt-5.5 here and needs `--ignore-user-config`). Avoid **claude** for this role — see gotchas.

### Load-bearing gotchas (verified by running them)

| CLI | Gotcha |
|---|---|
| claude | Global `~/.claude/CLAUDE.md` (~24k tokens) is injected on **every** call — it extracted facts from CLAUDE.md instead of the input, costs ~$0.02/call even on haiku, and was **non-deterministic** (`[]` vs full extraction on identical input). `--bare` strips it but needs `ANTHROPIC_API_KEY` (not set here). |
| codex | ChatGPT-token auth **locks the model to gpt-5.5** (no cheap tier). User config triggers a "superpowers" bootstrap that **shell-execs** and floods stdout — `--ignore-user-config` kills it. `exec` defaults sandbox `read-only`, approval `never`. |
| agy | 8 fixed model presets only (names contain spaces → must quote); effort is part of the preset name, not a flag; no JSON-output mode (not needed — output is already clean). No surprises. |

## Deployment shape

engramd runs **inside a container** (`deploy/local/docker-compose.yml:48-81`) and reaches peers by compose service name (`-extract-url http://stub-llm:8082`). The CLIs and their auth/config live on the **host** and can't easily run inside the container. Therefore:

- The **shim runs on the host** (where `agy`/`codex`/`claude` and their auth live).
- engramd (in-container) must reach it via **`http://host.docker.internal:<port>`**, not `localhost`. On Docker Desktop (macOS, this box) that resolves by default; on Linux it needs `extra_hosts: ["host.docker.internal:host-gateway"]`.
- engramd currently `depends_on` `stub-llm` `service_healthy` (`compose:73-74`) — that dependency must be dropped/relaxed since the host shim isn't a compose service with a healthcheck. Extraction calls retry, so a shim that's briefly down is tolerable.
- The e2e harness (`e2e/harness.go:87-118`) runs everything on the host and wires `-extract-url` to `http://localhost:<port>` — a second, simpler reference for host-side wiring.

## Requirements

**Must:**
1. A host HTTP server exposing `POST /chat/completions` + `GET /health`, mirroring `engram-stub-llm`'s envelope exactly.
2. Translate each request: extract last `user` message + system prompt → invoke a CLI backend (cheap model, low effort) → return the model's JSON array as `choices[0].message.content`, with a synthetic `usage`.
3. Robustness the stub didn't need: **strip code fences**, tolerate leading/trailing prose, and if the model returns junk, return `[]` (or a clean error) rather than crash — never emit non-array content that would trip `ErrMalformed` needlessly.
4. **Backend selectable** (`agy` default; `codex`, `claude` switchable) via flag/env, since we want to A/B `codex` vs `agy`.
5. Repoint the local stack's `-extract-url` at the host shim and relax the `stub-llm` dependency.
6. **Backfill the 182**: they already have `processed_at` set, so they will not re-extract on their own — a re-extract sweep (or a way to clear `processed_at` for tenant `rtd`) is required to prove the semantic tier populates.
7. Verify: after backfill, `semantic_count > 0`, facts are faithful, and reconciliation (dedup/supersede) runs.

**Nice-to-have:**
8. A zero-fact log/metric in engramd's `ErrNoFacts` branch so a silent 0 can't masquerade as "working" again (the last session recommended this).
9. Batch/concurrency awareness so scaling past 182 (thousands of events at ~4s/call) stays tractable.

**Out of scope:**
- Changing engramd's Go extractor (the missing `Authorization` header in `http.go:99` — only matters for a real *hosted* API, not a local shim; record it, don't fix it here).
- The `memory_search` token-bloat / paged-results redesign (embeddings inlined in `fields_json`) — separate thread, being worked in tab 2.
- grug-style "dream" surprising-cross-link discovery — net-new feature, not extraction.

## Riskiest assumptions (to resolve in plan/build)

| # | Assumption | If wrong |
|---|---|---|
| 1 | `host.docker.internal` resolves from the engramd container on this box | shim unreachable; need `extra_hosts` or run shim as a compose service (but then no host CLI auth) |
| 2 | A backfill/re-extract path exists or can be built without violating engram's append-only invariants | the 182 stay stuck at `processed_at`; can't prove population |
| 3 | Cheap models reliably emit the strict `{subject,predicate,object}` triple schema, not `{text}` or prose | frequent `ErrMalformed`; extraction quality too low to trust → needs a spot-check gate |
| 4 | ~3.7s/call latency is acceptable through 182 and beyond | scaling to thousands serial is slow; may need batching/concurrency |
| 5 | Backends stay deterministic enough for a personal store (temp 0 is sent but CLIs may ignore) | inconsistent facts run-to-run; agy/codex observed steadier than claude |

## Language / placement (decision for plan)

Natural fit is a **Go `cmd/engram-extract-shim`** mirroring `cmd/engram-stub-llm` — reuses the same request/response struct shapes, stays in-repo, testable alongside the stub. Alternative: a throwaway script (bun/python). Lean Go for consistency and because it can share the stub's envelope code; confirm in planning.

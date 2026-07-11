# Plan: memory_search breadth-first results + memory_read drill-down

**Created:** 2026-07-11
**Status:** in-progress
**Started:** 2026-07-11 14:30
**Current Phase:** 1
**Complexity:** medium

---

## Context

**Problem:** `memory_search` returns full records inline. Episodic `text` bodies are fat (a single memory ~1 KB), so a wide search either blows the byte budget (hits omitted/spilled) or forces the caller to spend context on bodies it didn't want — and there is no clean way to fetch one record's full body on demand. An agent hitting this bailed to a `jq` probe over Claude Code's private tool-results cache instead of engram tooling.

**Solution shape** (from confirmed research `.code-foundations/research/2026-07-11-memory-search-results-drilldown.md`): make a search result carry just enough to decide read/don't-read (breadth, compact-line format), and add a deliberate `memory_read(id, source)` drill for the full body. Only episodic is genuinely two-phase; semantic/graph results are already the whole memory.

**Already done, not in scope:** the stale-deploy root cause (server predating the slimming merge) was fixed and verified this session — `engram-local` image rebuilt from HEAD `8ed5258`, `local-engramd-1` recreated, slim output confirmed. No "deploy the slimming" phase.

## Constraints

- **Go**, all touch points under `internal/` (`mcp`, `retrieval`, `server`, `store`, `engramclient`) plus `api/` proto for the new RPC.
- **ACL correctness (non-negotiable):** a by-id read must not bypass the fail-closed provenance-as-ACL barricade that search enforces (`internal/retrieval/opensearch.go:206–290`; `Audit` checks `s.ACL.CanRead`, `internal/server/server.go`). The new episodic getter must **fetch → authorize → project**, never the reverse.
- **gRPC `Hit` stays full-fidelity** — snippet truncation lives in the MCP presentation layer so full `text` still crosses gRPC and `memory_read` can reuse it.
- **Adopted open-decision leans:** always-breadth default (no `detail` param in v1); snippet = lead ~200 chars, single-line normalized; graph has no drill.
- Do not break existing programmatic consumers (`engramclient`, e2e/integration tests) or the append-only memory model.

---

## Implementation Phases

### Phase 1: Breadth results + compact-line rendering
**Skills:** `code-foundations:cc-routine-and-class-design` -- designing the renderer routine + rune-safe truncation
**Model:** sonnet
**Gate:** Standard
**Depends on:** none
**File scope:** internal/mcp/** (renderer, tools output, tests) -- excludes budget/spill hint wording (Phase 3)

**Goal:** `memory_search` returns a compact-line results list where episodic `text` is a normalized lead snippet and every result exposes `id` + `source`; the gRPC `Hit` is unchanged.

**Scope:**
- IN: a compact-line renderer for the `memory_search` MCP tool output; episodic `text` → single-line lead snippet (~200 chars, a named constant) computed in the MCP layer; per-hit line carries `id`, `source`, `score`, gist, and key display fields; semantic/graph render their `statement`/`subject`/`predicate`/`object` as the gist (content unchanged, new format).
- OUT: `memory_read` (Phase 2); `hint`/`overflow_path` wording (Phase 3); any gRPC/proto/store/retrieval change; snippet truncation in `projectFields`; a `detail=full` param.

**Edge cases:** text shorter than the limit (no truncation, no dangling ellipsis); text with newlines/tabs/control chars (normalize to one line); empty/missing `text`; multi-byte UTF-8 truncation must be **rune-safe** (the ghost memory contains 👻🕯️ emoji — never split a rune); hits with missing display fields; score formatting stable/compact.

**Produces:** the compact-line format for `memory_search`, and the addressing contract Phase 2 consumes — **every result exposes its `id` (OpenSearch `_id`) and `source` (`episodic|semantic|graph`), parseable/round-trippable**.

**Done when:**
- [ ] DW-1.1: a multi-hit `memory_search` returns compact-line text, not escaped `fields_json` JSON.
- [ ] DW-1.2: episodic `text` renders as a normalized single-line lead snippet within the length cap.
- [ ] DW-1.3: truncation is UTF-8 rune-safe — an emoji/multibyte body is never split mid-rune (dirty test with the 👻 body).
- [ ] DW-1.4: every result line exposes `id` and `source` unambiguously (parse round-trips to the same pair).
- [ ] DW-1.5: semantic/graph hits render full `statement`/s-p-o (content identical to today, format changed).
- [ ] DW-1.6: the gRPC `Hit` still carries full untruncated `text` — a gRPC/`engramclient` consumer sees no truncation.
- [ ] DW-1.7: empty/short/newline-laden `text` inputs render without panic or dangling ellipsis.

### Phase 2: memory_read(id, source) drill-down
**Skills:** `code-foundations:cc-defensive-programming` -- barricade design + fail-closed ACL at a new trust boundary
**Model:** fable
**Gate:** Full
**Security-sensitive:** yes
**Depends on:** Phase 1
**File scope:** api/** (proto), internal/server/server.go, internal/store/facts.go, internal/engramclient/client.go, internal/mcp/mcp.go, internal/mcp/tools.go, + generated proto stubs and tests

**Goal:** add `memory_read(id, source)` returning one record's full content — a new gRPC `Read` RPC (proto + regen), an episodic store getter with fail-closed ACL, reuse of `Audit` for semantic, wired through `engramclient` → `Backend.Read` seam → MCP tool, emitting structured (un-nested) JSON.

**Scope:**
- IN: proto `Read` RPC + `buf` regen; episodic realtime GET-by-`_id` getter (mirrors `GetFact`, `internal/store/facts.go:26`) applying fail-closed `CanRead`/`Enforcer` with **fetch → authorize → project** ordering; a server `Read` handler dispatching on `source` (episodic → new getter; semantic → `Audit`/`GetFact`; graph → unsupported); `engramclient.Read`; `Backend.Read` seam (`internal/mcp/mcp.go:52`); `memory_read` MCP tool registration (`internal/mcp/tools.go`); structured JSON output with `fields` as a real object (un-nested `fields_json`).
- OUT: graph drill (return unsupported/empty per research); breadth/format changes (Phase 1); `hint` wording (Phase 3).

**Edge cases:** unknown/absent id → `NOT_FOUND`; cross-tenant id → deny **without leaking existence** (mirror `Audit` fail-closed); id/`source` mismatch (semantic id with `source=episodic`) → `NOT_FOUND`, **no cross-index probing**; ACL denial indistinguishable from not-found; superseded semantic id → returns that immutable version with its (possibly closed) validity interval — expected, not an error; missing/blank `source` arg → validation error.

**Security-sensitive:** yes — a by-id read is a new trust boundary; it must run the same fail-closed authorization as search/`Audit`, fetching ACL fields (`tenant_id`/`team_id`/`scope`/`owner_agent_id`) and authorizing **before** projecting them away.

**Produces:** `memory_read` MCP tool + gRPC `Read(id, source) -> record` contract; the episodic get-by-id store method. Structured un-nested JSON is the read output shape.

**Done when:**
- [ ] DW-2.1: `memory_read(id, source=episodic)` returns the full untruncated `text` for an id surfaced by a Phase-1 result.
- [ ] DW-2.2: `memory_read(id, source=semantic)` returns the full fact plus provenance/version history (via `Audit`).
- [ ] DW-2.3: a cross-tenant / unauthorized id yields fail-closed `NOT_FOUND` with **no content or existence leak** (dirty test on the ACL path).
- [ ] DW-2.4: a read whose ACL fields would deny is rejected fail-closed (observable denied-read test); the fetch→authorize→project ordering is a Full-gate security-review obligation (see Security-sensitive).
- [ ] DW-2.5: read output is structured JSON with `fields` as a real object, not a stringified `fields_json`.
- [ ] DW-2.6: proto is regenerated, the `Read` RPC is present in generated stubs, and the build/tests are green.
- [ ] DW-2.7: an id/`source` mismatch or unknown id returns `NOT_FOUND` without probing the other index.

### Phase 3: hint → overflow_path escape hatch
**Skills:** `code-foundations:code-clarity-and-docs` -- clarity of the caller-facing hint guidance text
**Model:** sonnet
**Gate:** Standard
**Depends on:** Phase 1, Phase 2 (DW-3.3's `hint` names `memory_read`, which Phase 2 produces)
**File scope:** internal/mcp/budget.go, internal/mcp/spill.go, + tests

**Goal:** when results are budget-capped and spilled, the `hint` tells the caller to read the full set from engram's own `overflow_path` and to drill single hits with `memory_read` — so it never invents a private cache path.

**Scope:**
- IN: `hint` references `overflow_path` as the full-set source when it is set; `hint` names `memory_read` as the single-hit drill; wording steers away from external/private caches.
- OUT: spill mechanics (already exist); the read tool itself (Phase 2).

**Edge cases:** `omitted==0` → no `overflow_path`, `hint` must not dangle a nonexistent path; spill write failed (existing graceful-degradation path) → `hint` must not promise a path that isn't there.

**Produces:** the `hint` envelope referencing `overflow_path` + `memory_read`.

**Done when:**
- [ ] DW-3.1: when `omitted>0` and `overflow_path` is set, the `hint` names `overflow_path` as the full-set source.
- [ ] DW-3.2: when `omitted==0` or the spill write failed, the `hint` does NOT reference a nonexistent `overflow_path`.
- [ ] DW-3.3: the `hint` names `memory_read` as the single-hit drill path.

---

## Test Coverage
**Level:** 100% of done-when items, each code-touching phase with ≥1 dirty test.

## Test Plan
- [ ] T1.1 (DW-1.1/1.2/1.4/1.5): multi-hit `memory_search` → compact lines; episodic snippet capped; `id`+`source` parse round-trips; semantic/graph gist intact.
- [ ] T1.2 (DW-1.3, dirty): body with 👻🕯️ emoji + a multibyte run at the truncation boundary → no split rune.
- [ ] T1.3 (DW-1.7, dirty): empty `text`, sub-limit `text`, and newline/tab-laden `text` → clean single-line render, no panic/dangling ellipsis.
- [ ] T1.4 (DW-1.6): gRPC/`engramclient` search result still carries full untruncated `text`.
- [ ] T2.1 (DW-2.1/2.2/2.5): episodic read → full text; semantic read → fact + history; output is un-nested structured JSON.
- [ ] T2.2 (DW-2.3/2.4, dirty): cross-tenant/unauthorized id → fail-closed `NOT_FOUND`, no leak; getter authorizes before projecting.
- [ ] T2.3 (DW-2.7, dirty): semantic id with `source=episodic`, and an unknown id → `NOT_FOUND`, no cross-index probe.
- [ ] T2.4 (DW-2.6): `buf` regen clean; `Read` RPC in stubs; build + suite green.
- [ ] T3.1 (DW-3.1/3.3): capped result → `hint` names `overflow_path` and `memory_read`.
- [ ] T3.2 (DW-3.2, dirty): `omitted==0` and simulated spill-failure → `hint` references no dangling path.

---

## Notes
- Phase 1 → Phase 2 seam is the `(id, source)` addressing contract; both touch `internal/mcp`, so Phase 2 depends on Phase 1 (not parallel).
- Phase 3 extends Phase 1's MCP output envelope but depends on Phase 2 (its `hint` names `memory_read`); the three phases run serially, no parallel wave.
- Snippet length (~200) and exact compact-line delimiter/field-order are finalized in Phase 1 build; keep the `id`+`source` parse unambiguous.
- gRPC `Hit`/protobuf stays structured; only the MCP tool's emitted string changes for search — `memory_read` returns structured JSON.
- ACL ordering (fetch→authorize→project) is the load-bearing correctness property for Phase 2; it drives the Full gate + security-sensitive review.

---

## Execution Log

### Phase 1: Breadth results + compact-line rendering (Gate: Standard)
- [x] BUILD: Discovery + design + implementation complete (new `internal/mcp/render.go` + 15 tests)
- [x] REVIEW: Verification passed (all 7 DW with execution evidence; 69/69 mcp tests, full suite + vet + lint clean)
- [x] Committed
Commit: ac9fb65
Summary: `memory_search` now renders compact tab-separated lines (`id\tsource\tscore\tgist\tkey=value…`) in the MCP layer — episodic `text` is a rune-safe single-line lead snippet; every hit exposes `id`+`source` (the Phase-2 drill contract); the gRPC `Hit` still carries full untruncated `text`.

### Phase 2: memory_read(id, source) drill-down (Gate: Full, security-sensitive)
- [x] BUILD: Discovery + design + implementation complete (proto `Read` RPC + regen, `GetEpisodic`, `internal/server/read.go`, `Backend.Read`, `memory_read` tool; 10 new test funcs)
- [x] REVIEW: 3-sample fable majority PASS (3/3) — fetch→authorize→project proven via spy enforcer; every denial byte-identical opaque NOT_FOUND; no cross-index probing; proto regen idempotent (proto-check exit 0 post-commit)
- [x] Committed
Commit: ca571d1
Summary: `memory_read(id, source)` drills to a single record's full body — new gRPC `Read` RPC, fail-closed episodic getter (`GetEpisodic`, fetch→authorize→project), Audit-reusing semantic branch, graph UNIMPLEMENTED; wired through `engramclient`→`Backend.Read`→`memory_read` MCP tool; output is structured un-nested JSON.

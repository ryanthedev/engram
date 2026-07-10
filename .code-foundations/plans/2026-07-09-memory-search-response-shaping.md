# Plan: memory_search Response Shaping & Budget-Aware Paging

**Created:** 2026-07-09
**Status:** complete
**Started:** 2026-07-09 14:05
**Completed:** 2026-07-09 14:52
**Duration:** ~47 min
**Complexity:** medium

---

## Context

**Problem:** `memory_search`'s MCP response returns raw OpenSearch documents — including full 1024-float embedding arrays — inline in `fields_json`, with no size bound and no paging. A default query returns ~143KB for 19 hits (>95% embedding noise), blowing past the client's tool-result cap and forcing the LLM caller to detour through file dumps instead of reading a normal result.

**Success criteria:**
- Embeddings never appear in any hit; each tier (episodic/semantic/graph) returns its natural fields plus a populated score.
- A default `memory_search` returns well under the byte budget — no more 143KB responses.
- When results exceed the inline budget: the response carries a budget-packed page **plus** an omitted count, top facets, and a refine hint; **and** the full slim result set is written to a scratch file whose absolute path is returned, and reading that path yields valid parseable JSON (verified end-to-end).
- The whole change lives behind the existing gRPC + MCP seam.

## Constraints

- **Wire compat:** `fields_json` stays a `string` and keeps carrying the hit's content — don't retype/remove it (buf `breaking: FILE`). No proto change is required (slim hits are cheap; MCP receives all and pages locally).
- **Non-breaking for consumers:** the `Export` RPC / Obsidian exporter is decoupled and untouched. Existing readers of `fields_json` (`e2e/scenarios_*.go`, `internal/server/server_test.go`) read `text`/`statement` — both retained by the projection; they must still pass.
- **Spill assumes a shared filesystem** — valid because `engram-mcp` is a local stdio process. Mirror the atomic-write pattern at `internal/cli/export.go:461` (`os.CreateTemp` + `os.Rename`); spill dir defaults to the OS temp dir, overridable by env.
- **Byte budget configurable**, default safely below the smallest plausible client cap (~25K tokens / Claude Code's `MAX_MCP_OUTPUT_TOKENS`).
- **Style (code-standards):** consumer-defined interfaces, OpenSearch/vendor types out of public signatures; table-driven tests + fake `Backend`/`refClient` conformance (`internal/mcp`) + `httptest` OpenSearch fake (`internal/retrieval`); DW-tagged test names; ≥1 dirty test per code-touching phase.

## Chosen Approach

**A — Split by actor.** Strip embeddings + per-tier projection + score in the retrieval layer (data-minimization where the data lives, so embeddings never cross the gRPC wire); budget-pack + facets + spill in the MCP layer (next to the caller it serves, where the shared-filesystem spill path exists). **Fallback:** if per-tier projection in retrieval proves entangled with merge/ranking, project in `internal/server` just before `fields_json` marshaling — same result, one layer out.

## Rejected Approaches

- **B — Fat MCP adapter:** smallest diff, but embeddings still cross gRPC wastefully and the MCP adapter must hard-code OpenSearch field names, leaking retrieval schema across the boundary (DIP violation).
- **C — Fat server / thin MCP:** breaks spill — the server may not share a filesystem with the LLM client; the spill path must live on the local MCP process's host.

---

## Implementation Phases

### Phase 1: Shape hits at the retrieval boundary
**Model:** fable
**Skills:** cc-routine-and-class-design, cc-defensive-programming
**Gate:** Full
**Security-sensitive:** yes

**Goal:** Make retrieval return slim, embedding-free hits — each source projected to its natural display fields plus a populated score — clamping the per-tier query size to a maximum, WITHOUT disturbing ACL authorization.

**Scope:**
- IN: `_source` exclusion of `text_embedding`/`fact_embedding` in `buildQuery` (bandwidth); a `projectFields(source, fields)` allowlist applied as the **final step of `MultiRetriever.Search`, after the last `filterAuthorized` (opensearch.go:258)** so ACL still reads provenance and graph/tier-source hits are covered; clamp per-tier query `size` to `[1, MaxK]` (MaxK=100); ensure every returned `Hit` carries a populated `Score`.
- OUT: any MCP-layer behavior; byte budgeting; proto changes; score *normalization* across tiers (documented, not implemented — YAGNI); re-truncating post-hook (graph) additions (deliberately preserved — `expand.go:238`).

**Constraints:** projection MUST run after both `filterAuthorized` calls — `recordFromHit` (`opensearch.go:278`) reads `tenant_id/team_id/scope/owner_agent_id` from `Hit.Fields` fail-closed; stripping them earlier is a total ACL blackout. `k` arrives from the MCP caller across a process boundary — external input: clamp defensively (`DefaultK`=10 when ≤0, `MaxK`=100 when over, else unchanged), never assert. `projectFields` tolerates a missing/nil field by omitting it, not panicking.
**Edge cases:** nil/absent `Fields`; an unknown `source` (registered tier sources — safe default: keep `text/statement/subject/predicate/object` if present, drop all else incl. `*_embedding`/`tenant_id`/`team_id`/`scope`/`owner_agent_id`); `k`≤0 and `k`>MaxK; graph hop hits (score `1/(hop+1)`, no `_score`) still projected and kept.
**Depends on:** none | **Unlocks:** Phase 2
**File scope:** `internal/retrieval/**`
**Produces:** after `MultiRetriever.Search`, every `retrieval.Hit.Fields` (`map[string]any`) contains ONLY the per-source allowlist — episodic: `text, kind, occurred_at, event_id, source_ids`; semantic: `statement, subject, predicate, object, valid_at, source_ids`; graph: `statement, subject, predicate, object, hop`; unknown: the safe-default subset — with no `*_embedding`/`tenant_id`/`team_id`/`scope`/`owner_agent_id` surviving; per-tier query `size ≤ MaxK`; every hit has a non-zero-value `Score`. ACL results are byte-for-byte unchanged. Server serializes this to `fields_json` unchanged.

**Approach notes:** projection is a data→outcome map (source → allowlist), so a single table-driven `projectFields` routine fits better than per-source branches. Location is the load-bearing decision: end-of-`Search`, post-authorization — NOT `parseHits` (misses graph + breaks ACL).
**File hints:** `internal/retrieval/opensearch.go` — `buildQuery` (`_source` exclude), `MultiRetriever.Search` line 258 (projection insert point, after final `filterAuthorized`), `recordFromHit:278` (why order matters), `DefaultK` (k clamp); `internal/graph/expand.go:233` (`edgeHit` — the graph hit shape projection must cover); `internal/retrieval/opensearch_test.go` — `httptest` fake + table test pattern.

**Done when:**
- [ ] DW-1.1: `buildQuery` sets `_source` to exclude `text_embedding` and `fact_embedding`; a query-shape test asserts the exclusion is present.
- [ ] DW-1.2: `projectFields` reduces each source (episodic/semantic/graph/unknown) to its allowlist; no `*_embedding`/`tenant_id`/`team_id`/`scope`/`owner_agent_id` survives (table test over all four shapes, incl. the `edgeHit` graph shape).
- [ ] DW-1.3: per-tier query `size` is clamped to `[1, MaxK]`; below/at/above-bound cases covered.
- [ ] DW-1.4: every returned hit has a populated `Score`, including graph hop hits.
- [ ] DW-1.5: ACL is unaffected — an ACL-enabled `MultiRetriever.Search` returns the SAME hits before and after projection (authorization runs on un-projected fields); existing `fields_json` consumers (`server_test.go`, `e2e/scenarios_*.go`) still pass (`text`/`statement` retained).

**Difficulty:** MEDIUM
**Uncertainty:** none material — the projection point (post-authorization, end of `Search`) is pinned by the ACL/graph ordering above.

### Phase 2: Budget-pack + facets + refine hint at MCP
**Model:** sonnet
**Skills:** cc-control-flow-quality, cc-routine-and-class-design, cc-defensive-programming
**Gate:** Standard

**Goal:** Have the MCP `memory_search` tool request a generous number of slim hits, pack them into the response until a configurable byte budget is reached, and report what was omitted with top facets and a refine hint.

**Scope:**
- IN: a default requested `k` (=50, ≤ Phase 1's `MaxK`=100); a byte-budget packer over the slim hits that reserves headroom for the envelope; omitted-count; top-facet computation over the omitted set; a refine-hint string; budget config (`ENGRAM_MCP_SEARCH_BUDGET_BYTES`, default 16384).
- OUT: writing anything to disk (Phase 3); changing the gRPC/`Backend` interface.

**Constraints:** `ENGRAM_MCP_SEARCH_BUDGET_BYTES` is read at the process boundary — validate (positive; fall back to the 16384 default when unset/invalid). The budget bounds the **full serialized result** (hits + envelope: `omitted`/`omitted_facets`/`hint`/Phase-3 `overflow_path`), so the packer reserves envelope headroom before packing hits. The packing loop tests at the beginning (a zero-hit result must produce an empty page, not a panic). Measure cumulative size against the *serialized* hit, not an estimate that can drift from what's emitted.
**Edge cases:** zero hits; a single hit already at/over budget (still emit it — never return an empty page when hits exist; the rest spill in Phase 3); all hits fit (omitted=0, no facets, no hint); ties in facet counts (stable ordering).
**Depends on:** Phase 1 | **Unlocks:** Phase 3
**File scope:** `internal/mcp/**`
**Produces:** the `memory_search` tool-result JSON = `{hits:[...packed slim hits...], omitted:int, omitted_facets:{field:value,...}, hint:string}`, where `hits` are packed so the FULL serialized result stays ≤ the configured budget, and `omitted`/`omitted_facets`/`hint` are present only when `omitted>0`. The **complete slim result set** (packed page + the un-packed remainder) is handed to Phase 3 for spill.

**Approach notes:** cap-plus-refine-hint was the chosen paging model (no next-page cursor) — LLM callers reformulate rather than page. Facets = the most common `subject`/`predicate`/`kind` values among omitted hits, so the caller can narrow.
**File hints:** `internal/mcp/tools.go` — `callSearch` (request k, pack, assemble result); `internal/mcp/mcp.go` — `Hit`/result shape; `internal/mcp/mcp_test.go` — `fakeBackend` + `refClient` conformance pattern.

**Done when:**
- [ ] DW-2.1: a default `memory_search` (no explicit `k`) returns a response whose FULL serialized size (hits + envelope) is ≤ the configured byte budget.
- [ ] DW-2.2: when hits exceed the budget, the result carries `omitted>0`, non-empty `omitted_facets`, and a `hint`; when all fit, those fields are absent/zero.
- [ ] DW-2.3: byte budget is configurable via `ENGRAM_MCP_SEARCH_BUDGET_BYTES` with the documented default 16384 (safely below ~25K tokens); invalid/unset falls back to the default (dirty test).
- [ ] DW-2.4: a single over-budget hit is still emitted (no empty page when hits exist).
- [ ] DW-2.5: facet counts are computed over the omitted set with stable ordering on ties.
- [ ] DW-2.6: no `.proto` diff is introduced; `buf breaking` stays clean (the seam-preservation criterion).

**Difficulty:** MEDIUM
**Uncertainty:** byte-count vs token-count for the budget — default to bytes (deterministic, no tokenizer dep); revisit only if a token cap proves necessary.

### Phase 3: Spill-to-disk overflow at MCP
**Model:** sonnet
**Skills:** cc-defensive-programming
**Gate:** Full
**Security-sensitive:** yes

**Goal:** When results exceed the inline budget, atomically write the full slim result set to a private scratch file and return its absolute path alongside the packed page, with the read-back verified end-to-end.

**Scope:**
- IN: atomic spill write (`os.CreateTemp` + `os.Rename`) of the full slim result set as JSON to a scratch file with `0600` perms; an overridable spill dir (`ENGRAM_MCP_SPILL_DIR`, default OS temp); `overflow_path` in the result; graceful degradation if the write fails.
- OUT: any cross-page cursor; cleanup/GC of old spill files (documented non-goal for v1); remote-filesystem support.

**Constraints:** the spill content may contain sensitive memory text — create the file `0600` (owner-only), never world-readable. File I/O crosses a trust boundary: do not swallow errors (no empty catch) — on write failure, log a warning and return the capped response *without* `overflow_path` (robustness: a spill failure must not fail the search). The returned path must be absolute so the local caller can read it. Do not promise a recovery path that doesn't work (the VS Code failure mode) — only emit `overflow_path` after the file is durably renamed into place.
**Edge cases:** spill dir unwritable/permission-denied; disk full mid-write (temp file must not be renamed into place partial); `omitted==0` (no spill written, no `overflow_path`); env-overridden dir that doesn't exist.
**Depends on:** Phase 2 | **Unlocks:** (none)
**File scope:** `internal/mcp/**`
**Produces:** over-budget responses additionally carry `{overflow_path:string}` — an absolute path to a `0600` file containing the full slim result set as valid, parseable JSON; a same-process read-back of that path round-trips to the complete result set.

**Approach notes:** spill is the escape hatch for cap-plus-refine — the inline page stays small, the full slim set is grep-able on disk. Mirror `internal/cli/export.go:461` `writeFileAtomic` (`CreateTemp`+`Rename`) exactly; it's the repo's established atomic-write pattern.
**File hints:** `internal/cli/export.go:461` — `writeFileAtomic` precedent to mirror; `internal/mcp/tools.go` — extend `callSearch`'s result assembly; `internal/mcp/mcp_test.go` — conformance pattern for the round-trip test.

**Done when:**
- [ ] DW-3.1: when `omitted>0`, a `0600` file is written atomically (CreateTemp+Rename) and `overflow_path` (absolute) is returned; when `omitted==0`, no file and no `overflow_path`.
- [ ] DW-3.2: reading `overflow_path` yields valid JSON that unmarshals to the FULL slim result set (end-to-end round-trip test).
- [ ] DW-3.3: file mode is exactly `0600` (asserted via `os.Stat`).
- [ ] DW-3.4: a failed spill write degrades gracefully — capped response returned, no `overflow_path`, warning logged, no panic (dirty test with an unwritable dir).
- [ ] DW-3.5: spill dir is `ENGRAM_MCP_SPILL_DIR`-overridable, defaulting to the OS temp dir; a nonexistent override dir degrades gracefully.
- [ ] DW-3.6: a write/marshal failure mid-spill leaves NO file renamed into place (no partial `overflow_path`) — atomicity holds under failure.

**Difficulty:** MEDIUM
**Uncertainty:** whether stale spill files need lifecycle management — deferred as a documented non-goal; revisit if disk accumulation becomes real.

---

## Test Coverage
**Level:** 100% — a test item per done-when across all phases, plus boundary and dirty tests; each code-touching phase carries dirty tests for its error paths (spread favors dirty on the I/O and config boundaries).

## Test Plan

**Phase 1 — retrieval (`internal/retrieval`, unit via `httptest` OpenSearch fake + table tests):**
- [ ] T1.1 (DW-1.1, clean): `buildQuery` body sets `_source` excluding `text_embedding` + `fact_embedding` — query-shape assertion.
- [ ] T1.2 (DW-1.2, clean ×4): table test of `projectFields` over episodic/semantic/graph (the `edgeHit` shape)/unknown — projected `Fields` equal the allowlist exactly; assert no `*_embedding`/`tenant_id`/`team_id`/`scope`/`owner_agent_id` survives.
- [ ] T1.3 (DW-1.3, boundary): per-tier query `size` below (`≤0`→DefaultK), at (`=MaxK`), above (`>MaxK`→MaxK).
- [ ] T1.4 (DW-1.4, clean): graph hop hit retains its hop-derived score; fusion hit carries `_score`.
- [ ] T1.5 (DW-1.5, integration — the ACL guard): an ACL-enabled `MultiRetriever.Search` (with `m.acl` set + a graph post-hook) returns the SAME authorized hits with projection as without — proves projection runs after `filterAuthorized` and doesn't blackout. This is the finding-1/2 regression test; the `httptest` fake alone (nil acl) would miss it.
- [ ] T1.6 (DW-1.2, dirty): nil/absent `Fields` → no panic; wrong-typed field value (e.g. `source_ids` not a slice) → omitted safely, no panic; `k=-1`.
- [ ] T1.7 (DW-1.5, integration): `internal/server/server_test.go` + `e2e/scenarios_*.go` `fields_json` readers still pass.

**Phase 2 — MCP budget/facets (`internal/mcp`, conformance via `fakeBackend` + `refClient`):**
- [ ] T2.1 (DW-2.1, clean): default `memory_search` FULL serialized size (hits + envelope) ≤ configured budget.
- [ ] T2.2 (DW-2.2, clean ×2): over-budget → `omitted>0` + non-empty `omitted_facets` + `hint`; all-fit → those fields absent/zero.
- [ ] T2.3 (DW-2.3, dirty ×2): `ENGRAM_MCP_SEARCH_BUDGET_BYTES` unset → 16384 default; garbage/negative/zero → fallback to default.
- [ ] T2.4 (DW-2.4, boundary): a single hit already over budget is still emitted (no empty page when hits exist).
- [ ] T2.5 (DW-2.5, clean): facet counts computed over the omitted set; stable ordering on count ties.
- [ ] T2.6 (DW-2.1, dirty): zero hits → empty page, no panic; omitted hits with a facet field entirely absent → facets skip it, no panic.
- [ ] T2.7 (DW-2.6, integration): `buf breaking` / no `.proto` diff — guards the seam-preservation criterion.

**Phase 3 — MCP spill (`internal/mcp`, conformance + end-to-end round-trip):**
- [ ] T3.1 (DW-3.1, clean): `omitted>0` → `0600` file written atomically (CreateTemp+Rename), absolute `overflow_path` returned; `omitted==0` → no file, no `overflow_path`.
- [ ] T3.2 (DW-3.2, e2e): reading `overflow_path` → valid JSON unmarshaling to the FULL slim result set.
- [ ] T3.3 (DW-3.3, clean): `os.Stat` confirms mode exactly `0600`.
- [ ] T3.4 (DW-3.4, dirty): unwritable/permission-denied spill dir → capped response returned, no `overflow_path`, warning logged, no panic.
- [ ] T3.5 (DW-3.5, clean + dirty): spill dir `ENGRAM_MCP_SPILL_DIR`-overridable, defaults to OS temp; nonexistent override dir → graceful degrade.
- [ ] T3.6 (DW-3.6, dirty): injected write/marshal failure mid-spill → no file renamed into place (glob the spill dir: zero non-temp artifacts), no partial `overflow_path`.

---

## Assumptions
| Assumption | Confidence | Verify Before Phase | Fallback If Wrong |
|---|---|---|---|
| Projection at end-of-`Search` (post-`filterAuthorized`) leaves ACL byte-for-byte unchanged and covers graph/tier-source hits | High | Phase 1 | If entangled, project in `internal/server` after re-auth, before `fields_json` marshal |
| Existing `fields_json` readers only use `text`/`statement` (not dropped fields) | High | Phase 1 | Add the needed field back to the allowlist |
| Slim hits are cheap enough to bulk-fetch over gRPC (no proto paging) | High | Phase 2 | Add k/offset fields to proto (buf `FILE`-safe) |
| Byte-count budget suffices (no tokenizer needed) | Med | Phase 2 | Switch to a token estimate |
| `engram-mcp` shares a filesystem with the client (local stdio) | High | Phase 3 | Disable spill when remote; return omitted-only |

## Decision Log
| Decision | Alternatives Considered | Rationale | Phase |
|---|---|---|---|
| Split shaping (retrieval) from budget/spill (MCP) | Fat MCP adapter; fat server | Data-minimization at source; spill needs the local filesystem | All |
| Cap + refine-hint, no cursor paging | Offset paging; stateful snapshot cursor | LLM callers reformulate; research warned against stateful cursors | 2 |
| Keep spill-to-disk in v1 | Defer as documented non-goal | User choice — escape hatch for the full slim result set | 3 |
| No proto change | Add paging fields to proto | Slim hits are cheap; MCP pages locally | All |
| Bytes, not tokens, for the budget | Token count | Deterministic, no tokenizer dependency | 2 |
| Leave score un-normalized across tiers | Normalize hop vs fusion score | YAGNI for v1; merge already sorts by score | 1 |

---

## Notes
- No proto change: the design keeps `fields_json` as-is and does all paging/spill at the MCP layer, because slim hits are cheap enough to fetch in bulk over gRPC.
- Score comparability across tiers (fusion score vs graph hop-score) is imperfect and left as-is for v1 (documented, not normalized).

---

## Execution Log

### Phase 1: Shape hits at the retrieval boundary (Gate: Full)
- [x] BUILD: Discovery + design + implementation (stub → implement → validate) complete
- [x] REVIEW: Verification passed — 3-sample fable majority (3/3 PASS)
- [x] Committed
Commit: bb6e419
Summary: Retrieval now returns slim, embedding-free hits — `projectFields` (table-driven, per-source allowlist) applied at the end of `MultiRetriever.Search` after authorization, `_source` excludes embeddings in `buildQuery`, query size clamped to MaxK=100, every hit carries a populated score. `retrieval.Hit.Fields` now holds only the per-tier natural fields (episodic text/kind/occurred_at/event_id/source_ids; semantic statement/s-p-o/valid_at/source_ids; graph statement/s-p-o/hop); ACL results unchanged. This is the slim seam Phase 2 packs to a byte budget.

### Phase 2: Budget-pack + facets + refine hint at MCP (Gate: Standard)
- [x] BUILD: Discovery + design + implementation (stub → implement → validate) complete
- [x] REVIEW: Verification passed (single-sample sonnet)
- [x] Committed
Commit: 7b557b1
Summary: `internal/mcp/budget.go` packs `memory_search` results to a byte budget (`ENGRAM_MCP_SEARCH_BUDGET_BYTES`, default 16384) via shrink-from-full (drop lowest-ranked, remeasure real serialized envelope). `callSearch` defaults k=50. Over-budget responses carry `omitted`, `omitted_facets` (subject/predicate/kind over the omitted set, stable-ordered), and a refine `hint`. The omitted remainder is an order-preserving suffix `hits[N:]` of the packed prefix — so Phase 3 can spill the FULL slim set. No proto change.

### Phase 3: Spill-to-disk overflow at MCP (Gate: Full)
- [x] BUILD: Discovery + design + implementation (stub → implement → validate) complete
- [x] REVIEW: Verification passed — 3-sample fable majority (3/3 PASS)
- [x] Committed
Commit: 2132ed0
Summary: `internal/mcp/spill.go` writes the full slim result set to a `0600` scratch file (`ENGRAM_MCP_SPILL_DIR`, default OS temp) via atomic CreateTemp+Chmod+Write+Close+Rename when `omitted>0`, setting `overflow_path` on the result. Marshal precedes any FS call; every error branch removes the temp file (no partial rename); spill failure degrades gracefully (no `overflow_path`, warning logged, search never fails). Completes the response-shaping feature: slim hits → byte-budgeted page + facets → full set on disk.

### Post-verification fix: overflow_path budget accounting (Gate: Standard)
- [x] Found by end-to-end manual verification (fable agent): DW-2.1 violated — the packer measured hits+facets+hint but not `overflow_path`, which is attached after the fit-check; at `ENGRAM_MCP_SEARCH_BUDGET_BYTES=2048` the emitted response was 2157–2180B > 2048.
- [x] FIX: `maxSpillPath()` (spill.go) computes an upper bound on the `overflow_path` field from the live `spillDir()` + `os.CreateTemp`'s max 10-digit suffix; `searchResultFits` reserves that headroom when a remainder exists. One-hit floor (DW-2.4) preserved. Regression tests drive the real JSON-RPC path and measure actual emitted bytes.
- [x] REVIEW: single-sample sonnet PASS (5/5 requirements, upper-bound cross-checked against Go stdlib). Committed.
Commit: (fix) fix(mcp): count overflow_path in the search byte budget

# Plan: Knowledge Search — Fragments, Drill-down & Paging

**Created:** 2026-07-23
**Status:** in-progress
**Started:** 2026-07-23
**Current Phase:** 1
**Complexity:** medium
---
## Context

**Problem:** `knowledge_search` returns the entire stored document per hit — a hit on a 5,499-char markdown file is the whole file (2016 tokens, measured live against `knowledge-docs-v1`). With no drill-down path, no fragment extraction, and no offset paging, a useful `k` on the `docs` collection is 1–2 and the byte-budget packer truncates the rest to an overflow file. A capable LLM reader needs an llms.txt-sized medium — enough to judge relevance and decide what to open — not the raw body.

Full design record: `.code-foundations/research/2026-07-22-knowledge-search-fragments-and-paging.md` (decisions D1–D5, D3a settled; live token measurements 2026-07-23).

## Constraints

- **Server-side only, no re-harvest.** No change to the doc-id scheme or ingest. Harvest-time chunking is explicitly out of scope (tracked separately in the research doc).
- **New `KnowledgeHit` proto message** (D1) — do not overload the memory `Hit`. Give it a real `collection` field (clearing the `Hit.source` = "index tier" wart) and add `total` to the response. Proto is free to change; no external API-compatibility constraint on this repo.
- **Fragments are the default; markers off** (D3). Extraction is the win (7×, 2016→285 tokens); markers are a ~20% tax that also corrupt fenced code blocks (`$«derived»(...)` observed live). If ever enabled, marker cost is tokenizer-dependent — measure, never hardcode `«»`.
- **`memory_read` gains a knowledge drill-down** (A′) so the suppressed body stays reachable; a `full_body: true` request flag preserves today's whole-body behavior.
- **Reuse house idioms** (`docs/code-standards.md`): `internal/engramclient` is the sole gRPC transport boundary — CLI/business packages classify status via `engramclient.IsAlreadyExists`/`IsPermissionDenied`, never raw `status.Code()`. Sentinel errors returned unwrapped, stdlib `testing`, in-process gRPC-stub test convention.
- **Sizing knobs on `CollectionSpec`** (D4) with global fallbacks (`fragment_size` 240, `number_of_fragments` 3).

## Chosen Approach

**Server-side OpenSearch highlighting + new `KnowledgeHit` proto + `memory_read` drill-down.** OpenSearch performs fragment extraction natively on the BM25 path already in use; `pre_tags`/`post_tags` set to `""` give clean extraction with no markers. A dedicated `KnowledgeHit` message keeps knowledge and memory response shapes independent. Suppressing the body in search is made safe by extending `memory_read` to fetch the full doc by id. **Fallback:** if per-collection sizing on `CollectionSpec` proves too invasive for the proto/translation surface, ship global constants only and defer the knobs.

## Rejected Approaches

- **Harvest-time chunking (one doc per heading):** the deeper fix for BM25 *relevance*, but changes the doc-id scheme and forces a full re-harvest — out of scope here; this plan is server-side only.
- **Overloading the memory `Hit` message with a `fragments` field:** puts a permanently-empty field on every memory hit and leaves the `Hit.source` wart in place. `KnowledgeHit` is cleaner and the proto is free to change.
- **Markers on by default (`«»` or `<em>`):** live measurement shows extraction banks the whole win and markers only spend it back; markers also inject into fenced code. Off by default, per-collection opt-in.

---
## Implementation Phases

### Phase 1: Proto & query-builder foundation
**Model:** fable
**Skills:** aposd-designing-deep-modules, cc-routine-and-class-design
**Gate:** Full

**Goal:** Establish the shared seams every downstream phase consumes — the `KnowledgeHit` proto message, `offset`/`total`/`full_body` wire fields, per-collection sizing on `CollectionSpec`, and the `buildQuery` options-struct refactor.

**Scope:**
- IN: `api/proto/engram.proto` (new `KnowledgeHit`; `KnowledgeSearchRequest.offset`/`full_body`; `KnowledgeSearchResponse.hits: KnowledgeHit` + `total`; `CollectionSpec` sizing fields); regenerate pb via `make` codegen; refactor `buildQuery` (8 positional params → options struct) preserving memory-path behavior; add `FragmentSize`/`NumberOfFragments`/marker fields to both `mcp.CollectionSpec` and `knowledge.CollectionSpec` + their proto translation (`collectionSpecProto`/`collectionSpecFromProto`).
- OUT: highlight query logic, `_source` suppression, offset/total query logic, `memory_read` changes (Phases 2–3 consume these types).

**Constraints:** `buildQuery` is shared with the memory retrieval path — the refactor must be behavior-preserving there (its own regression check), and it returns TWO values (`body []byte`, `usePipeline bool`): the guardrail must pin the pipeline flag, not just the body. Two `CollectionSpec` Go types plus the proto message must stay in sync through the translation functions.
**Edge cases:** sizing fields absent on an existing collection → global fallback, not zero-size; proto default (0) for `offset`/sizing must mean "unset → default", not literal 0.
**Depends on:** none | **Unlocks:** Phase 2, Phase 3
**File scope:** `api/proto/**, internal/retrieval/opensearch.go, internal/knowledge/knowledge.go, internal/mcp/mcp.go, internal/engramclient/knowledge.go, internal/server/knowledge.go`
**Produces:**
- Proto: `message KnowledgeHit { string id=1; double score=2; string collection=3; string fields_json=4; repeated string fragments=5; }`; `KnowledgeSearchRequest { ...; int32 offset=6; bool full_body=7; }`; `KnowledgeSearchResponse { repeated KnowledgeHit hits=1; int64 total=2; }`; `CollectionSpec` gains `int32 fragment_size`, `int32 number_of_fragments`, `string highlight_pre_tag`, `string highlight_post_tag` (both empty = markers off; per-collection opt-in per D3, never a hardcoded `«»`).
- Go: `buildQuery(opts queryOpts) (body []byte, usePipeline bool)` where `queryOpts` carries the existing 8 inputs plus zero-value-safe highlight/offset fields.

**Done when:**
- [ ] DW-1.1: `KnowledgeHit` message exists in the proto and regenerated Go; `KnowledgeSearchResponse` uses `repeated KnowledgeHit` + `total`; `KnowledgeSearchRequest` carries `offset` + `full_body`.
- [ ] DW-1.2: `CollectionSpec` (proto + both Go structs) carries `fragment_size`, `number_of_fragments`, `highlight_pre_tag`, `highlight_post_tag`, round-tripping through `collectionSpecProto`/`collectionSpecFromProto` without loss (test asserts round-trip); absent sizing → global fallback (240/3), not zero.
- [ ] DW-1.3: `buildQuery` takes an options struct; a golden-body matrix — {BM25Only, KNNOnly, hybrid} × {filters nil/set} × {sort nil/set} — asserts BOTH byte-identical body AND identical `usePipeline` before/after the refactor; all memory-path call sites compile and their tests pass unchanged.
- [ ] DW-1.4: `make` codegen is clean (no uncommitted proto drift) and `go build ./...` + `go vet ./...` pass.

**Difficulty:** HIGH
**Uncertainty:** Whether the two-`CollectionSpec`-types translation absorbs the sizing fields cleanly, or wants a shared helper.

### Phase 2: Fragment extraction & drill-down
**Model:** fable
**Skills:** aposd-designing-deep-modules, cc-defensive-programming
**Gate:** Full
**Security-sensitive:** yes

**Goal:** Make search return extracted fragments instead of the body (markers off by default, sized per collection), and extend `memory_read` — widening the read-authz barricade — so the suppressed full body is reachable by id.

**Scope:**
- IN: highlight clause in `buildQuery` opts (`pre_tags`/`post_tags` = `[""]` default, per-collection `fragment_size`/`number_of_fragments`, tags from `highlight_pre_tag`/`highlight_post_tag`); drop `text_field` from `_source` unless `full_body`; `parseHits` reads `highlight[textField]`; `KnowledgeRetriever.Search` returns fragments; thread `full_body`/fragments through `engramclient.KnowledgeSearch` and `mcp.Backend.KnowledgeSearch`; server maps to `KnowledgeHit.fragments`; `full_body: true` restores whole body inline; add a fetch-doc-by-id path in `retrieval/knowledge.go`; relax `readSources` (`internal/mcp/tools.go:38`) so `memory_read(id, <collection>)` is accepted, with the collection-existence + read-authz check enforced **server-side in `internal/server/read.go`** (fail closed there).
- OUT: offset/total paging (Phase 3), vault export changes (Phase 3).

**Constraints:** `parseHits` is shared with the memory path — the `highlight` read must be inert there (key absent), and memory callers pass zero-value `queryOpts` (no highlight/offset). Marker default off; tags come from the per-collection `highlight_pre_tag`/`highlight_post_tag` strings, never a hardcoded `«»`. Fragment content is untrusted harvester text passed to an LLM caller — existing search behavior (no new sanitization; `vaultknowledge` sanitizes downstream). The read-authz widening is fail-closed at the server, matching the existing memory-read barricade.
**Edge cases:** empty-query (filter-only) search → no fragments AND no body under default suppression (only scalars + drill-down remain — state this in the tool description), not an error; a match inside a fenced code block still extracts (markers off avoids corruption); `memory_read` on a collection the caller can't read → opaque not-found (fail-closed); unknown source that is neither a memory tier nor a registered collection → self-correcting error.
**Depends on:** Phase 1 | **Unlocks:** Phase 3
**File scope:** `internal/retrieval/opensearch.go, internal/retrieval/knowledge.go, internal/server/knowledge.go, internal/server/read.go, internal/mcp/tools.go, internal/mcp/mcp.go, internal/engramclient/knowledge.go, internal/knowledge/knowledge.go`
**Produces:**
- `knowledge_search` returns per-hit `{id, score, collection, fields_json (scalars only), fragments[]}`; `memory_read(id, source=<collection>)` returns the full stored document.
- Go contract (consumed by Phase 3): `engramclient.KnowledgeSearch(ctx, collection, query string, filters, sort, k int, fullBody bool) ([]mcp.KnowledgeHit, error)` and `KnowledgeRetriever.Search(...)` return fragments.

**Done when:**
- [ ] DW-2.1: A `knowledge_search` hit returns extracted fragments and NO `body` in `fields_json` by default; live check on `knowledge-docs-v1` shows ~16 hits fitting the 16 KB budget where 1 did before.
- [ ] DW-2.2: Fragments carry no markers when `highlight_pre_tag`/`highlight_post_tag` are empty; setting them on a collection produces fragments wrapped in exactly those strings.
- [ ] DW-2.3: `knowledge_search` with `full_body: true` returns the whole body inline (today's behavior preserved).
- [ ] DW-2.4: `memory_read(id, source)` where source is a readable registered collection returns the full document; an unreadable collection fails closed (opaque not-found) and an unknown source returns a self-correcting message — authz enforced in `server/read.go`.
- [ ] DW-2.5: memory-tier `memory_search`/`memory_read` behavior is unchanged (regression test green).

**Difficulty:** MEDIUM
**Uncertainty:** Resolved — collection existence + read-authz is validated server-side in `read.go` (fail closed); the MCP layer only relaxes the `readSources` gate, so no registry is threaded into `callRead`.

### Phase 3: Offset paging & vault export fix
**Model:** sonnet
**Skills:** cc-defensive-programming, cc-routine-and-class-design
**Gate:** Standard

**Goal:** Add `offset`/exact-`total` paging to knowledge search, and fix the vault export to drain collections via paging — deleting its documented truncation warning.

**Scope:**
- IN: `buildQuery` opts set `"from": offset` (when >0) and `"track_total_hits": true` for knowledge searches; `KnowledgeRetriever.Search` accepts offset, returns total; clamp `offset`+`offset+k` against `max_result_window` with a self-correcting error (not a raw OpenSearch 500); thread `offset`/`total` through `engramclient.KnowledgeSearch`, the `mcp.Backend.KnowledgeSearch` interface method, and the server handler; update `internal/cli/vaultknowledge.go` `fetchKnowledgeDocs` to page with `offset` until drained, passing `full_body: true`, and remove the `len(hits) == MaxK` truncation warning (lines 85–88) and its stale "no offset/cursor" comment.
- OUT: `search_after` deep paging (explicitly out of scope — `from`/`size` only).

**Constraints:** `engramclient.KnowledgeSearch` signature changes — `vaultknowledge.go` is the caller and updates in this phase. Clamp errors follow the house self-correcting-error pattern so an LLM caller can fix its own call.
**Edge cases:** `offset` beyond `total` → empty hits, not an error; `offset+k > max_result_window` → self-correcting error naming the cap; `total` must be exact (`track_total_hits`), never a capped estimate; a collection with fewer than `k` docs pages to completion in one call.
**Depends on:** Phase 1, Phase 2 | **Unlocks:** none
**File scope:** `internal/retrieval/opensearch.go, internal/retrieval/knowledge.go, internal/server/knowledge.go, internal/engramclient/knowledge.go, internal/cli/vaultknowledge.go, internal/mcp/tools.go, internal/mcp/mcp.go`
**Produces:** `knowledge_search` accepts `offset` and returns exact `total`; `vaultknowledge` export drains fully with no truncation warning. Go contract: `engramclient.KnowledgeSearch(ctx, collection, query, filters, sort, k, offset int, fullBody bool) (hits []mcp.KnowledgeHit, total int64, err error)` and `KnowledgeRetriever.Search(...)` return `(hits, total)` — extends Phase 2's signature; `vaultknowledge` is the caller updated here.

**Done when:**
- [ ] DW-3.1: `knowledge_search` with `offset` returns the correct page; response `total` is the exact match count.
- [ ] DW-3.2: `offset+k` exceeding `max_result_window` yields a self-correcting error, not an OpenSearch 500.
- [ ] DW-3.3: `fetchKnowledgeDocs` pages until drained; the `MaxK` truncation warning and stale no-offset comment are removed; the export of a >`MaxK` collection is complete.
- [ ] DW-3.4: `go build ./...` and existing `vaultknowledge` tests pass with the new signature.

**Difficulty:** MEDIUM
**Uncertainty:** Whether `retrieval.MaxK`/`clampK` already encode `max_result_window` or a new bound is needed.

### Phase 4: Harvester doc cleanup
**Model:** haiku
**Skills:** code-clarity-and-docs
**Gate:** Minimal

**Goal:** Remove the live token embedded in `docs/harvester.md` and correct its stale mark-and-sweep-scope section to reflect the shipped per-repo scoping.

_Gate rationale: docs-only string deletion + prose fix. Not marked Security-sensitive: the marker triggers a code-logic REVIEW, and there is no logic here — the security substance is coverage (repo-wide grep, DW-4.1) and rotation (ops notice, DW-4.3), both captured as observable done-when items. A REVIEW of this diff would review nothing._

**Scope:**
- IN: replace the real `egm_...` token at `docs/harvester.md:105` (and any other occurrence) with an obvious placeholder; rewrite the "Multi-repo & sweep scope" section to state that per-repo sweep scoping shipped (`githubSource.SweepScopes()` returns one scope per repo, commit `ebfe8d2`), removing the "possible future enhancement" language and the claim that a single-repo run sweeps the others.
- OUT: git-history scrub and token revocation/rotation (ops actions, tracked in Notes — a doc edit does not remove the secret from history).

**Constraints:** placeholder must not resemble a real token format that a reader could mistake for usable.
**Edge cases:** token string may appear more than once — grep the whole repo, not just this file.
**Depends on:** none | **Unlocks:** none
**File scope:** `docs/harvester.md`
**Produces:** `docs/harvester.md` with no live credential and an accurate sweep-scope description.

**Done when:**
- [ ] DW-4.1: the specific token string appears nowhere in the repo (repo-wide grep clean, not just `docs/harvester.md`); a non-real-looking placeholder is in its place.
- [ ] DW-4.2: the sweep-scope section states per-repo scoping as shipped, with no "future enhancement" or "second run sweeps the first" language.
- [ ] DW-4.3: the user has been explicitly told the token requires out-of-band revocation/rotation, because a doc edit does not scrub it from git history.

**Difficulty:** LOW
**Uncertainty:** None.

---
## Test Coverage
**Level:** 100%

## Test Plan
- [ ] DW-1.1: unit — proto round-trips a `KnowledgeHit`; `KnowledgeSearchResponse` decodes `total`. Integration — a real search returns the new shape.
- [ ] DW-1.2: unit — `CollectionSpec` sizing/tag fields survive `collectionSpecProto`→`collectionSpecFromProto` round-trip; **dirty**: spec with zero/absent sizing → global fallback (240/3) applied, not zero-size.
- [ ] DW-1.3: unit — golden matrix {BM25Only, KNNOnly, hybrid} × {filters nil/set} × {sort nil/set}: each cell asserts byte-identical body AND identical `usePipeline` before/after the options-struct refactor.
- [ ] DW-1.4: build — `make` codegen leaves no proto drift; `go build ./...` + `go vet ./...` clean.
- [ ] DW-2.1: integration — search on `knowledge-docs-v1` returns fragments, no `body`; assert ≥12 hits fit the 16 KB budget. **dirty**: a 5,000-word doc returns ≤ `number_of_fragments` fragments, not the body.
- [ ] DW-2.2: unit — empty tags → clean fragment text; set tags → fragments wrapped in exactly those strings. **dirty**: a match inside a ```code``` fence extracts without injecting markers when tags empty.
- [ ] DW-2.2b: **dirty** — empty-query (filter-only) search under default suppression returns scalars only (no body, no fragments), not an error.
- [ ] DW-2.3: integration — `full_body: true` returns the whole body.
- [ ] DW-2.4: integration — `memory_read(id, <collection>)` returns the doc. **dirty**: unreadable collection → opaque not-found; unknown source → self-correcting error (authz enforced in `server/read.go`).
- [ ] DW-2.5: regression — memory-tier `memory_search` + `memory_read` unchanged.
- [ ] DW-3.1: integration — paging with `offset` returns correct successive pages; `total` exact. **dirty**: `offset` > `total` → empty, no error.
- [ ] DW-3.2: unit — `offset+k` > `max_result_window` → self-correcting error naming the cap.
- [ ] DW-3.3: integration — vault export of a >`MaxK` collection is complete; no warning emitted.
- [ ] DW-3.4: build — `go build ./...` + existing `vaultknowledge` tests pass with the new signature.
- [ ] DW-4.1 / DW-4.2: manual — repo-wide grep for the token string (expect zero); read the sweep-scope section for accuracy.
- [ ] DW-4.3: manual — rotation notice delivered to the user and recorded in the execution log.

---
## Assumptions
| Assumption | Confidence | Verify Before Phase | Fallback If Wrong |
|---|---|---|---|
| Proto is free to change; no external consumer pins `KnowledgeSearchResponse`'s `Hit` shape | High | Phase 1 | If a pinned consumer exists, add `KnowledgeHit` alongside and deprecate `Hit` reuse gradually |
| OpenSearch `highlight` works on the existing BM25 knowledge query with no mapping change | High | Phase 2 | Add `index_options`/`term_vector` to the `text_field` mapping (would touch collection provisioning) |
| Per-collection sizing fits the two `CollectionSpec` Go types + proto translation cleanly | Medium | Phase 1 | Ship global constants only (Chosen-Approach fallback); defer the knobs |
| `retrieval` already exposes/encodes `max_result_window` or it's a simple constant | Medium | Phase 3 | Add an explicit `MaxResultWindow` const (default 10000) and clamp against it |

## Decision Log
| Decision | Alternatives Considered | Rationale | Phase |
|---|---|---|---|
| New `KnowledgeHit` message | Overload memory `Hit` with `fragments` | Avoids a permanently-empty memory field; clears the `Hit.source` wart | 1 |
| Fragments default, markers off | Markers on (`«»`/`<em>`) | Live: extraction banks 7×; markers add ~20% and corrupt fenced code | 2 |
| Extend `memory_read` for drill-down | New `knowledge_read` tool | Reuses the validated `{id, source}` path; makes the existing hint text true | 2 |
| `from`/`size` offset paging | `search_after` cursors | Sufficient for corpus sizes here; `search_after` deferred as out-of-scope | 3 |
| Fold harvester-doc cleanup into this plan | Separate task | User directive; small, isolated, docs-only | 4 |

---
## Notes
- **Ops action, NOT covered by Phase 4:** the token at `docs/harvester.md:105` is live and committed 12 days ago. Scrubbing the doc does not remove it from git history — it must be **revoked/rotated** out-of-band. Flag to the user post-build.
- `buildQuery` is shared with the memory retrieval path; the Phase 1 refactor's golden-body regression test (DW-1.3) is the guardrail against silently changing memory queries.
- Two `CollectionSpec` Go types exist (`mcp.CollectionSpec`, `knowledge.CollectionSpec`) plus the proto message; sizing fields touch all three and their translation functions.
- DAG is effectively serial for the code phases: 1 → 2 → 3 (Phase 3 consumes the `engramclient.KnowledgeSearch`/`Search` signatures Phase 2 introduces, so it depends on Phase 2, not just Phase 1). Phase 4 is fully independent (docs-only) and wave-eligible against any of them.
- Unresearched (from the research doc, non-blocking because markers default off): whether marking measurably helps LLM output, and llms.txt snippet-budget guidance.
---
## Execution Log

### Phase 1: Proto & query-builder foundation (Gate: Full)
- [x] BUILD: Discovery + design + implementation complete (fable)
- [x] REVIEW: Verification passed (sonnet) — golden matrix confirmed to pin pre-refactor buildQuery behavior (reviewer re-ran HEAD~1 impl against all 15 cells, byte-identical)
- [x] Committed
Commit: eaa62ab
Summary: Added the KnowledgeHit proto message (id/score/collection/fields_json/fragments) replacing memory-Hit reuse, offset/full_body/total wire fields, four CollectionSpec sizing/tag fields with a 240/3 fallback at consumption, and refactored buildQuery to a queryOpts struct — memory-path queries byte-identical. Downstream phases now consume KnowledgeHit + queryOpts; no behavior change to search output yet.

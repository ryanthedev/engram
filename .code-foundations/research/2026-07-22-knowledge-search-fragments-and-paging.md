# Knowledge Search — Highlighted Fragments & Paging

**Summary:** `knowledge_search` returns whole stored documents, so a hit on a large markdown file is the entire file. Add OpenSearch **highlighting** (return matched fragments with markers instead of the full body), a **drill-down read path** so a fragment can be resolved back to its full document, and **offset paging** so a result set can be walked instead of truncated.

**Date:** 2026-07-22
**Status:** draft — not yet grilled or cold-read

**Motivating observation (live, 2026-07-22):** after harvesting `sveltejs/svelte` docs into the `docs` collection (106 docs, `text_field: body`), a `knowledge_search` with `k: 4` returned **one** hit; the other three were dropped by the response byte budget and spilled to `overflow_path`. The single returned body was a ~5,000-word markdown file. Usable `k` on this collection is 1–2.

---

## Problem

Three distinct defects, all confirmed by reading the code:

### 1. Hits carry the entire document

`internal/server/knowledge.go:179-183` marshals the whole `Hit.Fields` map into `fields_json`:

```go
fieldsJSON, err := json.Marshal(h.Fields)
out[i] = &engrampb.Hit{Id: h.ID, Score: h.Score, Source: h.Source, FieldsJson: string(fieldsJSON)}
```

`Hit.Fields` is populated verbatim from OpenSearch `_source` (`internal/retrieval/opensearch.go:718`). The only `_source` exclusion is the embedding vectors (`opensearch.go:697`), which knowledge collections do not have. So the `body` field comes back whole, every time.

This is inherited from the memory tiers, where it is correct — an episodic event or a semantic triple *is* small. It does not survive contact with a document corpus.

### 2. No highlighting

Zero `highlight` references anywhere in the repo. `buildQuery` (`opensearch.go:665-703`) emits `size`, `query`, `_source`, and optionally `sort` — nothing else. OpenSearch supports highlighting natively on the BM25 path already in use; engram simply never asks for it.

### 3. No paging

`buildQuery` never sets `from`. `retrieval.Hit` (`retriever.go:83-88`) has no offset or total-count concept. `KnowledgeSearchRequest` (`api/proto/engram.proto:491-499`) carries `collection`, `query`, `filters`, `sort`, `k` — no offset. `KnowledgeSearchResponse` carries only `repeated Hit hits` — no total.

What exists instead is **truncation, not paging**: `internal/mcp/budget.go` packs the response to `searchBudgetBytesDefault = 16384` (override: `ENGRAM_MCP_SEARCH_BUDGET_BYTES`), then spills the full set to a temp file and reports `omitted` + `overflow_path`. Raising `k` strictly worsens the ratio of useful bytes to returned bytes.

### 4. (Discovered while speccing) Knowledge hits have no drill-down

`readSources` is `{episodic, semantic, graph}` (`internal/mcp/tools.go:38`); `callRead` rejects anything else. The comment at `tools.go:308-310` is explicit:

> knowledge_search keeps the raw structured JSON — its docs have no memory_read drill-down, so it never truncates a body to a gist.

**This makes fragments a prerequisite-blocked change, not a drop-in one.** Today the full body is returned *because there is no other way to ever see it*. Shipping highlighting without a read path would replace "too much" with "unreachable".

---

## Goal

A `knowledge_search` caller gets, per hit: identity, score, the small scalar fields (`repo`, `path`, `source_version`, …), and **matched fragments with markers** — not the body. A caller who wants the whole document asks for it by id. A caller who wants the next page of results asks for it by offset.

## Non-goals

- **Chunking at harvest time** (splitting markdown by heading into one doc per section). It is the deeper fix for *relevance* — today a 5,000-word file scores as one bag of words, so a passing mention ranks like a dedicated section — but it changes the doc-id scheme and forces a full re-harvest. Tracked separately; this spec is server-side only and needs no re-ingest.
- Any change to the memory tiers' response shape. Both changes are additive and knowledge-scoped.
- Vector/kNN retrieval for knowledge collections (still BM25-only by design).

---

## Design

### A. Highlighted fragments

**Wire shape.** Add to `Hit` (or to a knowledge-specific hit message — see Open questions):

```proto
// Matched fragments from the collection's text_field, in OpenSearch order.
// Empty for a filter-only search, or when the query matched no text.
repeated string fragments = 5;
```

**Query.** `buildQuery` gains an optional highlight clause. Because `buildQuery` is shared with the memory path, the cleanest seam is a new parameter (or an options struct — `buildQuery`'s signature is already 8 positional parameters, which is over the house threshold and worth fixing while we are here):

```go
query["highlight"] = map[string]any{
    "fields": map[string]any{textField: map[string]any{
        "fragment_size":       fragmentSize,      // per-collection; default 240
        "number_of_fragments": numberOfFragments, // per-collection; default 3
    }},
    "pre_tags":  []string{preTag},  // default "" — see D3
    "post_tags": []string{postTag}, // default ""
}
```

Marking is **off by default** (empty tags): the feature we want here is OpenSearch's fragment *extraction*, not its match *decoration*. See D3 for the evidence and for the `«»` convention if a collection opts marking on. Note that OpenSearch's default `<em>`/`</em>` is doubly wrong for this corpus — it is both the most token-expensive candidate measured and a literal string that occurs in the documents themselves.

**Body suppression.** With fragments present, the `text_field` must be dropped from `_source` — otherwise the change adds bytes rather than removing them. Extend the existing exclusion:

```go
query["_source"] = map[string]any{"excludes": append([]string{"text_embedding", "fact_embedding"}, suppressedFields...)}
```

**Parsing.** `parseHits` (`opensearch.go:707-722`) reads only `_id`, `_score`, `_source`. It gains a read of `hm["highlight"][textField]`. Note `parseHits` is shared with the memory path — the highlight key is simply absent there, so the change is inert for memory.

**Behavior when the caller wants the body anyway.** A `full_body: true` escape hatch on the request preserves today's semantics for any caller that depends on it (e.g. `internal/cli/vaultknowledge.go` — must be checked before changing the default).

### A′. Drill-down read (required by A, not optional)

Extend `memory_read` to accept a knowledge collection as `source`, or add `knowledge_read(collection, id)`. Prefer **extending `memory_read`**: `callRead` (`tools.go:347-369`) already validates `{id, source}` and delegates to `s.backend.Read`; `readSources` becomes a check against the collection registry rather than a fixed map. The `graph` short-circuit branch stays as-is.

This makes the search hint text (`"drill one hit's full body with memory_read(id, source)"`) true for knowledge collections, which it currently is not.

### B. Offset paging

**Wire shape.**

```proto
// KnowledgeSearchRequest
int32 offset = 6;   // 0-based; server clamps offset+k to the index max_result_window

// KnowledgeSearchResponse
int64 total = 2;    // exact match count (track_total_hits), so a caller can page deterministically
```

**Query.** `buildQuery` sets `"from": offset` when non-zero, and `"track_total_hits": true` for knowledge searches so `total` is exact rather than a silently-capped estimate. Precedent exists: `collectionMeta` already uses `track_total_hits` for exactly this reason (`knowledge.go:175`).

**Clamping.** `clampK` (`opensearch.go:58-67`) handles `k`; offset needs the analogous guard, and `offset + k` must be clamped against OpenSearch's `max_result_window` (default 10,000) with a self-correcting error rather than a raw OpenSearch 500 — the house pattern for LLM-facing errors.

**Deep paging is explicitly out of scope.** `from`/`size` degrades past a few thousand results. If deep pagination is ever needed, that is `search_after` + a sort tiebreaker, not this.

### Interaction with the byte budget

The budget packer is not removed — it stays as the backstop. But fragments change it from the *primary* limiter to a rarely-hit one. A hit becomes roughly: id + score + scalar fields + 3 × 240-char fragments ≈ 950 bytes, so a 16 KB budget holds ~16 hits instead of today's 1. `omitted` / `overflow_path` behavior is unchanged.

---

## Phasing

| Phase | Scope | Independently shippable? |
|---|---|---|
| 1 | A′ — extend `memory_read` to knowledge collections | Yes (pure addition; makes drill-down possible) |
| 2 | A — highlight clause, `_source` suppression, `fragments` on the wire, `full_body` escape hatch | Yes, but only *useful* after Phase 1 |
| 3 | B — `offset` + `total`, clamping, `track_total_hits` | Yes, fully independent of 1–2 |

Phase 3 does not depend on 1 or 2 and could go first if paging is the more urgent pain. Recommended order is as listed because fragments are the actual problem; paging is the smaller, cheaper companion.

---

## Decisions (settled 2026-07-22)

**D1 — New `KnowledgeHit` message; do not overload `Hit`.** Knowledge responses get their own hit type rather than bolting a permanently-empty `fragments` field onto memory hits. This repo has no external API-compatibility constraint, so the proto is free to change:

```proto
// KnowledgeHit is one knowledge-collection match: identity, BM25 score, the
// stored scalar fields, and the matched fragments. The collection's text_field
// is NOT in fields_json unless full_body was requested — fragments replace it.
message KnowledgeHit {
  string id = 1;
  double score = 2;
  string collection = 3;      // replaces Hit.source, which meant "index tier"
  string fields_json = 4;
  repeated string fragments = 5;
}

message KnowledgeSearchResponse {
  repeated KnowledgeHit hits = 1;
  int64 total = 2;
}
```

This also clears a standing wart: `Hit.source` is documented as `"episodic" | "semantic" | "graph"` (`engram.proto:194`), but knowledge search stuffs the collection name into it.

**D2 — Fragments are the default; fix the one consumer.** `internal/cli/vaultknowledge.go` is the only non-MCP consumer, and it is fixable rather than a constraint:

- `fetchKnowledgeDocs` (line 73) drains each collection with an **empty query** (`KnowledgeSearch(col.Name, "", nil, nil, retrieval.MaxK)`), and highlighting an empty query yields no fragments — so it would keep working by accident. Do not rely on that. It reads the body explicitly at line 116 (`stringField(fields, text)`) and writes it into the note file (line 262), so it genuinely needs the body: it passes `full_body: true` and says so.
- **It should also adopt Phase 3 paging.** Its own comments already state the defect this spec fixes — line 66: *"the RPC has no offset/cursor, so a second call could never see more than the first"* — and lines 85-88 emit a user-facing truncation warning whenever a collection returns exactly `MaxK` docs. With `offset`, the export drains properly and that warning is *deleted* rather than explained. Phase 3 is therefore a bug fix for an existing, in-code, user-visible limitation, not just an ergonomic win.

**D4 — `fragment_size` / `number_of_fragments` live on `CollectionSpec`, with global fallbacks.** An arXiv abstract and a 5,000-word markdown file want different budgets, and per-collection tuning is the point. The target shape is **llms.txt-sized**: enough medium for a capable reader to judge relevance and decide what to drill into, and no more. Global constants remain the default when a spec omits them.

**D5 — Refactor `buildQuery` to an options struct.** It is already 8 positional parameters (`opensearch.go:665`) and this change adds at least three more (highlight config, offset, source-suppression list). It is shared with the memory path, so the refactor carries its own regression check — cheaper than the call sites it would otherwise produce.

**D3 — Extract fragments; do NOT mark matches by default.** Web research (2026-07-22) turned up a genuine counter-finding: among production search APIs built specifically for LLM consumption, **most do not mark matches inline at all**. Tavily returns plain chunk text (1–3 chunks, ≤500 chars, `[...]` separator). Anthropic's own `web_search` tool exposes no snippet field whatsoever — page content is opaque and Claude self-cites ≤150 chars after reading. The official Elasticsearch and community OpenSearch MCP servers add no marking convention, passing raw DSL and raw JSON through. Brave is the lone counterexample (`<strong>`), and that is a repurposed human-UX feature, not a design-for-models decision.

The resolution is that **fragment extraction and match marking are separable concerns**, and only the first one is our actual problem. OpenSearch's `highlight` feature performs both, but `pre_tags`/`post_tags` accept empty strings. Returning 5,000-word bodies is fixed by extraction alone; markers are a decoration on top that the ecosystem has largely declined to add.

Therefore:
- **Default: `pre_tags: [""]`, `post_tags: [""]`** — clean fragment text, no markers. Relevance is signalled by ranking and fragment selection, as it is for Tavily and Anthropic's own tool.
- **Per-collection opt-in** (alongside D4's `fragment_size`/`number_of_fragments` on `CollectionSpec`) for a corpus where marking earns its keep.
- **If markers are ever enabled, the choice is tokenizer-dependent — measure against the target model, do not assume `«»`.** An earlier draft recommended guillemets as cheapest, from research measured on legacy tokenizers (cl100k / Anthropic-legacy). A **live measurement (2026-07-23)** against the actual `docs` index, tokenized with `o200k_base`, *inverted that ranking*: for one hit with 10 matched terms, `«»` cost **+60 tokens** while `<em>` cost **+51** — guillemets are non-ASCII and fragment into more BPE tokens on modern tokenizers, whereas `<em>` is common ASCII. So the "cheapest marker" answer flips between tokenizer generations. Whatever marker is chosen must still avoid corpus collision (`<em>` and `**` both appear literally in the docs and are disqualified on that ground regardless of cost), but no marker should be locked in without measuring on the model that will actually read the results.
- **A deeper reason not to mark, found in the same live run:** the highlighter is body-blind — it injects markers *inside fenced code blocks*. A real marked fragment came back as `let doubled = $«derived»(count * 2);`. In a documentation corpus an LLM may reproduce that snippet, so markers do not just cost tokens — they can hand the model syntactically corrupted code. The unmarked fragment is clean. This is concrete evidence for the no-marker default, independent of the token math.

**Measured impact (live, 2026-07-23, `knowledge-docs-v1`, query "derived reactivity dependency", top hit `$derived.md`, o200k_base):**

| Variant | Tokens | vs today |
|---|---|---|
| TODAY (full body) | 2016 | 1.00× |
| Fragments, unmarked | 285 | **0.14×** |
| Fragments, `«»` | 345 | 0.17× |
| Fragments, `<em>` | 336 | 0.17× |

The headline is fragments-vs-body (**7× reduction**), not markers (a ~20% tax on an already-small payload). Extraction banks the entire win; marking only spends part of it back.

**Evidence quality, stated honestly.** Well-sourced: the full-body-vs-fragment token reduction and the marker overhead (direct o200k_base measurement on the live index, reproducible); OpenSearch defaults; Tavily's and Anthropic's response shapes; the absence of any empirical study comparing marker families for span-recognition reliability. Inference only: that unmarked fragments serve the LLM as well as marked ones (no study either way — but the ecosystem precedent and the code-corruption finding both point this way). **Unresearched:** whether marking measurably helps or hurts LLM output quality, and whether llms.txt specifies anything about snippet size budgets. Neither gates the build, because the default is no markers.

Because the default is now *no markers*, none of the unresearched questions block the build. They would only matter if a collection opts marking on.

**D3a — Fragment sizing.** `fragment_size: 240`, `number_of_fragments: 3` as the global defaults. Grounding: OpenSearch's own default is 100 chars, which is tuned for prose snippets and is too short for markdown/code, where a fragment must survive containing a function signature, a prop table row, or a JSX block. Tavily's production choice (≤500 chars, 1–3 chunks) is the closest real-world precedent for LLM consumption. 240×3 sits between them. **This specific number is engineering judgment, not evidence** — no source specifies fragment sizing for markdown/code corpora. It is a per-collection knob precisely so it can be tuned against real results rather than argued about up front.

## Open questions

None blocking. See D3 for the two unresearched questions, neither of which gates the default configuration.

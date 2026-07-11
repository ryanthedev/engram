# Discovery + Design: Phase 5 - KnowledgeRetriever — BM25 + generic filters + sort + staleness

## Files Found
- `internal/retrieval/opensearch.go` (551 lines) — `MultiRetriever`/`tierRetriever` (memory hybrid path), `buildQuery` (line 499, the function this phase extends), `parseHits`, `clampK`/`DefaultK`/`MaxK` (all reusable, same package).
- `internal/retrieval/retriever.go` — `Hit{ID,Score,Source,Fields}` (reused as the knowledge search return type — no new hit type needed), `Retriever` interface (knowledge retriever is deliberately NOT this — BM25-only, no `Filter`/ACL shape).
- `internal/retrieval/opensearch_test.go` (package `retrieval_test`, black-box) — table-driven query-shape assertions via an `httptest` fake cluster + `reqCapture`; this is the pattern knowledge_test.go's black-box cases follow.
- `internal/knowledge/knowledge.go` — frozen `CollectionSpec{Name,Index,TextField,Mappings map[string]FieldSpec{Type,Filterable,Sortable},Access}`, `CollectionRegistry` interface (`Get/Create/Update/List/Provision`), `ErrNotFound`.
- `internal/store/registry.go` — `CollectionRegistry` OpenSearch impl: confirms every physical knowledge index carries `harvested_at` (date), `collection`/`source`/`source_version`/`harvest_id` (keyword), `title` (text) plus `spec.TextField` (text) and declared `Mappings` fields — the shape `Search`/`Collections` query against. `validateCollectionName`/`indexNameRE`-style barricades are the precedent for validating `spec.Index` before it enters a URL here (Phase-3 SECURITY LESSON, restated in Phase 4's `validateIndexName`).
- `internal/store/knowledge.go` (Phase 4) — `KnowledgeStore` holds **no embedder field/import at all**, making "zero embedding calls" a structural guarantee rather than a runtime check. `KnowledgeRetriever` follows the same pattern.
- `internal/mcp/mcp.go` — frozen MCP-facing DTOs `Predicate{Field,Op,Value}`, `SortKey{Field,Order}`, `CollectionInfo{CollectionSpec, Count int64, NewestHarvestedAt *time.Time, NewestDocDate *time.Time}` (Phase 1). Confirms the exact staleness shape `retrieval.CollectionMeta` must carry so Phase 6 can zip it 1:1 with a registry-fetched spec.
- `.code-foundations/research/2026-07-09-engram-knowledge-collection.md:128-130` — `knowledge_collections` response shape example: `{ name, count, text_field, filterable_fields, sortable_fields, newest_harvested_at, newest_doc_date }`.
- No existing `internal/retrieval/knowledge.go` or `knowledge_test.go` — both created fresh by this phase.

## Current State
`internal/retrieval` only implements the memory hybrid path (`MultiRetriever` over episodic/semantic tiers, BM25+kNN+RRF). Nothing in this package talks to a `knowledge.CollectionRegistry` or a knowledge index today. `buildQuery` has no `sort` parameter.

## Gaps
- `buildQuery` needs a `sort []any` parameter (additive) plus a `match_all` fallback for `text == ""` (currently unreachable by memory — both `MultiRetriever.Search` and `tierRetriever.search` short-circuit on `q.Text == ""` before calling `buildQuery`, so this addition changes zero currently-exercised behavior; the knowledge retriever *does* need it for filter-only search, DW-5.2's "empty query with only filters" edge case).
- `newest_doc_date`'s source field is not defined anywhere per-collection (no generic "the doc date field" concept exists in `knowledge.FieldSpec`). Resolved below (Design Decision).

## Code Standards
Applied: `doJSON`-style raw `net/http` + `map[string]any` (a package-local `postSearch` twin, mirroring how Phase 4 added `doNDJSON` as `doJSON`'s sibling in the same package rather than exporting across packages); `"pkg: verb-ing noun: %w"` error wraps; validate any caller-supplied name before it enters an OpenSearch REST path (Phase-3 SECURITY LESSON — applied here to `spec.Index`); functional low-ceremony constructor (`NewKnowledgeRetriever(client, baseURL, registry)`); stdlib `testing` only, no testify; three-group imports; sentinel errors (`knowledge.ErrNotFound`) returned unwrapped/matched via `errors.Is`.

## Test Infrastructure
`opensearch_test.go` uses `package retrieval_test` (black-box) with an `httptest`-based fake cluster (`newFakeSearchServer`/`reqCapture`) asserting substrings in the raw captured request body — reused for `KnowledgeRetriever.Search`/`Collections` black-box cases. `buildQuery` is unexported, so the DW-5.1 regression guard must live in an internal (`package retrieval`) test file — `knowledge_test.go` is written as `package retrieval` (white-box), matching Phase 4's `knowledge_test.go` precedent (`package store`, to reach `validateCollectionName`-style unexported barricades) and coexisting fine alongside the existing external `opensearch_test.go` in the same directory (Go supports one `_test` external package + white-box files together). A fake `knowledge.CollectionRegistry` (mirroring `internal/knowledge/seed_test.go`'s `fakeRegistry`) backs the `Collections` unit tests.

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-5.1 | BM25 search over `text_field` returns ranked hits; `buildQuery` memory path is byte-identical when `sort` is nil | COVERED | `TestKnowledgeSearchReturnsRankedHits` (black-box, fake cluster); `TestBuildQueryMemoryPathByteIdenticalWhenSortNil` — golden-byte comparison captured from the **pre-change** `buildQuery` for 6 mode/filter combinations (bm25 w/ and w/o filters, knn-only, hybrid w/ and w/o vector, hybrid+filters), asserting byte-exact match with `sort=nil` |
| DW-5.2 | term + range + prefix filters validated against the registry apply correctly; an unknown field errors with the valid-field list | COVERED | `TestKnowledgeSearchFilterClauseShapes` (table-driven: term/range/prefix → exact clause JSON in the captured request); `TestKnowledgeSearchUnknownFilterFieldNamesValidFields` |
| DW-5.3 | sort by a registered sortable field orders results; sort by a non-sortable field errors | COVERED | `TestKnowledgeSearchSortAppliesSortClause`; `TestKnowledgeSearchNonSortableFieldErrors` |
| DW-5.4 | `Collections` reports count + newest `harvested_at`/doc date per collection | COVERED | `TestCollectionsReportsCountAndStaleness`; `TestCollectionsEmptyCollectionHasNilStaleness` (boundary); `TestCollectionsUnprovisionedIndexReadsAsZero` (edge, mirrors house 404-as-empty rule) |

**All items COVERED:** YES

## Design: KnowledgeRetriever

### Approaches Considered
1. **Extend `buildQuery` in place** (add `sort []any`, add a `match_all` fallback for `text==""`) and reuse it for `KnowledgeRetriever.Search`; `Collections` holds a `knowledge.CollectionRegistry` dependency to enumerate collections and run one aggregation query per collection index.
2. **Fork a knowledge-local query builder**, leaving `buildQuery` untouched — zero risk to the memory path by construction, but duplicates the mode-dispatch/`_source` exclusion logic `buildQuery` already owns (not needed here since knowledge is BM25-only, but the bm25-clause+filter-wrapping logic is still shared).
3. **No registry dependency on the retriever at all**: `Search` takes a `spec` (as frozen), and `Collections` discovers collections by listing physical indices via an OpenSearch wildcard (`knowledge-*`) and reading each one's live mapping through the Get-Mapping API, deriving filterable/sortable-looking fields itself.

### Comparison
| Criterion | 1. Extend in place | 2. Fork builder | 3. No registry, wildcard discovery |
|-----------|---------------------|------------------|--------------------------------------|
| Interface simplicity | 2 methods, 1 constructor | 2 methods, 1 constructor | 2 methods, 1 constructor |
| Information hiding | High — one query builder, one place that knows the OpenSearch query shape | Medium — two builders now encode "how a BM25 clause is assembled" | **Low** — re-derives collection existence/field metadata the registry already owns; breaks Phase 3's "registry hides index mechanics" invariant (physical `knowledge-<name>-vN` naming leaks into the retriever) |
| Risk to memory path (DW-5.1) | Provably zero: the only new branch (`text==""`→`match_all`) is unreachable from memory today (both callers short-circuit earlier); proven via a **golden byte-capture** regression test taken from the pre-change function | Zero by construction, but throws away a working, tested function for no behavioral gain | N/A (no `buildQuery` change), but a different, larger risk: two independent sources of truth for "does this collection exist / what are its fields" |
| Assumption verification | **Confirms** the plan's HIGH-confidence assumption | Would trigger the plan's stated fallback despite the assumption holding | Off-topic to the assumption; rejected on other grounds |
| Caller ease of use | `Search(ctx, spec, query, filters, sort, k)` — spec already resolved by the barricade caller (Phase 6), no extra I/O per search | Same | `Search` unaffected; `Collections` now duplicates registry logic, more code for the same result |

### Choice: 1 — extend `buildQuery` in place + registry-backed `Collections`
Rationale: the golden-byte regression test proves the assumption holds (Assumption Verification below) — extension carries no memory-path risk and the frozen contract (`buildQuery(..., filters []any)` + additive `sort`) is exactly what the plan specifies, so forking would silently deviate from a directive without a real reason (the plan's own fallback trigger — "if extending disturbs the memory path" — did not fire). Approach 3 is rejected outright: it duplicates registry knowledge (filterable/sortable metadata, collection existence) and reaches through the registry's abstraction boundary (physical index names), which Phase 3 explicitly designed to hide. `Collections` needs *some* way to enumerate every registered collection with its full spec (to know `Index` and which `Mappings` fields are date-typed) since its frozen signature takes no parameters — a `knowledge.CollectionRegistry` constructor dependency is the only design that respects both the frozen signature and the registry's ownership of collection metadata.

### Depth Check
- Interface methods: 2 (`Search`, `Collections`).
- Hidden details: OpenSearch query-shape construction (term/range/prefix clause assembly, sort-clause assembly), the aggregation body/response shape for staleness (`max` aggs over `harvested_at` + every date-typed `Mappings` field, `track_total_hits` for an exact count), the 404-as-empty translation for an unprovisioned collection, index-name path-safety validation, per-item validation-error message construction (self-correcting field lists).
- Common case complexity: simple — one call with an already-resolved `CollectionSpec`, no filters/sort/registry round-trip required for a plain BM25 query.

## Design Decision: `newest_doc_date` source (resolved)
No `knowledge.FieldSpec` marks "the" date field for a collection — a collection can declare zero, one, or several `date`-typed `Mappings` fields (e.g. arXiv's `published`). Staying generic (never hardcoding a field name like `published_date`, per the phase's core constraint), `newest_doc_date` is defined as: **the max value across every `date`-typed field declared in `spec.Mappings`** — one `max` aggregation per date field, then the max of those maxes. A collection with no declared date field reports `NewestDocDate: nil` (not an error — "undated" is a legitimate collection state per `mcp.CollectionInfo`'s doc comment: "Nil timestamps mean the collection is empty or undated").

## Design Decision: HTTP plumbing (`postSearch`)
`tierRetriever.search`'s existing request-build/response-decode block is left untouched — DW-5.1 pins the memory path byte-for-byte, so refactoring working, tested code purely for deduplication is needless regression risk for a phase whose explicit job is *not* to touch memory behavior. Instead, `postSearch` is a small new package-local twin (status+decoded, same semantics as `store.doJSON`, which cannot be imported/reused directly — it's unexported in a different package), used only by `KnowledgeRetriever`. This mirrors Phase 4's precedent (`doNDJSON` added as `doJSON`'s sibling in the *same* package rather than exporting cross-package).

## Defensive-programming decisions (cc-defensive-programming)
- **Barricade before any path interpolation.** `spec.Index` is caller-supplied input to `Search`/`Collections` (an "internal team API is still external" case — Phase 6 fetches it from the registry today, but nothing stops a future or test caller from constructing a `CollectionSpec` by hand). A `validateKnowledgeIndex` barricade (same grammar as `store.indexNameRE`, redeclared locally since it's unexported in a different package) runs before every URL is built, closing the exact path-traversal class Phase 3 found.
- **Field/op/order validation is self-correcting, not silent.** An unknown or unfilterable field, an unknown filter op, a malformed `range` value, an unknown/unsortable sort field, and an invalid sort order all return a validation error naming the offending value AND (for field errors) the valid field list — never a silent empty result. This is the "unknown unknowns" guard the plan explicitly calls for (LLM caller self-repairs on the next turn).
- **Correctness over robustness for `Count`.** The aggregation query sets `track_total_hits: true` explicitly rather than relying on OpenSearch's default (which can cap `hits.total` at 10,000 and mark it `"gte"` instead of exact) — a knowledge platform surfacing a wrong document count to a caller deciding whether to re-harvest is a worse failure than the extra aggregation cost.
- **Zero embedding calls is structural, not tested-only.** `KnowledgeRetriever` holds no `embed.Embedder` field and does not import `internal/embed` — matching `KnowledgeStore`'s precedent (Phase 4), "BM25-only" is a compile-time guarantee, not a runtime promise that could regress silently.
- **404-as-empty, not error, for an unprovisioned collection.** A registered-but-not-yet-provisioned collection (Phase 3's documented `Create`-partial-failure repair scenario) reads as zero docs / nil staleness in `Collections`, and zero hits in `Search` — consistent with the house `index_not_found_exception`-as-empty rule (`store.isIndexNotFound`), redeclared locally as `isKnowledgeIndexNotFound` for the same cross-package-unexported reason as `postSearch`.

## Assumption Verification
**Assumption:** "`buildQuery` can gain a `sort` block without disturbing memory queries" (HIGH confidence).
**Verified:** captured the pre-change `buildQuery`'s actual output (via a throwaway test run against the unmodified function) for 6 representative mode/filter combinations spanning every branch memory traffic can hit (BM25-only ±filters, kNN-only, hybrid ±vector ±filters). After adding the `sort []any` parameter and the (memory-unreachable) `text==""→match_all` branch, `TestBuildQueryMemoryPathByteIdenticalWhenSortNil` reconstructs each of those 6 golden bodies independently and asserts byte-exact equality against the new function called with `sort=nil`. All 6 passed — **the assumption holds**. Extending in place proceeds; the fallback (fork a knowledge-local builder) is not triggered.

## Prerequisites
- [x] Phase 3 `knowledge.CollectionSpec`/`CollectionRegistry`/`FieldSpec` frozen and available.
- [x] Phase 1 `mcp.Predicate`/`SortKey`/`CollectionInfo` shapes confirm the DTOs this phase's types feed.
- [x] `buildQuery`, `clampK`, `parseHits` reusable (same package, unexported, no cross-package export needed).
- [x] No missing prerequisites.

## Recommendation
BUILD.

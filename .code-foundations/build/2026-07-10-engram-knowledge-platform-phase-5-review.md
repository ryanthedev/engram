# Review: Phase 5 - KnowledgeRetriever

## Executed Results (Step 0)

All commands run from the worktree root, `/Users/r/repos/engram/.claude/worktrees/engram-knowledge-platform`, first via the PATH-resolved `go`/`make`, then re-verified with the absolute-path binary `/usr/local/go/bin/go` per the dispatch prompt's exit-code-swallowing warning. No discrepancy was found between the two invocations.

- `go build ./...` → success. Re-verified: `/usr/local/go/bin/go build ./...` → `REAL_EXIT=0`.
- `make test` → all packages `ok` (including `internal/retrieval`). `MAKE_EXIT=0`. Re-verified: `/usr/local/go/bin/go test ./internal/retrieval/... -count=1` → `ok github.com/ryanthedev/engram/internal/retrieval 0.028s`, `REAL_EXIT=0`.
- `make lint` → `go vet ./...` and `revive` both ran clean. `LINT_EXIT=0`.
- Integration: `ENGRAM_OPENSEARCH_URL=http://localhost:9200 /usr/local/go/bin/go test -tags=integration -count=1 -v ./internal/retrieval/ -run 'Knowledge|Collections|BuildQuery|Sort|Filter'` → all matched tests `PASS`, `INTEGRATION_EXIT=0`. Note: no dedicated `knowledge_integration_test.go` exists in this package — the name filter re-ran the same httptest-fake-backed knowledge unit tests (`TestKnowledgeSearch*`, `TestCollections*`, `TestBuildQueryMemoryPath*`, `TestValidateKnowledgeIndex*`) plus unrelated memory-path ACL/filter integration tests that happened to match the regex (`TestDW_4_1_ScopeMatrixCompiledFilter`, `TestTierRetrieverAppliesTenantAndUserFilters`, etc.). No test in the repo exercises `KnowledgeRetriever.Search`/`Collections` against a real, live OpenSearch cluster; all knowledge-retriever coverage is against `httptest` fakes that faithfully reproduce the OpenSearch wire format (request path, JSON body echoed back to assertions, and hand-built response JSON).

## Requirement Fulfillment

### DW-5.1
PREMISE:  "BM25 search over the collection's text field returns ranked hits; the `buildQuery` MEMORY path is byte-identical when the new `sort` param is nil (regression test — this guards the untouched-memory boundary; verify the golden-byte test actually compares real query bodies)."
EVIDENCE: `internal/retrieval/knowledge.go:103-131` (Search, calls `buildQuery(ModeBM25Only, spec.TextField, "", query, nil, clampK(k), filterClauses, sortClauses)`); `internal/retrieval/opensearch.go:515-553` (buildQuery, additive `sort []any` param); `internal/retrieval/knowledge_test.go:132-157` (`TestKnowledgeSearchReturnsRankedHits`); `internal/retrieval/knowledge_test.go:167-222` (`TestBuildQueryMemoryPathByteIdenticalWhenSortNil`).
TRACE: `git diff HEAD -- internal/retrieval/opensearch.go` shows the pre-Phase-5 `buildQuery` built `bm25Query := map[string]any{"match": ...}` unconditionally and had no `sort` param; Phase 5 only adds an `if text != ""` guard (unreachable from memory — both memory callers short-circuit on `q.Text == ""` before calling `search`) and an additive `if len(sort) > 0 { query["sort"] = sort }` tail. `TestBuildQueryMemoryPathByteIdenticalWhenSortNil` calls the real `buildQuery` function directly (not a mock) across all 6 branches memory traffic can hit (BM25-only ±filters, kNN-only, hybrid ±vector ±filters) with `sort=nil`, and asserts the marshaled JSON bytes equal literal strings pinned as the pre-Phase-5 output — e.g. `{"_source":{"excludes":[...]},"query":{"match":{"text":"hello"}},"size":5}` with no `"sort"` key. Ran: PASS on all 6 subtests. This is a genuine golden-byte comparison of real `buildQuery` output, not a re-derivation of the code under test. `TestKnowledgeSearchReturnsRankedHits` confirms a BM25 query over the non-default configured `TextField` ("abstract") returns 2 hits sourced "arxiv" and the request body contains a `match` clause over `abstract` with no `"knn"`/`"hybrid"` substrings.
VERDICT: PASS

### DW-5.2
PREMISE:  "term + range + prefix filters validated against the registry mapping apply correctly; an unknown filter field errors with a message that NAMES the valid filterable fields."
EVIDENCE: `internal/retrieval/knowledge.go:286-332` (buildFilterClauses, filterClause); `internal/retrieval/knowledge_test.go:253-323` (`TestKnowledgeSearchFilterClauseShapes`, `TestKnowledgeSearchUnknownFilterFieldNamesValidFields`).
TRACE: `Predicate{Field:"categories", Op:"term", Value:"cs.CL"}` → `spec.Mappings["categories"].Filterable==true` → `filterClause` term branch → `{"term":{"categories":"cs.CL"}}` in the outgoing request body — PASS. `Predicate{Field:"published", Op:"range", Value:{"gte":"2024-01-01"}}` → range branch, one bound present → `{"range":{"published":{"gte":"2024-01-01"}}}` — PASS. `Predicate{Field:"published", Op:"range", Value:map[string]any{}}` (no bounds) → `len(rng)==0` → error — PASS (wantErr). `Predicate{Field:"doi", ...}` (not in Mappings) → `buildFilterClauses` returns `unknown or unfilterable field "doi" on collection "arxiv"; valid filterable fields: categories, published` — test asserts message contains both the offending field and every valid filterable field. `Predicate{Field:"internal", ...}` (declared, `Filterable:false`) → same error path (`!fs.Filterable`) — also asserted.
VERDICT: PASS

### DW-5.3
PREMISE:  "sort by a registered sortable field orders results; sort by a non-sortable field errors."
EVIDENCE: `internal/retrieval/knowledge.go:338-355` (buildSortClauses); `internal/retrieval/knowledge_test.go:329-368` (`TestKnowledgeSearchSortAppliesSortClause`, `TestKnowledgeSearchNonSortableFieldErrors`).
TRACE: `SortKey{Field:"published", Order:"desc"}` → `spec.Mappings["published"].Sortable==true`, `Order` valid → request body contains `"sort":[{"published":{"order":"desc"}}]` — PASS (matches how the OpenSearch-side ordering is requested; consistent with how the existing memory-path BM25/kNN ranking tests validate query construction rather than re-implementing OpenSearch's own ordering). `SortKey{Field:"categories", ...}` (declared, `Sortable:false`) → `!fs.Sortable` → error. `SortKey{Field:"doi", ...}` (undeclared) → `!ok` → error. `SortKey{Field:"published", Order:"sideways"}` → `Order` not "asc"/"desc" → error "invalid sort order". All three asserted to return non-nil errors.
VERDICT: PASS

### DW-5.4
PREMISE:  "`Collections` reports count + newest `harvested_at`/doc date per collection."
EVIDENCE: `internal/retrieval/knowledge.go:139-198` (Collections, collectionMeta); `internal/retrieval/knowledge_test.go:404-490` (`TestCollectionsReportsCountAndStaleness`, `TestCollectionsEmptyCollectionHasNilStaleness`, `TestCollectionsUnprovisionedIndexReadsAsZero`, `TestCollectionsPropagatesRegistryErrors`).
TRACE: fake response `{"hits":{"total":{"value":42}}, "aggregations":{"newest_harvested_at":{"value":<harvested-millis>}, "newest_doc_date_0":{"value":<published-millis>}}}` → `totalHits` reads 42 → `maxDateFromAgg` decodes both max aggs (keyed by `dateMappingFields` sorted + `docDateAggKey(i)`, here `published` is the sole date-typed Mappings field so index 0) → `CollectionMeta{Name:"arxiv", Count:42, NewestHarvestedAt:harvested, NewestDocDate:published}` — all four fields asserted equal. Request body asserted to contain `"size":0`, `"track_total_hits":true`, and max-agg fields over both `harvested_at` and `published`. Empty-collection case (`total.value=0`, `newest_harvested_at.value=nil`) → `Count=0`, both timestamps nil — asserted. 404 index-not-found case → `CollectionMeta{Name:spec.Name}` (zero Count, nil timestamps), not an error — asserted.
VERDICT: PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] DW-5.1: `TestKnowledgeSearchReturnsRankedHits`, `TestBuildQueryMemoryPathByteIdenticalWhenSortNil` (ran in Step 0, PASS)
- [x] DW-5.2: `TestKnowledgeSearchFilterClauseShapes`, `TestKnowledgeSearchUnknownFilterFieldNamesValidFields` (PASS)
- [x] DW-5.3: `TestKnowledgeSearchSortAppliesSortClause`, `TestKnowledgeSearchNonSortableFieldErrors` (PASS)
- [x] DW-5.4: `TestCollectionsReportsCountAndStaleness`, `TestCollectionsEmptyCollectionHasNilStaleness`, `TestCollectionsUnprovisionedIndexReadsAsZero`, `TestCollectionsPropagatesRegistryErrors` (PASS)
- [x] All DW items have corresponding tests that ran in Step 0
- Per-function `go tool cover` on `knowledge.go` (filtered to knowledge tests): most functions at 100%; `Search` 78.9%, `Collections` 73.3%, `collectionMeta` 86.4%, `postSearch` 80.0%, `fieldListOrNone` 66.7% — the uncovered lines are HTTP-transport error branches (`http.NewRequestWithContext` failure, `client.Do` failure, body-read failure) and the "(none declared)" formatting branch of `fieldListOrNone`, none of which map to a DW item or a listed edge case. See Notes.

## Dead Code
None found. All imports in `knowledge.go` are used; no unreachable statements after early returns; no debug prints or commented-out blocks.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | `KnowledgeRetriever` holds no mutable shared state across calls beyond the read-only `*http.Client`/`CollectionRegistry`; `Collections` iterates `registry.List`→`Get` sequentially with no goroutines — no concurrency introduced by this phase to probe. |
| Error Handling | PASS | Traced a malformed-JSON 200 response: `postSearch` swallows `json.Unmarshal` errors (`_ = json.Unmarshal(...)`, `knowledge.go:393`) leaving `decoded` as a nil map; `parseHits`/`totalHits`/`maxDateFromAgg` all read from `decoded` via the `v, ok := decoded[...].(T)` pattern, which is safe on a nil map (returns the zero value, no panic) — traced through to `Search` returning `(nil hits, nil err)` rather than crashing. Registry errors (`List`/`Get`) are wrapped and propagated (`TestCollectionsPropagatesRegistryErrors`, PASS). |
| Resources | PASS | `postSearch` (`knowledge.go:386`) always `defer resp.Body.Close()` regardless of subsequent read/decode failure — traced, no leak on any exit path. |
| Boundaries | PASS | Traced `filterClause` with `Op:"range", Value:"not-a-map"` (not a `map[string]any`) → `bounds, ok := value.(map[string]any)`; `ok=false` → explicit error, no panic (`TestKnowledgeSearchFilterClauseShapes/range_malformed_value`, PASS). Traced `dateMappingFields`/`filterableFields`/`sortableFields` against a spec with a nil `Mappings` map (`TestCollectionsEmptyCollectionHasNilStaleness`'s spec) — ranging over a nil map in Go is a zero-iteration no-op, no panic. |
| Security | PASS | Traced `spec.Index = "../secret"` into `Search`: `validateKnowledgeIndex(spec.Index)` (`knowledge.go:104`) runs first, `knowledgeIndexNameRE` rejects the leading `.` and the `strings.Contains(index, "..")` check independently catches it — `Search` returns the error immediately, before `postSearch`/`r.baseURL+"/"+spec.Index+"/_search"` is ever constructed. Same guard is the first statement of `collectionMeta` (`knowledge.go:167`). `TestValidateKnowledgeIndexRejectsPathTraversal` exercises the validator directly against `"", "../secret", "knowledge-arxiv/../../x", "has spaces", "UPPER"` (all rejected) and one valid value (accepted) — PASS. |

## Loaded-Skill Criteria
N/A — no skills loaded (dispatch prompt had no `## Additional Skills` section).

## Notes (non-blocking)

- **No live-OpenSearch coverage for the knowledge retriever specifically.** The dispatch prompt's suggested integration command (`-tags=integration ... -run 'Knowledge|Collections|BuildQuery|Sort|Filter'`) ran successfully against the live cluster at `localhost:9200`, but there is no `knowledge_integration_test.go` in the package — every knowledge-retriever assertion (BM25 query shape, filter/sort clause shape, staleness aggregation parsing) is validated against `httptest`-fake OpenSearch responses, not a real cluster's actual query-execution behavior (e.g., whether OpenSearch truly ranks/sorts as requested, or whether a `prefix`/`range` clause against a real `keyword`/`date` mapping behaves as expected). This is consistent with how the existing memory-path `tierRetriever` unit tests are structured (also fake-server-based, with separate `opensearch_integration_test.go`/`acl_integration_test.go` files covering the *memory* tiers against real OpenSearch) — but no analogous real-cluster test exists yet for the knowledge tiers. Not a DW-listed or edge-case-listed requirement, so not a FAIL, but worth flagging since the dispatch prompt explicitly offered a live-OpenSearch run path expecting it to exercise the knowledge retriever specifically.
- Minor: `fieldListOrNone`'s `"(none declared)"` branch (a collection with zero filterable/sortable fields) is not exercised by any test — cosmetic message-formatting code with no failure mode possible; not a DW item or listed edge case.
- `TestKnowledgeSearchReturnsRankedHits`/`TestKnowledgeSearchSortAppliesSortClause` validate that the correct query/sort clause is *sent* to OpenSearch, not that the retriever independently re-sorts the returned hits — this mirrors the existing memory-path pattern (OpenSearch is trusted to rank/sort server-side; `parseHits` is a straight pass-through of response order). Not a gap introduced by this phase.

**Verdict: PASS**

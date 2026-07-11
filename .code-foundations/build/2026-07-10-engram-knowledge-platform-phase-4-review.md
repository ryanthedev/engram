# Review: Phase 4 - KnowledgeStore (bulk upsert + mark-and-sweep delete)

## Executed Results (Step 0)
- `go build ./...` → Success.
- `make test` → all packages `ok` (unit suite, no `integration` build tag; `internal/store` passes).
- `make lint` → `go vet ./...` clean; `revive` clean, exit 0.
- Integration suite (`ENGRAM_OPENSEARCH_URL=http://localhost:9200 go test -tags=integration -count=1 -v ./internal/store/ -run 'DW_4|BulkIndex|DeleteByQuery|TextField'`) → 16 tests run, all `--- PASS`, `ok  github.com/ryanthedev/engram/internal/store  0.881s`. This includes all 15 `func Test...` in `internal/store/knowledge_test.go` (confirmed by name-matching the full `grep ^func Test` list against the run log — every test in the file fired and passed).

## Requirement Fulfillment

### DW-4.1
PREMISE:  `BulkIndex(ctx, index, textField, docs, harvestID)` upserts N docs by `_id`, writes `Document.Text` under the caller-supplied `textField` (NOT a hardcoded `"text"`), stamps `harvest_id`/`source_version`/`harvested_at`, issues zero embedding calls. A collection with a NON-default `TextField` (e.g. `abstract`) must round-trip.
EVIDENCE: `internal/store/knowledge.go:106-141` (`BulkIndex`), `:146-176` (`buildBulkBody`, row keyed by `textField` at line 161, `harvest_id`/`harvested_at` stamped last at 165-166); `internal/store/knowledge_test.go:93-138` (`TestDW_4_1_BulkIndexStampsFieldsNoEmbedCalls`), `:165-195` (`TestDW_4_1_BulkIndexRoundTripsCustomTextField`).
TRACE: `provisionedCollection(t, "abstract")` → `spec.TextField == "abstract"` → `BulkIndex(ctx, spec.Index, "abstract", [{ID:"p1", Text:"quantum entanglement in superconductors", ...}], "h1")` → `buildBulkBody` sets `row["abstract"] = "quantum entanglement..."`, never `row["text"]` → live doc read shows `src["abstract"] == "quantum entanglement in superconductors"`, no `"text"` key present → `matchQueryHits(..., "abstract", "entanglement")` returns 1. Zero embedding calls confirmed structurally: `KnowledgeStore` holds only `client`/`baseURL` (no embedder field, knowledge.go:34-37), no embed-package import anywhere in the file, and the first test asserts no `*_embedding` key lands on the stored doc.
VERDICT: PASS

### DW-4.2
PREMISE:  re-`BulkIndex` of the same `_id` overwrites in place (no duplicate row).
EVIDENCE: `internal/store/knowledge.go:149` (`_bulk` action `"index"`, not `"create"` — upsert-by-`_id`); `internal/store/knowledge_test.go:230-254` (`TestDW_4_2_ReBulkIndexOverwritesInPlace`).
TRACE: `BulkIndex([{ID:"dup", Title:"First", Text:"v1 text"}], "h1")` then `BulkIndex([{ID:"dup", Title:"Second", Text:"v2 text"}], "h2")` → `countDocs(...) == 1` → `getDoc(..., "dup")` returns `title="Second", text="v2 text", harvest_id="h2"`.
VERDICT: PASS

### DW-4.3
PREMISE:  `DeleteByQuery` removes rows matching `collection` AND `source` AND `harvest_id != currentHarvestID`, leaving current-run rows.
EVIDENCE: `internal/store/knowledge.go:242-258` (`deleteByQueryBody`: `must=[term collection, term source]`, `must_not=[term harvest_id=currentHarvestID]`); `internal/store/knowledge_test.go:260-296` (`TestDW_4_3_DeleteByQuerySweepsStaleRows`).
TRACE: seed `stale{h1,kaggle}`, `otherSource{h1,oai-pmh}` via `BulkIndex(...,"h1")`; seed `current{h2,kaggle}` via `BulkIndex(...,"h2")` → `DeleteByQuery(index, spec.Name, "kaggle", "h2")` → `deleted == 1` → `stale` gone, `current` (same harvest) kept, `otherSource` (different `source`) kept — proves the three-way AND, not just harvest-id mismatch.
VERDICT: PASS

### DW-4.4
PREMISE:  a `_bulk` response containing per-item errors surfaces them (does not report full success).
EVIDENCE: `internal/store/knowledge.go:136-139` (non-nil `failures` → non-nil error, never masked by a 200 top-level status); `:182-199` (`bulkItemResults`, per-item walk); `internal/store/knowledge_test.go:303-324` (`TestDW_4_4_BulkIndexSurfacesPerItemErrors`).
TRACE: batch of `good{Fields: declared only}` + `bad{Fields: undeclared_field}` against a strict-mapped collection → `BulkIndex` returns `(1, non-nil error)` → `good` doc present, `bad` doc absent.
VERDICT: PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] All 4 DW items have dedicated, name-matching automated tests that ran and passed live against OpenSearch in Step 0 (`TestDW_4_1_...` x2, `TestDW_4_2_...`, `TestDW_4_3_...`, `TestDW_4_4_...`).
- [x] Coverage matches the stated 100% level — every one of the 15 tests in `knowledge_test.go` ran and passed; no DW item or listed edge case lacks a test.

## Dead Code
None found. `internal/store/opensearch.go`'s diff is purely additive (`doNDJSON`, 27 lines) with no leftover/unreachable code. `internal/store/knowledge.go` has no unused imports, no unreachable statements after returns, no debug/commented-out code.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | `KnowledgeStore` carries only immutable `client`/`baseURL` fields (knowledge.go:34-37); `BulkIndex`/`DeleteByQuery` hold no shared mutable state, no cache, no goroutines — concurrent calls trace independently through `doNDJSON`/`doJSON`, each with its own request/response. |
| Error Handling | PASS | Traced: nil `Fields` map → `for k, v := range d.Fields` is a safe no-op on nil in Go, no panic (knowledge.go:157). Traced: malformed/absent `"items"` in the `_bulk` response → `decoded["items"].([]any)` type-asserts to `(nil, false)`, loop body never runs, `succeeded=0, failures=nil` — reported as success with 0 docs rather than a spurious panic (no test exercises this exact shape, but the code path is safe by construction). Transport errors, non-200 statuses, and `_delete_by_query`'s own `"failures"` array are all separately checked and surfaced (knowledge.go:130-135, 233-235). |
| Resources | PASS | `doJSON`/`doNDJSON` both `defer resp.Body.Close()` (opensearch.go:236, 263) on every return path including early errors after a successful `client.Do`. No file handles, locks, or connections held across calls. |
| Boundaries | PASS | Traced empty `docs` → immediate `(0, nil)` before any validation or network call (knowledge.go:107-109). Traced empty `harvestID` on `DeleteByQuery` → rejected before the request is built (knowledge.go:215-221), preventing the `harvest_id != ""` full-wipe scenario the comment calls out. Traced empty doc `ID` → rejected per-doc before any HTTP call (knowledge.go:119-123). |
| Security | PASS | Traced `textField="harvest_id"` (reserved) and `textField="bad-field!"` (regex fail) against a network-fail-fast `httptest.Server` → both rejected pre-request (`TestBulkIndexRejectsReservedTextField`, live PASS). Traced `index="../other-index"` on both `BulkIndex` and `DeleteByQuery` against the same fail-fast server → both rejected pre-request (`TestBulkIndexRejectsPathTraversalIndex`, `TestDeleteByQueryRejectsPathTraversalIndex`, live PASS). `collection`/`source`/`currentHarvestID` are placed into the delete query via `json.Marshal` of a `map[string]any` (knowledge.go:242-256), never string-concatenated — no injection surface. |

## Loaded-Skill Criteria
N/A — no skills loaded (dispatch prompt carried no `## Additional Skills` section).

## Notes (non-blocking)
- `DeleteByQuery`'s own `"failures"` branch (knowledge.go:233-235, OpenSearch-reported partial version-conflict failures inside an otherwise-200 `_delete_by_query` response) returns `(0, error)` rather than the partial `deleted` count that did succeed — asymmetric with `BulkIndex`, which does return the partial `succeeded` count alongside its error. Not a DW item, not a listed edge case (the listed edge case only covers zero-match, not partial-failure, for `DeleteByQuery`), and no test exercises it — recorded for awareness only.
- `bulkItemResults` on a response missing/malformed `"items"` degrades to `(0, nil)` — reported as a successful no-op rather than an error. Plausible only if OpenSearch itself returns 200 with a broken body shape, which the live cluster never does; not demonstrated as reachable, so not a FAIL.
- Document comments are unusually dense (SECURITY LESSON callbacks to Phase 3, explicit "do not fix this back to append-only" warnings) — this is deliberate anti-regression documentation given the type doc's stated intent, not accidental clutter.

## Issues (if FAIL)
None.

**Verdict: PASS**

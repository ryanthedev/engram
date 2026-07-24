# Review: Phase 2 - knowledge-search fragments + memory_read drill-down (sample 2)

## Executed Results (Step 0)
- Unit suite: `go test ./...` → all packages ok, 0 failures
- Build/vet: `go build ./... && go vet ./... && go vet -tags=integration ./...` → clean (BUILD_VET_OK)
- Lint: `make lint` (go vet + revive) → clean, exit 0
- Integration (live OpenSearch 3.1.0 at :9200): `ENGRAM_OPENSEARCH_URL=http://localhost:9200 go test -tags=integration -count=1 -v ./internal/server/ ./internal/retrieval/ ./internal/mcp/...` → **247 PASS / 0 FAIL / 0 SKIP** (verbose log: scratchpad/review-p2-s2/integration-v.log)

## Requirement Fulfillment

### DW-2.1
PREMISE:  "A `knowledge_search` hit returns extracted fragments and NO `body`/text field in `fields_json` by default; the fragment payload is materially smaller than the full body (many more hits fit a fixed byte budget)."
EVIDENCE: internal/retrieval/opensearch.go:743-754 (highlight clause + text-field `_source` exclude gated on one knob); internal/retrieval/knowledge.go:132-135 (default path sets FragmentSizing + tags); internal/server/knowledge.go:188-196 (Fragments mapped to wire); internal/mcp/tools.go:413-439.
TRACE:    16 docs with ~5KB bodies ingested → default `KnowledgeSearch(col, "gadget", …, false)` → each hit: `fields_json` has no `text` key, 1–3 fragments present, collection tagged; 12-hit page serializes ≤ 16384 bytes where 12 full bodies would be ~60KB+.
VERDICT:  **PASS** — TestDW_2_FragmentsAndDrillDownEndToEnd (live e2e incl. the byte-budget assertion at knowledge_fragments_integration_test.go:99-104), TestKnowledgeSearchDW_2_1_DefaultRequestsFragments, TestKnowledgeSearchToolEmitsCollectionAndFragments; all executed and passing.

### DW-2.2
PREMISE:  "Fragments carry no markers when the collection's `highlight_pre_tag`/`highlight_post_tag` are empty; setting them produces fragments wrapped in exactly those strings."
EVIDENCE: internal/retrieval/opensearch.go:749-750 (`pre_tags`/`post_tags` emitted as-is — `[""]` when unset, OpenSearch's markers-off escape); internal/knowledge/knowledge.go:67-68 (no fallback for tags — empty means off).
TRACE:    Untagged collection → fragments are verbatim substrings of the stored body (integration asserts `strings.Contains(body, frag)` per fragment); UpdateCollection to «/» tags → same query yields a fragment containing `«frobnicate_widget»` exactly.
VERDICT:  **PASS** — TestBuildQueryDW_2_2_HighlightDefaultsEmptyTags, TestBuildQueryDW_2_2_CustomTags, TestKnowledgeSearchPerCollectionSizingAndTags (unit), and the live round-trip in TestDW_2_FragmentsAndDrillDownEndToEnd:106-140.

### DW-2.3
PREMISE:  "`knowledge_search` with `full_body: true` returns the whole body inline (pre-fragment behavior preserved)."
EVIDENCE: internal/retrieval/knowledge.go:132-135 (fragment opts only set when `!fullBody`); internal/server/knowledge.go:175 (threads `req.GetFullBody()`); internal/mcp/tools.go:420,432.
TRACE:    `fullBody=true` → request body byte-equal to zero-value-highlight `buildQuery` output (asserted against golden pre-fragment query in TestKnowledgeSearchDW_2_3_FullBodySkipsHighlight:171-176); live e2e: `fields_json["text"] == stored body`, zero fragments.
VERDICT:  **PASS** — TestKnowledgeSearchDW_2_3_FullBodySkipsHighlight, TestKnowledgeSearchHandlerMapsFragmentsAndFullBody, TestKnowledgeSearchToolForwardsFullBody, e2e §DW-2.3.

### DW-2.4
PREMISE:  "`memory_read(id, source)` where source is a readable registered collection returns the full document; an unreadable collection fails closed (opaque not-found) and an unknown source returns a self-correcting message — authz enforced in `internal/server/read.go`."
EVIDENCE: internal/server/read.go:138-168 (`readKnowledge`: resolve → authorize (line 153) → fetch (line 156)); internal/retrieval/knowledge.go:159-197 (GetDocument); internal/mcp/tools.go:352-376 (source forwarded, graph still short-circuits).
TRACE:    Readable: `Read("doc-03", col)` → full stored `_source` incl. body and title (live e2e). Unreadable (role-gated, doc EXISTS): NotFound "record not found", and the fake reader recorded NO fetch (read_knowledge_test.go:76-78). Unknown source: InvalidArgument naming "episodic", "semantic", "knowledge_collections".
VERDICT:  **PASS** — TestReadKnowledge_DW_2_4_{ReturnsFullDocument, UnreadableFailsClosedOpaque, UnknownSourceSelfCorrecting}, TestDW_2_4_DrillDownFailsClosedEndToEnd (live, through the real auth interceptor).

### DW-2.5
PREMISE:  "memory-tier `memory_search`/`memory_read` behavior is unchanged (regression)."
EVIDENCE: internal/retrieval/opensearch.go:652-684 (queryOpts zero value = pre-fragment bytes), 762-800 (parseHits inert without a highlight key).
TRACE:    Zero-value highlight opts → no highlight clause, unchanged embedding-only excludes (byte-level golden matrix in buildquery_golden_test.go + TestBuildQueryMemoryPathByteIdenticalWhenSortNil); memory-shaped response → nil Fragments; episodic/semantic/graph Read dispatch unchanged with a knowledge registry wired.
VERDICT:  **PASS** — TestBuildQueryDW_2_5_ZeroValueOmitsHighlight, TestParseHitsDW_2_5_NoHighlightKeyInert, TestReadMemoryTiersUntouchedByKnowledgeWiring, TestMemoryReadGraphStillShortCircuits, TestDW_6_5_MemoryToolsUnchanged, TestServerRead_DW_2_1/2_2/2_3_2_4/2_7 — all in the -count=1 run.

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have corresponding tests that ran in Step 0 (DW-IDs in test names throughout)
- [x] Coverage matches the stated 100% level: unit (query construction, parse, GetDocument edge grid, handler mapping, tool forwarding) + live end-to-end for every DW item
- No gaps.

## Edge cases (prompt-listed)
| Edge case | Status | Evidence |
|---|---|---|
| Empty-query (filter-only) → scalars only, no fragments, no body, no error | HANDLED | opensearch.go:702-706 (match_all fallback) + live e2e §DW-2.2b: hits keep `lang`, lose `text`, zero fragments |
| Match inside fenced code block, empty tags → verbatim, no marker corruption | HANDLED | e2e:106-122 — fragment containing `frobnicate_widget(cfg)` is a verbatim substring of the stored body |
| Unreadable collection → opaque not-found (fail-closed) | HANDLED | read.go:153-154; unit (no-fetch assertion) + live e2e error-string equality with a genuine miss |
| Unknown source → self-correcting error | HANDLED | read.go:146-148; unit + live e2e assert the vocabulary is named |

## Dead Code
None blocking. No debug statements, commented-out blocks, or unreachable code in the diff; revive + go vet clean. (Stale comments → Notes.)

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | KnowledgeRetriever is stateless (client/baseURL/registry, all read-only after construction); server handlers add no shared mutable state; traced Search/GetDocument — no writes to shared structures |
| Error Handling | PASS | GetDocument: non-200/404 → error (never masqueraded as absence, TestGetDocumentUnexpectedStatusErrors); 200-without-_source → loud error; reader failure → Internal, not opaque NotFound (TestReadKnowledge_ReaderErrorIsInternal) |
| Resources | PASS | `defer resp.Body.Close()` on both GetDocument (knowledge.go:174) and postSearch (knowledge.go:452); no new handles/locks |
| Boundaries | PASS | empty id → opaque miss without a cluster call (knowledge.go:163-165); empty query → match_all (opensearch.go:703-706); k clamped; numberOfFragments=0 → highlighting off; adversarial traces all handled |
| Security | PASS | See loaded-skill table — authorize-before-fetch, byte-identical opaque denial, injection probes, fail-closed all demonstrated |

### Security deep-dive (dispatch-mandated)
- **Authorize-before-fetch:** read.go authorizes at line 153, fetches at line 156. Demonstrated two ways: unit test asserts the fake reader recorded **no fetch** on denial (read_knowledge_test.go:76-78), and live e2e compares the denial error against a genuine-miss error for **string equality** (integration test:232-234). Both converge on the single `errReadNotFound` var (read.go:68) — byte-identical by construction. Denial short-circuits before any index I/O, so document existence cannot influence the denied caller's response or its timing.
- **No traversal/injection via id or source:** `source` is only ever a registry lookup key (read.go:144) — never interpolated into a path; the resolved `spec.Index` passes the `^[a-z0-9][a-z0-9_.-]{0,254}$` + no-`..` barricade (knowledge.go:59-72, TestGetDocumentRejectsBadIndex). `id` is `url.PathEscape`d into one path segment (knowledge.go:166, TestGetDocumentEscapesID). Live probe: OpenSearch parses a raw `/_doc/..` as a literal doc id inside the `{index}/_doc/{id}` route (404 index/doc semantics, no dot-segment normalization) — no climb-out is reachable.
- **Fail-closed:** any non-nil `AuthorizeRead` → `errReadNotFound`, never a fetch (read.go:153-154); `KnowledgeAuth` is a value type whose zero value denies invalid identities (knowledgeauth.go:45-58) — no nil-interface panic path; a registry error → Internal without fetching; unconfigured platform → memory-only vocabulary (tested).

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| aposd-designing-deep-modules | Interface depth / info hiding | PASS | `queryOpts` options struct replaced positional params with a zero-value-safe contract (golden-pinned); fragment sizing + fallback hidden behind `spec.FragmentSizing()`; highlight-and-suppress is one knob (opensearch.go:735-744) so callers can't get fragments+body or neither; Read stays one entry point dispatching four sources |
| aposd-designing-deep-modules | Information leakage | PASS | Redeclared `knowledgeIndexNameRE`/`postSearch`/`isKnowledgeIndexNotFound` are documented deliberate copies to avoid cross-package exports for one caller — not shared-knowledge drift; no violation demonstrable |
| cc-defensive-programming | External input validated at entry | PASS | MCP edge validates arg shape (tools.go:352-365); server re-validates id, resolves source against registry, authorizes; retrieval layer re-validates `spec.Index` even from the internal caller ("internal team API is still external", knowledge.go:60-66) — defense-in-depth on a security path |
| cc-defensive-programming | No empty catch / swallowed errors | PASS | The two `_ = json.Unmarshal` sites (knowledge.go:181, 459) are followed by nil-shape checks that convert a bad body into a loud error (TestGetDocumentMissingSourceErrors); `json.Marshal` of scalar-map query bodies cannot fail (pre-existing house pattern) |
| cc-defensive-programming | Fail-closed barricade | PASS | Every authz/infra error on the read path denies or errors — none falls through to a fetch (traced above; tested) |

## Notes (non-blocking)
- internal/mcp/tools.go:313-315 — comment states knowledge_search docs "have no memory_read drill-down"; this phase added exactly that drill-down (and the tool descriptions at tools.go:95/157 correctly advertise it). Stale rationale, behavior unaffected.
- api/proto/engram.proto:537, 540-541 — "once fragment extraction lands" / "empty until server-side highlight extraction is wired": extraction landed this phase; comments are one phase behind.
- internal/retrieval/knowledge.go:185-188 — a 200 whose path was mangled to a non-_doc route (e.g. doc id `..`, which OpenSearch treats as a literal id) would surface as Internal rather than NotFound to an *already-authorized* caller; not an existence oracle (collection-level authz already passed), could not demonstrate any leak.
- fragments_test.go:181-184 — `getDocSpec` is a pass-through wrapper around `arxivSpec`; trivially shallow, harmless.

## Issues
None.

**Verdict: PASS**

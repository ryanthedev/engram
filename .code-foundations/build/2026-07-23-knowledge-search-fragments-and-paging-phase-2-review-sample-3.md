# Review: Phase 2 - knowledge_search fragments + memory_read drill-down (sample 3)

## Executed Results (Step 0)
- Test suite: `go test ./...` → all packages ok, 0 failures (exit 0)
- Typecheck/build: `go build ./... && go vet ./... && go vet -tags=integration ./...` → clean (exit 0)
- Lint: `make lint` (go vet + revive) → clean (exit 0)
- Integration: `ENGRAM_OPENSEARCH_URL=http://localhost:9200 go test -tags=integration -count=1 ./internal/server/ ./internal/retrieval/ ./internal/mcp/...` → ok server 1.311s, retrieval 2.020s, mcp 0.021s; verbose re-run: **0 `--- FAIL` lines** (live OpenSearch 3.1.0)

## Requirement Fulfillment

### DW-2.1
PREMISE:  "A `knowledge_search` hit returns extracted fragments and NO `body`/text field in `fields_json` by default; the fragment payload is materially smaller than the full body (many more hits fit a fixed byte budget)."
EVIDENCE: internal/retrieval/opensearch.go:743-754 (numberOfFragments>0 gates BOTH the highlight clause and appending textField to `_source.excludes` — one knob, so fragments and body-suppression can never diverge); internal/retrieval/knowledge.go:132-135 (default path sets sizing/tags from spec; fullBody skips); internal/server/knowledge.go:188-195 (Hit.Fragments → KnowledgeHit.fragments, fields→fields_json); internal/mcp/mcp.go KnowledgeHit DTO.
TRACE:    16 docs with ~5KB bodies ingested → `KnowledgeSearch(col, "gadget", …, fullBody=false)` through real gRPC+OpenSearch → every hit: `fields["text"]` absent, 1–3 fragments present; 12 hits serialize to ≤16384 bytes where one full body alone is ~5KB (internal/server/knowledge_fragments_integration_test.go:73-104).
VERDICT:  PASS — TestDW_2_FragmentsAndDrillDownEndToEnd (integration, live cluster), TestKnowledgeSearchDW_2_1_DefaultRequestsFragments, TestBuildQueryDW_2_2_HighlightDefaultsEmptyTags (excludes assertion), TestKnowledgeSearchToolEmitsCollectionAndFragments — all executed, all pass.

### DW-2.2
PREMISE:  "Fragments carry no markers when the collection's `highlight_pre_tag`/`highlight_post_tag` are empty; setting them produces fragments wrapped in exactly those strings."
EVIDENCE: internal/retrieval/opensearch.go:749-750 (`pre_tags`/`post_tags` emitted as-is — `[""]` when unset, OpenSearch's markers-off escape from its `<em>` default; never hardcoded); internal/knowledge/knowledge.go:67-68 (no fallback for tags — empty is a valid value); internal/store/registry.go metaDocFrom/spec round-trips the four fields.
TRACE:    Empty tags: search for a term appearing 60+ times → every fragment is a verbatim substring of the stored body (`strings.Contains(body, frag)` — any injected marker would fail this). Then `UpdateCollection` with tags «/» → re-search → fragments contain exactly `«frobnicate_widget»` (knowledge_fragments_integration_test.go:106-140, against real OpenSearch highlighting).
VERDICT:  PASS — TestDW_2_FragmentsAndDrillDownEndToEnd sections DW-2.2/DW-2.2-tags (executed live), TestBuildQueryDW_2_2_HighlightDefaultsEmptyTags, TestBuildQueryDW_2_2_CustomTags, TestKnowledgeSearchPerCollectionSizingAndTags.

### DW-2.3
PREMISE:  "`knowledge_search` with `full_body: true` returns the whole body inline (pre-fragment behavior preserved)."
EVIDENCE: internal/retrieval/knowledge.go:132-135 (fullBody=true leaves all four highlight opts zero → buildQuery emits no highlight clause and embedding-only excludes); api/proto/engram.proto:524 `full_body = 7`; internal/mcp/tools.go:420,432 (arg threaded); internal/server/knowledge.go:175.
TRACE:    `full_body:true` search → request body compared byte-for-byte against the pre-fragment `buildQuery` output for the same inputs — equal (fragments_test.go:156-177); e2e: `fullFields["text"] == body` (whole ~5KB body inline) and zero fragments (integration test:165-179). MCP edge: `full_body:true` reaches Backend; omitted defaults false (knowledge_fragments_test.go:29-42).
VERDICT:  PASS — TestKnowledgeSearchDW_2_3_FullBodySkipsHighlight (byte-identity), TestDW_2_FragmentsAndDrillDownEndToEnd DW-2.3 section, TestKnowledgeSearchToolForwardsFullBody, TestKnowledgeSearchHandlerMapsFragmentsAndFullBody — all executed, all pass.

### DW-2.4
PREMISE:  "`memory_read(id, source)` where source is a readable registered collection returns the full document; an unreadable collection fails closed (opaque not-found) and an unknown source returns a self-correcting message — authz enforced in `internal/server/read.go`."
EVIDENCE: internal/server/read.go:52-63 (dispatch: non-tier source → readKnowledge), 144-151 (resolve; unknown → InvalidArgument naming episodic/semantic/knowledge_collections), 153-155 (AuthorizeRead BEFORE fetch; any non-nil → errReadNotFound), 156-162 (fetch after authz; miss → same errReadNotFound), 163-167 (full doc as fields_json). internal/mcp/tools.go:360-370 (edge forwards collection sources; graph still short-circuits).
TRACE:    Readable: `Read("doc-03", col)` through real gRPC → full stored doc (`fields["text"] == whole body`, title intact). Unreadable: role-gated collection, role-less caller, EXISTING id → NotFound whose `Error()` string is asserted equal to a genuine missing-id NotFound (integration test:228-234); unit leg additionally proves the fake reader was never called on denial (read_knowledge_test.go:76-78). Unknown source: `Read(id, "no-such-collection")` → InvalidArgument mentioning `"nonesuch"`, episodic, semantic, knowledge_collections.
VERDICT:  PASS — TestDW_2_4_DrillDownFailsClosedEndToEnd, TestDW_2_FragmentsAndDrillDownEndToEnd DW-2.4 section, TestReadKnowledge_DW_2_4_{ReturnsFullDocument,UnreadableFailsClosedOpaque,UnknownSourceSelfCorrecting}, TestMemoryReadForwardsCollectionSource — all executed, all pass.

### DW-2.5
PREMISE:  "memory-tier `memory_search`/`memory_read` behavior is unchanged (regression)."
EVIDENCE: internal/retrieval/opensearch.go:667-684 (highlight opts zero-value-off; memory callers never set them), 743 (gate), parseHits:779 + parseFragments:788-800 (no highlight key → nil Fragments); internal/server/read.go readEpisodic/readSemantic untouched by the knowledge branch; internal/mcp/mcp.go Hit unchanged (no fragments field — separate KnowledgeHit DTO).
TRACE:    Zero-value opts → buildQuery output pinned byte-for-byte by the golden matrix across all 6 mode/filter combinations (TestDW_1_3_BuildQueryGoldenMatrix) and TestBuildQueryMemoryPathByteIdenticalWhenSortNil (6 subtests); memory-shaped response → `Fragments == nil` (TestParseHitsDW_2_5_NoHighlightKeyInert); memory_read episodic full text / structured JSON / graph short-circuit / wrong-source opacity all re-executed green (TestToolsCall_DW_2_1/2_5, TestMemoryReadGraphStillShortCircuits, TestToolsCallMemoryReadWrongSourceIsOpaque, TestReadMemoryTiersUntouchedByKnowledgeWiring, TestDW_6_5_MemoryToolsUnchanged).
VERDICT:  PASS — all executed in Step 0, all pass.

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have corresponding tests (ran in Step 0); test names reference DW-IDs (TestDW_2_*, Test*DW_2_1..2_5)
- [x] Coverage matches the stated 100% level: every DW item has BOTH a hermetic unit leg and a live-cluster integration leg (DW-2.5 additionally pinned by the byte-for-byte golden matrix)
No gaps.

## Edge Cases (prompt-listed — all handled)
| Edge case | Evidence | Result |
|---|---|---|
| Empty-query (filter-only) → scalars only, no fragments, no body, not an error | opensearch.go:702-706 (match_all fallback); integration test:143-163 asserts no `text` key, zero fragments, scalars intact; TestKnowledgeSearchEmptyQueryIsFilterOnly | PASS |
| Match inside fenced code block extracts with empty tags, no marker corruption | integration test:106-122: fragment containing `frobnicate_widget(cfg)` is a verbatim substring of the stored body (real OpenSearch) | PASS |
| memory_read on unreadable collection → opaque not-found, fail-closed | read.go:153-155 authz-before-fetch; TestDW_2_4_DrillDownFailsClosedEndToEnd asserts `denyErr.Error() == missErr.Error()` | PASS |
| Unknown source → self-correcting error | read.go:146-148; TestReadKnowledge_DW_2_4_UnknownSourceSelfCorrecting + integration test:190-195 | PASS |

## SECURITY (maximum scrutiny)
| Property | Verdict | Evidence |
|---|---|---|
| Authorize-before-fetch | PASS | read.go:153 (AuthorizeRead) precedes :156 (GetDocument) in code; unit test proves the reader is never invoked on denial (fake's docID stays empty, read_knowledge_test.go:76-78); denial and genuine miss both return the single shared `errReadNotFound` var (read.go:68) — same gRPC code AND message, asserted byte-equal end-to-end through the real auth interceptor (integration test:228-234). Timing: a denied caller takes the identical no-fetch path for existing and non-existent ids, so no per-document timing oracle exists. |
| No traversal/injection via id or source | PASS | `source` is only ever a key into the registry's in-memory spec cache (store/registry.go:285-295 — no path interpolation of caller input); the resolved `spec.Index` is regex-barricaded before every REST-path embed (knowledge.go:59-72, re-checked in GetDocument:160); `id` is `url.PathEscape`d into one path segment (knowledge.go:166), empty id short-circuits as a miss (:163-164). Executed: TestGetDocumentEscapesID (`docs/read me.md#s1` stays one segment), TestGetDocumentRejectsBadIndex (`../episodic-events` rejected), TestValidateKnowledgeIndexRejectsPathTraversal. |
| Fail-closed authz | PASS | `AuthorizeRead(...) != nil` → opaque not-found (read.go:153-155) — ANY authorizer outcome other than explicit allow denies; the authorizer itself is default-deny (knowledgeauth.go:45-58: invalid identity → forbidden, non-public with no matching role → forbidden, empty role list grants nobody). Authorizer is a zero-value-ready value type — no nil-receiver bypass. Registry resolution errors go to Internal, never fall through to a fetch (read.go:150). |

## Dead Code
None found. (`readSources` map correctly deleted with the edge relaxation; no unreachable code, debug statements, or commented-out blocks in the diff; build+revive clean.)

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | Probed the new generic packer for aliasing: packSearchResult copies into a fresh slice before shrinking (budget.go); toolSchemas returns a fresh map per call; no new shared mutable state introduced. No defect demonstrable. |
| Error Handling | PASS | Probed GetDocument with 500 (must not read as absence → errors, TestGetDocumentUnexpectedStatusErrors), 200-without-_source (loud error, TestGetDocumentMissingSourceErrors), 404 (absence, TestGetDocumentMissesReadAsAbsent); reader infrastructure error surfaces as Internal, never disguised as opaque not-found (TestReadKnowledge_ReaderErrorIsInternal). |
| Resources | PASS | Both new HTTP paths `defer resp.Body.Close()` (knowledge.go:174, postSearch:452); bodies fully read before decode. |
| Boundaries | PASS | Probed: empty id (miss, not malformed request), empty query (match_all), k≤0 (clampK / server-chosen), fragments empty-vs-nil (omitempty on both DTOs — memory hits never grow an empty fragments key, TestParseHitsDW_2_5_NoHighlightKeyInert), non-string highlight entries skipped in parseFragments. |
| Security | PASS | See SECURITY table — all three properties traced and executed. |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| aposd-designing-deep-modules | Information hiding — fragment/suppression coupling not leaked to callers | PASS | One knob (numberOfFragments>0) drives both the highlight clause and body exclusion inside buildQuery (opensearch.go:735-753); no caller can produce fragments+body or suppressed-body-without-fragments. |
| aposd-designing-deep-modules | No shallow/leaky DTO reuse | PASS | KnowledgeHit is a separate DTO (wire + MCP), so memory hits never carry a permanently-empty fragments field; `packable` interface hides which DTO the budget packer handles behind one method. |
| aposd-designing-deep-modules | Specialization pushed to the right layer | PASS | FragmentSizing() centralizes the 240/3 fallback in knowledge.CollectionSpec; the read-authz vocabulary decision lives solely in server/read.go (MCP edge merely forwards) — single authority, no duplicated policy. |
| aposd-designing-deep-modules | Information leakage across modules | PASS (see Note 1) | knowledgeIndexNameRE duplicates store.indexNameRE's grammar — deliberate, documented, and a degree/taste call, not a demonstrable defect. |
| cc-defensive-programming | External input validated at entry | PASS | id: required non-empty (read.go:49), PathEscaped (knowledge.go:166); source: registry lookup only, never interpolated; spec.Index: regex barricade on every path embed even though registry-assigned ("internal team API is still external" honored, knowledge.go:62-66). |
| cc-defensive-programming | Fail-closed on the authz path | PASS | Any AuthorizeRead non-nil → deny (read.go:153-155); authorizer default-deny incl. empty-role misconfiguration (knowledgeauth.go:45-58). |
| cc-defensive-programming | No silently swallowed failures | PASS (see Note 2) | Ignored `json.Unmarshal` in GetDocument/postSearch is guarded: an undecodable 200 body yields a loud "no _source" error, never a silent empty doc (TestGetDocumentMissingSourceErrors executed). |
| cc-defensive-programming | Barricade design (authorize→fetch→project ordering) | PASS | read.go:138-167 RESOLVE→AUTHORIZE→FETCH→PROJECT, proven by the never-fetched fake on denial and byte-equal e2e denial/miss. |

## Notes (non-blocking)
1. `knowledgeIndexNameRE` (retrieval/knowledge.go:59) restates store's unexported index-name grammar. Documented trade against a one-caller cross-package export; if the grammar ever changes it must change in two places.
2. `readResultFromProto` (engramclient/client.go) leaves `Fields` nil on a fields_json decode failure instead of surfacing an error. The server always marshals valid JSON so this is only reachable on wire corruption, but the caller would see an empty read with no explanation. Robustness-posture choice; not demonstrated as a defect.
3. A collection cannot explicitly configure `number_of_fragments: 0` to opt out of fragments per-collection — 0 means "unset → default 3" via FragmentSizing; `full_body:true` is the opt-out. No requirement asks for a per-collection opt-out; recording as design observation only.
4. `parseFragments` ranges over the highlight map (nondeterministic key order), but buildQuery requests exactly one highlighted field, so within-field fragment order is preserved. The invariant is documented at the function.

## Issues
None.

**Verdict: PASS**

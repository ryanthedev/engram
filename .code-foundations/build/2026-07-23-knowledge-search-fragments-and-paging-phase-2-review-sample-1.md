# Review: Phase 2 - knowledge_search fragments + memory_read drill-down (security sample 1)

## Executed Results (Step 0)
- Test suite (unit): `go test ./...` → all packages ok; forced fresh run `go test -count=1 ./internal/server/ ./internal/retrieval/ ./internal/mcp/... ./internal/engramclient/ ./internal/store/ ./internal/cli/` → all ok
- Integration: `ENGRAM_OPENSEARCH_URL=http://localhost:9200 go test -tags=integration -count=1 ./internal/server/ ./internal/retrieval/ ./internal/mcp/...` → all ok (server 1.348s, retrieval 1.842s, mcp 0.021s); targeted verbose rerun: `TestDW_2_FragmentsAndDrillDownEndToEnd` PASS (0.16s), `TestDW_2_4_DrillDownFailsClosedEndToEnd` PASS (0.09s)
- Typecheck/Build: `go build ./... && go vet ./... && go vet -tags=integration ./...` → OK
- Lint: `make lint` (vet + revive) → clean, exit 0

## Requirement Fulfillment

### DW-2.1
PREMISE:  "A `knowledge_search` hit returns extracted fragments and NO `body`/text field in `fields_json` by default; the fragment payload is materially smaller than the full body (many more hits fit a fixed byte budget)."
EVIDENCE: internal/retrieval/opensearch.go:743-754 (numberOfFragments>0 → text field appended to `_source.excludes` AND highlight clause emitted, one knob); internal/retrieval/knowledge.go:128-136 (default path sets fragmentSize/numberOfFragments from spec.FragmentSizing()); internal/server/knowledge.go:188-195 (Fragments mapped to KnowledgeHit); internal/mcp/mcp.go:42-59 (KnowledgeHit DTO with fragments, body-suppressed fields_json)
TRACE:    query "gadget" over 16 docs of ~5KB body each → buildQuery emits `"highlight":{...,"fragment_size":240,"number_of_fragments":3}` + `"excludes":[...,"text"]` → every hit's fields_json lacks "text", carries 1-3 fragments → 12 hits serialize to ≤16384 bytes (12 full bodies would be ~60KB+)
VERDICT:  PASS — TestKnowledgeSearchDW_2_1_DefaultRequestsFragments (unit, request bytes), TestDW_2_FragmentsAndDrillDownEndToEnd DW-2.1 section (integration: no `text` key in fields_json, fragments present, 12-hit page ≤16KB budget assertion), TestKnowledgeSearchToolEmitsCollectionAndFragments (MCP wire shape). All ran and passed.

### DW-2.2
PREMISE:  "Fragments carry no markers when the collection's `highlight_pre_tag`/`highlight_post_tag` are empty; setting them produces fragments wrapped in exactly those strings."
EVIDENCE: internal/retrieval/opensearch.go:749-750 (`pre_tags`/`post_tags` emitted as-is — `[""]` when unset, OpenSearch's markers-off escape); internal/retrieval/knowledge.go:134 (tags threaded from spec); internal/store/registry.go:124 + templates/knowledge-collections.json (tags persisted)
TRACE:    empty tags + match on `frobnicate_widget(cfg)` inside a fenced code block → every fragment is a verbatim substring of the stored body (no `<em>`, no corruption); UpdateCollection sets `«`/`»` → same search returns a fragment containing `«frobnicate_widget»`
VERDICT:  PASS — TestBuildQueryDW_2_2_HighlightDefaultsEmptyTags, TestBuildQueryDW_2_2_CustomTags, TestKnowledgeSearchPerCollectionSizingAndTags (unit); TestDW_2_FragmentsAndDrillDownEndToEnd DW-2.2 sections (integration, real OpenSearch highlighting, verbatim-substring check + exact-wrap check). All ran and passed.

### DW-2.3
PREMISE:  "`knowledge_search` with `full_body: true` returns the whole body inline (pre-fragment behavior preserved)."
EVIDENCE: internal/retrieval/knowledge.go:132-135 (fullBody=true skips all highlight opts → zero-value → buildQuery emits neither highlight nor text-field exclude); internal/mcp/tools.go:420,432 (full_body threaded); internal/engramclient/knowledge.go:62; internal/server/knowledge.go:175
TRACE:    fullBody=true → request bytes equal `buildQuery(zero-value highlight opts)` byte-for-byte (asserted in test) → hit's fields_json carries `text` == the full stored body, fragments empty
VERDICT:  PASS — TestKnowledgeSearchDW_2_3_FullBodySkipsHighlight (unit, byte-for-byte equality with the pre-fragment query), TestDW_2_FragmentsAndDrillDownEndToEnd DW-2.3 section (integration: whole body inline, no fragments), TestKnowledgeSearchToolForwardsFullBody + TestKnowledgeSearchHandlerMapsFragmentsAndFullBody (threading at both edges). All ran and passed.

### DW-2.4
PREMISE:  "`memory_read(id, source)` where source is a readable registered collection returns the full document; an unreadable collection fails closed (opaque not-found) and an unknown source returns a self-correcting message — authz enforced in `internal/server/read.go`."
EVIDENCE: internal/server/read.go:138-168 (readKnowledge: RESOLVE line 144 → AUTHORIZE line 153 → FETCH line 156); internal/server/read.go:61-63 (default dispatch); internal/retrieval/knowledge.go:159-197 (GetDocument); internal/mcp/tools.go:352-376 (tool edge forwards non-graph sources)
TRACE:    Read("doc-03", col) as public reader → registry resolves spec → AuthorizeRead nil → GetDocument returns full _source → fields_json carries whole doc incl. body. Read("secret-1", role-gated col) as role-less caller → AuthorizeRead ≠ nil → errReadNotFound returned at line 154, GetDocument never called. Read(id, "nonesuch") → registry miss → InvalidArgument naming episodic/semantic/knowledge_collections.
VERDICT:  PASS — TestReadKnowledge_DW_2_4_ReturnsFullDocument, TestReadKnowledge_DW_2_4_UnreadableFailsClosedOpaque, TestReadKnowledge_DW_2_4_UnknownSourceSelfCorrecting (unit); TestDW_2_4_DrillDownFailsClosedEndToEnd + DW-2.4 section of TestDW_2_FragmentsAndDrillDownEndToEnd (integration through the real auth interceptor); TestMemoryReadForwardsCollectionSource (MCP leg). All ran and passed.

### DW-2.5
PREMISE:  "memory-tier `memory_search`/`memory_read` behavior is unchanged (regression)."
EVIDENCE: internal/retrieval/opensearch.go:743 (highlight/suppression gated on numberOfFragments>0 — zero on every memory call); internal/server/read.go diff (readEpisodic/readSemantic untouched; only the `default:` arm changed); internal/mcp/mcp.go:32-39 (memory Hit DTO unchanged, no fragments field); internal/mcp/render.go:64 (renderSearchResult still takes searchResult[Hit])
TRACE:    zero-value highlight opts → buildQuery emits NO highlight key and the unchanged `"excludes":["text_embedding","fact_embedding"]`; memory-shaped response (no highlight key) → parseHits yields nil Fragments; episodic/graph Read dispatch codes unchanged with knowledge wiring present
VERDICT:  PASS — TestBuildQueryDW_2_5_ZeroValueOmitsHighlight, TestParseHitsDW_2_5_NoHighlightKeyInert, TestDW_1_3_BuildQueryGoldenMatrix (pins exact memory-path query bytes), TestBuildQueryMemoryPathByteIdenticalWhenSortNil (6 subtests), TestReadMemoryTiersUntouchedByKnowledgeWiring, TestMemoryReadGraphStillShortCircuits, plus the full pre-existing memory suites (unit + integration) all green. All ran and passed.

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have corresponding tests that ran in Step 0 (test names reference DW IDs: TestKnowledgeSearchDW_2_1_*, TestBuildQueryDW_2_2_*, TestKnowledgeSearchDW_2_3_*, TestReadKnowledge_DW_2_4_*, Test*DW_2_5_*, TestDW_2_FragmentsAndDrillDownEndToEnd, TestDW_2_4_DrillDownFailsClosedEndToEnd)
- [x] Coverage matches the stated 100% level: every DW item is covered at both unit level and live-OpenSearch integration level; every listed edge case has a dedicated assertion
No gaps.

## Dead Code
None found. No debug statements, TODOs, or unreachable code introduced by the diff (grep over the full phase diff). Compiler/vet/revive clean.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | No new shared mutable state; KnowledgeRetriever is stateless over an http.Client; registry cache concurrency pre-existing and untouched |
| Error Handling | PASS | Probed the adversarial branches: GetDocument 5xx → error (TestGetDocumentUnexpectedStatusErrors), 200-without-_source → error (TestGetDocumentMissingSourceErrors), 404 → absent not error (TestGetDocumentMissesReadAsAbsent), reader infra error → Internal never disguised as NotFound (TestReadKnowledge_ReaderErrorIsInternal), registry non-NotFound error → Internal (read.go:150) |
| Resources | PASS | Both new HTTP paths `defer resp.Body.Close()` (knowledge.go:174, postSearch:452); no files/locks/goroutines added |
| Boundaries | PASS | Empty id → InvalidArgument at RPC edge (read.go:49-50) and opaque miss at retriever (TestGetDocumentEmptyIDIsAbsent); empty query → match_all filter-only, scalars-only hits, not an error (integration DW-2.2b); k clamped (clampK); zero-value queryOpts proven inert (golden matrix) |
| Security | PASS | See Loaded-Skill Criteria + Edge cases. Authorize-before-fetch demonstrated: on denial the fake reader's docID stays "" (TestReadKnowledge_DW_2_4_UnreadableFailsClosedOpaque) — no fetch ever issued. Denial and absence both return the single `errReadNotFound` value (read.go:68,154,161) → byte-identical status; asserted string-equal in unit AND through the real gRPC stack (TestDW_2_4_DrillDownFailsClosedEndToEnd). Denied-path timing is constant w.r.t. document existence (no fetch occurs), so no existence oracle on documents. `id` is url.PathEscape'd into a single path segment (knowledge.go:166, TestGetDocumentEscapesID with `docs/read me.md#s1`); `source` never reaches a REST path — Registry.Get is a cached map lookup (store/registry.go:285-295) and the registry-assigned spec.Index is re-validated by validateKnowledgeIndex (`^[a-z0-9][a-z0-9_.-]{0,254}$` + no "..", TestGetDocumentRejectsBadIndex with `../episodic-events`). Fail-closed: ANY non-nil AuthorizeRead result denies (read.go:153); AuthorizeRead itself denies unauthenticated callers even for public collections and treats empty/unknown role policies as deny-everyone (knowledgeauth.go:45-58) |

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | External input validated at entry (id/source are external at the MCP and gRPC edges) | PASS | tools.go:360 rejects empty id/source; read.go:49 rejects empty id; source resolved via map lookup, never interpolated; id path-escaped before REST use (knowledge.go:166) |
| cc-defensive-programming | Barricade ordering: authorize before fetch at the trust boundary | PASS | read.go:153 (authz) precedes :156 (fetch); demonstrated by the no-fetch-on-denial assertion (read_knowledge_test.go:76-78) |
| cc-defensive-programming | Fail-closed: any authz-path error denies, never falls through to a fetch | PASS | read.go:153-155 treats every non-nil AuthorizeRead as errReadNotFound; registry errors return before any fetch (read.go:144-151); invalid identity fails id.Valid() → deny (knowledgeauth.go:46) |
| cc-defensive-programming | No empty catch blocks / no silently swallowed errors | PASS | The `_ = json.Unmarshal` sites (knowledge.go:181, opensearch postSearch:459, engramclient client.go:156) are documented degradations whose failure modes are handled by the following status-code/nil branches — none swallows a failure invisibly |
| cc-defensive-programming | Assertions for bugs only / no executable code in assertions | N/A | No assertions used (Go idiom: explicit error returns throughout) |
| aposd-deep-modules | No false abstraction / information leakage in the new DTO split | PASS | KnowledgeHit is a separate DTO so memory hits never carry a permanently-empty fragments field and collection is a named field, not a repurposed Source (mcp.go:42-48); wire split mirrored in proto |
| aposd-deep-modules | Deep interface: fragment mechanics hidden from callers | PASS | One knob (numberOfFragments>0) couples highlight emission and body suppression inside buildQuery (opensearch.go:735-753) — callers cannot construct fragments+body or suppressed-body-without-fragments states; sizing fallback hidden behind spec.FragmentSizing() |
| aposd-deep-modules | No shallow pass-through layers added | PASS | packable interface (budget.go) is 1 method generalizing the existing packer over both hit types instead of duplicating it; MCP/client layers thread full_body without re-deciding policy |

## Notes (non-blocking)
- Collection-existence oracle by design: an unreadable registered collection returns NotFound while an unregistered source returns InvalidArgument, so callers can learn a collection *name* exists (never its contents or documents). This is exactly the split DW-2.4 mandates (self-correcting unknown-source message), and read.go:127-131 documents the trade as per-plan. Flagged for awareness only.
- `KnowledgeRetriever.Search` is at 7 parameters (ctx, spec, query, filters, sortKeys, k, fullBody) — at the routine-design threshold; a future options struct would absorb the next flag without churn.
- fragments_test.go:181-184 `getDocSpec` is a trivial wrapper around `arxivSpec` (test code only).
- Under default suppression, a filter-only hit carries neither fragments nor body, so the body is reachable only via memory_read — intended per the edge-case spec, but worth remembering in tool docs (tools.go:157 already states it).

## Issues (if FAIL)
None.

**Verdict: PASS**

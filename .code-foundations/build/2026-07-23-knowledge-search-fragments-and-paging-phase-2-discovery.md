# Discovery + Design: Phase 2 - Fragment extraction & drill-down

## Files Found
- `internal/retrieval/opensearch.go` — `queryOpts` already carries the Phase-1 seams (`offset`, `fragmentSize`, `numberOfFragments`, `highlightPreTag`, `highlightPostTag`), all inert; `parseHits` reads `_id/_score/_source` only; `retrieval.Hit` has no fragments field yet.
- `internal/retrieval/knowledge.go` — `KnowledgeRetriever.Search(ctx, spec, query, filters, sortKeys, k)` exists; no by-id fetch path; `postSearch` helper (POST-only); `validateKnowledgeIndex` barricade.
- `internal/server/knowledge.go` — `KnowledgeReader` seam (Search+Collections); handler maps hits → `engrampb.KnowledgeHit` with empty fragments; `req.offset/full_body` accepted but inert (Phase-1 comment says so).
- `internal/server/read.go` — Read RPC dispatches on `episodic|semantic|graph`, default = InvalidArgument; `errReadNotFound` opaque denial; fetch→authorize→project ordering documented.
- `internal/mcp/tools.go` — `readSources` map at line 38 gates memory_read; `callKnowledgeSearch` has no `full_body`; `packAndSpill` is `[]Hit`-typed.
- `internal/mcp/mcp.go` — Backend interface; **`mcp.KnowledgeHit` does NOT exist** (only `engrampb.KnowledgeHit` on the wire); `KnowledgeSearch` returns `[]mcp.Hit`.
- `internal/engramclient/knowledge.go` — `KnowledgeSearch` returns `[]mcp.Hit` (collection rides `Hit.Source`); `client.go` `Read` adapter translates episodic/semantic only.
- `internal/knowledge/knowledge.go` — `FragmentSizing()` (240/3 fallback) live per Phase 1.
- `docs/code-standards.md` — present.

## Current State
Phase 1 landed the proto (`KnowledgeHit`, `offset/full_body/total`, `CollectionSpec` sizing/tags) and the `queryOpts` refactor with golden-matrix pins. Nothing behavioral changed yet: search still returns whole bodies, `memory_read` rejects collection sources, highlight fields are declared but unread.

## Gaps
1. **`mcp.KnowledgeHit` doesn't exist.** The dispatch prompt lists it as a live seam, but only the *proto* message is live. The Go DTO must be created here (in `internal/mcp/mcp.go`, which IS in file scope) — the Phase-2 Produces contract (`[]mcp.KnowledgeHit`) requires it.
2. **`ReadResponse` has no carrier for a knowledge document** (fields 1–5: source/episodic/fact/provenance/versions). DW-2.4 ("`memory_read(id, source)` … returns the full document") is unimplementable without a wire field. Forced deviation: add `string fields_json = 6;` to `ReadResponse` (additive; plan Context says "Proto is free to change; no external API-compatibility constraint"). `api/proto` is outside the phase's file list, so this is documented loudly as a deviation, with `make proto` drift-check run.
3. **The byte-budget packer is `[]Hit`-typed** (`budget.go`, `spill.go`, `render.go`). The pinned tool shape `{id, score, collection, fields_json, fragments[]}` cannot ride `mcp.Hit` (key is `source`, no fragments). Minimal mechanical generalization: `searchResult[H]`, `packSearchResult[H]`, `spillFullResult[H]` — same package, JSON output for the memory instantiation byte-identical (existing budget tests are the regression pin).
4. **`internal/cli/vaultknowledge.go` calls `engramclient.KnowledgeSearch`** and breaks when the signature changes. Minimal compile fix here (pass `fullBody=true` — exactly what Phase 3 pins for the export — and adapt `decodeKnowledgeHit` to `mcp.KnowledgeHit`); the real paging rework stays in Phase 3.

## Code Standards
- `internal/engramclient` is the sole gRPC transport boundary; status classification only via engramclient predicates. (No new classification needed this phase — tool errors surface server messages verbatim.)
- Sentinel errors unwrapped for `errors.Is`; stdlib `testing`; in-process gRPC-stub test convention (`internal/engramclient/knowledge_test.go` precedent); httptest fake-cluster convention in `internal/retrieval/knowledge_test.go`.
- Barricade comments: validation at the edge, inner seams assume validated input; fail-closed reads with one opaque NOT_FOUND.

## Test Infrastructure
- Unit: `go test ./...`; retrieval knowledge tests use `newFakeKnowledgeServer` (httptest) + `fakeKnowledgeRegistry`; server read tests use fake `EpisodicReader`; mcp tests use a fake Backend.
- Integration: `make integration` (`-tags=integration`, live dev cluster :9200 — UP, verified). e2e cluster :9201 with the harvested `knowledge-docs-v1` — UP, reserved for the manual DW-2.1 budget-fit spot check only.
- Golden pins to preserve: `buildquery_golden_test.go` matrix (memory path byte-identical), `TestBuildQueryMemoryPathByteIdenticalWhenSortNil`, `internal/server/read_test.go` DW-2.x memory read tests, mcp budget tests.

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-2.1 | Hit returns fragments, NO body in fields_json by default; live budget-fit check | COVERED | `TestKnowledgeSearchDW21FragmentsReplaceBody` (integration, ephemeral index: fragments present, body key absent, a 5,000-word doc yields ≤ number_of_fragments fragments); unit `TestBuildQueryHighlightSuppressesTextField`; manual spot check on :9201 `knowledge-docs-v1` recorded in output |
| DW-2.2 | No markers when tags empty; set tags wrap fragments exactly | COVERED | unit `TestBuildQueryHighlightTagsDefaultEmpty` / `TestBuildQueryHighlightCustomTags`; integration `TestKnowledgeSearchDW22MarkersOffByDefault` (incl. fenced-code-block doc) and `TestKnowledgeSearchDW22CustomTagsWrap` |
| DW-2.3 | `full_body: true` returns whole body inline | COVERED | integration `TestKnowledgeSearchDW23FullBodyInline`; unit `TestKnowledgeSearchFullBodySkipsHighlight` (query bytes identical to pre-phase body) |
| DW-2.4 | `memory_read(id, collection)` returns full doc; unreadable → opaque not-found; unknown source → self-correcting; authz in server/read.go | COVERED | server unit `TestServerReadKnowledge_{ReturnsDoc,UnreadableFailsClosed,UnknownSourceSelfCorrecting,MissingDocOpaque,Unconfigured}`; retrieval unit `TestGetDocument*`; integration `TestMemoryReadKnowledgeDrillDown` |
| DW-2.5 | memory-tier search/read unchanged | COVERED | existing pinned suites re-run green (`buildquery_golden_test.go`, `read_test.go` DW-2.x, mcp budget/render tests); new unit `TestParseHitsNoHighlightKeyInert` |

**All items COVERED:** YES

## Design Decisions

### Design: fragment threading through the packer (aposd design-it-twice)

Approaches considered:
1. **A — Generalize the packer with generics**: `searchResult[H]`, `packSearchResult[H hitPackable]` where `hitPackable` exposes `fieldsJSON() string`; `mcp.KnowledgeHit` is a first-class DTO with the pinned JSON keys (`collection`, `fragments`).
2. **B — Convert `KnowledgeHit`→`Hit` at the tool layer** (add `Fragments`/reuse `Source` for collection): zero packer churn, but the tool JSON emits `source` not `collection` and grows fragment fields on every memory hit type — violates the pinned Produces shape and re-creates the "permanently-empty memory field" wart the plan rejected at the proto level.
3. **C — Separate knowledge envelope + duplicated mini-packer in tools.go**: keeps budget.go untouched but duplicates the shrink-remeasure-spill loop (~60 lines) that took two DW cycles to get right — divergence risk on every future budget fix.

| Criterion | A | B | C |
|-----------|---|---|---|
| Interface simplicity | one generic envelope | one envelope, wrong keys | two envelopes |
| Information hiding | packer stays sole budget authority | ok | budget logic leaks into tools.go |
| Caller ease of use | `packAndSpill(hits, …)` unchanged shape | unchanged | new parallel API |
| Contract fidelity (pinned `{collection, fragments}`) | exact | violated | exact |

**Choice: A.** Sacrifice: mechanical churn in `budget.go`/`spill.go`/`render.go` (outside the listed file scope, same package); memory instantiation is `searchResult[Hit]` with identical JSON — existing tests pin it.

Depth check: interface methods unchanged (pack/spill/render); hidden details: budget measurement, spill headroom, facet computation; common case (memory) complexity: unchanged.

### Design: knowledge drill-down wire carrier
- `ReadResponse` gains `string fields_json = 6` — the full stored `_source` as JSON, set only when source is a collection (mirrors `KnowledgeHit.fields_json` precedent). Alternatives: cram into `EpisodicRecord` (lossy, wrong shape — rejected), client-side fetch via KnowledgeSearch RPC (moves authz out of `read.go`, violating the plan's resolved uncertainty — rejected).
- Server flow in `read.go` (fail-closed ordering): resolve source via Registry (unknown → self-correcting InvalidArgument, the same existence-naming trade `resolveCollection` already makes) → `KnowledgeAuth.AuthorizeRead` (denied → `errReadNotFound`, opaque) → `KnowledgeReader.GetDocument` (miss → `errReadNotFound`) → project `_source` to `fields_json`. Knowledge platform unconfigured → the pre-phase InvalidArgument message (memory-only vocabulary).
- `KnowledgeRetriever.GetDocument(ctx, spec, id)` uses realtime `GET /{index}/_doc/{url.PathEscape(id)}` (mirrors `store.GetEpisodic`; ids are harvester-chosen external strings, hence PathEscape); 404/`index_not_found` → ok=false.

### Design: highlight clause & suppression coupling
- `buildQuery` gates BOTH the highlight clause and the `_source` exclude of `textField` on `opts.numberOfFragments > 0`: fragments-replace-body is one concept, one knob. Memory callers (zero value) produce byte-identical bodies — golden matrix untouched.
- `pre_tags`/`post_tags` always emitted as `[opts.highlightPreTag]`/`[opts.highlightPostTag]` — `[""]` when unset (markers off, OpenSearch's escape from its `<em>` default), never a hardcoded marker.
- `KnowledgeRetriever.Search` gains `fullBody bool`: false → sizing from `spec.FragmentSizing()` + tags from spec; true → zero sizing (no highlight, body inline — bytes identical to today's query, which IS the DW-2.3 preservation proof).
- `parseHits` reads `hm["highlight"]` and collects fragments from its (single-key) field map — no signature change, structurally inert for memory (key absent).
- `retrieval.Hit` gains `Fragments []string` (nil on memory path; memory server handler never reads it).

### Contracts produced (Phase 3 consumes)
- `engramclient.KnowledgeSearch(ctx, collection, query string, filters []mcp.Predicate, sort []mcp.SortKey, k int, fullBody bool) ([]mcp.KnowledgeHit, error)`
- `mcp.KnowledgeHit{ID, Score, Collection, Fields (json:"fields_json"), Fragments}`
- `KnowledgeRetriever.Search(ctx, spec, query, filters, sortKeys, k, fullBody)` returning `[]Hit` with `Fragments`.
- `mcp.Backend.KnowledgeSearch(ctx, collection, query, filters, sort, k, fullBody) ([]KnowledgeHit, error)`

### Defensive placement (cc-defensive-programming)
- External inputs: `full_body` (bool, no validation surface), `source` (relaxed at MCP edge, validated server-side — the barricade WIDENS but stays server-side, per plan), doc id (embedded in URL path → PathEscape + non-empty check already at Read barricade), fragments (untrusted harvester text — passed through unsanitized BY DESIGN, matching existing search behavior; `vaultknowledge` sanitizes downstream).
- Fail-closed: unreadable collection and missing doc share `errReadNotFound`; `AuthorizeRead` error (not just denial) also denies.
- No empty catches; no assertions on runtime input.

## Prerequisites
- [x] Phase 1 seams live (`queryOpts`, `engrampb.KnowledgeHit`, `FragmentSizing`, CollectionSpec fields)
- [x] Dev cluster :9200 up (integration), e2e :9201 up (manual spot check)
- [x] protoc toolchain (`make proto`) — required by the ReadResponse deviation

## Recommendation
**BUILD** — with documented deviations from the phase file list, all forced by pinned contracts:
1. `api/proto/engram.proto` + regenerated pb: additive `ReadResponse.fields_json` (DW-2.4 has no wire carrier otherwise).
2. Mechanical same-package generalization of `internal/mcp/{budget,spill,render}.go` and a compile-fix in `internal/cli/vaultknowledge.go` (signature change lands here; Phase 3 owns the real rework).

## Deviations surfaced during build (added post-discovery)
3. **`internal/store/registry.go` + `internal/store/templates/knowledge-collections.json`** — the Phase-2 end-to-end integration test for DW-2.2 (per-collection marker tags) FAILED against the live cluster: the registry's persisted form (`collectionMetaDoc`) silently dropped the four Phase-1 sizing/tag fields, so a collection's fragment configuration could never survive Create/Update → Get. This is the third translation beside the proto and MCP ones — Phase 1's DW-1.2 covered only the proto round-trip. Fixed additively (4 fields, `omitempty`, template properties under `dynamic: strict`); pinned by `TestMetaDocRoundTripsFragmentSizing` and the now-green live tag test. NOTE for ops: an ALREADY-PROVISIONED meta index on a deployed cluster was created from the old strict template and will reject writes carrying the new fields until its mapping is updated (`PUT <meta-index>/_mapping` with the four properties) — scratch/test meta indices are unaffected.
4. **`internal/retrieval/opensearch_integration_test.go`** — pre-existing (verified at clean HEAD) duplicate `hitIDs` declaration collided with `splitexpanded_test.go` under `-tags=integration`, breaking the whole retrieval integration build. Minimal dedupe (removed the tagged copy) so the integration suite can run at all.

## Live DW-2.1 spot check (e2e :9201, knowledge-docs-v1, read-only)
Query "component state" (68 matching docs): fragment mode fits **15 hits** in the 16 KB budget (68,419 B for all 68); full-body mode fits **1 hit** (463,321 B for all 68) — the plan's "~16 where 1 fit before" confirmed on the real harvested corpus.

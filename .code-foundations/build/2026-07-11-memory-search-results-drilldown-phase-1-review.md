# Review: Phase 1 - Breadth results + compact-line rendering

## Executed Results (Step 0)
- Test suite: `go test ./internal/mcp/... -v` → 69/69 PASS (all `TestDW_1_*` present and passing; full log captured)
- Full suite: `go test ./...` → all packages `ok` (e2e is `-tags=e2e` gated, correctly not run)
- Build: `go build ./...` → clean, exit 0
- Typecheck (`go vet ./...`) → clean, exit 0
- Lint (`make lint`) → clean, exit 0

## Requirement Fulfillment

### DW-1.1
PREMISE:  a multi-hit `memory_search` returns compact-line text, not escaped `fields_json` JSON.
EVIDENCE: internal/mcp/tools.go:151-157 (`rendered := renderSearchResult(result); return toolResultWithText(rendered, compactLines(rendered))`); internal/mcp/render.go:210-224 (`compactLines`)
TRACE:    3-hit request (episodic+semantic+graph) → `callSearch` packs then calls `renderSearchResult`/`compactLines` → `content[0].text` is 3 newline-joined tab lines; `TestDW_1_1_MultiHitSearchReturnsCompactLineText` asserts the text fails `json.Unmarshal`, contains no `"fields_json"` substring, and has exactly one line per hit.
VERDICT:  PASS

### DW-1.2
PREMISE:  episodic `text` renders as a normalized single-line lead snippet within the length cap.
EVIDENCE: internal/mcp/render.go:110-120 (`gistFor`), render.go:169-183 (`leadSnippet`)
TRACE:    short text ("the deploy key rotates weekly") → `leadSnippet` len(runes) ≤ 200 → returned unmodified, no ellipsis (verified). Long text (250 `a`s) → capped to exactly 200 runes + `…` (verified via rune-length assertion == 201).
VERDICT:  PASS

### DW-1.3
PREMISE:  truncation is UTF-8 rune-safe — an emoji/multibyte body is never split mid-rune.
EVIDENCE: internal/mcp/render.go:176-183 (`leadSnippet` slices `[]rune`, never raw bytes)
TRACE:    body = `pad` 'a's + "👻🕯️👻🕯️👻🕯️" for pad ∈ [197,203] (straddling the 200-rune boundary from both sides) → `TestDW_1_3_RuneSafeTruncationOnEmojiBody` asserts `utf8.ValidString(gist)` and no `�` replacement char, for all 7 pad values. All passed.
VERDICT:  PASS

### DW-1.4
PREMISE:  every result line exposes `id` and `source` unambiguously (parse round-trips to the same pair).
EVIDENCE: internal/mcp/render.go:198-208 (`formatHitLine`: `parts := []string{h.ID, h.Source, ...}`)
TRACE:    3 hits (episodic/semantic/graph) with distinct IDs → `formatHitLine` → split on `\t`, take first 2 tokens → equals original (id, source) for all 3 (`TestDW_1_4_IDSourceRoundTrip`). Adversarial case: statement/text body = `"fake\tid\tsource\ninjection"` (embedded tab+newline trying to forge extra fields) → content is normalized via `normalizeToSingleLine` before ever reaching the line, so it cannot inject a fake tab-delimited field before id/source; round trip still holds (`TestDW_1_4_IDSourceRoundTripSurvivesAdversarialContent`).
VERDICT:  PASS

### DW-1.5
PREMISE:  semantic/graph hits render full `statement`/s-p-o (content identical to today, format changed).
EVIDENCE: internal/mcp/render.go:110-120 (`gistFor`: non-episodic path returns `normalizeToSingleLine(statement)` untruncated — no length cap applied)
TRACE:    graph hit with 29-char statement → `rh.Gist == statement` exactly (no truncation marker) and `Fields["subject"/"predicate"/"object"]` match source values (`TestDW_1_5_SemanticGraphGistIsFullStatement`, both semantic and graph subtests pass).
VERDICT:  PASS

### DW-1.6
PREMISE:  the gRPC `Hit` still carries full untruncated `text` — a gRPC/`engramclient` consumer sees no truncation.
EVIDENCE: internal/mcp/render.go:56-59 (renderSearchResult never mutates `result`/`result.Hits`; every value copied into new structs), internal/mcp/budget.go:82-84 (`packSearchResult` copies the `Hit` slice, and `Hit.Fields` is an immutable Go string so no in-place mutation is possible along the pack→render path); this phase's diff (`git diff HEAD` scoped to `internal/mcp/{tools.go,budget_test.go}` + new `render.go`/`render_test.go`) touches nothing in `api/engrampb` or `internal/engramclient`.
TRACE:    episodic Hit with a 1500-char body ending in a unique marker → `backend.hits[0].Fields` (the Backend's own slice, i.e. what a gRPC/engramclient-level consumer would see) still contains the marker after the full wire call, byte-identical to the original; the *rendered* gist (structuredContent) is truncated and does not contain the marker (`TestDW_1_6_BackendHitsNotMutated`).
VERDICT:  PASS

### DW-1.7
PREMISE:  empty/short/newline-laden `text` inputs render without panic or dangling ellipsis.
EVIDENCE: internal/mcp/render.go:149-167 (`normalizeToSingleLine`), render.go:176-183 (`leadSnippet`: ellipsis appended only when `len(runes) > maxRunes`)
TRACE:    text="" → normalizeToSingleLine("")="" , runes len 0 ≤ 200 → returned as "" (no ellipsis). text="short note" → returned unmodified. text="line one\nline two\tindented\r\nline three" → control chars replaced with single spaces, collapsed → one line, no ellipsis. All three run under `recover()` guards in `TestDW_1_7_EmptyShortNewlineTextRendersCleanly` with no panic recorded, and `wantEll=false` assertions all pass.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] DW-1.1 → `TestDW_1_1_MultiHitSearchReturnsCompactLineText` (ran, PASS)
- [x] DW-1.2 → `TestDW_1_2_EpisodicLeadSnippetNormalizedAndCapped` (ran, PASS, 2 subtests)
- [x] DW-1.3 → `TestDW_1_3_RuneSafeTruncationOnEmojiBody` (ran, PASS, 7 subtests, dirty test as required)
- [x] DW-1.4 → `TestDW_1_4_IDSourceRoundTrip` + `TestDW_1_4_IDSourceRoundTripSurvivesAdversarialContent` (ran, PASS)
- [x] DW-1.5 → `TestDW_1_5_SemanticGraphGistIsFullStatement` (ran, PASS, 2 subtests)
- [x] DW-1.6 → `TestDW_1_6_BackendHitsNotMutated` (ran, PASS)
- [x] DW-1.7 → `TestDW_1_7_EmptyShortNewlineTextRendersCleanly` (ran, PASS, 3 subtests, dirty test as required)
- [x] 100% of DW items covered by automated tests; ≥1 dirty/adversarial test present (DW-1.3 rune-straddle sweep, DW-1.7 empty/newline cases, DW-1.4 adversarial-content injection attempt)

## Dead Code
None found. All imports in render.go/tools.go/render_test.go/budget_test.go are used; no unreachable statements after early returns; no commented-out blocks or debug prints.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | render.go is pure/stateless (no shared mutable state); `toolSchemas()` comment notes fresh-map-per-call precedent but is unrelated to this diff |
| Error Handling | PASS | `parseFields` (render.go:93-102) treats empty/malformed `fields_json` as nil rather than panicking/erroring, matching the existing `topFacets` idiom in budget.go for the same external data; traced malformed-JSON input (`"not json"`) through `TestRenderHitMissingOrMalformedFieldsNoPanic/bad-json` — no panic, identity (`ID`/`Source`) preserved |
| Resources | N/A | no file handles/connections/locks introduced by this diff |
| Boundaries | PASS | traced `leadSnippet` at the emoji/rune boundary (DW-1.3) and empty-string boundary (DW-1.7) — both hold; traced `displayFields` with all display keys absent → returns `nil` (not an invented empty map), verified via `TestRenderHitMissingOrMalformedFieldsNoPanic` |
| Security | PASS | traced the adversarial-injection case (`"fake\tid\tsource\ninjection"` as a hit's statement/text body) attempting to forge extra tab-delimited fields ahead of a later hit's id/source — `normalizeToSingleLine` strips the embedded tab/newline before the value ever reaches `formatHitLine`, so it cannot masquerade as a field delimiter; round trip verified to hold in `TestDW_1_4_IDSourceRoundTripSurvivesAdversarialContent` |

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-routine-and-class-design | RP-6 functional cohesion (one operation per routine) | PASS | Every routine in render.go (`renderHit`, `parseFields`, `gistFor`, `displayFields`, `normalizeToSingleLine`, `leadSnippet`, `formatScore`, `formatHitLine`, `compactLines`) does exactly one named operation at its declared abstraction level; no "and/then" compound routines |
| cc-routine-and-class-design | PP-4 parameter count ≤7 | PASS | Max parameter count in render.go is 2 (`gistFor(source, fields)`, `displayFields(source, fields)`, `leadSnippet(text, maxRunes)`); no routine approaches the 7-param threshold |
| cc-routine-and-class-design | CQ-5/CQ-6/RF-2 inheritance/LSP | N/A | render.go and tools.go introduce no types with inheritance — pure functions and flat structs (`renderedHit`, `renderedResult`) only |
| cc-routine-and-class-design | RF-11 routine hides failure as neutral default | Note (not FAIL) | `parseFields` returns `nil` for both "no fields" and "malformed fields_json" — indistinguishable cases. This mirrors a pre-existing pattern already established in budget.go's `topFacets` (not introduced by this diff) and is explicitly documented as intentional in the function's comment; no DW item or listed edge case requires distinguishing the two, so this is not demonstrated as a defect this phase introduced |

## Notes (non-blocking)
- `hitDisplayFields` (render.go:22-26) intentionally omits `source_ids` for episodic and semantic tiers (present in `internal/retrieval/opensearch.go`'s `allowedFields` for both), beyond just the field that became the gist. The doc comment ("minus the field that became the gist for each source") slightly overstates what was dropped — a clarity nit, not a functional defect; no DW item or edge case requires `source_ids` in the compact-line display.
- `formatHitLine`/`renderedHit` treat `ID`/`Source` as trusted, unnormalized pass-through. This is correct per DW-1.4/DW-1.4-adversarial as tested (both tests only inject adversarial content into text/statement fields, and `Hit.ID` is a server/OpenSearch-assigned document id, not raw user text). Whether `event_id`-derived document ids could ever contain a tab character is a question about `internal/ingest`/document-id generation, which this phase's diff does not touch — out of scope here, flagging for awareness only.

## Issues (if FAIL)
None.

**Verdict: PASS**

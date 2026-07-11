# Discovery + Design: Phase 1 - Breadth results + compact-line rendering

## Files Found
- `internal/mcp/mcp.go` — `Hit` struct (id/score/source/fields_json), `Backend` interface (`Search` returns `[]Hit`).
- `internal/mcp/tools.go` — `callSearch` (drives `packSearchResult` + spill), `toolResult`/`toolError` (wrap payload as MCP content).
- `internal/mcp/budget.go` — `searchResult` envelope, `packSearchResult`/`buildSearchResult`/`searchResultFits` (byte-budget packing over raw `Hit`), `topFacets`/`refineHint`.
- `internal/mcp/spill.go` — spill-to-disk on omission; untouched by this phase.
- `internal/mcp/mcp_test.go`, `internal/mcp/budget_test.go`, `internal/mcp/spill_test.go` — existing test suite/fixtures (`fakeBackend`, `fixedHitsBackend`, `semanticHit`, `manyHits`, `searchViaWire`).
- `internal/retrieval/opensearch.go:306/322` — `allowedFields`/`projectFields` (projection allowlist; OUT of scope, read-only reference for which fields exist per source).

## Current State
`callSearch` packs hits via `packSearchResult` (byte-budget-aware, unchanged by this phase) then wraps the raw `searchResult{Hits []Hit, Omitted, OmittedFacets, Hint, OverflowPath}` through `toolResult`, which `json.Marshal`s the whole payload into **both** the `content[0].text` block and `structuredContent`. Each `Hit.Fields` is a raw JSON-encoded string (`fields_json`), so the text block today is JSON containing an escaped JSON string per hit — exactly DW-1.1's target defect. Episodic `text` (up to ~1KB) crosses inline, untruncated, inside that escaped string.

## Gaps
- No renderer exists: `Hit.Fields` is never parsed/projected in `internal/mcp`; it is opaque to this package.
- No snippet/truncation logic exists anywhere in `internal/mcp`.
- No id+source addressing contract is materialized as its own type — today it's just two of four `Hit` fields alongside the opaque `Fields` blob.
- `toolResult` conflates the text-block rendering with the structured payload (one marshal for both); Phase 1 needs the text block to diverge from a straight marshal.

## Code Standards
`docs/code-standards.md` is a greenfield placeholder ("no code yet") but the codebase has since converged on real conventions I will match: doc comments starting with the symbol name and explaining *why*, not just what (see `budget.go`/`spill.go`); errors wrapped with `fmt.Errorf("mcp: ...: %w", err)`; external/wire input parsed defensively (never trusted, e.g. `topFacets`'s tolerate-malformed-JSON pattern); table-driven and named `TestDW_<phase>_<item>_...` tests; every code-touching change ships a dirty/error-path test.

## Test Infrastructure
`go test ./...` (table-driven, `t.Run` subtests). MCP tests drive the real JSON-RPC wire via `refClient`/`startServer` (`mcp_test.go`) rather than calling handlers directly, so behavior is exercised end-to-end through `Serve`. `budget_test.go` adds `fixedHitsBackend` + `semanticHit`/`manyHits` fixtures for constructing precise `Fields` JSON. No emoji/multibyte fixture exists yet — Phase 1 adds one.

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-1.1 | multi-hit `memory_search` returns compact-line text, not escaped `fields_json` JSON | COVERED | `TestDW_1_1_MultiHitSearchReturnsCompactLineText` (content text fails `json.Unmarshal`, contains no `fields_json` substring, one line per hit) |
| DW-1.2 | episodic `text` renders as a normalized single-line lead snippet within the length cap | COVERED | `TestDW_1_2_EpisodicLeadSnippetNormalizedAndCapped` (gist ≤ cap runes, no embedded newline, short text unmodified) |
| DW-1.3 | truncation is UTF-8 rune-safe — an emoji/multibyte body is never split mid-rune | COVERED | `TestDW_1_3_RuneSafeTruncationOnEmojiBody` (dirty; 👻🕯️ padded to straddle the truncation boundary; `utf8.ValidString` + no U+FFFD replacement char) |
| DW-1.4 | every result line exposes `id` and `source` unambiguously (parse round-trips to the same pair) | COVERED | `TestDW_1_4_IDSourceRoundTrip` (tab-split parse recovers the exact id+source for all three tiers) |
| DW-1.5 | semantic/graph hits render full `statement`/s-p-o (content identical to today, format changed) | COVERED | `TestDW_1_5_SemanticGraphGistIsFullStatement` (gist equals the untruncated `statement`; subject/predicate/object surfaced as display fields) |
| DW-1.6 | the gRPC `Hit` still carries full untruncated `text` — a gRPC/`engramclient` consumer sees no truncation | COVERED | `TestDW_1_6_BackendHitsNotMutated` (post-call, the `Hit` slice the fake `Backend.Search` returned is byte-identical to what it constructed — no in-place mutation/truncation escapes the render layer) |
| DW-1.7 | empty/short/newline-laden `text` inputs render without panic or dangling ellipsis | COVERED | `TestDW_1_7_EmptyShortNewlineTextRendersCleanly` (dirty; subtests: empty text, sub-cap text, newline/tab-laden text) |

**All items COVERED:** YES

## Design Decisions

**Where rendering happens.** The renderer sits strictly *after* `packSearchResult` in `callSearch` (tools.go), operating on the already budget-packed `searchResult`. This keeps `budget.go`'s byte-accounting untouched (out of this phase's stated scope beyond hint wording) while still guaranteeing the final emitted bytes stay within budget: the compact-line/unnested-fields rendering is provably smaller than the raw-`fields_json`-escaped-in-JSON form it replaces (no double-encoding tax, plus episodic text is now truncated), so "packed form fits budget" ⇒ "rendered form fits budget." No budget.go changes were needed or made.

**New unexported types (`internal/mcp/render.go`):**
- `renderedHit{ID, Source string; Score float64; Gist string; Fields map[string]any}` — the compact-line result: enough to decide read/don't-read, never the fat body. `Fields` here is *already* the per-source display allowlist (subject/predicate/object/valid_at/hop for semantic/graph; kind/occurred_at/event_id for episodic) — un-nested from `fields_json` into a real object, which also happens to satisfy the plan's "free win" un-nesting called out in research (a byproduct of this phase, not scope creep: it was already necessary to compute the gist from parsed fields).
- `renderedResult{Hits []renderedHit; Omitted int; OmittedFacets map[string]string; Hint string; OverflowPath string}` — mirrors `searchResult`'s envelope shape/gating (omitted/omitted_facets/hint present only when hits were actually left out), now with rendered hits.

**Compact-line format decision (delimiter):** the plan's Notes explicitly defer the exact schema to this build ("Snippet length (~200) and exact compact-line delimiter/field-order are finalized in Phase 1 build"). The research doc's illustrative example uses positional, space-separated fields across two lines. I chose **one line per hit, tab-separated**, instead: `<id>\t<source>\t<score>\t<gist>\t<key=value>...`. Rationale — id (OpenSearch `_id`) and source (a fixed 3-value enum) are structurally tab-free; the gist and every trailing display-field value are passed through `normalizeToSingleLine`, which maps all control characters (including tab) to a collapsed single space. That makes the first two tab-separated tokens **unconditionally** parseable as (id, source) with zero ambiguity, even adversarially (an attacker-controlled `statement`/`text` cannot inject a fake delimiter). A two-line positional format would require documenting and defending a fixed field count/order per source and gives no such structural guarantee. Tab-separated is also strictly one line per hit, matching "compact-line" literally.

**`toolResult` vs. text/structured divergence:** added `toolResultWithText(payload any, text string)` alongside the existing `toolResult` (unchanged, still used by ingest/status) so `callSearch` can emit a `content[0].text` that differs from `structuredContent`'s JSON encoding — the compact-line render for text, the full `renderedResult` for structured clients. This is a minimal, additive change; no existing caller of `toolResult` is touched.

**Snippet/normalization routines** (functional cohesion, one operation each): `normalizeToSingleLine` (control-char/whitespace collapse), `leadSnippet` (rune-safe cap + conditional ellipsis, delegates normalization to the above), `gistFor` (source-dispatch: episodic → truncated `text`; everything else → untruncated `statement`, with a `text` fallback for an unregistered source), `parseFields` (tolerant `fields_json` → `map[string]any`, mirrors `budget.go`'s `topFacets` malformed-input handling), `displayFields` (per-source allowlist projection), `formatScore` (fixed 3-decimal string), `formatHitLine`, `compactLines`. Each name states one operation; no "and"/"then" compound routines.

**Existing anchored-test impact (not a regression):** `budget_test.go`'s `searchViaWire` helper currently decodes `decoded` by `json.Unmarshal`-ing `content[0].text`. Since DW-1.1 intentionally makes that block non-JSON, this helper is updated to instead read `structuredContent` directly (already parsed by the JSON-RPC layer into `map[string]any` — no re-unmarshal needed). This is a direct, intended consequence of the phase's contract change, not an unintended break: every existing assertion built on `decoded[...]` (omitted/omitted_facets/hint/hits presence and counts) continues to hold unmodified against `structuredContent`, and `TestDW_2_1_DefaultSearchFitsBudget`'s `len(text) <= budget` check (text length, not JSON-ness) needs no change at all — the compact form is provably smaller. All previously-passing budget/spill tests remain green after this update (verified below).

## Prerequisites
- [x] Required files exist (`internal/mcp/*.go`)
- [x] Dependencies available (stdlib only: `encoding/json`, `strconv`, `strings`, `unicode`, `unicode/utf8` in tests)
- [x] No missing prerequisites

## Recommendation
BUILD. Add `internal/mcp/render.go` (+ `render_test.go`), wire it into `callSearch`/`toolResultWithText` in `tools.go`, and update `budget_test.go`'s `searchViaWire` decode source. No gRPC/proto/store/retrieval/budget-accounting changes needed or made.

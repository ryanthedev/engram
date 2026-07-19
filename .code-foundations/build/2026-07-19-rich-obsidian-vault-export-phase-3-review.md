# Review: Phase 3 - event + concept note rendering

## Executed Results (Step 0)
- Test suite: `go test ./internal/cli/ -count=1` → `ok github.com/ryanthedev/engram/internal/cli 0.031s` (all tests pass, including all `TestDW_3_*` and untagged Phase 3 tests)
- Typecheck/build: `go vet ./internal/cli/...` → clean (no output)
- Lint: `gofmt -l internal/cli/vaultnotes.go internal/cli/vaultnotes_test.go` → clean (no output)
- Coverage: `go test ./internal/cli/ -coverprofile=... && go tool cover -func=...` → every Phase 3 function in `vaultnotes.go` at **100.0%**: `renderEvent`, `renderConcept`, `writeEventFrontmatter`, `writeConceptFrontmatter`, `writeFrontmatter`, `writeConceptsFooter`, `writeClaims`, `sortedClaims`, `sourceQuote`, `writeRelatedConcepts`, `resolveLinks`.
- Additional adversarial probe: wrote a scratch test (`internal/cli/zz_review_temp_test.go` + `zz_review_yaml_test.go`, deleted after use, never committed) constructing a `Concept`/`Event` with hostile `Name`, `Body`, `Statement`, and `Alias` fields (`---`, `> [!danger]`, `<script>`, `![[x]]`, `[[y]]`) and ran it via `go test -run TestZZ... -v`. Results folded into Correctness Dimensions / Security below.

## Requirement Fulfillment

### DW-3.1
PREMISE:  event note carries full `sanitizeBody`'d prose, a UTC-foldered path (`events/YYYY/YYYY-MM-DD <slug>.md`; nil `OccurredAt` → `events/undated/`), and a `**Concepts:**` footer resolving links via `VaultRefs`.
EVIDENCE: vaultnotes.go:42-56 (`renderEvent`), :122-130 (`writeConceptsFooter`)
TRACE:    `TestDW_3_1_EventNoteBodyPathAndFooter` — `Event{Body: "Alice met Bob in the park.\nThey talked for an hour."}` + `refs["ev-1"] = {File: "2026-01-15 alice-meets-bob", Folder: "events/2026"}` → `renderEvent` returns `relPath = "events/2026/2026-01-15 alice-meets-bob.md"`; body written verbatim through `sanitizeBody` (benign prose passes unchanged); footer `**Concepts:** [[Alice|Alice]], [[Bob|Bob]]` sorted by `ref.File`. `TestDW_3_1_EventNoteUndatedFolder` confirms `refs["ev-2"].Folder = "events/undated"` (nil `OccurredAt`) round-trips to `relPath = "events/undated/undated-thing.md"`. All ran and passed in Step 0.
VERDICT:  PASS

### DW-3.2
PREMISE:  concept note renders claims oldest-first (tie-break `EdgeID`); each claim shows its `Statement` plus a folded `> [!quote]-` callout whose body is the SOURCE EVENT'S PROSE run through `sanitizeBody`+`quoteBlock`, attributed with the event-note wikilink; a claim whose source event is absent (empty SourceEventID, or not in the events map, or not in refs) → Statement alone, no callout.
EVIDENCE: vaultnotes.go:138-153 (`writeClaims`), :159-168 (`sortedClaims`), :181-195 (`sourceQuote`)
TRACE:    `TestDW_3_2_ConceptNoteClaimsOldestFirstWithQuote` — claim `ValidAt=2000` (born) renders before claim `ValidAt=2027` (joined); each claim's callout title is `> [!quote]- Source: [[2000-01-01 born|Born]]` / `[[2027-01-01 joined|Joined]]`, body is the *source event's* prose (`"> Alice was born in a small hospital downtown."`), not the claim's own statement. `sourceQuote` (line 182: `SourceEventID == ""` → `""`; line 185-187: `events[id]` miss → `""`; line 189-191: `refs[id]` miss → `""`) is exercised by `TestDW_3_2_ConceptNoteClaimWithoutSourceQuote`, `...ClaimSourceEventNotInEventsMap`, `...ClaimSourceEventNotInRefs` — all three assert `Statement` present and no `[!quote]` substring. `TestDW_3_2_ConceptNoteClaimTieBreaksOnEdgeID` confirms `ed-a` before `ed-b` when both `ValidAt` are nil (tie). All ran and passed in Step 0.
VERDICT:  PASS

### DW-3.3
PREMISE:  only degree ≥2 concepts get files; degree ≤1 appear solely as unresolved ghost `[[links]]`.
EVIDENCE: vaultnotes.go (no gating logic in the renderer itself — file-vs-ghost gating is the Phase 5 assembler's job); `renderConcept`'s only Phase-3 obligation is to render whatever concept it is asked to and to resolve neighbor links (ghost or hub) identically via `refs`.
TRACE:    `TestDW_3_3_GhostConceptsResolveAsLinksNotFiles` builds a 3-concept model (`hub`, `hub2` degree 2, `ghost` degree 1/`Ghost:true`), simulates the Phase-5 discipline (`if c.Ghost { continue }` before calling `renderConcept`), and renders only 2 files (`concepts/Hub.md`, `concepts/Hub2.md`) — `concepts/Ghost.md` is absent. `hub`'s "Related concepts" section still contains `[[Ghost|Ghost]]`, proving the ghost resolves as a valid link target purely off `refs` (Phase 2's `buildVaultRefs` assigns every concept, ghost or not, a `noteRef`) without `renderConcept` needing any special-case. Ran and passed in Step 0. (Whether the *actual* Phase-5 assembler in this codebase performs this same skip is outside Phase 3's file scope and not evaluated here — that is Phase 5's own DW-item, not this phase's.)
VERDICT:  PASS

### DW-3.4
PREMISE:  identical model+refs → byte-identical notes across two runs.
EVIDENCE: vaultnotes.go — no wall-clock read, no unsorted map iteration; `sortedClaims` (line 159-168) is a stable, deterministic sort; `resolveLinks` (line 219-235) sorts by `ref.File`; `writeFrontmatter` marshals an ordered `yaml.MapSlice` (not a map) so field order is fixed.
TRACE:    `TestDW_3_4_EventNoteByteIdenticalAcrossRuns` and `TestDW_3_4_ConceptNoteByteIdenticalAcrossRuns` call `renderEvent`/`renderConcept` twice on the same inputs and assert `path1 == path2 && content1 == content2`. Both ran and passed in Step 0.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have corresponding tests, all ran in Step 0: `TestDW_3_1_*` (5 tests), `TestDW_3_2_*` (8 tests), `TestDW_3_3_GhostConceptsResolveAsLinksNotFiles`, `TestDW_3_4_*` (2 tests).
- [x] Test coverage matches the stated 100% level: `go tool cover -func` confirms 100.0% for every Phase 3 function in vaultnotes.go (11/11 functions).
- No gaps found.

## Dead Code
None found. Imports (`fmt`, `sort`, `strings`, `time`, `yaml`) are all used; no unreachable statements after early returns (`sourceQuote`'s three guard clauses each return before falling through, `writeClaims`/`writeConceptsFooter`/`writeRelatedConcepts` guard on empty inputs cleanly).

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | Both renderers are pure functions over value/map arguments — no shared mutable state, goroutines, or channels. |
| Error Handling | PASS | `writeFrontmatter` (line 113-118) discards `yaml.Marshal`'s error, but the traced input space (`string`, `int`, `[]string` scalars in a fixed `yaml.MapSlice`, no cycles, no unsupported types) cannot produce a marshal error — traced `writeConceptFrontmatter`/`writeEventFrontmatter` call sites confirm only these types ever reach it. Documented inline; not a defect. |
| Resources | N/A | No file handles, connections, locks, or caches — pure in-memory string building. |
| Boundaries | PASS | Traced: nil `ConceptIDs`/`RelatedIDs`/`Claims` slices (`writeConceptsFooter`, `writeClaims`, `writeRelatedConcepts`) all guard on `len(...) == 0` before any indexing — no panic on nil-slice range (Go ranges over nil slices safely) or empty-collection edge. `resolveLinks` skips any id absent from `refs` (line 222-226) rather than emitting a malformed `[[|]]` link — traced via `TestEventNoteConceptFooterSkipsUnresolvableID` (asserts no `"[[|"` substring). |
| Security | PASS | Traced adversarial construction (scratch test, not committed): `Event{Title: hostile, Body: hostile}` and `Concept{Name: hostile, Aliases: [hostile], Claims: [{Statement: hostile, SourceEventID: ev}]}` where each hostile string embeds `---`, `> [!danger]`, `<script>`, `![[x]]`, `[[y]]`. Result: `<script>` → `&lt;script>` (via `sanitizeInline`), `![[x]]`/`[[y]]` → bracket-broken (`![\[x]]`/`[\[y]]`), line-leading `> [!danger]` → backslash-escaped, `obsidian://`/`javascript:`/`data:` schemes defused — in both the event Body path and the claim-Statement / quoted-source-event-prose path (`sourceQuote` → `sanitizeBody` then `quoteBlock`). The H1 in both note types comes from `refs[...].Display` (already `cleanInline`'d by Phase 2), never the raw `Name`/`Title` — confirmed no raw hostile `Name`/`Title` string appears anywhere in either rendered note. See Notes below for one nuance (Aliases). |

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-control-flow-quality | Nesting depth ≤3, guard clauses at function entry for early-exit cases | PASS | `sourceQuote` (vaultnotes.go:181-195) is a clean 3-guard-clause chain (`if cl.SourceEventID == "" { return "" }`, `if !ok { return "" }` ×2) with the nominal path (title+quote build) flowing unnested at the bottom — textbook guard-clause pattern. No function in the file nests past 2 levels (a `for` with one `if` inside, in `writeClaims`). |
| cc-control-flow-quality | McCabe complexity ≤10 (no mandatory-refactor routines) | PASS | Highest complexity in the file is `sourceQuote` at ~4 and `writeClaims` at ~3 — all well under the 6-10 "start simplifying" band, let alone the 20+ mandatory-refactor threshold. |
| cc-control-flow-quality | Loop-index/variable naming, boolean comparisons | PASS | No numeric loop indices in the file (all `for _, x := range ...`); no `== true`/`== false` boolean anti-patterns; `compareTimePtr`'s int-return comparisons (`c < 0`) are simple and consistently used both here and in the (already-reviewed) Phase 2 sort. |
| cc-defensive-programming | External input validated/sanitized at the barricade before use | PASS (with one Note) | `Body`, `Statement`, and quoted source-event prose are barricaded through `sanitizeBody`(+`quoteBlock`) exactly at the point they enter markdown (vaultnotes.go:51, :145, :194) — traced and confirmed hazard-free in the adversarial probe. `Aliases` bypasses `cleanInline`/`sanitizeFilename` entirely and is marshaled raw into the YAML frontmatter (vaultnotes.go:103-105); see Notes — this is contained by the YAML encoder (verified by round-tripping the frontmatter through a real YAML unmarshaler: `engram_id` stays `"c-1"`, the hostile alias including its own embedded `engram_id: hacked` line stays a single opaque string value, never a second top-level key) and mirrors the pre-existing Phase 1 precedent (`export.go:330-343`'s `renderNote`), so it is not a demonstrated defect, but it does deviate from the literal primitive named in the dispatch prompt's security-focus paragraph. |
| cc-defensive-programming | No empty catch blocks / no silently swallowed bugs | PASS | Go has no catch blocks; the sole discarded error (`yaml.Marshal` in `writeFrontmatter`) is discussed above under Error Handling — not silent-bug-swallowing since the input space provably cannot error. |

## Notes (non-blocking)

- **Aliases frontmatter routing**: the dispatch prompt's security-focus paragraph names `sanitizeFilename`/`cleanInline` as the expected primitive for "entity/concept names, aliases, link labels." `Concept.Name` and all link labels do go through `cleanInline` (via `refs[...].Display`, built in Phase 2) — but `Concept.Aliases` is passed raw into the YAML frontmatter (`writeConceptFrontmatter`, vaultnotes.go:103-105), relying on the YAML encoder rather than `cleanInline` as its barricade. I verified this is safe in practice: (1) a real YAML unmarshal of the produced frontmatter shows `engram_id` is never overridden by a hostile alias smuggling its own `engram_id: hacked` line, because the encoder emits the alias as an indented block-literal (`|-`) so embedded `---`/`key:` lines are indented and inert; (2) Obsidian's frontmatter/properties pane does not render YAML property values as markdown/HTML, so a raw `<script>`/`![[x]]` substring sitting inside a YAML string value is inert, unlike the same substring in the note body or H1. This matches the already-established Phase 1 pattern in `export.go`'s `renderNote` (same raw-aliases-into-YAML-MapSlice approach), so it reads as a deliberate, consistent architectural choice rather than a Phase-3-introduced gap. Flagging for visibility since the dispatch prompt specifically called out aliases; not failing it since no executable injection was demonstrated and the precedent predates this phase.
- `writeFrontmatter`'s discarded `yaml.Marshal` error is documented inline with a correct justification; a future caller that starts feeding it richer types (e.g., nested maps or custom types with `MarshalYAML` that can error) would need to revisit this — worth a comment-linked TODO if that ever happens, but out of scope today.
- `resolveLinks`' silent skip of an id absent from `refs` (vaultnotes.go:222-226) is documented as "should never happen given the Phase 2 contract" — reasonable defense-in-depth, not a gap given Phase 2's contract was already reviewed.

## Issues (if FAIL)
None.

**Verdict: PASS**

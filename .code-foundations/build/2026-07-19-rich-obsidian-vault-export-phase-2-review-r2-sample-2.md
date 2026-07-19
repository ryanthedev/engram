# Review: Phase 2 - vault model, refs, sanitizer (r2 sample 2, security-sensitive)

## Executed Results (Step 0)
- Test suite: `go test ./internal/cli/ -count=1` → **ok** (all pass, 0.03s)
- Coverage: `go test ./internal/cli/ -cover` → 60.4% package-wide; `go tool cover -func` shows **100.0%** on every Phase 2 function in sanitize.go and vaultmodel.go (sanitizeBody, quoteBlock, normalizeNewlines, sanitizeInline, escapeLineStart, calloutForgeLeading, buildVaultModel, dedupeEvents, episodicBefore, compareTimePtr, collapseEntities, normalizeConceptName, assembleConcepts, addNeighbor, pickSourceEvent, assembleEvents, eventTitle, buildVaultRefs, sortedKeys).
- Typecheck/build: `go run` of the independent adversarial harness compiled clean against a copy of sanitize.go; package builds.
- Lint: not separately configured; `go vet` implied clean by passing build.

## Requirement Fulfillment

### DW-2.1
PREMISE:  `sanitizeBody` neutralizes leading `---`, callout forge, `[[`/`![[`, fence breakout, raw HTML tags, and dangerous URI schemes (obsidian://, data:, javascript:), leaving benign prose intact except neutralized tokens.
EVIDENCE: sanitize.go:50-57 (pipeline), :90-120 (sanitizeInline: control-drop, `<`→`&lt;`, `[[` break, scheme defuse), :125-138 (escapeLineStart: `---`/```` ``` ````/`~~~`/callout).
TRACE:    Independent harness (/tmp/r2rev2-p2) — `"---\n..."`→`\---`; `"> [!danger] pwn"`→`\> [!danger] pwn`; `"[[[[deep"`→`[\[\[\[deep`; `"![\x00[img]]"`→`![\[img]]`; `` "```bash" ``→`` \```bash ``; `"<script>"`→`&lt;script>`; `"javascript:alert(1)"`→`javascript :alert(1)`; benign `"see [the doc](https://example.com)"` unchanged. Full 6-detector hazard sweep: zero survivors.
VERDICT:  PASS

### DW-2.2
PREMISE:  `quoteBlock` prefixes every line incl. blank lines with `> `; adversarial multi-line input cannot exit or forge a callout.
EVIDENCE: sanitize.go:64-75 — unconditional `"> "` prefix per line; callout-leading (`[!`… or `>…[!`) escaped before prefixing.
TRACE:    `quoteBlock("a\n\nb")`→`"> a\n> \n> b"` (blank line = `"> "`); `quoteBlock("")`→`"> "`; `"[!danger] pwn"`→`"> \[!danger] pwn"`; `"> [!tip] x"`→`"> \> [!tip] x"`; `">>> [!x] z"`→`"> \>>> [!x] z"`. Harness forge-regex `^(>[ \t]*)+\[!` matched no output line; every line carried the `> ` prefix. Test `TestDW_2_2_QuoteBlockCannotForgeOrExitCallout` ran green.
VERDICT:  PASS

### DW-2.3
PREMISE:  hub rule `Ghost = Degree < 2`; normalized-name-equal entities collapse; distinct-named entities sharing a source_id do NOT; duplicate event_ids dedupe deterministically.
EVIDENCE: vaultmodel.go:31 (hubMinDegree=2), :290 (`Ghost: degree < hubMinDegree`); :171-221 collapse by `normalizeConceptName` equality only (:177, srcSet plays no role in the key); :106-135 dedupe (earliest OccurredAt→Kind→Text, empty id skipped, sorted by EventID).
TRACE:    `TestDW_2_3_HubRuleGhostBelowDegree2` → e-a Degree=2 Ghost=false, e-b/e-c Degree=1 Ghost=true, self-loop + unknown endpoint excluded from degree. `TestDW_2_3_SharedSourceIDDoesNotMerge` → 2 concepts (shared ev-1 does NOT fuse alpha/beta). `TestDW_2_3_NormalizedNameCollapse` → `  "alice   SMITH" ` and `Alice Smith` → 1 concept, canonical=smallest id. `TestDW_2_3_DuplicateEventIDsDedupeDeterministically` → all 5 tie-break paths + order-independence + empty-id skip.
VERDICT:  PASS

### DW-2.4
PREMISE:  `buildVaultModel` joins claims via `source_ids → event_id` (absent event → quote-less claim); output + `VaultRefs` byte-identical across runs.
EVIDENCE: vaultmodel.go:308-325 (pickSourceEvent: smallest source id with an exported Event, else smallest id, else ""), :243-294 claim attached to every resolvable endpoint, :374-444 deterministic collision-suffixed refs sorted by (id, folder).
TRACE:    `TestDW_2_4_ClaimJoinSourceIDsToEventID` → ed1 picks ev-1 (present) over ev-0 (absent); ed2 keeps ev-9 though no Event exists (quote-less); ed3 empty provenance; 3 claims survive on both endpoints, sorted ValidAt→EdgeID. `TestDW_2_4_ByteIdenticalAcrossRunsAndPermutations` → two runs and reversed-input run produce byte-identical JSON of model+refs.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have automated tests that ran in Step 0 (`TestDW_2_1_*`, `TestDW_2_2_*`, `TestDW_2_3_*`, `TestDW_2_4_*`, plus `TestQuoteBlockOfSanitizedBodyComposes` for the composed hot path).
- [x] Coverage matches the stated 100% level: `go tool cover -func` reports 100.0% on all 19 Phase 2 functions.
- No gaps.

## Dead Code
None found. `export.go`'s `noteRef` type was lifted to vaultmodel.go:39-43; export.go:53-54 leaves only a comment pointer (no orphaned declaration). export.go still contains its own pre-existing `vaultFilenames`/`renderNote` (out of Phase 2 scope, not dead — used by the existing exporter). No unreachable code, debug prints, or commented-out blocks in the reviewed files.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | Pure, stateless functions; no shared mutable state, goroutines, or I/O. |
| Error Handling | PASS | External export data treated as untrusted: empty-id episodics/entities skipped (vaultmodel.go:109,174), join total (claims never dropped), no panics reachable — slices guarded by `len`, map reads use comma-ok / nil-safe `sortedKeys`. |
| Resources | N/A | No file handles, connections, or locks in the reviewed functions. |
| Boundaries | PASS | Rune-cap on title (:360), `idPrefix` shorter-than-n handled (export.go:274), residual-clash loop provably terminates (extends prefix then counter, :429-435), empty maps → empty slices. Adversarially traced `[[[[[`, double-NUL, zero-width-space bracket splits — no recombination. |
| Security | PASS | Injection barricade attacked hard (see below) — no breach. |

### Security — injection barricade attack log (independent harness, /tmp/r2rev2-p2)
Ran a copy of sanitize.go (as `package main`) under a 6-detector hazard sweep, NOT the repo's test corpus. Every constructed input reported below produced zero surviving `[[`, `![[`, active callout, live scheme, or raw `<`:

| Attack class | Representative input → output | Result |
|---|---|---|
| Contiguous brackets | `[[[[deep` → `[\[\[\[deep` | defused |
| Control chars between brackets (NUL/DEL/`\x01\x02\x1f`) | `![\x00[img]]` → `![\[img]]` | defused — dropped controls do NOT clear prevBracket (:98-103) |
| Zero-width space (≥0x20, kept) between brackets | `[​[x` → `[​[x` | no adjacency |
| `<` interleaved | `[<[x` → `[&lt;[x` | no adjacency |
| Scheme + bracket same line | `[[data:x` → `[\[data :x` | both defused |
| Scheme colon reformed after control drop | `java\x00script:alert(1)` → `javascript :alert(1)` | defused (scheme pass runs on emitted string) |
| Label-link schemes | `[click](javascript:alert(1))`, `obsidian://`, mixed-case `DaTa:` | all `scheme :`-defused |
| Raw HTML | `<iframe…>`, `<script>` | every `<`→`&lt;` |
| Line-start forgery incl. CR/CRLF dodge | `safe\r---\r> [!x] hi` → `safe\n\---\n\> [!x] hi` | normalized then escaped |
| Callout smuggled to quoteBlock | `quoteBlock("[!danger] pwn")` → `> \[!danger] pwn` | escaped before prefix |
| Callout exit via blank/unprefixed line | `quoteBlock("a\n\nb")` → `"> a\n> \n> b"` | blank line = `"> "`, no exit |
| Composition hot path | `quoteBlock(sanitizeBody(hostile blob))` | both contracts hold simultaneously |

Considered-and-safe (documented, not defects):
- **U+2028/U+2029 line separators** are NOT normalized to `\n`, so a post-U+2028 `---`/`> [!` is not escaped. This is correct: CommonMark/Obsidian define block line-endings as LF/CR/CRLF only, so those tokens render as inert inline text, not frontmatter/callouts. Normalizing exactly `\r\n`/`\r` matches the parser's line model. No block hazard activates. Not a breach.
- **`vbscript:` / other schemes** are not defused — outside the DW-listed set (obsidian/data/javascript). Not a requirement; noted only.

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| aposd-designing-deep-modules | Interface depth / low caller cognitive load | PASS | `sanitizeBody`/`quoteBlock` are 1-arg pure transforms hiding all hazard-defusal; `buildVaultModel` is a single deep entry point returning the whole model + ref table. No shallow pass-throughs. |
| aposd-designing-deep-modules | No information leakage / red flags | PASS | Hazard rules localized to sanitize.go; ref-collision policy fully inside buildVaultRefs; no duplicated knowledge across the reviewed modules. `noteRef` consolidated in one place. |
| aposd-designing-deep-modules | No silent failure | PASS | Skips are documented and observable (record count drops, empty-id records excluded from Events/refs — asserted by `TestEmptyIDEntitySkipped`, `empty event_id skipped`); no error swallowed into a wrong-but-quiet result. |
| cc-defensive-programming | External input validated at entry | PASS | Export records are external input; ids validated (empty skipped) at collapse/dedupe/refs boundaries; join is total so malformed provenance degrades to quote-less rather than crashing. |
| cc-defensive-programming | Barricade with defense-in-depth on security path | PASS | quoteBlock does NOT assume its input passed sanitizeBody (sanitize.go:12-14) — re-escapes callouts itself; composition test confirms both layers active. |
| cc-defensive-programming | No empty catch / no side-effecting assertions | PASS | Go; no swallowed errors, no assertion misuse in reviewed code. |
| cc-defensive-programming | Correctness over robustness for a data pipeline | PASS | Deterministic, order-independent output (byte-identical test); transform-not-reject preserves data while neutralizing hazards. |

## Notes (non-blocking)
1. `vbscript:`, `file:`, and other exotic schemes are not neutralized — outside the DW-listed trio. If the threat model later widens, `schemePattern` (sanitize.go:28) is the single edit point.
2. U+2028/U+2029 are intentionally not treated as line breaks (correct per CommonMark). Worth a one-line code comment to preempt a future reviewer re-flagging it, but not a defect.
3. Package-wide coverage is 60.4% because pre-existing export.go paths (dial/drain/write) are not Phase 2 scope; all Phase 2 functions are at 100%.

## Issues (if FAIL)
None.

**Verdict: PASS**

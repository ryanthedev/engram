# Review: Phase 2 - vault model, refs, sanitizer (r2, sample 3)

## Executed Results (Step 0)
- Test suite: `go test ./internal/cli/ -count=1` → **ok** (all pass, 0.029s)
- Coverage: `go test ./internal/cli/ -cover` → 60.4% package-wide; **every Phase 2 function in sanitize.go and vaultmodel.go = 100.0%** (`go tool cover -func`)
- Vet: `go vet ./internal/cli/` → clean
- Independent adversarial harness (sanitize.go copied verbatim into /tmp/r2rev3-p2, package renamed): **0 failures** over my own corpus (bracket runs, control/NUL/DEL interleaving, Unicode look-alikes + zero-width + line/paragraph separators, backslash interleavings, CR dodges, scheme reconstruction through dropped control chars, HTML, and the quoteBlock composition path)
- Independent black-box vaultmodel tests (my own fixtures, run in-package then removed): 3/3 pass

## Requirement Fulfillment

### DW-2.1
PREMISE:  `sanitizeBody` neutralizes leading `---`, callout forge, `[[`/`![[`, fence breakout, raw HTML tags, and dangerous URI schemes (obsidian://, data:, javascript:), leaving benign prose intact except neutralized tokens.
EVIDENCE: sanitize.go:50-57 (sanitizeBody), :90-120 (sanitizeInline), :125-138 (escapeLineStart), :28/:34 (patterns)
TRACE:    `---` → escapeLineStart prepends `\` → `\---` (no `^ {0,3}---`); `> [!x]` → calloutForgeLeading → `\> [!x]`; `[[` → emitted-rune tracking → `[\[`; `<iframe>` → `&lt;iframe>`; `javascript:` → schemePattern → `javascript :`. My harness ran all six hazard classes; zero survivors. Benign prose (`the metadata: field is set`, `col1\tcol2`) round-trips unchanged.
VERDICT:  PASS

### DW-2.2
PREMISE:  `quoteBlock` prefixes every line incl. blank lines with `> `; adversarial multi-line input cannot exit or forge a callout.
EVIDENCE: sanitize.go:64-75
TRACE:    Every line → `"> " + line`; blank → `"> "` (never a bare unprefixed line). A line reading as a callout (`[!`-leading or `>…[!`) is `\`-escaped before prefixing, so `> [!x]` → `> \> [!x]` (no `^(>[ \t]*)+\[!`). My harness swept 18 quoteBlock inputs incl. blank/CR-split/deeply-quoted forges + the composition path; no line exited or forged.
VERDICT:  PASS

### DW-2.3
PREMISE:  hub rule `Ghost = Degree < 2`; normalized-name-equal entities collapse; distinct-named entities sharing a source_id do NOT; duplicate event_ids dedupe deterministically.
EVIDENCE: vaultmodel.go:31 (hubMinDegree=2), :290 (Ghost: degree<2), :171-221 (collapseEntities), :230-235 (normalizeConceptName), :106-135 (dedupeEvents/episodicBefore)
TRACE:    Collapse keys on `normalizeConceptName` equality only; `"  ACME   Corp. "` and `"Acme corp"` share key → 1 concept (canonical = smallest id). `Distinct` sharing source_id `s1` keys on its own name → separate. Two empty-normalized names each key on `\x00id:<id>` → never fuse. Degree counts distinct canonical neighbors (self-loop, unknown endpoint, dup pair excluded) → my `n1` case Degree=1 Ghost=true. Dedupe winner = earliest OccurredAt→Kind→Text, order-independent.
VERDICT:  PASS

### DW-2.4
PREMISE:  `buildVaultModel` joins claims via `source_ids → event_id` (absent event → quote-less claim); output + `VaultRefs` byte-identical across runs.
EVIDENCE: vaultmodel.go:90-97, :243-294 (assembleConcepts), :308-325 (pickSourceEvent), :374-444 (buildVaultRefs; sorted (id,folder) drain)
TRACE:    pickSourceEvent = smallest source id with an exported Event, else smallest source id (renders quote-less), else "" — join never drops a claim. My determinism test with a dupe event, a collapsed member, and an `absent-ev` source produced an identical JSON encoding of `(model|refs)` across a run and a fully reversed-input permutation; the `absent-ev` claim survived with its id intact.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have automated tests that ran in Step 0 (DW-2.1: TestDW_2_1_*; DW-2.2: TestDW_2_2_* + TestQuoteBlockOfSanitizedBodyComposes; DW-2.3: TestDW_2_3_* + TestNormalizeConceptName; DW-2.4: TestDW_2_4_* + TestVaultRefs*)
- [x] Coverage matches stated level: 100% of every Phase 2 function in sanitize.go and vaultmodel.go
- No gaps.

## Dead Code
None found. All functions reachable and exercised (100% per-func coverage); no unused imports, no unreachable branches, no debug/commented-out blocks.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | Pure, stateless, deterministic transforms; no shared state, goroutines, or I/O. |
| Error Handling | PASS | Total over input: empty-id records skipped (documented), unknown edge endpoints/absent source events degrade to quote-less, no panics on nil slices/maps. My reversed-permutation + absent-source inputs produced correct graceful output. |
| Resources | N/A | No file handles, connections, or locks; only in-memory builders/maps. |
| Boundaries | PASS | Rune-based scans; NUL/DEL/`<0x20`/`\x7f`, empty strings, bracket runs of length 1..N, empty/whitespace names, nil times all handled. Title rune-cap at maxEventTitleRunes verified; idPrefix guards `n>=len`. My battery hit each boundary; none broke. |
| Security | PASS | Injection barricade attacked hard and independently (see below); no surviving `[[`/`![[`, active callout, live scheme, or raw `<`. |

### Security probe detail (the barricade)
Attempted, all defused (input → output):
- `[[[[deep` → `[\[\[\[deep`; `[\x00[link]]` → `[\[link]]`; `\\[[` → `\\[\[`; `[\t[`, `[\x7f[`, `[\x01\x02\x1f[` → no adjacency (emitted-rune tracking, control chars dropped without clearing the bracket flag).
- Unicode: fullwidth `［`, zero-width space, line/paragraph separators between brackets — none form literal `[[`; Obsidian wikilinks require literal adjacent `[[`, so these are inert.
- `![[embed.png]]` → `![\[embed.png]]` (transclusion broken).
- `java\x00script:x` → `javascript :x` (scheme pass runs AFTER control-char removal); `JaVaScRiPt:` and `DaTa:` defused; `metadata:`/`x-data:` correctly handled by `\b`.
- `<iframe>`/`<script>` → `&lt;…` (every `<` escaped).
- Callout smuggling: `> [!x]` escaped in sanitizeBody; and quoteBlock independently re-neutralizes (`\t> [!x]` → `> \t\> [!x]`) — defense in depth confirmed on the composition path.
- Callout exit: blank/CR-split lines all receive `> ` prefix; no unprefixed line produced.

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| aposd-designing-deep-modules | Interface depth (few methods hiding substantial logic) | PASS | sanitizeBody/quoteBlock: 2 pure entry points hiding multi-pass rune-level defense; buildVaultModel: 1 total entry point hiding dedupe/collapse/join/ref-assignment. |
| aposd-designing-deep-modules | No shallow modules / no information leakage | PASS | Sanitization knowledge centralized as the renderers' barricade (documented vaultmodel.go:10-14); model carries raw text, no duplicated escaping logic across modules. |
| aposd-designing-deep-modules | No silent failure (surface failure states) | PASS | Failure modes are documented and observable: empty-id records skipped, absent source → quote-less claim (SourceEventID field), ghost flag exposed — not hidden. |
| cc-defensive-programming | External input validated at entry / barricade design | PASS | Export data treated as external; barricade applied where text enters markdown (sanitizeBody/quoteBlock/sanitizeFilename/cleanInline); control chars dropped, schemes defused, empty ids rejected. |
| cc-defensive-programming | Defense in depth on security path | PASS | quoteBlock does not assume sanitizeBody ran (sanitize.go:12-14); standalone quoteBlock still blocks forge/exit — confirmed by my standalone harness. |
| cc-defensive-programming | No empty catch / no silent bug-swallowing | PASS | Skips are explicit and documented, not swallowed errors; no empty error handling. |

## Notes (non-blocking)
- Barricade placement is deliberate: VaultModel stores RAW untrusted `Body`/`Name`/`Statement` and relies on Phase 3+ renderers to apply sanitizeBody/quoteBlock/sanitizeFilename/cleanInline at the markdown/filesystem boundary. This is correct per the documented design, but it means a future renderer that emits any of these fields WITHOUT the barricade would reach the vault raw. Downstream phase reviews must confirm every raw-field emission passes through the barricade. Not a Phase 2 defect.
- `escapeLineStart` measures indent with `TrimLeft(line, " ")` (spaces only) while `sanitizeInline` preserves tabs, so a tab-led `\t---` / `\t> [!` passes sanitizeBody unescaped. This is safe: a leading tab is a CommonMark indented-code-block (≥4 cols), rendering the construct inert; my independent Obsidian-semantics detectors (≤3-space rule) agree, and the composition path's quoteBlock re-neutralizes the callout case. Observation only.

**Verdict: PASS**

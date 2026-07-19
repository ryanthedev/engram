# Review: Phase 2 - vault model, refs, sanitizer (r2, security sample 1)

## Executed Results (Step 0)
- Test suite: `go test ./internal/cli/ -count=1` → PASS (`ok ... 0.030s`)
- Typecheck/build: `go vet ./internal/cli/` → clean (no diagnostics)
- Lint/format: `gofmt -l internal/cli/` → clean (no files listed)
- Coverage: `go test ./internal/cli/ -cover` → 60.4% package-wide; **every Phase 2 function in sanitize.go and vaultmodel.go = 100.0%** per `go tool cover -func` (sanitizeBody, quoteBlock, normalizeNewlines, sanitizeInline, escapeLineStart, calloutForgeLeading, buildVaultModel, dedupeEvents, episodicBefore, compareTimePtr, collapseEntities, normalizeConceptName, assembleConcepts, addNeighbor, pickSourceEvent, assembleEvents, eventTitle, buildVaultRefs, sortedKeys). The sub-100% package number is pre-existing export.go code outside this phase's scope.
- Adversarial barricade harness (in-package scratch test, ~45 constructed attack inputs, removed after run): **PASS** — no live wikilink, scheme, raw tag, fence, frontmatter, quote-exit, or active callout survived.

## Requirement Fulfillment

### DW-2.1
PREMISE:  `sanitizeBody` neutralizes leading `---`, callout forge, `[[`/`![[`, fence breakout, raw HTML tags, and dangerous URI schemes (obsidian://, data:, javascript:), leaving benign prose intact except neutralized tokens.
EVIDENCE: sanitize.go:50-57 (orchestration), :90-120 (sanitizeInline: control-drop, `<`→`&lt;`, bracket adjacency, scheme defusal), :125-138 (escapeLineStart: `---`/```` ``` ````/`~~~`/callout), :28/:34 (patterns).
TRACE:    `---\n> [!danger] a\n[[link]]\n<script>x</script>\njavascript:go()` → per-line: `\---`; `\> [!danger] a`; `[\[link]]`; `&lt;script>x&lt;/script>`; `javascript :go()`. Benign `Alice met Bob...` round-trips byte-identical (test :92-98). All six hazard detectors report zero survivors on the full corpus (test :38-90) plus my independent adversarial sweep.
VERDICT:  PASS

### DW-2.2
PREMISE:  `quoteBlock` prefixes every line incl. blank lines with `> `; adversarial multi-line input cannot exit or forge a callout.
EVIDENCE: sanitize.go:64-75. Every line gets `"> " + line`; lines whose trimmed form starts with `[!` or matches `calloutForgeLeading` are backslash-escaped before prefixing.
TRACE:    `""` → `"> "` (blank prefixed, cannot exit). `[!danger] pwned` → `> \[!danger] pwned` (forge defused). `> [!x] nested` → escaped to `> \> [!x] nested`. Harness ran `""`, `\n\n`, nbsp-lead, CR-smuggled, and quoted-callout inputs: every output line starts `> ` and none matches `^(>[ \t]*)+\[!`. Tests :117-167 confirm.
VERDICT:  PASS

### DW-2.3
PREMISE:  hub rule `Ghost = Degree < 2`; normalized-name-equal entities collapse; distinct-named entities sharing a source_id do NOT; duplicate event_ids dedupe deterministically.
EVIDENCE: vaultmodel.go:290 (`Ghost: degree < hubMinDegree`, const=2 at :31), :171-235 (collapse on exact normalized-name key; empty-name entities key on private `\x00id:` space so they never fuse), :243-294 (degree = distinct canonical neighbors; self-loop/unknown-endpoint excluded at :263-269), :106-135 (dedupe: earliest OccurredAt→Kind→Text, nil last).
TRACE:    Degree: e-a with edges to e-b, e-c, dup(e-b), self-loop, unknown e-x → Degree=2, Ghost=false; e-b/e-c Degree=1 Ghost=true (test :36-69). Collapse: `  "alice   SMITH" ` and `Alice Smith` → one concept, canonical=smallest id e-1 (test :71-108). Shared source_id, distinct names alpha/beta → 2 concepts (test :132-141). Two empty-normalized names → 2 concepts (test :143-152). Dedupe earliest-wins, order-independent, tie-break chain all covered (test :154-194).
VERDICT:  PASS

### DW-2.4
PREMISE:  `buildVaultModel` joins claims via `source_ids → event_id` (absent event → quote-less claim); output + `VaultRefs` byte-identical across runs.
EVIDENCE: vaultmodel.go:308-325 (pickSourceEvent: smallest source id with an exported Event, else smallest id, else "" — total join), :254-259 (claim built for every edge), :90-97 (assembly), :374-444 (buildVaultRefs deterministic: sorted (id,folder), sorted-key map drains, no wall clock).
TRACE:    Edge with SourceIDs `[ev-0,ev-1]`, only ev-1 exported → SourceEventID=`ev-1`; SourceIDs `[ev-9]` (absent) → SourceEventID=`ev-9` but no Event exists → quote-less; no SourceIDs → `""`; all three claims survive (test :196-243). Determinism: two runs and reversed-input runs produce byte-identical JSON of model+refs (test :245-288).
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have automated tests that ran in Step 0 (TestDW_2_1_*, TestDW_2_2_*, TestDW_2_3_*, TestDW_2_4_*), names reference DW ids.
- [x] Coverage matches the stated 100% level: every Phase 2 function in both files is at 100.0% statement coverage (verified via `go tool cover -func`).
- No gaps.

## Dead Code
None found. No unused imports, unreachable code, debug prints, or commented-out blocks in the reviewed files. `go vet` and `gofmt -l` clean. (`noteRef` was intentionally lifted to vaultmodel.go:39 and is consumed by export.go:211/234/257/333 — not dead.)

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | Pure, stateless transforms; no shared state, goroutines, or async. |
| Error Handling | PASS | External export data treated as untrusted: empty-id episodics/entities skipped (vaultmodel.go:109,174), join is total (absent events degrade to quote-less, never crash). Adversarial prose neutralized, never rejected/dropped. No panics reachable on constructed hostile input. |
| Resources | N/A | No file handles, connections, locks, or caches opened in this phase. |
| Boundaries | PASS | Empty string, bracket runs of length 1..8, all-control-char lines, rune-cap at 80 (eventTitle :360), nil maps (sortedKeys :447), nil slices to buildVaultModel — all traced/tested without failure. |
| Security | PASS | Primary focus. Attacked sanitizeBody with `[[[[`, NUL/DEL/other `<0x20` and `\x7f` between brackets, tab-between-brackets, backslash+bracket, NUL-split schemes (`java\x00script:`), mixed-case schemes, CR dodges, `<<script>>`, U+2028 line-sep. Bracket scanner tracks last-**emitted** rune (:88-89) so dropped control chars cannot let `[`+ctrl+`[` recombine; scheme regex runs **after** the control-drop pass so `java\x00script:` collapses to `javascript:` then defuses to `javascript :`. No live `[[`, `![[`, scheme, or `<` survived any input. Composition `quoteBlock(sanitizeBody(x))` holds both contracts (defense in depth — `[!`-lead smuggled past sanitizeBody, since bare `[!` without `>` is not a callout, is caught by quoteBlock's own `[!` check before it could be activated by the `> ` prefix). |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| aposd-designing-deep-modules | Deep interface (small surface hiding substantial logic) | PASS | sanitizeBody/quoteBlock are string→string; buildVaultModel is a single call returning (VaultModel, VaultRefs). Hazard-neutralization and collapse/join/dedupe/ref-assignment complexity all hidden behind minimal signatures. |
| aposd-designing-deep-modules | No information leakage / no silent failure | PASS | Skip-on-empty-id is documented graceful degradation, not a hidden failure of a promised result; the model's raw-text-plus-barricade-at-render contract is stated explicitly (vaultmodel.go:10-14). No demonstrable leakage of internal representation across the boundary. |
| cc-defensive-programming | External input validated/neutralized at entry | PASS | Export records and ingested prose treated as external; validated (empty-id skip) and neutralized (sanitizeBody) at the boundary. |
| cc-defensive-programming | No empty catch / no side-effecting assertion | PASS | Go, no exceptions; no `panic`/assert used as control flow; total functions. |
| cc-defensive-programming | Barricade design consistency | PASS | Sanitization placed at the render boundary; model layer holds raw text and documents the barricade (vaultmodel.go:10-14). quoteBlock does not assume sanitizeBody ran (defense in depth, sanitize.go:12-14) — verified by composition test and my harness. |

## Notes (non-blocking)
1. `normalizeNewlines` maps only CR/CRLF to LF; Unicode line separators U+2028/U+2029 are not split. This is correct — CommonMark/Obsidian do not treat U+2028/U+2029 as line boundaries, so a `---` or `[!` after one stays mid-line and inert. No action needed; noted for completeness.
2. A non-breaking space (U+00A0) before `[!` is not stripped by quoteBlock's `TrimLeft(" \t")`, so ` [!x]` renders as `>  [!x]`. I could not demonstrate Obsidian activates a callout when a non-breaking space precedes `[!` (its callout detector keys on ASCII-space layout), so this is not a demonstrated defect — recorded as a suspicion only.
3. Package-wide coverage is 60.4% because export.go (prior phase) is partially covered; this is outside Phase 2 scope and every Phase 2 function is at 100%.

## Issues (if FAIL)
None.

**Verdict: PASS**

# Review: Phase 2 - vault model, refs, sanitizer (security sample 2)

## Executed Results (Step 0)
- Test suite: `go test ./internal/cli/ -count=1` → PASS (`ok ... 0.029s`)
- Typecheck / vet: `go vet ./internal/cli/` → clean (no output)
- Coverage (statement, package): `59.0%` overall; functions under review: sanitizeBody 100%, quoteBlock 100%, sanitizeInline 81.2%, buildVaultModel 100%, dedupeEvents 100%, collapseEntities 97.4%, assembleConcepts 100%, buildVaultRefs 87.0%
- Adversarial harness (constructed independently, /tmp/rev2-p2): the standing corpus + my compose/standalone probes all passed EXCEPT the control-char bracket-recombination probe, which **failed** (see DW-2.1 / Security).

## Requirement Fulfillment

### DW-2.1
PREMISE:  `sanitizeBody` neutralizes leading `---`, callout forge, `[[`/`![[`, fence breakout, raw HTML tags, and dangerous URI schemes (obsidian://, data:, javascript:) on an adversarial corpus, leaving benign prose intact except neutralized tokens.
EVIDENCE: sanitize.go:50-116 (sanitizeInline bracket scan at :103-113; control-char drop at :97-99)
TRACE:    input `"[\x00[link]]"` → sanitizeInline: `[` emits `[` sets prevBracket=true → `\x00` matches `r < 0x20`, emits nothing but **sets prevBracket=false** → next `[` sees prevBracket=false, emits bare `[` → output `"[[link]]"`, a live Obsidian wikilink. Confirmed by executed probe (see Issues).
VERDICT:  FAIL

### DW-2.2
PREMISE:  `quoteBlock` prefixes every line incl. blank lines with `> `; adversarial multi-line input (blank lines, `[!x]`-leading lines) cannot exit or forge a callout.
EVIDENCE: sanitize.go:64-75; tests TestDW_2_2_QuoteBlockPrefixesEveryLine, TestDW_2_2_QuoteBlockCannotForgeOrExitCallout, TestQuoteBlockOfSanitizedBodyComposes (all ran, PASS)
TRACE:    every line → `strings.Split` → each gets `"> "` prefix (:72); `[!`-leading or `>...[!` lines backslash-escaped before prefixing (:69-71). Blank line → `"> "`. My standalone + compose probes (nbsp lead, tab lead, CRLF-smuggled lines, `>[!x]tight`, blank-then-callout) produced no line lacking `"> "` and no forge match.
VERDICT:  PASS

### DW-2.3
PREMISE:  hub rule `Ghost = Degree < 2`; normalized-name-equal entities collapse; distinct-named entities sharing a source_id do NOT; duplicate event_ids dedupe deterministically.
EVIDENCE: vaultmodel.go:290 (`Ghost: degree < hubMinDegree`, hubMinDegree=2 at :31); collapse by normalizeConceptName at :171-235; dedupe at :106-135. Tests TestDW_2_3_HubRuleGhostBelowDegree2, _NormalizedNameCollapse, _SharedSourceIDDoesNotMerge, _EmptyNamesNeverCollapseTogether, _DuplicateEventIDsDedupeDeterministically (all ran, PASS).
TRACE:    e-a with 2 distinct neighbors (dup pair, self-loop, unknown endpoint excluded) → Degree=2, Ghost=false; e-b/e-c Degree=1 Ghost=true. Collapse: `"alice SMITH"` variant and `"Alice Smith"` share normalized key → 1 concept, canonical=smallest id. `alpha`/`beta` sharing ev-1 → 2 concepts. Dup ev-1 → earliest OccurredAt wins, order-independent (I ran the permutation cases).
VERDICT:  PASS

### DW-2.4
PREMISE:  `buildVaultModel` joins claims via `source_ids → event_id` (absent event → quote-less claim); output + `VaultRefs` byte-identical across runs.
EVIDENCE: pickSourceEvent vaultmodel.go:308-325; join in assembleConcepts :243-294; determinism via sortedKeys/sort.Slice throughout; buildVaultRefs sorted by (id,folder) :410-415. Tests TestDW_2_4_ClaimJoinSourceIDsToEventID, TestDW_2_4_ByteIdenticalAcrossRunsAndPermutations (ran, PASS).
TRACE:    edge with SourceIDs `[ev-0,ev-1]`, only ev-1 exported → smallest-with-event = ev-1; `[ev-9]` (none exported) → ev-9 kept, renders quote-less; `[]` → "". Byte-identical: two runs and reversed-input runs marshal to identical JSON (executed).
VERDICT:  PASS

**All requirements met:** NO (DW-2.1 fails)

## Test-DW Coverage
- [x] All four DW items have dedicated tests that ran in Step 0.
- [ ] DW-2.1's corpus is **incomplete**: it tests contiguous bracket runs (`[[[[`, `a[[b[[[c`) but never a control-character-separated bracket pair. The dropped-control-char recombination path is unexercised — which is why the bug ships green. Coverage of sanitizeInline is 81.2%, and the uncovered branch is exactly the drop-then-reset interaction.
- Coverage level "100%": DW-to-test mapping is complete in intent, but the DW-2.1 test misses the demonstrated bypass class; statement coverage on the security-critical inline scanner is 81.2%, not 100%.

## Dead Code
None found. No unreachable code, no debug statements, no commented-out blocks in the reviewed files.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | Pure, deterministic, single-threaded transforms; no shared state or goroutines. |
| Error Handling | PASS | buildVaultModel total over input: empty-id records skipped (:109, :174), unknown endpoints degrade to quote-less, no panics; validated at the barricade. |
| Resources | N/A | No files, handles, locks, or connections in scope. |
| Boundaries | FAIL | Control-char-interleaved brackets recombine into an active `[[`/`![[` — demonstrated below. (Contiguous runs, empty input, 4-space indent, CR/CRLF all handled correctly.) |
| Security | FAIL | Wikilink/transclusion neutralization bypass on untrusted ingested prose containing control bytes (NUL/SOH/DEL). This is the injection barricade; a live `[[link]]` survives. |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | External input validated at entry (barricade) | FAIL | sanitizeBody is the trust boundary for untrusted ingested prose; it drops control chars but the drop breaks its own bracket-adjacency invariant, letting a hazard through the barricade (sanitize.go:97-113). |
| cc-defensive-programming | No side-effecting assertions / no empty catch | PASS | Go; no assertions compiled out, no swallowed errors. |
| cc-defensive-programming | Defense in depth on security path | PASS (partial) | quoteBlock is safe standalone and re-guards callouts independently; compose path holds for callouts/HTML/schemes. The bracket-adjacency invariant is the one link that fails. |
| aposd-designing-deep-modules | Deep module, information hiding | PASS | sanitizeBody/quoteBlock/buildVaultModel present small interfaces hiding markdown-hazard and assembly complexity; no shallow-module or information-leakage defect demonstrable. |
| aposd-designing-deep-modules | No silent failure | FAIL (as a defect, not silence) | The module does not signal failure, but it also does not neutralize the hazard — it returns text that reads as clean but carries a live wikilink. Surfaced as the DW-2.1 defect below. |

## Notes (non-blocking)
- The scheme reassembly analogue is **safe**: `"javas\x00cript:"` → the control char is dropped, then `schemePattern` runs on the reassembled string (:115) and catches `javascript:` → `javascript :`. Line-start forgery (`"-\x00--"` → `"---"`) is also safe because escapeLineStart runs after sanitizeInline on the reassembled line. Only the in-line bracket scan tracks per-input-rune state and is corrupted by a dropped rune.
- nbsp-before-`[!` inside a quote (`" [!x]"`) is not escaped by quoteBlock (TrimLeft only trims `" \t"`), but the result `">  [!x]"` is not a valid Obsidian callout (parser requires `[!` after ASCII space/tab only) and is not matched by the forge regex — not a demonstrable bypass, left as an observation.
- Claim sort ties break on (ValidAt, EdgeID); truly duplicate EdgeIDs would be order-unstable, but edge ids are unique in practice and duplicate-edge dedupe is not a Phase-2 DW item.

## Issues (FAIL)
1. Control-character-interleaved brackets recombine into an active wikilink/transclusion, defeating sanitizeBody's `[[`/`![[` neutralization.
   - File: internal/cli/sanitize.go:97-99 (control-char drop branch sets `prevBracket = false`) interacting with :103-113 (bracket scan).
   - Demonstrated by: executed probe `TestProbe_ControlCharBracketRecombine` (in /tmp/rev2-p2, run against the real package):
     - `sanitizeBody("[\x00[link]]")` = `"[[link]]"` — live Obsidian internal link
     - `sanitizeBody("![\x00[img]]")` = `"![[img]]"` — live transclusion
     - `sanitizeBody("a[\x7f[b")` = `"a[[b"`, `sanitizeBody("[\x01[embed")` = `"[[embed"`
   - Root cause: `prevBracket` is meant to track "was the last **emitted** rune a `[`", but a dropped control char emits nothing yet still executes `prevBracket = false`, so the following `[` is treated as non-adjacent and left un-escaped even though it is adjacent in the output.
   - Fix: in the `r < 0x20 || r == 0x7f` branch, drop the rune **without** modifying `prevBracket` (e.g. `continue` before the `prevBracket = false` assignment, leaving the flag unchanged so a preceding `[` still guards the next one). Then add a control-char-separated bracket case to the DW-2.1 corpus.

**Verdict: FAIL — DW-2.1 / Boundaries / Security: control-char-interleaved `[[`/`![[` recombine into live Obsidian links, bypassing the injection barricade (sanitize.go:97-113).**

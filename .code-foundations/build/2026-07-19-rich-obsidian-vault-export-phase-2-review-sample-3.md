# Review: Phase 2 - vault model, refs, sanitizer (security sample 3)

## Executed Results (Step 0)
- Test suite: `go test ./internal/cli/ -count=1` → **ok** (all pass); re-run with `-race -count=1` on the DW subset → **ok** (1.0s)
- Typecheck/build: `go build ./internal/cli/` → **BUILD OK**; `go vet ./internal/cli/` → clean
- Coverage: `go test -covermode=set` per-func → sanitizeBody/quoteBlock/normalizeNewlines 100%, sanitizeInline **81.2%**, escapeLineStart 100%, buildVaultModel/dedupeEvents/assembleConcepts/assembleEvents/eventTitle/pickSourceEvent 100%, episodicBefore **80%**, collapseEntities **97.4%**, buildVaultRefs **87.0%**
- Independent adversarial harness: `/tmp/rev3-p2` (standalone copy of sanitize.go + `main.go`), `go run .` — no injection bypass found

## Requirement Fulfillment

### DW-2.1
PREMISE:  `sanitizeBody` neutralizes leading `---`, callout forge, `[[`/`![[`, fence breakout, raw HTML tags, and dangerous URI schemes (obsidian://, data:, javascript:) on an adversarial corpus, leaving benign prose intact except neutralized tokens.
EVIDENCE: internal/cli/sanitize.go:50-116 (sanitizeBody → sanitizeInline + escapeLineStart); test TestDW_2_1_SanitizeBodyAdversarialCorpus (sanitize_test.go:38), _BenignProseIntact (:87), _NoRecombination (:95)
TRACE:    `---`/```` ``` ````/`~~~`/`> [!` at indent ≤3 → escapeLineStart prepends `\` (:127-132); `<` → `&lt;` (:100-101); adjacent `[` → `\[` via prevBracket scan (:103-109); `(?i)\b(obsidian|javascript|data):` → `${1} :` (:28,:115). My harness confirmed: `<iframe>`→`&lt;iframe`, `[[[[`→`[\[\[\[` (zero adjacent `[[`), `javascript:`/`obsidian://`/`DaTa:` all defused, `metadata:` untouched, benign prose byte-identical.
VERDICT:  PASS

### DW-2.2
PREMISE:  `quoteBlock` prefixes every line incl. blank lines with `> `; adversarial multi-line input (blank lines, `[!x]`-leading lines) cannot exit or forge a callout.
EVIDENCE: internal/cli/sanitize.go:64-75; test TestDW_2_2_QuoteBlockPrefixesEveryLine (:106), _CannotForgeOrExitCallout (:126), TestQuoteBlockOfSanitizedBodyComposes (:158)
TRACE:    every split line gets `"> " + line` (:72) with no skip path → no exit; a `[!`-leading or `>...[!` line is `\`-escaped before prefixing (:69-71) → no forge. Harness: `[!danger] pwned`→`> \[!danger] pwned`; `"a\n\n\nb"`→ all four lines `> `-prefixed, blanks `"> "`; `quoteBlock("")`→`"> "`. quoteBlock is safe standalone (does not assume sanitizeBody ran).
VERDICT:  PASS

### DW-2.3
PREMISE:  hub rule `Ghost = Degree < 2`; normalized-name-equal entities collapse; distinct-named entities sharing a source_id do NOT; duplicate event_ids dedupe deterministically.
EVIDENCE: hubMinDegree=2 (vaultmodel.go:31), Ghost=degree<hubMinDegree (:290); collapse key = normalizeConceptName (:177,:230); dedupeEvents (:106); tests TestDW_2_3_HubRuleGhostBelowDegree2 (:36), _NormalizedNameCollapse (:71), _SharedSourceIDDoesNotMerge (:132), _EmptyNamesNeverCollapseTogether (:143), _DuplicateEventIDsDedupeDeterministically (:154)
TRACE:    Degree = distinct canonical neighbors (self-loop excluded via from!=to :266, unknown endpoint via fromOK/toOK :260-269) → alpha with dup-pair+self-loop+unknown-endpoint = Degree 2, not ghost; e-b/e-c Degree 1 → ghost. Collapse keys ONLY on name → distinct names sharing `ev-1` stay 2 concepts. dedupe keeps 1/id, tie-break earliest OccurredAt(nil last)→Kind→Text (:127-135), order-independent (sorts result :121).
VERDICT:  PASS

### DW-2.4
PREMISE:  `buildVaultModel` joins claims via `source_ids → event_id` (absent event → quote-less claim); output + `VaultRefs` byte-identical across runs.
EVIDENCE: pickSourceEvent (vaultmodel.go:308-325), buildVaultRefs (:374-444); tests TestDW_2_4_ClaimJoinSourceIDsToEventID (:192), _ByteIdenticalAcrossRunsAndPermutations (:241)
TRACE:    pickSourceEvent → smallest source id with an exported Event, else smallest source id (dangling → renderer emits no quote), else "" → join is total, claim never dropped. Determinism: every map drained via sortedKeys/sorted slices, times via .UTC(), refs assigned over sorted (id,folder) cands; test marshals model+refs and asserts identical across two runs AND across reversed input slices. Passed under `-race`.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] All 4 DW items have corresponding tests that ran in Step 0 (test names encode DW ids)
- [x] All 4 prompt-listed edge cases have execution evidence (see below)
- [ ] **Test coverage does NOT match the stated 100% level** — 6 defensive/pathological branches are uncovered by the repo suite (all behave correctly; verified by inspection and/or my harness, none tied to an unmet DW item or unhandled listed edge case):
  | Uncovered | Lines | What | Verified-correct by |
  |---|---|---|---|
  | sanitizeInline control-char drop | 97-99 | `r<0x20 \|\| 0x7f` dropped | harness: `\v`/`\f` dropped, no line split |
  | sanitizeInline tab branch | 94-96 | tab preserved | harness: tab-in-scheme / tab-indent |
  | episodicBefore Kind tiebreak | 131-133 | `a.Kind<b.Kind` | inspection (deterministic) |
  | collapseEntities empty-id skip | 174-175 | `e.ID==""` continue | inspection (mirror of tested event skip) |
  | buildVaultRefs event forced-slug | 389-391 | title→"" fallback | inspection (unreachable w/ id fallback) |
  | buildVaultRefs residual-clash + cross-kind dup-id | 414,419-434 | suffix-extension loop | inspection (terminates, deterministic) |

Listed edge cases → evidence:
| Edge case | Evidence |
|---|---|
| claim w/ absent source event → no quote | TestDW_2_4 ed2 (ev-9 absent, kept, no Event ev-9) |
| equal normalized names → one concept; distinct names sharing source_id → NOT merged | TestDW_2_3_NormalizedNameCollapse + _SharedSourceIDDoesNotMerge |
| duplicate event_id → one Event, deterministic | TestDW_2_3_DuplicateEventIDsDedupeDeterministically |
| literal `---`/`> [!`/`<iframe>`/fence/`[!danger]`-lead → neutralized yet legible | TestDW_2_1 corpus + TestDW_2_2 + my harness |

## Dead Code
None found. The `noteRef` type is lifted to vaultmodel.go:39-43 and referenced (not re-declared) from export.go:53; `go build` and `go vet` are clean.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | Pure, deterministic, stateless transforms; no shared state, goroutines, or I/O. Passed under `-race`. |
| Error Handling | PASS | Total functions: empty ids skipped, missing edge endpoints/source events degrade to quote-less, no panics on nil/malformed export input (tests pass nil slices). |
| Resources | N/A | No files, handles, locks, or connections opened. |
| Boundaries | PASS | Bracket run `[[[[` cannot recombine (prevBracket scan, harness-confirmed); rune-capped titles/filenames; nil maps → empty via sortedKeys; indent 0-3 vs 4+ handled; tab always pushes indent ≥4 (inert code) so space-only trim in escapeLineStart is sound. |
| Security | PASS | This is the injection barricade. I constructed adversarial inputs to smuggle a callout past sanitizeBody then activate via quoteBlock, exit the callout via blank/unprefixed lines, recombine `[[`, reach obsidian://data:/javascript: through `[label](dest)`, and embed raw HTML — **every attempt neutralized**. Bare `[!` (no `>`) is correctly left by sanitizeBody (not a callout without `>`) and escaped by quoteBlock when `>` is prepended (defense in depth). No line-separator bypass (U+2028/U+0085/`\v`/`\f` are not CommonMark line breaks; controls dropped). Angle-bracket link destinations impossible (`<`→`&lt;`), so the tab-in-scheme XSS trick yields no live link. |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| aposd-designing-deep-modules | Deep interface (few methods, much hidden) | PASS | `sanitizeBody(string)→string`, `quoteBlock(string)→string`, `buildVaultModel(3 slices)→(model,refs)` — single-purpose, common case trivial, substantial logic hidden. |
| aposd-designing-deep-modules | No information leakage / centralized knowledge | PASS | All body sanitization lives in sanitize.go; the trust boundary is documented once (vaultmodel.go header: "sanitization is the renderers' barricade"). No shallow/pass-through modules, no classitis. |
| aposd-designing-deep-modules | No silent failure | PASS | Degradations (absent event, unknown endpoint) are observable in the model (quote-less claim, ghost) rather than hidden. |
| cc-defensive-programming | External input validated at entry | PASS | Export data + ingested prose treated as external; empty ids skipped, joins total, prose neutralized at the barricade before entering markdown/FS paths. |
| cc-defensive-programming | Barricade + defense-in-depth on security path | PASS | quoteBlock does not assume sanitizeBody ran and re-escapes callout markers itself; harness confirmed each primitive safe standalone. |
| cc-defensive-programming | No side-effecting assertions / no empty catch | N/A | Go, no assertions or exception handlers in scope. |

## Notes (non-blocking)
1. **Coverage shortfall vs stated 100%** (detailed above). All uncovered lines are defensive/pathological branches that behave correctly; none leaves a DW item or listed edge case without execution evidence. Recommend adding tests for the control-char-drop path (security-relevant on the barricade), the empty-id-entity skip, and the buildVaultRefs residual-clash loop to literally meet the 100% bar.
2. Tab-indented line-start hazards (`\t---`, `\t> [!x]`) are deliberately left unescaped by escapeLineStart (space-only trim). This is correct — a leading tab expands to a ≥4-column indent, making the line an inert indented code block — but it rests on the renderer's CommonMark tab=4 rule; worth a one-line comment.
3. Duplicate edge ids would make claim ordering (sort by ValidAt then EdgeID) order-dependent under input permutation. Unlisted edge case; export ids are expected unique. No demonstrated defect.

## Issues (if FAIL)
None.

**Verdict: PASS**

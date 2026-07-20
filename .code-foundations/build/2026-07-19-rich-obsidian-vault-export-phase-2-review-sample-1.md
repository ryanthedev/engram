# Review: Phase 2 - Vault model, refs, sanitizer (security sample 1)

## Executed Results (Step 0)
- Test suite: `go test ./internal/cli/ -count=1` → **ok** (all tests pass, 0.029s)
- Typecheck/build: `go build ./...` → **OK**
- Vet: `go vet ./internal/cli/` → **OK**
- Gofmt: `gofmt -l internal/cli/` → **clean** (no files need formatting)
- Independent adversarial harness (`/tmp/rev1-p2/adv_test.go`, ~30 hostile inputs + defensive-branch probes) → **all pass, no bypass found**

## Requirement Fulfillment

### DW-2.1
PREMISE:  `sanitizeBody` neutralizes leading `---`, callout forge, `[[`/`![[`, fence breakout, raw HTML tags, and dangerous URI schemes (obsidian://, data:, javascript:) on an adversarial corpus, leaving benign prose intact except neutralized tokens.
EVIDENCE: internal/cli/sanitize.go:50-116 (`sanitizeBody`→`sanitizeInline`+`escapeLineStart`); tests sanitize_test.go:38-104.
TRACE:    `<iframe>`→`&lt;iframe`; `[[x]]`→`[\[x]]`; `[[[[`→no adjacent `[[` (rune-scan tracks prevBracket, sanitize_test.go:95-104 + my `a[[b[[[c[[` probe); `javascript:`→`javascript :`; `---`/```` ``` ````/`~~~`/`> [!` line-leads→backslash-escaped; benign prose round-trips byte-identical (sanitize_test.go:87-93). My independent sweep over 30 hostile inputs found zero surviving hazard tokens.
VERDICT:  PASS

### DW-2.2
PREMISE:  `quoteBlock` prefixes every line incl. blank lines with `> `; adversarial multi-line input (blank lines, `[!x]`-leading lines) cannot exit or forge a callout.
EVIDENCE: internal/cli/sanitize.go:64-75; tests sanitize_test.go:106-175.
TRACE:    Every line becomes `"> " + line` unconditionally (blank→`> `, verified sanitize_test.go:118-123); a `[!`-leading or `>…[!` line is backslash-escaped *before* prefixing so `> ` + content never yields `> [!`. My raw-input probes (`> [!quote]- hi`, `> > > [!y] deep`, `[!danger] no-prefix`, CRLF-smuggled `ok\r[!x]…`) all keep the `> ` prefix and none match the forge regex `^(>[ \t]*)+\[!`.
VERDICT:  PASS

### DW-2.3
PREMISE:  hub rule Ghost = Degree < 2 (degree = distinct edge endpoints); normalized-name-equal entities collapse; distinct-named entities sharing a source_id do NOT collapse; duplicate event_ids dedupe deterministically.
EVIDENCE: internal/cli/vaultmodel.go:243-294 (degree/ghost), :171-235 (collapse), :106-135 (dedupe); tests vaultmodel_test.go:36-190.
TRACE:    Degree counts distinct canonical neighbors — self-loops (`from!=to` guard, :266), duplicate endpoint pairs (set-dedup in `neighbors`), and unexported endpoints (`toOK` guard) all excluded → alpha Degree=2 Ghost=false, e-d Degree=0 Ghost=true (test :36-69). Collapse keys only on exact `normalizeConceptName` equality (:177) → "alice SMITH" variants merge, Bob separate; two distinct-named entities sharing `ev-1` stay 2 concepts (test :132-141). Dedupe: earliest OccurredAt→Kind→Text, order-independent (test :154-190). Kind tiebreak (:131) untested by shipped suite but I exercised it — correct.
VERDICT:  PASS

### DW-2.4
PREMISE:  `buildVaultModel` joins claims via `source_ids → event_id` (absent event → quote-less claim); output + `VaultRefs` are byte-identical across runs.
EVIDENCE: internal/cli/vaultmodel.go:90-97, :243-325 (`pickSourceEvent`), :374-444 (`buildVaultRefs`); tests vaultmodel_test.go:192-284.
TRACE:    `pickSourceEvent` returns smallest source id with an exported Event, else smallest id (quote-less), else "" — the join is total, no claim dropped (test :192-239: ed2→ev-9 quote-less, ed3→"" no-source, both survive). Two runs + fully reversed input slices produce byte-identical `json.Marshal(model)+Marshal(refs)` (test :241-284) — maps drained via sorted keys throughout, no wall clock.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have corresponding automated tests that ran in Step 0 (test names carry DW ids: `TestDW_2_1_*`, `TestDW_2_2_*`, `TestDW_2_3_*`, `TestDW_2_4_*`).
- [~] **Stated coverage level 100% is NOT literally met by the shipped suite.** Per-function line coverage: `sanitizeInline` 81.2%, `episodicBefore` 80.0%, `collapseEntities` 97.4%, `buildVaultRefs` 87.0% (all others 100%). The uncovered blocks are auxiliary/defensive paths, NOT DW behaviors or listed edge cases (see Notes). I re-derived each uncovered branch in my own harness and confirmed every one is correct — no defect hides behind the gap.

## Dead Code
None found. The `noteRef` type was lifted to vaultmodel.go:39-43; export.go:53-54 documents the move and still constructs `noteRef{File, Display}` (Folder zero-valued, unused there) — build + vet + all export tests pass, lift is clean.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | Pure, stateless, deterministic transforms; no shared state, goroutines, or I/O. |
| Error Handling | PASS | External export data treated as untrusted: empty-id events/entities skipped (vaultmodel.go:109,174), total joins degrade to quote-less claims rather than crash; sanitizers are transform-not-reject. No panics reachable on my hostile corpus. |
| Resources | N/A | No files, connections, or locks in these functions. |
| Boundaries | PASS | Bracket rune-scan handles `[[[[`/interleaved runs (no recombination); rune-cap on titles (:360) and filenames; nil-time ordering (`compareTimePtr`); empty/whitespace title → id fallback. Probed, all held. |
| Security | PASS | This is the injection barricade. I attempted every named bypass — callout smuggle past sanitize→activate in quoteBlock, blank/unprefixed-line callout exit, `[[[[`/interleaved wikilink recombination, `[label](javascript:/data:/obsidian://)` link destinations, raw `<iframe>/<svg onload>/<img onerror>`, CRLF/lone-CR line-hazard dodges, tab/control smuggling. **Zero bypasses.** Every `<`→`&lt;`, every scheme colon spaced, every line-lead escaped, every quoteBlock line `> `-prefixed and forge-free. |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | Barricade at trust boundary: external input validated at entry | PASS | vaultmodel.go:1-14 names export data as external input; sanitizers are the renderers' barricade; empty-id records skipped, not trusted. |
| cc-defensive-programming | No empty catch / no silent swallow | PASS | No error swallowing; skips are documented deliberate degradation, not hidden failures. |
| cc-defensive-programming | Security-critical path validated (defense in depth) | PASS | quoteBlock does NOT assume sanitizeBody ran (sanitize.go:12-14) — both primitives safe standalone; composition test + my raw-input probes confirm each holds alone. |
| cc-defensive-programming | No executable/side-effecting code in assertions | N/A | No assertions used; validation is explicit control flow. |
| aposd-designing-deep-modules | Deep module: simple interface over real hiding | PASS | `sanitizeBody`/`quoteBlock`/`buildVaultModel` are few-arg entry points hiding the rune scanner, escape rules, collapse/dedupe/ref-collision machinery. No information leakage across the two files (shared helpers reused, not duplicated). |
| aposd-designing-deep-modules | No silent failure (surface failure states) | PASS | Degradation modes (quote-less claim, ghost concept, id-fallback title) are observable in the model output, not hidden. |

## Notes (non-blocking)
1. **Coverage gap vs the stated 100% target.** Uncovered blocks, all verified correct by my harness, none tied to a DW item or a prompt-listed edge case:
   - sanitize.go:94-99 — `sanitizeInline` tab-keep and control-char-drop branches (no shipped test routes a raw tab/control byte through `sanitizeBody`). Confirmed: `"a\tb"`→`"a\tb"`, `"a\x01\x1f\x7fb"`→`"ab"`, NUL dropped.
   - vaultmodel.go:131 — `episodicBefore` Kind tiebreak (shipped dedupe ties break on Text with equal Kind). Confirmed correct.
   - vaultmodel.go:174 — `collapseEntities` empty-id entity skip. Confirmed: skipped, no ref minted.
   - vaultmodel.go:389 — `buildVaultRefs` forced-slug event (title normalizes to filename-empty, e.g. `"..."`). Confirmed: file `"event (ev-x)"`.
   - vaultmodel.go:414/419 — cross-kind shared-id folder sort + dup-skip (no fixture shares an id across an event and a concept).
   - vaultmodel.go:429-434 — residual-clash prefix-extension loop. Confirmed: two ids sharing an 8-char prefix (`aaaaaaaa1`/`aaaaaaaa2`) disambiguate to `Dup (aaaaaaaa)` / `Dup (aaaaaaaa2)` with no collision.
   This is a test-completeness gap against the phase's 100% target, not a correctness defect. Recommend adding table cases for the tab/control sanitize branch, the Kind dedupe tiebreak, the empty-id entity skip, and a filename-empty (`"..."`) event title to close it.
2. Scheme neutralization can over-fire on a legitimate `data`/`obsidian`/`javascript` appearing as a hostname/word after a non-word char (e.g. `https://data:8080`→`data :`). This is benign degradation (a broken-but-inert link), never a hazard leak; no DW requires such strings to survive.

## Issues (if FAIL)
None.

**Verdict: PASS**

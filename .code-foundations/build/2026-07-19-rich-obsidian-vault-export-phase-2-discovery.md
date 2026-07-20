# Discovery + Design: Phase 2 - Client — vault model, refs, sanitizer + quoteBlock

## Files Found
- `internal/cli/export.go` — existing exporter: `noteRef{File,Display}`, `vaultFilenames` (two-pass collision-suffixed filename assignment), `sanitizeFilename`, `cleanInline`, `idPrefix`, `maxFilenameRunes`. All reusable helpers for this phase.
- `internal/cli/export_test.go` — white-box `package cli` tests; stdlib `testing`, table tests, DW-named test funcs. All `noteRef` usages are field-named literals (adding a field is safe).
- `internal/engramclient/export.go` — Phase 1 output confirmed: `ExportEpisodic{EventID, Kind, Text string; OccurredAt *time.Time; SourceIDs []string}`, `ExportEntity` (has `SourceIDs`, `CreatedAt`), `ExportEdge` (has `Statement`, `ValidAt *time.Time`, `SourceIDs`, `CreatedAt`).
- `internal/cli/vaultmodel.go`, `vaultmodel_test.go`, `sanitize.go`, `sanitize_test.go` — do NOT exist yet (all new).

## Current State
The exporter renders one note per entity directly from export records. No vault model layer, no body sanitizer (only `sanitizeFilename`/`cleanInline` for names/predicates), no quote wrapper. Phase 1 landed episodics on the client wire.

## Gaps
1. **Plan says Event dedupe is "earliest `CreatedAt` then id tie-break" — `ExportEpisodic` carries no `CreatedAt`** (the plan's own Phase 1 Produces line confirms the wire has only `OccurredAt`). The "id tie-break" is also degenerate: duplicates by definition share the same `event_id`, and no per-doc id is on the wire. DW-2.3 requires only that duplicates "dedupe deterministically", so this is implementable without a plan change: winner = earliest `OccurredAt` (nil sorts last), tie-break by `Kind` then `Text` (lexicographic). Order-independent and deterministic; honors the plan's earliest-record intent. Recorded as a deviation, not UPDATE_PLAN.
2. `noteRef` gains a `Folder` field and its declaration moves to `vaultmodel.go` — the plan's note explicitly permits lifting it out of `export.go`. `export.go`'s writer wiring is otherwise untouched (Phase 5's job).

## Code Standards
`docs/code-standards.md` applied: stdlib `testing` only (no testify), table tests with named cases + `t.Run`, DW-IDs in test names, errors wrapped `"export: verb-ing noun: %w"`-style, three import groups, no transport imports in `internal/cli` beyond `engramclient` (transport-free view). Package `cli` white-box tests match the existing `export_test.go` pattern.

## Test Infrastructure
`go test ./internal/cli/...`; existing tests build fixtures as plain `engramclient.Export*` structs — the new model tests do the same (pure functions, no server stub needed).

## Assumption Verification
| Assumption | Verdict | Evidence |
|---|---|---|
| Edge `ValidAt` populated & meaningful for claim ordering | CONFIRMED | `graph.Edge.ValidAt time.Time` is a non-pointer, always-set field (internal/graph/graph.go:113); server maps it unconditionally via `timestamppb.New(e.ValidAt)` (internal/server/export.go:250,267); client exposes `*time.Time`. Nil only if a hypothetical zero proto — sort handles nil-last defensively. |
| Degree ≥2 yields a non-empty hub set | PLAUSIBLE, cutoff kept swappable | Cannot query the live store from this phase (memory-only client work). Plan's own live measurement (~1.1k edges over ~1.8k entities, mean degree ≈1.2) implies a minority-but-nonempty degree≥2 set. Implemented as a single constant `hubMinDegree = 2` per the plan's fallback, so dropping to ≥1 is a one-line change. |
| ~1.8k entities / 1.1k edges / 249 events fit in client memory | CONFIRMED | Trivial: a few MB of structs; the existing exporter already accumulates all entities+edges in memory. |

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-2.1 | `sanitizeBody` neutralizes leading `---`, callout forge, `[[`/`![[`, fence breakout, raw HTML tags, dangerous URI schemes on an adversarial corpus (≥5:1 dirty), benign prose intact | COVERED | `TestDW_2_1_SanitizeBodyAdversarialCorpus` (table, ≥5:1 dirty:benign), `TestDW_2_1_SanitizeBodyBenignProseIntact`, `TestDW_2_1_SanitizeBodyNoRecombination` |
| DW-2.2 | `quoteBlock` prefixes every line incl. blank with `> `; adversarial input cannot exit or forge a callout | COVERED | `TestDW_2_2_QuoteBlockPrefixesEveryLine`, `TestDW_2_2_QuoteBlockCannotForgeOrExitCallout` |
| DW-2.3 | `Ghost = Degree < 2`; normalized-name-equal collapse; shared-source_id distinct names NOT merged; duplicate event_ids dedupe deterministically | COVERED | `TestDW_2_3_HubRuleGhostBelowDegree2`, `TestDW_2_3_NormalizedNameCollapse`, `TestDW_2_3_SharedSourceIDDoesNotMerge`, `TestDW_2_3_DuplicateEventIDsDedupeDeterministically` |
| DW-2.4 | claims join via `source_ids → event_id` (absent event → quote-less claim); output + `VaultRefs` byte-identical across runs | COVERED | `TestDW_2_4_ClaimJoinSourceIDsToEventID`, `TestDW_2_4_AbsentSourceEventQuoteless`, `TestDW_2_4_ByteIdenticalAcrossRunsAndPermutations` |

**All items COVERED:** YES

## Design Decisions

### Design: sanitizeBody / quoteBlock (aposd-designing-deep-modules)

**Approaches Considered**
1. **A — Regex pipeline:** ordered whole-text regex/ReplaceAll passes per hazard.
2. **B — Line-oriented single pass:** normalize newlines, then per line: rune-scan inline transforms (`<`→`&lt;`, break `[[` adjacency, drop control chars) + scheme regex + line-start hazard escaping.
3. **C — Markdown AST parse + re-render:** parse to CommonMark AST, drop unsafe nodes, re-serialize.

**Comparison**
| Criterion | A | B | C |
|-----------|---|---|---|
| Interface simplicity | same (1 func) | same (1 func) | same |
| Line-start context correctness | poor (multiline regex fragile: `(?m)` anchors vs CRLF, indent) | natural — each line sees its own start | good |
| Recombination safety (`[[[`→ re-formed `[[`) | needs fixed-point loops | single scan with prev-rune state, provably no adjacency | good |
| Determinism / legibility | ok | ok — pure string transform, transform-not-reject | re-render is lossy; heavy dep |

**Choice: B.** One deep function per primitive (`sanitizeBody(string) string`, `quoteBlock(string) string`), all hazard knowledge hidden inside; callers compose them with zero markdown-security knowledge. C rejected (dependency + lossy re-render breaks "legible, transform-not-reject"). A rejected (multiline anchors + recombination need ad-hoc loops).

**Neutralization mechanics (transform-not-reject, all deterministic):**
- CRLF/CR → LF; control chars (except `\t`) dropped.
- `<` → `&lt;` everywhere (kills all raw HTML incl. `<iframe>`; renders back as `<` in preview).
- `[[` adjacency broken by escaping the second bracket (`[\[`) via a prev-rune scan — `[[[[` → `[\[\[\[`, no re-formed adjacency; `![[` → `![\[` (transclusion needs `[[`, so one rule covers both).
- Schemes: `(?i)\b(obsidian|javascript|data):` → `${1} :` (space breaks scheme parsing in link destinations; `\b` keeps "metadata:" untouched).
- Line-start (after ≤3 leading spaces; 4+ spaces is already an inert indented code block): `---`, ``` ```/`~~~` (fence), and callout forge `^(>\s*)+\[!` get a `\` prefix — CommonMark backslash-escape renders the token literally and voids frontmatter/fence/callout semantics. Plain `> quote` lines stay untouched (benign).
- `quoteBlock`: normalize newlines, then per line neutralize `[!`-leading (escape `[`) and `(>\s*)+[!` forge (escape `>`), then prefix EVERY line (blank included) with `> `. Exit is impossible (no unprefixed line); nesting forge is impossible (both forge shapes escaped before prefixing). Safe standalone — does not assume sanitizeBody ran first (defense in depth per cc-defensive-programming: this is a security path, validate again).

### Design: vault model + refs

**Approaches Considered**
1. **A — Monolithic `buildVaultModel`:** one function does dedupe/collapse/join/degree/refs inline.
2. **B — One deep entry, staged private helpers:** `buildVaultModel` composes `dedupeEvents`, `collapseEntities`, claim/degree assembly, `buildVaultRefs` — each pure, each hiding one rule.
3. **C — Builder object with mutable state and methods.**

**Choice: B.** Public surface is exactly the plan's Produces (`buildVaultModel`, plus the two primitives) — deep interface, all rules (normalization, hub cutoff, dedupe ordering, collision suffixing) hidden as private helpers/constants. A is untestable at rule granularity; C adds state for no benefit in a pure pipeline.

**Rules pinned (constants/documented):**
- `hubMinDegree = 2`; `Ghost = Degree < hubMinDegree`. **Degree = number of distinct canonical neighbor concepts** (self-loops and edges to unknown/unexported endpoints don't count — an unlinkable endpoint can't make a note navigable, which is what a hub file is for). RelatedIDs = that same sorted neighbor set.
- Collapse key: `normalizeConceptName` = lowercase → collapse internal whitespace → strip surrounding punctuation/quotes → re-collapse. Empty normalized name never collapses (each such entity keys on its own id — merging all unnamed entities would be a false fuse). Canonical = min entity id in group; `Name` = canonical's name; `Aliases` = union of member aliases plus non-canonical member surface names, sorted/deduped.
- Claims: one `Claim{Statement, ValidAt, EdgeID, SourceEventID}` per edge, attached to **both** resolvable endpoint concepts (a statement is provenance for both its subject and object; attached once if endpoints collapse together). `SourceEventID` = smallest edge SourceID that has an exported Event, else smallest SourceID, else "" — join is total: an absent event leaves the id (or "") and Phase 3 renders the Statement quote-less. Sorted `ValidAt` asc (nil last), then `EdgeID`.
- Events: dedupe per the Gaps note; `Title` = first non-blank line of Text, `cleanInline`d, capped at 80 runes; empty → `idPrefix(eventID, 8)`. `Body` = raw text (sanitization is the renderer's barricade — model stays presentation-free). `ConceptIDs` = sorted canonical ids of concepts whose member-entity `SourceIDs` contain the event id (ghosts included — the footer may link them unresolved). Records with empty `EventID` are skipped (unjoinable, unlinkable — mirrors `vaultFilenames` skipping empty entity ids).
- `VaultRefs` (`type VaultRefs map[string]noteRef`; `noteRef` gains `Folder`): events → `Folder "events/YYYY"` (UTC) or `"events/undated"`, `File "YYYY-MM-DD <slug>"`; concepts (ghosts included, as link targets) → `Folder "concepts"`. Collision suffixing reuses the export.go two-pass shape, generalized: **global** case-insensitive homonym detection across events+concepts (Obsidian resolves `[[name]]` vault-wide), all homonyms suffixed with `(idPrefix)`, residual clashes extend the prefix — assignment in sorted (id, folder) order, order-independent. Pathological id shared between an event and a concept: first candidate in sorted order wins the map slot, second skipped (documented guard, cannot occur with real ULIDs).
- Determinism: every map is drained via sorted keys; all output slices sorted; no wall-clock reads; all times compared/formatted via `.UTC()`.

`vaultFilenames` in export.go stays untouched (its behavior is pinned by anchored Phase-3-era tests; Phase 5 deletes it when rewiring). The only export.go edit is moving the `noteRef` declaration.

### Defensive posture (cc-defensive-programming)
Trust boundary: episodic text, entity names, statements are hostile input. `sanitizeBody`/`quoteBlock` are the body barricade (correctness-leaning: transform, never emit an unneutralized token); `buildVaultModel` validates structurally (skips empty-id records rather than crashing; never panics on nil times or missing endpoints). No assertions on external data — all anticipated-bad input is handled. Errors: none of these functions can fail (pure transforms; skip rules are documented behavior, and Phase 5 owns dropped-count reporting) — so signatures stay error-free per the plan's Produces.

## Prerequisites
- [x] Phase 1 merged: `engramclient.ExportEpisodic` exists with the pinned fields
- [x] Reused helpers present (`sanitizeFilename`, `cleanInline`, `idPrefix`)
- [x] No new dependencies needed (stdlib + existing yaml/engramclient only)

## Recommendation
BUILD — implement `internal/cli/sanitize.go` + `internal/cli/vaultmodel.go` with white-box tests; one deviation (dedupe key uses `OccurredAt`/`Kind`/`Text` because `CreatedAt` is not on the episodic wire) documented above.

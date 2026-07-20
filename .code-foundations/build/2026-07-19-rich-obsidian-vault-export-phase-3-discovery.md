# Discovery + Design: Phase 3 - Client — event + concept note rendering

## Files Found
- `internal/cli/vaultmodel.go` — Phase 2 output. `VaultModel{Events []Event, Concepts []Concept}`, `VaultRefs map[string]noteRef{File,Display,Folder}`, `Claim{Statement,ValidAt,EdgeID,SourceEventID}`. `buildVaultRefs` already computes the FULL folder+file for every event (UTC-foldered, undated fallback, id-suffixed on empty slug/collision) and every concept (ghost included). `compareTimePtr` is package-level and reusable.
- `internal/cli/sanitize.go` — `sanitizeBody(string) string`, `quoteBlock(string) string`, both pure/deterministic, package `cli`.
- `internal/cli/export.go` — precedent for the frontmatter pattern (`yaml.MapSlice` + `yaml.Marshal`, safe for untrusted aliases — "never hand-escaped"), `cleanInline`, `sanitizeFilename`, wikilink bullet style `[[File|Display]]`.
- `internal/cli/vaultnotes.go`, `internal/cli/vaultnotes_test.go` — do not exist yet; this phase creates them.
- `internal/cli/sanitize_test.go` — defines `hazardDetectors`/`assertNoHazards` (package-level, reusable from `vaultnotes_test.go` without duplication).

## Current State
Phase 2 is complete and merged. `buildVaultModel` + `buildVaultRefs` already resolve ALL the folder/filename edge cases named in Phase 3's "Edge cases" list (empty-slug fallback, UTC foldering, undated bucket, collision suffixing) — Phase 3 only needs to *look up* `refs[id]` for its own path, never recompute slug logic.

## Gaps
None blocking. One under-specified point resolved by design decision below: Claim.Statement has no cached "source event body" — DW-3.2's "quoteBlock ... linking the source event note" (exact DW-3.2 wording) means the callout content is a **wikilink to the source event note**, not the raw event body text (confirmed by DW-3.2's own phrasing; the Goal/Scope prose used the looser word "prose" but DW-3.2 is the authoritative, testable wording, and it says "linking").

## Code Standards
No `docs/code-standards.md` found in this worktree. Following the established in-package precedent instead: `export.go`'s frontmatter-via-`yaml.MapSlice`, wikilink format `[[File|Display]]`, doc-comment style, and `sanitize_test.go`'s adversarial hazard-sweep pattern.

## Test Infrastructure
Standard Go `testing`, table-driven tests, DW-tagged test names (`TestDW_3_1_...`), `go test ./internal/cli/ -count=1`. Reuses `tp(t, s)` (RFC3339 time fixture helper) from `vaultmodel_test.go` and `assertNoHazards` from `sanitize_test.go` (same package, no re-declaration).

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-3.1 | event note carries full `sanitizeBody`'d prose, a UTC-foldered path (nil → `undated/`), and a concept footer resolving links via `VaultRefs` | COVERED | `TestDW_3_1_EventNoteBodyPathAndFooter`, `TestDW_3_1_EventNoteUndatedFolder`, `TestDW_3_1_EventNoteUTCFoldering`, `TestDW_3_1_EventNoteNoConceptsOmitsFooter`, `TestDW_3_1_EventNoteSanitizesHostileBody` |
| DW-3.2 | concept note renders claims oldest-first, each `Statement` + a folded `quoteBlock` callout containing the source event's sanitized prose, attributed by wikilink | COVERED | `TestDW_3_2_ConceptNoteClaimsOldestFirstWithQuote`, `TestDW_3_2_ConceptNoteClaimTieBreaksOnEdgeID`, `TestDW_3_2_ConceptNoteClaimWithoutSourceQuote`, `TestDW_3_2_ConceptNoteClaimSourceEventNotInEventsMap`, `TestDW_3_2_ConceptNoteClaimSourceEventNotInRefs`, `TestDW_3_2_ConceptNoteZeroClaimsHubListsRelated`, `TestDW_3_2_ConceptNoteSanitizesHostileStatement`, `TestDW_3_2_ConceptNoteSanitizesHostileSourceEventProse` |
| DW-3.3 | only degree ≥2 concepts get files; degree ≤1 appear solely as unresolved ghost `[[links]]` | COVERED | `TestDW_3_3_GhostConceptsResolveAsLinksNotFiles` (simulates the Phase-5 filter discipline: iterate `model.Concepts`, render only `!Ghost`, prove the ghost still resolves as a valid `[[link]]` target wherever referenced) |
| DW-3.4 | identical model+refs → byte-identical notes across two runs | COVERED | `TestDW_3_4_EventNoteByteIdenticalAcrossRuns`, `TestDW_3_4_ConceptNoteByteIdenticalAcrossRuns` |

**All items COVERED:** YES

## Design Decisions

**1. Renderers are thin lookups over `VaultRefs`, not re-derivers.** All the Phase-3 "edge cases" listed in the plan (empty slug → id fallback, UTC foldering, undated bucket, collision suffix) are *already* resolved by Phase 2's `buildVaultRefs`. `renderEvent`/`renderConcept` do `self := refs[id]` and use `self.Folder + "/" + self.File + ".md"` — no slug/collision logic is duplicated in Phase 3. This keeps the phase genuinely independent and avoids two sources of truth for path assignment.

**2. H1 always comes from the ref's `Display`, never the raw model field.** `self.Display` is already `cleanInline`-safe (computed once in `buildVaultRefs`). Using it instead of re-deriving from `Concept.Name`/`Event.Title` means there is exactly one sanitization point for display names, and it is impossible for `renderConcept` to accidentally emit a raw `Concept.Name`.

**3. [SUPERSEDED — coordinator correction after initial review] `quoteBlock` wraps the source event's actual sanitized prose, attributed by a wikilink title, not just a bare link.** The initial implementation read DW-3.2's "linking the source event note" as license to quote only a wikilink (justified by the fixed signature lacking event-body access). The plan coordinator corrected this: the user-approved sample puts the source event's real prose inside the folded callout — that's the "receipts" the concept fact-sheet is for; a bare link loses the richness. Fix: `renderConcept`'s signature gained a third parameter, `events map[string]Event` (keyed by `EventID`; Phase 5's assembler supplies it from `VaultModel.Events`), since `Claim` itself carries no cached body text. The callout is now `"> [!quote]- Source: [[File|Display]]\n" + quoteBlock(sanitizeBody(events[cl.SourceEventID].Body))` — the title line is a fixed literal plus the already-safe `ref.Display` (no violation of "no hand-rolled `>` prefixing," which governs untrusted content, not this trusted header), and the quoted body goes through `sanitizeBody` THEN `quoteBlock` (both required in sequence — `quoteBlock` never assumes pre-sanitized input). A claim whose `SourceEventID` is `""`, absent from `events`, or absent from `refs` renders the `Statement` line with no callout (the documented "quote-less" edge case, now with three ways to reach it instead of two).

**4. `renderConcept` re-sorts its own copy of `Claims` (`ValidAt` then `EdgeID`) instead of trusting caller order.** `assembleConcepts` in Phase 2 already sorts this way, but Phase 3's own file-scope constraint list independently states "claims sorted `ValidAt` then `EdgeID`" as a Phase-3 invariant. Making the renderer self-sufficient (sort a copy, never mutate the input slice) means its correctness doesn't depend on an out-of-scope file, and it stays testable in isolation with hand-built `Concept` literals.

**5. Link lists (`**Concepts:**` footer, `## Related concepts`) are resolved through one shared helper, `resolveLinks`, sorted by `ref.File`.** The plan explicitly requires "footer links sorted by ref" for the event footer; applying the same helper (and therefore the same ordering policy) to the concept's related-list keeps one code path and one deterministic tie-break (`ref.File` is globally unique post-collision-suffixing, so no secondary tie-break is needed). IDs that fail to resolve in `refs` are skipped defensively (should not happen given the Phase 2 contract, but this avoids emitting a malformed `[[|]]`).

**6. DW-3.3 is validated by simulating the caller discipline, not by teaching `renderConcept` to refuse ghosts.** The fixed signature has no boolean gate, and "only hubs get files" is a Phase-5 (writer) concern per the plan's own phase split ("ghost concepts emit no file (they are link targets only)" is listed as a *constraint on the vault*, not a required check inside the pure renderer). The test proves the two halves of the invariant that Phase 3 owns: (a) a hub's related-concepts / claim-quote links resolve correctly even when the target is a ghost (no file, but a valid `noteRef` still exists in `VaultRefs`), and (b) filtering `model.Concepts` by `!Ghost` before calling `renderConcept` — the discipline Phase 5 will apply — yields exactly the hub set.

**7. Frontmatter kept minimal, YAML-encoded exactly like `export.go`'s precedent.** Event frontmatter: `engram_id`, `occurred_at` (omitted when nil). Concept frontmatter: `engram_id`, `degree`, `aliases` (omitted when empty). Untrusted fields (event/concept id, aliases) go through `yaml.MapSlice` + `yaml.Marshal` — the established safe encoder — never hand-interpolated into the frontmatter block, per the CRITICAL security constraint. `yaml.Marshal` cannot fail for these plain string/int/[]string inputs, so the error is not propagated (the `Produces` signature has no error return); this mirrors the same practical assumption already relied on implicitly elsewhere for simple `MapSlice` values.

## Prerequisites
- [x] Required files exist (vaultmodel.go, sanitize.go from Phase 2, merged)
- [x] Dependencies available (`go.yaml.in/yaml/v2` already in go.mod)
- [x] No missing prerequisites

## Recommendation
BUILD. The plan fits reality cleanly — Phase 2 already did the hard work of resolving every path/collision edge case into `VaultRefs`, leaving Phase 3 as pure, thin string renderers exactly as scoped. One ambiguity (raw body vs. link in the concept's provenance callout) is resolved in favor of DW-3.2's literal, testable wording and the fixed function signature — recorded above, not treated as grounds for UPDATE_PLAN.

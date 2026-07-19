# Discovery + Design: Phase 4 - Client — topic-map clustering + MOC notes

## Files Found
- `internal/cli/vaultmodel.go` — Phase 2 output. `VaultModel{Events, Concepts}`, `VaultRefs`, `noteRef`, `Concept{EntityID, Name, Aliases, Degree, Claims, RelatedIDs, Ghost}`, `Event{EventID, Title, Body, OccurredAt, ConceptIDs}`, plus reusable helpers: `compareTimePtr`, `addNeighbor`, `sortedKeys`, `idPrefix` (via export.go).
- `internal/cli/sanitize.go` — `sanitizeBody`/`quoteBlock` (not needed by this phase — maps carry no raw prose, only already-cleaned display names via `VaultRefs`).
- `internal/cli/export.go` — `sanitizeFilename`, `cleanInline`, `idPrefix`, `maxFilenameRunes`. Confirmed `Concept.RelatedIDs` is populated by `assembleConcepts` as a symmetric, sorted, concept-id-only adjacency list (both endpoints resolved via `canonicalByEntity`, self-loops excluded) — safe to use directly as the clustering graph with no extra validation.
- `internal/cli/vaultnotes.go` (Phase 3) — does NOT exist in this worktree yet (Phase 3 builds it in a parallel wave worktree). Not read, not touched — this phase's file scope is `vaultmaps.go`/`vaultmaps_test.go` only, and the Produces contract (`clusterConcepts`, `renderMap`) has no dependency on Phase 3's renderers.
- `internal/cli/vaultmaps.go`, `internal/cli/vaultmaps_test.go` — did not exist; both created by this phase.

## Current State
Phase 2 delivered a pure, deterministic `VaultModel` + `VaultRefs`. Nothing in the repo clusters concepts or renders map notes yet.

## Gaps
None against the plan. One judgment call the plan leaves implicit, resolved below: `clusterConcepts(VaultModel) []Cluster` takes no `VaultRefs`, so a cluster's `Title` must derive straight from `Concept.Name` (via `cleanInline`, mirroring how `buildVaultRefs` derives concept `Display`) rather than by looking up `refs[TopConceptID].Display` — keeping `clusterConcepts` a pure function of `VaultModel` alone, exactly as the Produces signature states.

## Code Standards
No `docs/code-standards.md` in the repo. Followed the file's own established idiom instead: package-level doc comment stating the security/determinism model, exported types with full field-level doc comments, small pure helper functions, `sortedKeys`/`addNeighbor`/`compareTimePtr`/`idPrefix` reused rather than re-implemented (DRY with vaultmodel.go/export.go), tests named `TestDW_<phase>_<item>_<description>`.

## Test Infrastructure
Standard `go test`, table-driven where natural. Reused `equalStrings`/`containsString` helpers already defined in `vaultmodel_test.go` (same package `cli`, same test binary) rather than redefining them.

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-4.1 | Clustering deterministic (fixed order, id tie-break); identical model → identical clusters/files; title-collision and misc-prefix collisions disambiguated by suffix, not clobbered | COVERED | `TestDW_4_1_DeterministicAcrossRuns`, `TestDW_4_1_TitleCollisionSuffixed`, `TestDW_4_1_ConceptTitleCollidesWithMiscPrefixReserved` |
| DW-4.2 | Sub-threshold components funnel into size-bounded misc buckets (no bucket exceeds cap, no per-node explosion); large component keeps its own map | COVERED | `TestDW_4_2_ManyTinyComponentsBoundedMiscBuckets`, `TestDW_4_2_MiscBucketCapNeverExceeded`, `TestDW_4_2_LargeComponentKeepsOwnMapNoSplit` |
| DW-4.3 | Map lists members, UTC source-event timeline, cross-cluster out-links; title = highest-degree member (deterministic); filename `sanitizeFilename`'d | COVERED | `TestDW_4_3_MapContentMembersTimelineOutLinks`, `TestDW_4_3_TitleIsHighestDegreeMemberIDTieBreak`, `TestDW_4_3_FilenameSanitized` |
| DW-4.4 | Empty graph → zero map notes, no error | COVERED | `TestDW_4_4_EmptyGraphNoClusters` |

**All items COVERED:** YES

## Design Decisions

**Graph source:** `Concept.RelatedIDs` (already symmetric, sorted, concept-id-valid — built once in Phase 2's `assembleConcepts`) is the clustering adjacency. No separate edge list is re-derived; re-deriving one from raw edges would duplicate Phase 2's endpoint-resolution logic for no benefit.

**Component discovery order:** concepts visited in ascending `EntityID` order; each unvisited start's BFS walks `RelatedIDs` (already sorted) via a FIFO queue. This makes component *discovery order* ascending-by-smallest-member-id automatically — no separate sort of the components list is needed. Component *membership* is a set and provably order-independent of traversal order; the fixed order only matters for reproducible discovery/bucket-assignment sequencing, which this satisfies.

**Misc bucketing:** every sub-threshold component's members are flattened together, sorted by concept id globally (not just concatenated per-component), then chunked into `miscBucketCap`-sized (50) groups. A global sort (vs. per-component concatenation) is what the plan's "split deterministically by sorted concept key" calls for, and it also means bucket boundaries don't depend on which components happened to be discovered in what order.

**Filename assignment is two-phase, mirroring `buildVaultRefs` exactly:**
1. Build all clusters (concept + misc) with a `Key` (smallest member id for concept clusters; `"misc:NNNNNN"` for misc clusters — globally unique, used only as the sort key and idPrefix suffix source).
2. `assignClusterFilenames` sorts ALL clusters by `Key` ascending, computes each one's unsuffixed base name (`sanitizeFilename(topConcept.Name)` or `"misc-NN"`), counts base collisions case-insensitively **over concept-cluster bases only**, and — same as `buildVaultRefs` — suffixes ALL colliding concept homonyms (not first-wins) with `idPrefix(Key, 8)`, extending on residual clash exactly like the existing algorithm. A concept cluster's base starting with `"misc-"` (case-insensitive) is *always* forced through the suffix path — reserving the whole namespace, not just exact-collision cases, per the plan's "misc- prefix is reserved" wording. The reservation is one-directional: a misc bucket's own `"misc-NN"` name is authoritative and never suffixed (its `MiscIndex` already makes it globally unique among misc buckets), so a concept cluster mimicking it always yields — the misc bucket itself is never bumped. An initial version tallied `baseCount` over ALL clusters symmetrically (matching `buildVaultRefs`'s "every homonym suffixed" policy exactly); that would have wrongly suffixed the real `misc-NN` bucket too whenever a concept cluster collided with it, violating "a concept map can never clobber a misc bucket" (nothing said the reverse should also hold) — fixed before writing tests.

This two-phase split is required because `renderMap`'s signature (`Cluster, VaultRefs) (relPath, content)`) only ever sees one cluster — cross-cluster collision detection cannot happen there. `clusterConcepts` is the only place with the whole-graph view, so it does all collision bookkeeping and hands `renderMap` a `Cluster` that already carries its final `RelPath`/`Title`.

**Per-cluster Timeline/OutLinks precomputed, not derived in `renderMap`:** `renderMap`'s signature has no `VaultModel` parameter, so it cannot look up `Event.OccurredAt` or walk adjacency itself. `clusterConcepts` builds a reverse concept→event index once (`indexEvents`, O(V+E) total, not O(events × clusters)) and attaches each cluster's deduped, chronologically-sorted `Timeline` and its outside-the-cluster `OutLinkIDs` before returning. `renderMap` is then a pure, cheap walk of one `Cluster` + `VaultRefs` — no graph traversal at render time.

**Alternative considered and rejected:** having `renderMap` accept `VaultModel` too, so it could compute title/timeline/outlinks itself. Rejected because (a) it violates the plan's pinned `renderMap(Cluster, VaultRefs) (relPath, content string)` signature — a cross-phase seam Phase 5 builds against — and (b) it would make collision-free filename assignment impossible per-cluster anyway (needs the whole cluster list), so the two-phase split is unavoidable regardless.

**`wikilink` skip-on-miss instead of emitting `[[|]]`:** every id `clusterConcepts` ever puts into a `Cluster` is drawn from the same `VaultModel` that produced `VaultRefs`, so a miss is unreachable in practice (never expected input, unlike the untrusted-prose surface `sanitizeBody` defends). Matches the existing codebase idiom in `export.go`'s `renderNote` ("caller guarantees presence") while still failing safe (empty string, not a malformed link) if that invariant is ever violated — covered by `TestMissingRefSkipsSilently`.

## Prerequisites
- [x] Phase 2 files present and read (`vaultmodel.go`, `sanitize.go`, `export.go`)
- [x] `sanitizeFilename`, `cleanInline`, `idPrefix`, `compareTimePtr`, `addNeighbor`, `sortedKeys` all present and reusable
- [x] No missing prerequisites

## Recommendation
BUILD. The plan's scope and Produces contract fit the actual Phase 2 types with no reinterpretation — proceed to implementation as designed above.

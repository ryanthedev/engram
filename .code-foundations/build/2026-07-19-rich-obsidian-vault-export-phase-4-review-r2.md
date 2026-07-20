# Review: Phase 4 - topic-map clustering + MOC notes (r2)

## Executed Results (Step 0)
- Test suite: `go test ./internal/cli/ -count=1` → `ok github.com/ryanthedev/engram/internal/cli 0.030s` (full package, all tests, no failures)
- Typecheck/build: `go vet ./internal/cli/` → clean, no output
- Coverage: `go test ./internal/cli/ -coverprofile=/tmp/rev-p4r2/cov.out -count=1` → `ok ... coverage: 67.3% of statements` (package-wide; vaultmaps.go-specific figures below)
- Determinism probe (scratch, added and removed — not part of committed suite): a temporary `TestZZPermutedInputDeterminism` test permuted `model.Concepts` order (Fisher-Yates, seed 42) and `Events` order, re-ran `clusterConcepts` + `renderMap`, and asserted `reflect.DeepEqual` cluster equality plus byte-identical map path/content — PASS. File was deleted after the run; `git status --short` confirms the tree is clean.
- Security probe (scratch, added and removed): a temporary test fed a concept named `"Evil]] [[Injected|Link\n# Fake Heading"` through `clusterConcepts`; the produced Title was `"Evil-- --Injected-Link # Fake Heading"` (no `[[`, `]]`, `|`, or raw newline survived) — PASS. File deleted after run.

## Requirement Fulfillment

### DW-4.1
PREMISE:  clustering deterministic (fixed order, id tie-break) — identical model → identical clusters and map files; two clusters whose titles sanitize to the same filename (or collide with a `misc-NN` bucket) are disambiguated by collision suffix, not silently clobbered.
EVIDENCE: internal/cli/vaultmaps.go:150-166 (`findComponents` sorts ids ascending before traversal), :182 (`walkComponent` sorts members ascending), :262-312 (`assignClusterFilenames`, sorted by Key, forced-suffix + residual-clash loop); internal/cli/vaultmaps_test.go:132-154 (`TestDW_4_1_DeterministicAcrossRuns`), :156-188 (`TestDW_4_1_TitleCollisionSuffixed`), :190-228 (`TestDW_4_1_ConceptTitleCollidesWithMiscPrefixReserved`)
TRACE:    Two independent 3-node chains ("a...", "b...") both with top-concept Name "Widget" → same `sanitizeFilename` base "widget" for both → `assignClusterFilenames` sees `baseCount["widget"]==2` → both entries get `forced=false` but `baseCount>1` triggers the id-prefix-suffixed name path (line 299-301) → distinct `RelPath`s, both containing `" ("`. Verified by `TestDW_4_1_TitleCollisionSuffixed`, which explicitly asserts `RelPath`s differ and both carry the suffix marker. Also independently re-verified with a scratch permuted-input-order test (see Executed Results) — `clusterConcepts` and `renderMap` output were identical regardless of `model.Concepts`/`model.Events` slice order, because both `findComponents` and `buildVaultRefs` sort before use.
VERDICT:  PASS

### DW-4.2
PREMISE:  sub-threshold components funnel into size-bounded misc buckets (no single misc note exceeds the cap; no per-node map explosion); a large component keeps its own map.
EVIDENCE: internal/cli/vaultmaps.go:56-62 (`minMembers`, `miscBucketCap` constants), :102-110 (big/small split), :220-237 (`miscBuckets` chunking); internal/cli/vaultmaps_test.go:84-99 (`TestDW_4_2_LargeComponentKeepsOwnMapNoSplit`), :101-119 (`TestDW_4_2_ManyTinyComponentsBoundedMiscBuckets`), :121-128 (`TestDW_4_2_MiscBucketCapNeverExceeded`)
TRACE:    120 isolated (singleton) concepts → `findComponents` returns 120 size-1 components → all fail `len(comp) >= minMembers(3)` → all land in `small` → `miscBuckets` flattens to 120 sorted ids, chunks into `ceil(120/50)=3` buckets of ≤50 → 0 concept clusters, 3 misc clusters, total members 120. Verified exactly by `TestDW_4_2_ManyTinyComponentsBoundedMiscBuckets`. Separately, a 10-node connected chain → one component of size 10 ≥ 3 → `big` → single concept cluster with all 10 members, 0 misc clusters — verified by `TestDW_4_2_LargeComponentKeepsOwnMapNoSplit`. 237 isolated concepts checked against `miscBucketCap` per-bucket cap in `TestDW_4_2_MiscBucketCapNeverExceeded`.
VERDICT:  PASS

### DW-4.3
PREMISE:  each map lists members, a UTC source-event timeline, and cross-cluster out-links; title = highest-degree member (deterministic); filename `sanitizeFilename`'d.
EVIDENCE: internal/cli/vaultmaps.go:393-427 (`renderMap` sections), :205-214 (`topByDegree`), :282-284 (title=`cleanInline(name)`, base=`sanitizeFilename(name)`), :410 (`.UTC().Format(...)`); internal/cli/vaultmaps_test.go:232-298 (`TestDW_4_3_MapContentMembersTimelineOutLinks`), :300-319 (`TestDW_4_3_TitleIsHighestDegreeMemberIDTieBreak`), :321-343 (`TestDW_4_3_FilenameSanitized`)
TRACE:    m1/m2/m3 cluster (m1-m2, m2-m3 edges; m1 also points to external "ext") → Members=[m1,m2,m3], OutLinkIDs=[ext] (ext outside cluster) → `renderMap` emits `## Concepts` with 3 wikilinks, `## Timeline` with ev-a (2026-01-01) before ev-b (2026-02-01) since `buildTimeline` sorts by `OccurredAt` ascending, `## Cross-cluster links` with ext's wikilink. Verified by `TestDW_4_3_MapContentMembersTimelineOutLinks`. Title tie-break: triangle m1/m2/m3 all Degree=2 → `topByDegree` iterates from `members[1:]` requiring strict `>` to replace `best`, so the first (smallest, since members pre-sorted ascending) id wins ties → TopConceptID="m1", Title="First" — verified by `TestDW_4_3_TitleIsHighestDegreeMemberIDTieBreak`. Filename: Name=`Weird/Name*Test"?` → `sanitizeFilename` strips/replaces all FS-illegal runs → resulting RelPath contains none of `/`,`*`,`"`,`?` after the `maps/` prefix — verified by `TestDW_4_3_FilenameSanitized`.
VERDICT:  PASS

### DW-4.4
PREMISE:  empty graph → zero map notes, no error.
EVIDENCE: internal/cli/vaultmaps.go:92-95 (`if len(model.Concepts) == 0 { return nil }`); internal/cli/vaultmaps_test.go:75-80 (`TestDW_4_4_EmptyGraphNoClusters`)
TRACE:    `clusterConcepts(VaultModel{})` → `model.Concepts` is nil, `len==0` → early return `nil`, no panic, no downstream `findComponents`/`renderMap` calls → 0 clusters → 0 map notes when a caller iterates the (empty) result. Verified by direct execution in `TestDW_4_4_EmptyGraphNoClusters`.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] DW-4.1 — `TestDW_4_1_DeterministicAcrossRuns`, `TestDW_4_1_TitleCollisionSuffixed`, `TestDW_4_1_ConceptTitleCollidesWithMiscPrefixReserved` (all ran in Step 0, all PASS)
- [x] DW-4.2 — `TestDW_4_2_LargeComponentKeepsOwnMapNoSplit`, `TestDW_4_2_ManyTinyComponentsBoundedMiscBuckets`, `TestDW_4_2_MiscBucketCapNeverExceeded`
- [x] DW-4.3 — `TestDW_4_3_MapContentMembersTimelineOutLinks`, `TestDW_4_3_TitleIsHighestDegreeMemberIDTieBreak`, `TestDW_4_3_FilenameSanitized`
- [x] DW-4.4 — `TestDW_4_4_EmptyGraphNoClusters`
- [x] Coverage matches the stated level: every one of the 13 functions in vaultmaps.go is at 100.0% statement coverage (see Loaded-Skill/coverage section below); additional non-DW tests (`TestMissingRefSkipsSilently`, `TestEmptyTimelineAndOutLinksOmitSections`, `TestClusterConceptsIncludesGhostsAsMembers`, `TestBuildTimelineTieBreaksByEventID`, `TestDigitWidthMinimumTwo`, `TestFilenameFallback_EmptyNameUsesMapBase`, `TestFilenameCollision_ThreeWayForcesExtendedSuffix`, `TestRenderMap_UndatedTimelineEntry`) close specific branches (nil OccurredAt, empty-name fallback, three-way Key collision, ghost members, missing refs).

No gaps found.

## Dead Code
None found. No unused imports, no unreachable statements after any return/break, no debug prints, no commented-out blocks in vaultmaps.go.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | No goroutines, channels, or shared mutable state across calls; `clusterConcepts`/`renderMap` are pure functions over their arguments. |
| Error Handling | N/A | No I/O, parsing, or external calls in this file — pure in-memory computation over an already-validated `VaultModel` (assembleConcepts upstream guarantees symmetric, valid-id RelatedIDs per the file's own header comment). Traced the one place a caller-bug defensive branch exists (`wikilink` returning `""` on a refs miss, vaultmaps.go:380-386) — confirmed via `TestMissingRefSkipsSilently` that a fully-unresolvable cluster renders no wikilinks rather than a malformed `[[|]]` or a panic. |
| Resources | N/A | No file handles, connections, locks, or caches held; all data structures are local maps/slices scoped to a single `clusterConcepts` call. |
| Boundaries | PASS | Traced `topByDegree(members, ...)` (vaultmaps.go:205-214) reading `members[0]` unconditionally — confirmed it is only ever called from vaultmaps.go:117 on a `comp` from the `big` split, which by construction (line 104: `len(comp) >= minMembers` where `minMembers=3`) is never empty, so no out-of-range panic is reachable. Traced `miscBuckets(nil)` (small components empty) → `flat` stays empty → `for len(flat) > 0` never executes → returns `nil` buckets, no panic. Traced the exact `minMembers` boundary (component of exactly 3) via `TestDW_4_3_MapContentMembersTimelineOutLinks`'s m1/m2/m3 fixture — correctly classified as `big`, not `small`. |
| Security | PASS | Traced a concept named `"Evil]] [[Injected|Link\n# Fake Heading"` through `clusterConcepts` (scratch probe, see Executed Results): `Title = cleanInline(name)` (vaultmaps.go:283) strips `[`, `]`, `|` to `-` and folds the embedded newline to a space, so the rendered Title carried no wikilink-breaking or heading-injection syntax (`"Evil-- --Injected-Link # Fake Heading"`). Filename base goes through `sanitizeFilename` (vaultmaps.go:284), which independently strips FS/Obsidian-illegal runes. Member/out-link display text is sourced from `refs[id].Display`, which `buildVaultRefs` (vaultmodel.go:404) already builds via `cleanInline(co.Name)` for concepts — so no raw untrusted name reaches rendered output through either path. |

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-control-flow-quality | Max nesting depth ≤ 3 levels, every function in vaultmaps.go | PASS | Walked every function by hand. Depth-2 functions: `clusterConcepts`, `findComponents`, `enqueueUnvisited`, `topByDegree`, `miscBuckets`, `indexEvents`. Depth-1: `walkComponent`, `digitWidth`, `wikilink`. Depth-3 (deepest in the file, none exceed 3): `assignClusterFilenames` (for→switch→case-block if, and separately for→for→if/else), `buildTimeline` (for→for→if), `outLinks` (for→for→if), `renderMap` (if→for→if, three separate top-level sections). No function reaches depth 4. `findComponents` itself tops out at depth 2 (`for start := range ids { if visited[start] { continue } }`) — the BFS body that could have nested deeper was extracted into `walkComponent`/`enqueueUnvisited`, each also ≤2. |
| cc-control-flow-quality | McCabe complexity, loop/guard-clause structure | PASS (note) | All functions stay in the 0-5 "probably fine" band by inspection (each has at most 2-3 decision points); guard clauses used appropriately (`enqueueUnvisited`'s `if visited[nb] { continue }`, `outLinks`'s `if !inCluster[nb]`); no boolean expression complex enough to need extraction into a named intermediate. |
| cc-pseudocode-programming | Pseudocode-first design, traceable correctness | PASS (note) | The package-level comment block (vaultmaps.go:17-43) is intent-level pseudocode preserved as the header comment, matching the actual algorithm structure statement-for-statement (component discovery → big/small split → misc chunking → filename assignment → per-cluster timeline/out-links → construction-order return). Every routine's own doc comment explains its contribution to that pseudocode and is falsifiable against the code beneath it (verified during Step 1 tracing — no divergence found between comment claims and executed behavior). |

## Notes (non-blocking)

- `assignClusterFilenames`'s residual-clash extension loop (vaultmaps.go:302-308) is only exercised by the deliberately pathological `TestFilenameCollision_ThreeWayForcesExtendedSuffix` fixture (two clusters sharing an identical `Key`, which real `clusterConcepts` output can never produce since component-smallest-id and misc-bucket-index are each globally unique by construction). This is honestly documented in the test's own comment as an adversarial/defensive-only path, not a reachable production state — flagging for visibility only, not a defect.
- `renderMap`'s comment on `wikilink` (vaultmaps.go:375-379) documents that a refs miss "would be a caller bug, not expected input" — consistent with what was traced; no requirement or edge case in this dispatch calls for hardening it further.
- Package-wide coverage (67.3%) is a whole-`internal/cli`-package figure, not a vaultmaps.go-specific one; the per-function breakdown for vaultmaps.go's own 13 functions (below) is what the dispatch asked for, and it's 100.0% across the board.

### Coverage detail (go tool cover -func, vaultmaps.go only)

```
internal/cli/vaultmaps.go:92:   clusterConcepts        100.0%
internal/cli/vaultmaps.go:150:  findComponents         100.0%
internal/cli/vaultmaps.go:172:  walkComponent          100.0%
internal/cli/vaultmaps.go:189:  enqueueUnvisited       100.0%
internal/cli/vaultmaps.go:205:  topByDegree            100.0%
internal/cli/vaultmaps.go:220:  miscBuckets            100.0%
internal/cli/vaultmaps.go:241:  digitWidth             100.0%
internal/cli/vaultmaps.go:262:  assignClusterFilenames 100.0%
internal/cli/vaultmaps.go:317:  indexEvents            100.0%
internal/cli/vaultmaps.go:332:  buildTimeline          100.0%
internal/cli/vaultmaps.go:359:  outLinks               100.0%
internal/cli/vaultmaps.go:380:  wikilink               100.0%
internal/cli/vaultmaps.go:393:  renderMap              100.0%
```

## Issues (if FAIL)

None.

**Verdict: PASS**

# Review: Phase 4 - topic-map clustering + MOC notes

## Executed Results (Step 0)
- Build: `go build ./...` → clean, no errors.
- Test suite: `go test ./internal/cli/ -count=1` → `ok  github.com/ryanthedev/engram/internal/cli  0.030s` (all tests pass, including all `TestDW_4_*` and the beyond-floor tests in `vaultmaps_test.go`).
- Coverage: `go test ./internal/cli/ -count=1 -coverprofile=/tmp/rev-p4-cover.out` → 66.6% package-wide (package also contains Phases 1-3/5 code, so the package-wide number is not the relevant signal); `go tool cover -func` for the Phase 4 functions specifically:

  ```
  clusterConcepts          100.0%
  findComponents           100.0%
  topByDegree              100.0%
  miscBuckets              100.0%
  digitWidth               100.0%
  assignClusterFilenames    87.5%
  indexEvents              100.0%
  buildTimeline            100.0%
  outLinks                 100.0%
  wikilink                 100.0%
  renderMap                 95.0%
  ```
- Typecheck: N/A (Go — `go build` above is the typecheck).
- Lint: no configured linter found in the phase's scope; not run.
- Extra empirical check (Determinism focus, scratch-only, not part of the reviewed suite): added a temporary in-package test that shuffles the input `[]Concept` order with a seeded RNG, re-runs `clusterConcepts` + `renderMap`, and asserts `reflect.DeepEqual` cluster equality and byte-identical rendered output against the unpermuted run. Passed. The file was deleted immediately after; `git status --short` is clean.

## Requirement Fulfillment

### DW-4.1
PREMISE:  clustering is deterministic (fixed order, id tie-break) — identical model → identical clusters and map files; two clusters whose titles sanitize to the same filename (or collide with a `misc-NN` bucket) are disambiguated by collision suffix, not silently clobbered.
EVIDENCE: `internal/cli/vaultmaps.go:150-181` (`findComponents` — ids visited in `sort.Strings` order, adjacency walk over already-sorted `RelatedIDs`); `vaultmaps.go:188-197` (`topByDegree` — strict `>` in ascending-id order keeps smallest-id on ties); `vaultmaps.go:245-295` (`assignClusterFilenames` — ascending-`Key` processing, suffix-not-clobber logic, reserved `misc-` namespace).
TRACE:    `TestDW_4_1_DeterministicAcrossRuns` builds a model (5-chain + 30 isolated concepts), calls `clusterConcepts` twice, asserts `reflect.DeepEqual` → PASS (ran, green). Independently re-verified with a permuted input order (see Executed Results) → identical clusters, identical rendered bytes. `TestDW_4_1_TitleCollisionSuffixed`: two disjoint 3-node components both named "Widget" (joined only by name, not by edge) → both cluster, both keep `Title="Widget"`, but `RelPath` differs and each carries a `" ("` collision suffix → PASS (ran, green). `TestDW_4_1_ConceptTitleCollidesWithMiscPrefixReserved`: a concept cluster whose sanitized title is literally `"Misc-01"` never clobbers the real `maps/misc-01.md` bucket, and is itself forced through the suffix path → PASS (ran, green). `TestDW_4_3_TitleIsHighestDegreeMemberIDTieBreak`: 3-node all-degree-2 triangle, id tie-break picks `m1` → PASS (ran, green).
VERDICT:  PASS

### DW-4.2
PREMISE:  sub-threshold components funnel into size-bounded misc buckets (no single misc note exceeds the cap; no per-node map explosion); a large component keeps its own map.
EVIDENCE: `vaultmaps.go:102-110` (big/small split at `minMembers=3`), `vaultmaps.go:203-220` (`miscBuckets` — flatten, sort, chunk at `miscBucketCap=50`), `vaultmaps.go:59-62` (cap constant).
TRACE:    `TestDW_4_2_LargeComponentKeepsOwnMapNoSplit`: a 10-node chain (one component) → exactly 1 concept cluster with all 10 members, 0 misc clusters → PASS (ran, green). `TestDW_4_2_ManyTinyComponentsBoundedMiscBuckets`: 120 fully-isolated concepts (120 singleton components, all sub-threshold) → 0 concept clusters, exactly `ceil(120/50)=3` misc clusters totaling 120 members (none dropped) → PASS (ran, green). `TestDW_4_2_MiscBucketCapNeverExceeded`: 237 isolated concepts → every misc cluster's member count `<= miscBucketCap` → PASS (ran, green).
VERDICT:  PASS

### DW-4.3
PREMISE:  each map lists members, a UTC source-event timeline, and cross-cluster out-links; title = highest-degree member (deterministic); filename `sanitizeFilename`'d.
EVIDENCE: `vaultmaps.go:376-410` (`renderMap` — Concepts/Timeline/Cross-cluster-links sections), `vaultmaps.go:315-338` (`buildTimeline` — union + chronological sort, nil-last, id tie-break), `vaultmaps.go:342-356` (`outLinks`), `vaultmaps.go:265-267` (`Title = cleanInline(name)`, `base = sanitizeFilename(name)`).
TRACE:    `TestDW_4_3_MapContentMembersTimelineOutLinks`: 3-member component (m1/m2/m3) plus an asymmetrically-referenced singleton `ext`; renders content containing `## Concepts`, every member's file link, `## Timeline` with the earlier 2026-01-01 entry appearing before the later 2026-02-01 entry, `## Cross-cluster links` containing `ext`'s link → PASS (ran, green). `TestDW_4_3_TitleIsHighestDegreeMemberIDTieBreak` → PASS (ran, green, see DW-4.1). `TestDW_4_3_FilenameSanitized`: a concept named `` Weird/Name*Test"? `` produces a `RelPath` with none of `/`, `*`, `"`, `?` surviving in the filename portion, still ending `.md` → PASS (ran, green). `TestBuildTimelineTieBreaksByEventID`: two events at the exact same instant → `ev-a` before `ev-z` (id tie-break) → PASS (ran, green).
VERDICT:  PASS

### DW-4.4
PREMISE:  empty graph → zero map notes, no error.
EVIDENCE: `vaultmaps.go:92-95` (`if len(model.Concepts) == 0 { return nil }`).
TRACE:    `TestDW_4_4_EmptyGraphNoClusters`: `clusterConcepts(VaultModel{})` → `len(clusters) == 0`, no panic/error → PASS (ran, green).
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] All 4 DW items have corresponding, currently-passing automated tests (ran in Step 0): DW-4.1 → `TestDW_4_1_*` (3 tests); DW-4.2 → `TestDW_4_2_*` (3 tests); DW-4.3 → `TestDW_4_3_*` (2 tests) + `TestBuildTimelineTieBreaksByEventID`; DW-4.4 → `TestDW_4_4_EmptyGraphNoClusters`.
- [ ] Test coverage does **not** match the stated 100% level for two of the eleven Phase-4 functions:
  - `assignClusterFilenames`: 87.5%. Two branches never execute in any test: (1) `vaultmaps.go:268-270`, the `base == ""` → fallback-to-`"map"` path (no test uses a concept `Name` that sanitizes to empty, e.g. all-whitespace or empty); (2) `vaultmaps.go:285-290`(ish), the `for n := 8; used[...]; n += 4` loop **body** — every existing collision test resolves on the first candidate name, so the residual-extension logic (needed when even the first 8-char id-prefix suffix still collides, e.g. 2+ clusters whose keys share an 8+-char prefix) is never actually exercised.
  - `renderMap`: 95%. The `t.OccurredAt == nil` branch of the timeline render (`else { fmt.Fprintf(&b, "- %s\n", lk) }`, ~line 395) is never hit — no test renders a timeline entry for an event with a nil `OccurredAt`.
  - I hand-traced both `assignClusterFilenames` gaps against constructed multi-way collision scenarios (see Correctness Dimensions / Boundaries below) and found no defect, but the dispatch's stated Test Coverage Level (100%, to be verified via `go tool cover -func`) is not met by the current suite as measured.

## Dead Code
None found. No unreachable code after early returns, no unused imports, no debug statements, no commented-out blocks.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | Pure, single-goroutine functions; no shared mutable state, no goroutines. |
| Error Handling | N/A | No I/O, parsing, or external calls in this file; `clusterConcepts`/`renderMap` are total functions over their inputs. |
| Resources | N/A | No file handles, connections, locks, or caches. |
| Boundaries | PASS | Traced the untested `assignClusterFilenames` residual-collision loop (`vaultmaps.go:285-290`) against a constructed 3-way scenario where all three concept-cluster keys share a 12+-character prefix (forcing 2 loop iterations): the algorithm still resolves to 3 distinct paths, because concept-cluster `Key` values are always distinct (each is a component's unique smallest member id) and the loop's `n >= len(Key)` fallback eventually anchors on the full, unique key. Also traced the `base == ""` fallback (empty/all-whitespace concept `Name`): `Title` is still built via `cleanInline(name)` regardless of the base-empty branch, so no raw untrusted name reaches the rendered output even on this path. Traced `miscBuckets` at the exact cap boundary (50, 51, 100) via the existing 120/237-concept tests — bucket count and per-bucket cap hold. No defect found in any of these traces, but see the coverage gap above — these paths are correct by inspection, not by an executed assertion. |
| Security | PASS | `internal/cli/vaultmaps.go:265` is the only site in the file that reads `Concept.Name` (untrusted); it is immediately routed through `cleanInline` (→ `Title`) and `sanitizeFilename` (→ filename base) at `vaultmaps.go:266-267`, never used raw. `renderMap`'s member/timeline/out-link text is drawn entirely from `VaultRefs` (pre-sanitized upstream), never from raw `Concept`/`Event` fields. Traced `TestDW_4_3_FilenameSanitized`'s adversarial name (`` Weird/Name*Test"? ``) end-to-end through `sanitizeFilename` → confirmed no FS-illegal character reaches `RelPath`. |

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-pseudocode-programming | Pseudocode present, at intent-level (not target-language syntax), detailed enough that code generation is "nearly automatic" | PASS | `vaultmaps.go:17-43` is a package-level pseudocode header written in plain English (e.g. "visit concept ids in ascending order; walk each unvisited one's component via its (already-sorted) RelatedIDs adjacency..."), and it traces line-for-line onto the actual implementation (`findComponents`, `miscBuckets`, `assignClusterFilenames`) — no target-language leakage into the pseudocode. |
| cc-pseudocode-programming | An alternative approach was weighed | N/A | Not verifiable from code alone (would require the excluded design/discovery narrative); no code-level evidence either way, so this is not demonstrable as a violation. |
| cc-control-flow-quality | Max nesting depth ≤ 3 (Yourdon 1986a) | **FAIL** | `findComponents`, `vaultmaps.go:159-179`: `for _, start := range ids` (depth 1) → `for len(queue) > 0` (depth 2) → `for _, nb := range conceptByID[id].RelatedIDs` (depth 3) → `if !visited[nb] {` (depth 4, `vaultmaps.go:171-174`). This is 4 levels of block nesting, one past the stated threshold. Demonstrated, not hypothetical: the neighbor-visited check genuinely sits inside three enclosing loops. Straightforward fix available per the skill's own least-invasive option (#2, "extract nested code into a well-named routine") — e.g. pull the `for _, nb := range ...` block into a small `enqueueUnvisited(queue, visited, neighbors)` helper, which would drop this to 3 levels. |
| cc-control-flow-quality | McCabe complexity ≤ 10 per routine | PASS | Every Phase-4 function is well under the threshold; the most complex (`assignClusterFilenames`) has roughly 8-9 decision points by hand count (switch + 2 cases, 2 `if`s with one `&&`, one `for`-with-condition, one nested `if`/`else`), none approaching the 10-20 "strong justification" band. |
| cc-control-flow-quality | Guard clauses used for early-exit / error conditions | PASS | `wikilink` (`vaultmaps.go:363-369`) guards on the refs-miss case and returns immediately; `findComponents`' `if visited[start] { continue }` (`vaultmaps.go:160-162`) is a clean guard clause. |
| cc-control-flow-quality | Loop construct matches iteration-known-ahead-of-time vs not | PASS | `for _, x := range slice` used throughout for known-length iteration; the one open-ended case (`findComponents`' BFS queue drain, `miscBuckets`' chunking) correctly uses a conditional `for len(...) > 0` / `for len(flat) > 0` rather than a `for range`. |

## Notes (non-blocking)
- `assignClusterFilenames`'s comment block (`vaultmaps.go:232-244`) is unusually thorough and matches the code precisely — good documentation practice, not a finding.
- The residual-suffix loop's final fallback (`r.cl.Key + "-" + strconv.Itoa(n)`, `vaultmaps.go:289`ish) appends a numeric suffix to the already-unique full key, which by my trace is redundant once the full key is in play (concept-cluster keys are always distinct) — harmless defensive belt-and-suspenders, not a bug, but also dead-in-practice for concept clusters specifically. Worth a comment noting why it's there if a future change makes `Key` non-unique.
- `renderMap`'s untested nil-`OccurredAt` timeline branch (see Test-DW Coverage) is a one-line addition to close: add an `Event` fixture with `OccurredAt: nil` to `TestDW_4_3_MapContentMembersTimelineOutLinks` (or a small dedicated test) and assert the timeline line renders without a leading date.

## Issues (if FAIL)
1. Nesting depth exceeds the loaded cc-control-flow-quality skill's stated 3-level threshold.
   - File: `internal/cli/vaultmaps.go:159-179` (specifically the `if !visited[nb]` at line 171, 4 levels deep)
   - Demonstrated by: direct brace-nesting count, confirmed by re-reading the function (see Loaded-Skill Criteria table above); no test needed to demonstrate a structural nesting-depth violation.
   - Fix: extract the innermost neighbor-enqueue loop into a small named helper (e.g. `enqueueUnvisited(id string, conceptByID map[string]Concept, visited map[string]bool, queue []string) []string`), dropping the function to 3 levels of nesting.
2. Test Coverage Level (stated as 100% in the dispatch) is not met for two Phase-4 functions.
   - File: `internal/cli/vaultmaps.go:268-270` and the `for n := 8; ...` loop body around `vaultmaps.go:285-290` (`assignClusterFilenames`, 87.5%); the `else` branch around `vaultmaps.go:394-396` (`renderMap`, 95%).
   - Demonstrated by: `go tool cover -func=/tmp/rev-p4-cover.out` (see Executed Results) — these are 0-count blocks in the coverage profile, i.e., never executed by the current suite.
   - Fix: add (a) a concept whose `Name` sanitizes to `""` (e.g. all-whitespace) to exercise the `base == ""` → `"map"` fallback; (b) a 3-way filename collision where all keys share an 8+-character prefix, to force the residual-suffix loop's body to actually run at least once; (c) a timeline entry sourced from an event with `OccurredAt: nil` rendered through `renderMap`.

**Verdict: FAIL — blockers: (1) demonstrated 4-level nesting violation of the loaded cc-control-flow-quality skill in `findComponents` (vaultmaps.go:159-179); (2) stated 100% Test Coverage Level not met — `assignClusterFilenames` at 87.5%, `renderMap` at 95%, with three specific untested branches identified above. All 4 Done-When items (DW-4.1 through DW-4.4) themselves PASS with execution evidence, and no correctness defect was found in the untested branches by hand-trace — the FAIL is on the skill criterion and the stated coverage bar, not on unmet functional requirements.**

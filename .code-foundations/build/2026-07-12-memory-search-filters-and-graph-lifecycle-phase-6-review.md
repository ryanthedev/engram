# Review: Phase 6 - Honest k (separate expanded block)

## Executed Results (Step 0)
- Test suite: `go test ./...` (also re-run `-count=1 -race` for the phase-6-touched packages: internal/graph, internal/mcp, internal/retrieval, internal/server, internal/engramclient) → all packages `ok`, zero failures, zero race reports.
- Typecheck/vet: `go vet ./...` → clean (part of `make lint`).
- Lint: `make lint` → `go vet ./...` clean, `revive -config revive.toml -set_exit_status -exclude ./api/engrampb/... ./...` clean.
- Build: `go build ./...` → clean.
- Proto: `make proto-check` (runs `./scripts/codegen.sh` via `go run buf@v1.55.1 generate`, then `git diff --exit-code -- api/engrampb`) → `codegen: ok`, no diff — the checked-in `api/engrampb/engram.pb.go` matches what regeneration from `api/proto/engram.proto` produces.

## Requirement Fulfillment

### DW-6.1
PREMISE:  The returned `hits` array contains no element with `Source == "graph"` and has length <= k, with expansion enabled at depth 2.
EVIDENCE: `internal/retrieval/opensearch.go:100-112` (`SplitExpanded`); `internal/graph/honestk_test.go:93-136` (`TestDW_6_1_HonestKAtDepth2`); `internal/retrieval/splitexpanded_test.go:37-55` (`TestDW_6_1_SplitExpandedCapsMatchedAtK`)
TRACE:    Real `Store` chain A→works_at→B→located_in→C, expander at depth 2 registered as post-hook "graph", k=2, 3 seed hits available → `MultiRetriever.Search` fuses+truncates to k=2, post-hook appends graph hits after truncation → `SplitExpanded(hits, 2)` returns `matched` len==2, none with `Source=="graph"`, and a non-empty `expanded` block (test asserts `len(expanded)==0` would itself fail the test, proving expansion genuinely ran).
VERDICT:  PASS

### DW-6.2
PREMISE:  Graph expansions are returned in a distinct `expanded` block, never mixed into `hits`.
EVIDENCE: `internal/server/server.go:176-191` (`Search` calls `SplitExpanded` and assigns `Hits: matchedPB, Expanded: expandedPB`); `api/proto/engram.proto:204-213` (`SearchResponse.hits` / `.expanded` as separate repeated fields); `internal/mcp/render.go:238-257` (`compactLines` renders expansions below a header, never interleaved); `internal/mcp/expanded_test.go:33-67` (`TestDW_6_2_SearchResultRendersExpandedSection`)
TRACE:    backend returns 1 matched semantic hit + 1 graph hit → wire response decodes to `hits: [1]`, `expanded: [1]`, the matched entry never carries `source:"graph"`; compact-line text is 3 lines: matched hit, a header line naming "expanded" and "not counted against k", then the expansion line.
VERDICT:  PASS

### DW-6.3
PREMISE:  Zero expansions ⇒ no `expanded` block emitted.
EVIDENCE: `internal/retrieval/opensearch.go:97-99` (`SplitExpanded` returns `expanded` as nil, never empty-non-nil); `internal/mcp/budget.go:41-58` (`searchResult.Expanded` tagged `omitempty`); `internal/mcp/render.go:55-56` (`renderedResult.Expanded` `omitempty`); `internal/mcp/expanded_test.go:78-102` (`TestDW_6_3_ZeroExpansionsOmitsExpandedKey`); `internal/retrieval/splitexpanded_test.go:114-122` (`TestDW_6_3_SplitExpandedReturnsNilExpanded`)
TRACE:    backend returns 1 hit and no expansions → structuredContent decodes with no `expanded` key and no `expanded_omitted` key present at all (test explicitly checks `_, present := decoded["expanded"]` is false); marshaled `searchResult` never contains the literal substring `"expanded"`.
VERDICT:  PASS

### DW-6.4
PREMISE:  The MCP byte budget accounts for the `expanded` block separately; matched hits pack first and overflow spills via the existing path.
EVIDENCE: `internal/mcp/tools.go:300-312` (`callSearch`: `packAndSpill` runs first against the whole budget, `packExpanded` runs second against the leftover); `internal/mcp/budget.go:108-147` (`packExpanded` docstring + implementation: zero floor, never evicts a match); `internal/mcp/expanded_test.go:105-118, 141-167, 190-222` (`TestSearchUnsetKStillBoundsMatchedHits`, `TestDW_6_4_MatchedHitsPackFirstExpansionsDroppedFirst`, `TestDW_6_4_MatchedOverflowStillSpills`)
TRACE:    (a) budget sized to hold exactly 2 matched hits and nothing else → `packExpanded` returns `len(Hits)==2`, `Omitted==0`, `len(Expanded)==0`, `ExpandedOmitted==2` — expansions dropped wholesale, no match evicted. (b) 6 matched hits over a 700-byte budget plus 1 expansion → matched hits spill to disk, `overflow_path` present and the file exists on disk, `expanded` empty, `expanded_omitted==1` — the existing spill path is untouched and expansions get nothing once matched hits are already under pressure.
VERDICT:  PASS

### DW-6.5
PREMISE:  The stale comment in `internal/graph/expand.go` ("the retriever itself does not re-truncate post-hook additions (Phase 4 design)") is corrected to state the new contract.
EVIDENCE: `internal/graph/expand.go:276-288` (`edgeHit`'s doc comment now states the `expanded`-block/`SplitExpanded` contract and explicitly narrates the reversal: "This is a knowing reversal of the earlier \"Phase 4 design\" note that lived here"); `internal/graph/honestk_test.go:162-181` (`TestDW_6_5_PostHookContractCommentIsCurrent`)
TRACE:    test reads `expand.go` source text, fails if it still contains the literal substring `(Phase 4 design)` as a standalone stale marker, and fails unless it contains both `` `expanded` block `` and `SplitExpanded`. Ran green — file contains the new contract's mention only inside the "knowing reversal" retrospective sentence, and both required substrings are present.
VERDICT:  PASS

### DW-6.6
PREMISE:  `make proto` is run, the regenerated `api/engrampb/*.pb.go` are present, and `make proto-check` passes.
EVIDENCE: `api/proto/engram.proto:200-213` (`SearchResponse.expanded = 2`); `api/engrampb/engram.pb.go:568,610-614` (`Expanded []*Hit` field + `GetExpanded()`); `git status` shows `api/engrampb/engram.pb.go` and `api/proto/engram.proto` staged as modified together.
TRACE:    ran `make proto-check` directly → `./scripts/codegen.sh` executed `buf generate` against the current `.proto`, then `git diff --exit-code -- api/engrampb` reported no diff — the committed generated code is byte-identical to a fresh regeneration, i.e. it was genuinely regenerated from this phase's proto changes, not hand-edited out of sync.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] All 6 DW items have corresponding tests that ran green in Step 0 (`TestDW_6_1_*`, `TestDW_6_2_*`, `TestDW_6_3_*`, `TestDW_6_4_*`, `TestDW_6_5_*` across `internal/graph`, `internal/retrieval`, `internal/mcp`, `internal/server`, `internal/engramclient`; DW-6.6 verified via direct `make proto-check` execution, which is the mechanical, non-unit-testable form of that requirement).
- [x] Coverage matches the stated 100% level — every DW item has at least one dedicated test, most have 2-3 across layers (graph → retrieval → server → mcp → engramclient), consistent with the phase touching every layer of the pipeline.

## Dead Code
None found. `go vet` and `revive` (which includes unused-import/unreachable-code style checks) both ran clean over the full module. Manual scan of the diffed files for `TODO`/`FIXME`/debug prints found none.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | `MultiRetriever.Search` (opensearch.go:341-364) still fans tiers out via goroutines + `sync.WaitGroup`; the post-hook loop (opensearch.go:398-404) that Phase 6 relies on runs strictly sequentially after `wg.Wait()`, so no new shared-mutable-state hazard was introduced. Confirmed via `go test -race ./internal/graph/... ./internal/mcp/... ./internal/retrieval/... ./internal/server/... ./internal/engramclient/...` — clean. |
| Error Handling | PASS | `toHitsPB` (server.go:197-215) returns an `Internal` error on a marshal failure for either the `hits` or `expanded` block, propagated identically for both — traced: a hit whose `Fields` contains an unmarshalable value (e.g. a channel) would error `toHitsPB(matched)` before `toHitsPB(expanded)` is even reached, failing the whole RPC rather than silently dropping the expanded block. |
| Resources | N/A | No new file handles, connections, locks, or caches introduced by this phase; the existing spill-to-disk path (unaffected, per DW-6.4's evidence) is Phase-2/3 territory, reviewed previously. |
| Boundaries | PASS | k=0/unset (`TestSearchUnsetKStillBoundsMatchedHits`, `TestDW_6_1_SplitExpandedNormalizesKLikeTheRetriever`) and negative k both re-derive `clampK` identically to the retriever's own clamp — traced: k=-7 with 15 matched candidates → `SplitExpanded` clamps to `DefaultK` (10), `len(matched)==10`, never a panic or an unbounded return. Zero-hit and all-expansion inputs (`TestSplitExpandedEmptyInputs`, `TestSplitExpandedAllExpansions`) traced clean: nil in → nil,nil out; all-graph in → empty matched (never promoting a graph hit to fill the gap), full expanded. |
| Security | N/A | This phase adds no new untrusted-input parsing surface; `Filter.Sources`/`Predicates` validation is Phase 4/5 territory (explicitly out of scope per the dispatch's Dependency note) and was not touched here beyond routing through the existing validated path. |

## Loaded-Skill Criteria
`code-foundations:aposd-designing-deep-modules` was loaded per the dispatch's `## Additional Skills`. This skill's own instructions gate a *pre-implementation* design-it-twice workflow (generate 2-3 alternatives before writing code) — there is no code in this diff to which that workflow could retroactively apply, and the skill defines no other criterion checklist to probe already-written code against. I applied its **Depth Evaluation** and **Information Hiding** framing to the shipped interfaces as the closest applicable read:

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| aposd-designing-deep-modules | Interface simplicity: `SplitExpanded(hits, k)` is a single free function, not a stateful type or a family of methods | PASS | `internal/retrieval/opensearch.go:100-112` — two inputs, two outputs, no caller-visible internal state. Both `MultiRetriever.Search`'s post-hook contract and `packExpanded`'s pack-second/zero-floor rule are documented as deliberate, narrow additions rather than a new configuration surface. |
| aposd-designing-deep-modules | Information hiding: callers never learn how expansion is detected | PASS | Every caller (server.go, tools.go, render.go) discriminates purely on `Hit.Source == retrieval.ExpandedSource`, a single exported constant — no caller re-implements the graph-vs-match distinction or reaches into `Expander` internals. |
| aposd-designing-deep-modules | Single-use-method red flag: is `packExpanded` a general packer or a one-off bolted onto `packAndSpill`? | Note (non-blocking) | `packExpanded` is a second, subordinate pass explicitly coupled to an already-packed `searchResult` — a deliberate, documented "pack matched first, expand into the remainder" design, not an accidental single-use method. Not a violation, but worth naming: it is the shallower of the two designs the discovery doc could have chosen (a unified single-pass packer over both blocks was the alternative) in exchange for the stronger "expansions can never evict a match" guarantee, which is exactly what DW-6.4 asks for. This is a design trade-off, not a demonstrated defect. |

No FAIL-worthy skill violation found — the loaded skill's design-time workflow does not apply retroactively to a completed diff, and its depth/information-hiding lens found nothing to fault on the shipped surface.

## Notes (non-blocking)
- **Independently verified claim** (dispatch's "Specific claim to verify independently"): the claim that `internal/graph/expand_test.go`'s `semanticHit` helper omits `predicate`, and that tests built on it therefore do not exercise the seed-echo guard, is **half true and stale**. `semanticHit` (expand_test.go:16-24) does indeed omit `predicate`. However, a second helper, `tripleHit` (expand_test.go:237-241), explicitly adds `predicate` and is documented as existing *because* `semanticHit` cannot exercise the guard. `TestDW_2_3_SeedEdgeNotReturnedAsExpandedHit` (expand_test.go:252-286) is built on `tripleHit` and asserts the seed's own edge fingerprint (`abEdgeID`) never appears in the expansion output — a real, positive assertion that the guard fires, not a vacuous pass. Git history confirms `tripleHit` and this test were added in commit `89e9300` ("fix(graph): close superseded edges and repair the inert echo guard"), which predates Phase 6 and is unmodified by this phase's diff (`git diff HEAD -- internal/graph/expand_test.go` is empty). Phase 6's own new test suite (`honestk_test.go`) independently reinforces this: it uses `tripleHit`, not `semanticHit`, exactly because (per its own header comment) "without the predicate ... the seed's edge would come back as a bogus 'expansion'." **Conclusion: the echo guard is genuinely exercised, not merely nominally covered — the claim as stated describes a gap that was already closed before this phase began.**
- `packExpanded`/`withExpandedFits` re-marshal the candidate result on every shrink iteration (same pattern as the pre-existing `packSearchResult`/`searchResultFits`) — O(n²) in the number of expansions considered, bounded tightly by `DefaultMaxAdded=20`, so not a practical concern.
- The `## Edge cases` list's five items were each independently found covered by a dedicated test, not merely incidentally exercised: zero expansions (`TestDW_6_3_ZeroExpansionsOmitsExpandedKey`, `TestDW_6_3_SplitExpandedReturnsNilExpanded`), budget/spill interaction (`TestDW_6_4_MatchedHitsPackFirstExpansionsDroppedFirst`, `TestDW_6_4_MatchedOverflowStillSpills`), `sources` excluding graph (`TestSourcesExcludingGraphYieldsNoExpansion`), a non-expander post-hook (`TestSearchNonExpanderPostHookHitsStayInHits`, `TestSplitExpandedDoesNotMisfileNonGraphPostHookHits`), and k=0/unset (`TestSearchUnsetKStillBoundsMatchedHits`, `TestDW_6_1_SplitExpandedNormalizesKLikeTheRetriever`).

## Issues (if FAIL)
None.

**Verdict: PASS**

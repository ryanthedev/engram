# Discovery + Design: Phase 6 - Honest `k` — separate `expanded` block

## Files Found

| File | Role in this phase |
|---|---|
| `internal/retrieval/opensearch.go` | `MultiRetriever.Search` (`:250-376`): truncates `merged` to `q.K` at `:344-346`, THEN runs post-hooks (`:352-358`) that append graph hits. `clampK` (`:58`), `DefaultK=10`, `MaxK=100`. |
| `internal/graph/expand.go` | `Expander.Apply` → `Expand`: returns `append(seedHits, added...)`. `edgeHit` (`:274`) stamps `Source: "graph"`. The stale comment is at **`:279`** (plan said ~239 — it moved in Phase 2). |
| `internal/server/server.go` | `Search` (`:139-188`): calls `s.Retriever.Search`, marshals every hit into `SearchResponse{Hits}`. The designated split point. |
| `api/proto/engram.proto` | `SearchResponse` at `:200-203` — `repeated Hit hits = 1;` only. |
| `internal/engramclient/client.go` | `Search(ctx, query, k, mcp.SearchFilter) ([]mcp.Hit, error)` (`:230-259`). |
| `internal/mcp/mcp.go` | `Hit`, `SearchFilter`, `Backend.Search(...) ([]Hit, error)`. |
| `internal/mcp/budget.go` | `searchResult` envelope, `packSearchResult` (measure-the-real-marshal shrink loop, one-hit floor), `searchByteBudget`, `topFacets`, `refineHint`. |
| `internal/mcp/tools.go` | `callSearch` (`:285`), `packAndSpill` (`:318`) — shared with `knowledge_search`. |
| `internal/mcp/render.go` | `renderedResult`, `renderSearchResult`, `compactLines`. `hitDisplayFields["graph"]` already exists. |
| `internal/mcp/spill.go` | `spillFullResult(hits []Hit)` + `maxSpillPath()` headroom reservation. |
| `internal/cli/cli.go:272` | The only non-MCP `Backend.Search` caller. |

## Current State

`MultiRetriever.Search` authorizes → sorts → truncates to `clampK(q.K)` → runs post-hooks → re-authorizes → projects. The graph expander appends up to `maxAdded` (20) hits with `Source == "graph"` after the truncation, so a caller asking for `k=20` can receive 40 in one flat array. Everything downstream (`server.Search`, `engramclient`, `mcp`) treats that array as a single undifferentiated list; the MCP byte budget packs graph hits and matched hits with equal priority, and a graph hit can evict a matched hit from the packed page.

## Gaps (plan vs reality)

| Gap | Resolution |
|---|---|
| Plan cites the stale comment at `expand.go:239`. It is now at **`:279`** (Phase 2 edits shifted it). | Same comment, same text (`"the retriever itself does not re-truncate post-hook additions (Phase 4 design)"`). Edit at the real line. Not a plan break. |
| Plan's `Produces` says `mcp.Backend.Search(...) (SearchResult, error)` but does not mention `engramclient.Search`'s return type. | It must change in lockstep — `*engramclient.Client` is the production `mcp.Backend`. In scope (`internal/engramclient/**`). |
| Plan does not say whether dropped expansions are reported. | Silent drop is an APOSD "Silent Failure" red flag. Adding `expanded_omitted` (int, omitempty) — see Design D3. |
| `len(hits) <= k` is *already* true today for the graph post-hook (truncation precedes it). | The guarantee is incidental, not enforced. Phase 6 makes it a stated, tested contract that also survives a future non-graph post-hook (an explicit Edge Case). |

No blockers. No prerequisite gaps: Phases 2 and 5 are committed.

## Code Standards

Applied from `docs/code-standards.md`:
- **No transport types in business packages** — `engrampb` may be imported only at `internal/server` / `cmd/`. The split helper therefore lives in `internal/retrieval` speaking `retrieval.Hit`, and `internal/server` translates to `engrampb.Hit`.
- **gRPC handlers translate domain errors to status codes** — unchanged; `Search`'s existing `ErrInvalidFilter → InvalidArgument` mapping is untouched.
- Doc comments state the *contract and the why*, not the how (the prevailing style in every file touched).
- Tests: table-driven where it fits, `t.Fatalf` with the observed value, DW-ID in the name for traceable items.

## Test Infrastructure

Standard `go test` + `testing`, no external assert library. Fakes live in `_test.go` files in the package under test (`fakeRetriever` in `internal/server/server_test.go:54`, `fakeBackend` in `internal/mcp/mcp_test.go:32`, `searchCapture` gRPC server in `internal/engramclient/search_test.go:26`). `internal/graph/expand_test.go:209` already builds a **real** `MultiRetriever` with a registered `graph` post-hook and a fake seed tier over an unreachable OpenSearch — that is the existing harness for a genuine depth-2 expansion test. Integration tests are build-tagged and out of bounds here (`make integration` not to be run).

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-6.1 | `hits` contains no `Source == "graph"` element and `len(hits) <= k`, with expansion enabled at depth 2 | COVERED | `retrieval.TestDW_6_1_SplitExpandedCapsMatchedAtK`, `graph.TestDW_6_1_HonestKAtDepth2` (real `MultiRetriever` + real `Expander` at depth 2, `K=2`, expansions present), `server.TestDW_6_1_SearchHitsExcludeGraphAndRespectK` |
| DW-6.2 | Graph expansions are returned in a distinct `expanded` block, never mixed into `hits` | COVERED | `retrieval.TestDW_6_2_SplitExpandedPartitionsByGraphSource`, `server.TestDW_6_2_SearchExpandedBlockCarriesGraphHits`, `engramclient.TestDW_6_2_SearchCarriesBothBlocks`, `mcp.TestDW_6_2_SearchResultRendersExpandedSection` |
| DW-6.3 | Zero expansions ⇒ no `expanded` block emitted | COVERED | `server.TestDW_6_3_NoGraphHitsEmitsNoExpandedBlock`, `mcp.TestDW_6_3_ZeroExpansionsOmitsExpandedKey` (asserts the marshaled JSON has no `expanded` key), `retrieval.TestDW_6_3_SplitExpandedReturnsNilExpanded` |
| DW-6.4 | MCP byte budget accounts for `expanded` separately; matched hits pack first, overflow spills via the existing path | COVERED | `mcp.TestDW_6_4_MatchedHitsPackFirstExpansionsDroppedFirst` (tight budget: every matched hit that fits is kept, `expanded` is empty, `expanded_omitted` reports the drop), `mcp.TestDW_6_4_ExpandedFillsLeftoverBudget`, `mcp.TestDW_6_4_MatchedOverflowStillSpills` (`overflow_path` written, hits omitted, budget still honored with `expanded` present) |
| DW-6.5 | The stale `expand.go` comment is corrected to state the new contract | COVERED | `graph.TestDW_6_5_PostHookContractCommentIsCurrent` — reads `expand.go`, asserts the stale phrase is gone and the new contract phrase is present (a real assertion, not a manual check) |
| DW-6.6 | `make proto` run, regenerated `api/engrampb/*.pb.go` committed, `make proto-check` passes | COVERED | Command evidence: `make proto && make proto-check` exit 0 with `expanded` present in `api/engrampb/engram.pb.go`; plus `engramclient.TestDW_6_2_SearchCarriesBothBlocks` exercises the generated field over a real gRPC loopback. |

**DW-ID count: 6 in prompt, 6 in table. All items COVERED:** YES

Additional (past the DW floor, from the plan's Edge cases and the implementation):
- `sources` excluding graph ⇒ no `expanded` block at all (`graph.TestSourcesExcludingGraphYieldsNoExpansion`).
- A **non-expander** post-hook's hits are NOT misfiled into `expanded` (`retrieval.TestSplitExpandedDoesNotMisfileNonGraphPostHookHits`) — and, critically, still get capped at `k`.
- `k` bounds: `k=0` (unset), `k` > `MaxK`, `k` negative — `SplitExpanded` must use the SAME clamp the retriever used or the cap is a lie.
- CLI renders the expanded block separately.
- Zero-hit and nil-hit inputs never panic.

## Design Decisions

### Design: the matched/expanded split

**Approaches Considered**
1. **A — inline in `server.Search`.** A loop in the gRPC handler partitions on `h.Source == "graph"` and truncates to `k`.
2. **B — `retrieval.SplitExpanded(hits []Hit, k int) (matched, expanded []Hit)`.** A named contract exported by the package that already owns both the `"graph"` source name and the `k` clamp; `server.Search` calls it once.
3. **C — widen `retrieval.Retriever.Search` to return a two-block result.** Explicitly OUT per the plan's Constraints (`eval.NullRetriever` also implements the interface).

**Comparison**

| Criterion | A (inline) | B (`SplitExpanded`) | C (widen interface) |
|---|---|---|---|
| Interface simplicity | 0 new symbols, but the rule is invisible | 1 exported func + 1 exported const | Breaks 2 implementors, 6+ call sites |
| Information hiding | **Leaks**: the `"graph"` literal and the `clampK` rule (which the server does not otherwise know) migrate into the transport edge | The expanded-source identity and the clamp semantics stay in the package that defines them; the server knows only "split this" | Best hiding, but forbidden by plan |
| Caller ease of use | Server must re-derive `clampK(0) == DefaultK` — a duplicated rule that can silently drift | One call; the `k` bound is guaranteed by construction | n/a |
| Testability of the contract | Only reachable through a gRPC handler test | Directly unit-testable (`k=0`, `k>MaxK`, non-graph post-hook hits, nil input) | n/a |
| Blast radius | 1 file | 2 files | 6+ files, plan violation |

**Choice: B.**
Rationale: A's real cost is not the loop, it is the **information leakage** — `server.Search` would have to know both that `"graph"` is the expanded source *and* that an unset `k` means `DefaultK` (a rule `MultiRetriever` applies internally via `clampK`, which the server never sees). Duplicating the clamp is exactly how `len(hits) <= k` becomes a lie for `k=0`. B keeps both facts in `internal/retrieval` — one function, one exported constant (`retrieval.ExpandedSource`) — and reduces the server to a single call plus its existing proto marshal. Sacrificed: one more exported symbol in `retrieval`. Accepted.

**Depth check (B)**
- Interface methods: 1 (`SplitExpanded`) + 1 const (`ExpandedSource`).
- Hidden details: which source name post-hook expansions carry; the `k` normalization (`clampK`: `<=0 → DefaultK`, `>MaxK → MaxK`); the order-preserving partition; the defensive cap that holds even for a future non-expander post-hook.
- Common case complexity: **simple** — `matched, expanded := retrieval.SplitExpanded(hits, q.K)`.

### D2 — `mcp.SearchResult`, not a second return value

`Backend.Search` returns `(SearchResult, error)` (plan `Produces`), not `([]Hit, []Hit, error)`. A named struct with two labeled slices cannot be transposed by a caller; two same-typed positional returns can. It also leaves room for a future third block without another signature break.

### D3 — Budget: pack matched first, then fill with expanded (never the reverse)

`packSearchResult` runs on the **matched hits only**, byte-for-byte as it does today — so the existing budget/spill/one-hit-floor/`overflow_path`-headroom contracts (owned by the 2026-07-09 response-shaping and 2026-07-11 drilldown plans) are untouched and their tests hold unchanged. Only *after* `packAndSpill` has finished (including attaching the real `overflow_path`) does a new `packExpanded` append the longest prefix of `expanded` whose addition keeps the **actual marshaled** result within the same budget. Consequences, all intended:

- Matched hits get the entire budget; expansions live on the leftover ⇒ **expansions are structurally the first thing dropped** and can never evict a match (DW-6.4).
- Measuring the true marshal (not an estimate) mirrors `searchResultFits`' existing discipline; the reservation trick is unnecessary here because `overflow_path` is already real by then.
- Expansions have a **zero floor** (unlike matched hits' unconditional one-hit floor): under pressure the whole block vanishes.
- Dropped expansions are reported as `expanded_omitted` rather than vanishing silently (APOSD: hide the mechanism, never the failure). They are *not* spilled — the spill contract is the matched-hit escape hatch, and expansion is bonus context whose seeds are already in the response.

### D4 — Rendering: a delimiter line, not an interleave

`renderedResult` gains `Expanded []renderedHit` / `ExpandedOmitted int` (both `omitempty`). `compactLines` emits matched lines, then the existing omission line, then — only when expansions survived — one delimiter line naming the block and stating it is not counted against `k`, then the graph lines (which already have a `hitDisplayFields["graph"]` projection). One extra line is the entire token cost of making the block unambiguous.

## Prerequisites

- [x] Phase 2 committed (`Source == "graph"` hits are the expander's, echo guard repaired)
- [x] Phase 5 committed (`mcp.SearchFilter`, `Backend.Search(ctx, query, k, f)`)
- [x] `scripts/codegen.sh` regenerates via version-pinned `go run` (no host `buf` install needed)
- [x] `retrieval.Hit`, `clampK`, `DefaultK`, `MaxK` all present

## Recommendation

**BUILD.**

1. `internal/retrieval/opensearch.go`: add `ExpandedSource` const + `SplitExpanded`.
2. `internal/graph/expand.go`: correct the `:279` post-hook comment to the new contract.
3. `api/proto/engram.proto`: `SearchResponse.expanded = 2`; `make proto`.
4. `internal/server/server.go`: split at the boundary, marshal both blocks.
5. `internal/mcp/mcp.go`: `SearchResult`; `Backend.Search` returns it.
6. `internal/engramclient/client.go`: carry both blocks.
7. `internal/mcp/budget.go` + `tools.go` + `render.go`: `expanded` as a separately-budgeted, separately-rendered section.
8. `internal/cli/cli.go`: print the expanded block separately.
9. Tests per the DW table; `make test`, `make lint`, `make build`, `make proto-check`.

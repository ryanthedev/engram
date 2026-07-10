# Discovery + Design: Phase 2 - Budget-pack + facets + refine hint at MCP

## Files Found
- `internal/mcp/mcp.go` — `Hit` (id/score/source/fields_json string), `Backend` interface (`Search(ctx, query, k) ([]Hit, error)`), `Server`/`Serve`/dispatch.
- `internal/mcp/tools.go` — `callSearch` currently forwards `args.K` straight to `Backend.Search` and wraps `{"hits": hits}` via `toolResult`. No budget/facet/hint logic exists yet.
- `internal/mcp/mcp_test.go` — `fakeBackend` (in-memory `Backend`) + `refClient` (io.Pipe-driven JSON-RPC client) conformance pattern. Existing `TestDW_3_5_ConformanceCallTool` calls `memory_search` with `k:5` and expects exactly 1 hit in `structuredContent.hits` — must keep passing.
- `internal/engramclient/client.go` — real `Backend` impl; `Search` copies `engrampb.Hit.FieldsJson` verbatim into `mcp.Hit.Fields` (a JSON *string*, already slim per Phase 1). Confirms facet fields (`subject`/`predicate`/`kind`) live inside that JSON string, not as first-class `Hit` fields — need to `json.Unmarshal` per hit to read them.
- `internal/retrieval/opensearch.go` — Phase 1's `DefaultK=10`, `MaxK=100`, `clampK` already clamp per-tier query size server-side; the real `Backend` (gRPC) never sees an unclamped `k`, so MCP doesn't need to re-clamp defensively for correctness, only choose its own default.
- `cmd/engram-mcp/main.go` — process entrypoint; has an `envOr` helper but budget/spill-dir env vars are file-scoped to `internal/mcp/**` per the plan, so I read `ENGRAM_MCP_SEARCH_BUDGET_BYTES` inside the `mcp` package itself (fresh `os.Getenv` per call — plays well with `t.Setenv` in tests, no init-time caching to invalidate).
- `internal/cli/export.go:461` — atomic-write precedent (`os.CreateTemp`+`os.Rename`), referenced for Phase 3, not touched here.

## Current State
`callSearch` is a thin passthrough: unmarshal args, forward `k` (even 0/negative) to the backend, wrap the raw hits in `{"hits":[...]}`. No size bound, no default request size beyond whatever the backend/retrieval layer defaults to, no omission reporting.

## Gaps
- No default `k` at the MCP layer (plan wants 50, distinct from retrieval's `DefaultK=10`) — MCP should ask for more hits than it expects to return so the packer has real candidates to choose from.
- No byte-budget packer, no env-configurable budget, no facet/hint computation, no `searchResult` envelope type.

## Code Standards
From `docs/code-standards.md`: return errors, never panic in library code (defensive validation must degrade, not crash); table-driven tests; every code-touching phase ships ≥1 dirty test; consumer-defined interfaces, vendor types stay out of public signatures (already true — `Backend`/`Hit` are MCP-owned).

## Test Infrastructure
`internal/mcp/mcp_test.go`'s `fakeBackend` + `refClient` (io.Pipe, real JSON-RPC framing) is the conformance pattern to extend. New unit tests can call `packSearchResult`/`searchByteBudget`/`topFacets` directly (same package, no need to go through the wire) for tight table-driven coverage, plus a couple of conformance-level tests through `refClient` for the end-to-end envelope shape.

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-2.1 | Default `memory_search` (no explicit `k`) → FULL serialized size (hits+envelope) ≤ configured budget | COVERED | `TestDW_2_1_DefaultSearchFitsBudget` (fakeBackend returns many small hits, no `k` arg, assert marshaled tool result ≤ default budget) |
| DW-2.2 | Over-budget → `omitted>0` + non-empty `omitted_facets` + `hint`; all-fit → those fields absent/zero | COVERED | `TestDW_2_2_OmissionFieldsPresentOnlyWhenOmitted` (table: tiny budget → all three present; generous budget → all three absent/zero) |
| DW-2.3 | `ENGRAM_MCP_SEARCH_BUDGET_BYTES` configurable, default 16384, invalid/unset falls back (dirty) | COVERED | `TestDW_2_3_SearchByteBudgetFromEnv` (table: unset→16384, `"8000"`→8000, `""`/`"0"`/`"-5"`/`"abc"`→16384 dirty cases) |
| DW-2.4 | Single over-budget hit still emitted (no empty page when hits exist) | COVERED | `TestDW_2_4_SingleOverBudgetHitStillEmitted` (one huge hit, budget=1 → `len(Hits)==1`, no panic) |
| DW-2.5 | Facet counts computed over omitted set, stable ordering on ties | COVERED | `TestDW_2_5_TopFacetsStableOnTies` (equal-count values → first-encountered wins, deterministic across repeated runs) |
| DW-2.6 | No `.proto` diff; `buf breaking` clean | COVERED | verified via `git status --porcelain api/` showing no diff (no Go test needed — a repo-state check, per the plan's own note) |

**All items COVERED:** YES

## Design Decisions

**Packing algorithm — shrink-from-full, not grow-from-empty with a fixed headroom estimate.** The plan's constraint says "the packer reserves envelope headroom before packing hits," but the envelope's exact size (facets + hint) depends on *which* hits end up omitted, which depends on how many are packed — a circular dependency if reservation is a fixed a-priori estimate. Instead: start with all hits packed and `omitted=0` (cheapest path, one marshal, covers the common "everything fits" case in O(1) marshals); if the full result is over budget, drop the lowest-ranked hit into the remainder, recompute the *real* facets/hint over that growing remainder, and remeasure the *actual* serialized bytes — repeat until it fits or only one hit remains (force-kept per DW-2.4, unconditionally, without re-checking fit). This satisfies "measure cumulative size against the serialized hit, not an estimate that can drift" literally: every candidate is the real final JSON, not an approximation. Cost is O(n²) marshal+facet passes worst case, trivial at n≤100 (Phase 1's `MaxK`).

**`defaultRequestK = 50` is MCP's own constant, separate from `retrieval.DefaultK = 10`.** They serve different purposes: retrieval's default is "how many to fuse/rank when nothing is specified" (used when the *server* gets an unset k directly); MCP's default is "how many slim candidates to request so the byte packer has enough to choose from" — deliberately larger, per the plan's explicit `=50` value. The real backend (`engramclient.Client.Search`) passes MCP's `k` straight to the gRPC `SearchRequest`, which the server-side retriever still clamps to `[1,100]`, so MCP never needs its own MaxK re-clamp for correctness — it only needs a sensible default when the caller omits `k`.

**Budget env var read fresh per call, not cached at startup.** `os.Getenv` is cheap and this keeps `t.Setenv`-based tests trivial (no server restart needed to pick up a new value) and correctly matches "read at the process boundary" — the "boundary" is the MCP request handling, not process startup, since there's no other config-loading step in this package.

**`omitted_facets` shape: `map[string]string`, one top value per field (subject/predicate/kind).** The plan's `Produces` line pins this literally: `omitted_facets:{field:value,...}` — a flat map, not per-field count lists. Ties are broken by first-encountered order among the omitted hits (which are already in the backend's stable rank order), giving deterministic output without an extra sort.

**New file `internal/mcp/budget.go`** houses `searchByteBudget`, `packSearchResult`, `topFacets`, `refineHint`, and the `searchResult` type — keeps `tools.go`'s `callSearch` a thin orchestrator (functional cohesion: `callSearch` now reads as "parse args → request hits → pack → wrap", each step a named call) instead of growing one large routine that both parses JSON-RPC args and does byte-budget arithmetic.

**Malformed/missing `Fields` JSON in a hit** is treated as "no facet contribution from this hit," not a panic or a dropped hit — `topFacets` skips a hit whose `Fields` fails to unmarshal or lacks a given field, consistent with `cc-defensive-programming`'s "external input, never assert" and Phase 1's own "tolerates a missing field by omitting it" precedent.

## Prerequisites
- [x] Phase 1 shipped (slim, embedding-free `Hit.Fields`) — confirmed via `git log` (commit `bb6e419`) and by reading `internal/retrieval/opensearch.go`'s `projectFields`.
- [x] `internal/mcp/mcp_test.go`'s conformance harness exists and is reusable.
- [x] No missing prerequisites.

## Recommendation
BUILD. The plan's phase body is directly implementable against the existing `internal/mcp` seam; no scope conflicts found. One clarification resolved above (packing-loop shape) stays within the plan's stated constraints (full-serialized-size measurement, envelope headroom, force-keep-one-hit) rather than reinterpreting them.

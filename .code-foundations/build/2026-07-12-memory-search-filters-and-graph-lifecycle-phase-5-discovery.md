# Discovery + Design: Phase 5 — LLM-facing API surface (proto + flat MCP schema)

## Files Found
- `api/proto/engram.proto` — `SearchRequest` (`:137-148`, fields 1-5 incl. `valid_only`), `Predicate`/`Range` (`:334-350`, knowledge's generic shape).
- `api/engrampb/*.pb.go` — generated; `make proto` (`scripts/codegen.sh`, buf v1.55.1 via pinned `go run`) verified working on a clean tree.
- `internal/mcp/tools.go` — `memory_search` schema (`:59-70`, `{query,k}`), `callSearch` (`:264-291`); `readSources` (`:37`) is the precedent for a static, entry-validated source vocabulary.
- `internal/mcp/mcp.go:138` — `Backend.Search(ctx, query string, k int) ([]Hit, error)`; only two implementors: `engramclient.Client` and `mcp_test.fakeBackend`.
- `internal/engramclient/client.go:225` — `Search`, with the hardcoded `ValidOnly: true`.
- `internal/server/server.go:138-172` — `Search`: `SearchRequest` → `retrieval.Query`/`retrieval.Filter`; verified Identity overrides request tenancy.
- `internal/retrieval/filters.go` (Phase 4) — `FieldSpec`/`FilterableFields`, `episodicFilterable` (`kind`, `occurred_at`, `created_at`), `semanticFilterable` (`subject`, `predicate`, `object`, `extractor_version`, `valid_at`, `invalid_at`, `created_at`, `expired_at`), `resolveSources`, `validatePredicates`.
- `internal/retrieval/opensearch.go` — `MultiRetriever.Search` barricade (`:229-`), `tierRetriever.filterClauses` (`:558-596`); `ValidOnly` compiles to the `expired_at`+`invalid_at` clause only where `supportsValidity` (semantic).
- `cmd/engram-server/stages_experience.go:65` — `RegisterTier("experience", …)`; `stages_graph.go:86` — `RegisterPostHook("graph", …)`.

## Current State
`memory_search` is `{query, k}`. `engramclient` hardcodes `ValidOnly: true`; `SearchRequest.valid_only` exists on the wire but has exactly one producer, which always sets it to `true`. Phase 4 built the whole internal filter machinery (`Filter.Predicates`, `Filter.Sources`, per-tier registry, routing, barricade validation, injection guard) — nothing reaches it from a caller.

## Gaps (plan vs reality)
| # | Gap | Resolution |
|---|-----|------------|
| G1 | `since`/`until` have no single field both tiers own. Episodic's real-world time is `occurred_at`; semantic's is `valid_at`. Emitting a predicate on one leaves the *other tier unconstrained* — the exact trap Phase 4 exists to kill. Emitting both breaks under `sources:["semantic"]` (`occurred_at` is then "unfilterable for the selected sources"). | Registry gains a per-tier **target field** so one caller-facing alias (`time`) compiles to `occurred_at` on episodic and `valid_at` on semantic. Retrieval change (R1 below). |
| G2 | CARRY-FORWARD: registered tier sources (`experience`) and post-hooks (`graph`) receive no predicates — with `Sources: nil` + a `kind` filter their hits ride back **silently unconstrained**. | Explicit contract (R2 below): a filtered search excludes sources that declare no filterable fields; naming one explicitly alongside filters is an error. |
| G3 | A caller-supplied bad filter surfaces from `retrieval` as a plain `error`; `server.Search` maps every retriever error to `codes.Internal` — a caller error reported as a server fault. | `retrieval.ErrInvalidFilter` sentinel; the barricades map it to `InvalidArgument` / tool error (R3 below). |
| G4 | `SearchRequest.valid_only` and the new `include_superseded` would be two knobs for one bit. | Remove `valid_only` (no backward compat; field number reserved). `ValidOnly = !include_superseded`, so absent ⇒ today's behavior. |

### Scope extension — DISCLOSED
The plan's file scope for this phase is `api/**`, `internal/mcp/**`, `internal/engramclient/**`, `internal/server/**`. G1–G3 cannot be honestly resolved outside `internal/retrieval` (Phase 4's file). The three changes there are **additive and small**; none alters an existing behavior or seam:

- **R1** `FieldSpec.Target` (physical field; empty = the map key) + a `time` alias entry on both tiers. ~8 lines.
- **R2** unconstrainable-source contract in `MultiRetriever` (see Design Decisions). ~20 lines.
- **R3** `var ErrInvalidFilter` wrapping the six existing filter-validation errors. ~10 lines.

Rejected alternative for G1: map `since`/`until` to `created_at` (declared by both tiers already, zero retrieval change) — rejected because the harvester backdates `occurred_at`, so ingest time and event time genuinely differ and "memories since March" would silently mean "ingested since March".
Rejected alternative for G2: narrow `Sources` at the server. The server would have to know retrieval's registry — information leakage, and it breaks the moment a new source is registered.

## Code Standards (applies here)
- Never import `api/engrampb` or `grpc` into a business package — `internal/mcp` and `internal/retrieval` speak their own types; `internal/server` and `internal/engramclient` are the translating edges. (`internal/importlint` fails CI otherwise.)
- Never trust client-supplied tenancy/scope: the verified Identity overrides the request (`server.go:92`).
- Never post-filter a kNN query — filters go *inside* the `knn` clause (already true; predicates ride the same `filters` slice).
- Errors: `fmt.Errorf("pkg: …: %w", err)`, lowercase, package-prefixed.

## Test Infrastructure
- Table-driven `testing` stdlib; no mocking framework. `internal/mcp/mcp_test.go` has `fakeBackend`; `internal/server/server_test.go` has a fake `Retriever` that captures the `Filter`; `internal/engramclient/knowledge_test.go` has a fake `engrampb.EngramClient` capturing the request (the pattern for asserting the wire mapping).
- `internal/retrieval/opensearch_test.go` builds query bodies against an `httptest` server and asserts the emitted JSON — the home for the golden-byte and injection assertions.
- Integration/live tests are build-tagged; `make test` runs the unit suite only.

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-5.1 | Every flat filter param accepted; each maps to the correct internal predicate and tier | COVERED | `internal/mcp`: `TestDW_5_1_SearchAcceptsEveryFlatFilterParam` (args JSON → `SearchFilter`). `internal/server`: `TestDW_5_1_FlatParamsCompileToPredicates` (proto → `Filter.Predicates`, exact field/op/value set). `internal/retrieval`: `TestDW_5_1_TimeAliasRoutesToPerTierField` (`time` → `occurred_at` on episodic, `valid_at` on semantic), `TestDW_5_1_KindRoutesToEpisodicOnly` |
| DW-5.2 | `include_superseded: true` returns historical facts; absent/false preserves `ValidOnly` | COVERED | `internal/server`: `TestDW_5_2_IncludeSupersededDropsValidOnly`, `TestDW_5_2_AbsentIncludeSupersededKeepsValidOnly`. `internal/engramclient`: `TestDW_5_2_ClientSendsIncludeSuperseded`. `internal/retrieval`: `TestDW_5_2_IncludeSupersededCannotBypassACL` (ACL clause still in the body with `ValidOnly=false`) |
| DW-5.3 | `sources: ["semantic"]` excludes episodic and graph hits end-to-end from the MCP call | COVERED | `internal/mcp`: `TestDW_5_3_SourcesReachBackend`. `internal/engramclient`: `TestDW_5_3_ClientSendsSources`. `internal/server`: `TestDW_5_3_SourcesReachFilter`. `internal/retrieval`: `TestDW_5_3_SemanticOnlySearchHasNoEpisodicOrGraphHits` (end of the chain: only the semantic tier is queried, the graph hook does not run) |
| DW-5.4 | Invalid filter field or malformed time rejected at the MCP/gRPC entry, error names the valid fields, retriever never reached | COVERED | `internal/mcp`: `TestDW_5_4_UnknownFilterParamRejectedNamingValidFields`, `TestDW_5_4_MalformedTimeRejected`, `TestDW_5_4_SinceAfterUntilRejected`, `TestDW_5_4_UnknownSourceRejectedNamingValidSources` — each asserts `fakeBackend.calls == 0`. `internal/server`: `TestDW_5_4_SinceAfterUntilIsInvalidArgument` (fake retriever never called) |
| DW-5.5 | `make proto` run, regenerated `.pb.go` committed, `make proto-check` passes | COVERED | Executed in-build: `make proto && make proto-check` (recorded in output); the generated files are part of the diff |
| DW-5.6 | No filters ⇒ behaves identically to today (end-to-end) | COVERED | `internal/mcp`: `TestDW_5_6_NoFiltersSendsZeroFilter`. `internal/server`: `TestDW_5_6_NoFiltersProducesTodaysFilter` (`Filter{TenantID, UserID, ValidOnly:true, Identity}`, `Predicates`/`Sources` nil). `internal/retrieval`: the Phase-4 golden-byte test still passes unchanged |
| DW-5.7 | Adversarial filter value is parameterized into the query body, not interpolated | COVERED | `internal/server`: `TestDW_5_7_AdversarialFilterValueStaysData` (value byte-identical through the mapping, never merged into a field name). `internal/retrieval`: `TestDW_5_7_AdversarialValueParameterizedIntoQueryBody` (emitted body's `term.kind` is the raw string; body structure unchanged) |

**All items COVERED:** YES (7 DW-IDs in the prompt, 7 rows).

Beyond the DW floor: `k <= 0` → `defaultRequestK`; `k` above `MaxK` still clamped; empty `sources: []` rejected; blank source string rejected; `include_superseded` alone (no other filter) is a valid request; duplicate source names tolerated; the `experience`/`graph` exclusion contract (R2) and its explicit-naming error.

## Design: memory_search filter surface

### Approaches Considered
1. **Flat all the way down** — MCP flat params → **flat proto fields** → `internal/server` compiles them into `[]retrieval.Predicate`. The generic predicate form never appears on the wire.
2. **Flat MCP, generic wire** — MCP flat params → `engramclient` compiles to `repeated Predicate` (reusing knowledge's `Predicate`/`Range` messages) → server passes them through to `retrieval`.
3. **Generic everywhere** — expose `filters:[{field,op,value}]` at the MCP surface (knowledge_search's shape).

### Comparison
| Criterion | A (flat→flat) | B (flat→generic wire) | C (generic everywhere) |
|-----------|---|---|---|
| LLM token cost / error surface | Lowest (named optional scalars) | Same at MCP | Highest — rejected by the user's directive |
| Wire contract | An unknown filter field is **unrepresentable** | Any field name is representable; validation is the only guard | Same as B |
| Compile sites | One (`internal/server`) | Two halves (client compiles, server re-validates) | One, but at the wrong altitude |
| Information hiding | Generic predicate vocabulary stays inside `internal/retrieval` | Leaks the internal vocabulary onto the wire | Leaks it to the LLM |
| Caller ease | `kind: "conversation"` | same | `filters:[{field:"kind",op:"term",value:"conversation"}]` |
| Cost of a new filter | proto field + one map line | one map line | zero |

### Choice: A (flat → flat → compiled at the server edge)
The plan's constraint — "the generic predicate form stays INTERNAL" — reads most strongly as *internal to `internal/retrieval`*, not merely "hidden from the LLM". A honors it, and the payoff is defensive, not stylistic: with a flat proto, "a filter on a field no tier owns" is not a value you can put on the wire. Sacrificed: each future filter costs a proto field, and the gRPC surface is memory-specific rather than reusing knowledge's messages (the plan already forbids sharing a filter package across those two actors, so this is consistent, not duplicative).

The compile lives in `internal/server` (`searchfilter.go`) because that is the edge that already owns proto↔`retrieval` translation, and it is the only Phase-5 package permitted to import both.

### Depth check
- Caller-facing methods: 1 (`memory_search`). Backend seam: `Search(ctx, query, k, f SearchFilter)` — one added parameter, one flat struct.
- Hidden from the caller: predicate/op vocabulary, per-tier field ownership, the `time`→`occurred_at`/`valid_at` split, `ValidOnly`'s bi-temporal clause, source registry names.
- Common case (no filters): unchanged call, zero-value `SearchFilter`, byte-identical query body.

### Barricade (cc-defensive-programming)
| Boundary | Validates | On failure |
|---|---|---|
| MCP `callSearch` (agent input) | unknown argument keys (JSON decoded with `DisallowUnknownFields`, error names the valid params); non-empty `query`; RFC 3339 `since`/`until`; `since <= until`; `sources` non-empty, no blank entries, every name in the static memory-source vocabulary (`readSources` precedent) | tool error naming the valid vocabulary; **backend never called** |
| gRPC `server.Search` (token-authenticated, still external) | `since <= until`; blank source names; verified Identity overrides request tenancy (existing) | `InvalidArgument` |
| `retrieval.MultiRetriever.Search` (Phase 4) | field ownership, ops, scalar-only values, source vocabulary — now wrapped in `ErrInvalidFilter` | `InvalidArgument` at the server; defense in depth, the retriever is the authority |

Filter values are **data**: they are placed into clause structures and marshaled (Phase 4's `validatePredicateValue` rejects maps/slices), never interpolated into a query string. `include_superseded` only relaxes the *validity* clause; the ACL clause and the tenant term are compiled independently and unconditionally, so it cannot widen what a caller may read (asserted).

### R2 — the carry-forward decision (explicit, documented, tested)
**A filtered search excludes sources that cannot honor the filter.** When `len(Filter.Predicates) > 0`, registered tier sources and post-hooks that declare no filterable fields (today: `experience`, `graph`) do not run. If the caller *explicitly* names such a source in `Sources` alongside predicates, that is a validation error naming the source — never a silent unconstrained result.

Why not route predicates into them instead: `TierSource`/`PostHook` declare no field surface, and giving them one means an interface change reaching `internal/experience` and `internal/graph/expand.go` — Phase 6's file, and a redesign of Phase 4's seam.

Why the asymmetry with a built-in tier that does not own a field (which stays *unconstrained*, per DW-4.2) is acceptable: a built-in tier's filter surface is **declared and visible** — it appears in the error vocabulary, and `sources` lets a caller exclude it deliberately. A source with no declared surface is invisible: the caller has no way to learn its hits were never filtered. Visible routing is a contract; invisible routing is the bug Phase 4 exists to kill.

## Prerequisites
- [x] Phase 4 landed (`Filter.Predicates`, `Filter.Sources`, registry, barricade).
- [x] `make proto` toolchain works (buf v1.55.1 via pinned `go run`; clean regen on an unmodified tree).
- [x] Only two `mcp.Backend` implementors, both in-repo.

## Recommendation
**BUILD** — with the disclosed R1/R2/R3 additive changes in `internal/retrieval` (no DW item changes, no `Produces` contract changes).

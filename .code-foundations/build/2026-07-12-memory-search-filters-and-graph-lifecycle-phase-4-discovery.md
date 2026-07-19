# Discovery + Design: Phase 4 - Filter core (per-tier field registry + predicate routing)

## Assumption Verification

| Assumption | Verdict | Evidence |
|---|---|---|
| All target filter fields already mapped `keyword`/`date`; no reindex needed | **CONFIRMED** | `internal/store/templates/episodic.json`: `kind` keyword, `occurred_at`/`created_at`/`processed_at` date. `internal/store/templates/semantic.json`: `subject`/`predicate`/`object`/`extractor_version` keyword, `valid_at`/`invalid_at`/`created_at`/`expired_at` date. Both `"dynamic": "strict"` — a filter on an undeclared field would be a mapping error, which is exactly why the registry must gate. |

## Files Found

| File | Relevance |
|---|---|
| `internal/retrieval/retriever.go` | `Query`, `Filter`, `TierSource`, `PostHook`, `Hit`, `Retriever` — the seam that gains `Predicates`/`Sources` |
| `internal/retrieval/opensearch.go` | `MultiRetriever.Search` (:200-301), `RegisterTier`/`RegisterPostHook` (:177,184), `allowedFields` (:306), `tierRetriever.filterClauses` (:469-497), `buildQuery` (:515) |
| `internal/retrieval/knowledge.go` | `Predicate` (:25-29), `filterClause` (:308-332), `fieldListOrNone` (:273) — the vocabulary and compiler to reuse, same package |
| `cmd/engram-server/stages_experience.go:65`, `stages_graph.go:86` | The two production registration call sites |
| `internal/graph/expand_test.go:205-206`, `internal/graph/opensearch_integration_test.go` (4 sites), `internal/retrieval/acl_test.go` (5 sites), `internal/retrieval/project_test.go` (4 sites) | Test call sites that must be updated to the name-carrying registration signature (compile-forced) |

## Current State

- `retrieval.Filter` = `{TenantID, UserID, ValidOnly, Identity}`. No caller-facing filter surface at all.
- `tierRetriever` carries a hand-written `supportsValidity bool` gate (`:484`) — the exact "forgettable if" the plan rejects generalizing.
- `MultiRetriever` stores `tierSrcs []TierSource` and `postHooks []PostHook` with **no names**, so it cannot skip a source by name today. Confirmed: neither interface has a `Name()` method.
- `Predicate{Field, Op, Value}` already exists in `knowledge.go` — same Go package `retrieval`, so it is reusable **without** a new shared package (the plan's SRP-by-actor constraint is satisfied by reuse-in-place: no new import arrow, no cross-actor coupling; knowledge's `CollectionSpec.Mappings` stays knowledge's own registry).
- Golden-byte precedent exists: `TestBuildQueryMemoryPathByteIdenticalWhenSortNil` (`knowledge_test.go:167`) pins `buildQuery` output for the memory path.

## Gaps (plan vs reality)

| Gap | Resolution |
|---|---|
| Plan's "Range shape from knowledge.go:25-37" — there is no `Range` **type**; a range value is a `map[string]any{"gte","lte"}` inside `Predicate.Value`, handled by `filterClause` (`:315-328`) | Reuse as-is. No new type. Plan text was slightly off; the substance (reuse the shape) holds. |
| Signature change to `RegisterTier`/`RegisterPostHook` breaks 9 test call sites in `internal/graph/**` (outside the phase's declared file scope) | Mechanical compile fix (add the source name). Unavoidable and behavior-preserving; noted as a scope note, not a redesign. |
| `filterClause` currently returns `(any, error)`; `tierRetriever.filterClauses` returns `[]any` with no error | `filterClauses` gains an `error` return (fail-closed defense in depth, even though `Search` validates first). |

## Code Standards (applied)

- No transport/proto imports in `internal/retrieval` (importlint). This phase adds none.
- **Never post-filter a kNN query by ACL/tenancy** — predicate clauses join the same `filters []any` slice that already goes *inside* both the BM25 `bool.filter` and the `knn.filter` (`buildQuery:521-531`). Predicates inherit this automatically; no new code path.
- Never trust client-supplied scope — the ACL clause and `Identity` path are untouched by this phase.
- Errors wrapped with a `retrieval:` prefix; `%w` for wrapping.

## Test Infrastructure

- `internal/retrieval/opensearch_test.go` is `package retrieval_test` with `newFakeSearchServer` capturing `{path, raw body, query params}` per tier request — exactly the hook needed for query-body assertions and golden bytes.
- `knowledge_test.go`/`acl_test.go`/`project_test.go` are in-package (`package retrieval`) for unit-level access.
- `make test` = `go test ./...` (unit; integration is build-tagged and excluded).

---

## Design: Per-tier filter registry + predicate router

### Approaches Considered

1. **A — Registry on the tier, validation in `Search`, compile in the tier.** Each `tierRetriever` carries a `FilterableFields` map. `MultiRetriever.Search` is the barricade: it resolves `Sources`, validates every predicate against the *selected* tiers' declared fields, then each tier compiles only the predicates it declares.
2. **B — Central registry map in `MultiRetriever` (`map[source]FilterableFields`), tiers stay dumb.** `Search` compiles per-tier clause lists and hands each tier a pre-built `[]any`.
3. **C — Predicate carries its own target tier(s)** (caller or Phase-5 mapper decides routing). No registry; the router trusts the label.

### Comparison

| Criterion | A | B | C |
|---|---|---|---|
| Interface simplicity | 2 new exported types (`FieldSpec`, `FilterableFields`) + 2 `Filter` fields | Same + a registry type on `MultiRetriever` | 1 type, but `Predicate` grows a `Tiers` field — a parallel vocabulary (violates the reuse constraint) |
| Information hiding | Tier owns *both* what it can filter and how it compiles — one place to add a field | Splits "what a tier can filter" (MultiRetriever) from "how the tier queries" (tierRetriever) — information leakage across the seam | Routing knowledge escapes the module entirely, into the caller |
| Caller ease of use | Caller states `Predicates` + optional `Sources`; routing is invisible | Same | Caller must know the tier topology — the exact trap the phase exists to close |
| Structural trap-proofing (the phase's whole point) | Adding a filter field = one registry entry; forgetting a gate is impossible | Same, but the registry is remote from the compiler that consumes it | None — a mislabeled predicate silently zeroes a tier |
| `tierRetriever` usable standalone (eval harness / direct `Search`) | Yes — it still compiles its own clauses | No — a bare tier would lose all predicate support | Yes |
| Blast radius on `buildQuery` / golden bytes | Zero (clauses append to existing `filters []any`) | Zero | Zero |

### Choice: A

Rationale: A keeps the declaration next to the compiler that consumes it, so a new filterable field is **one** registry entry and cannot be half-added. It preserves `tierRetriever` as a self-sufficient `Retriever` (the eval harness and existing tests construct tiers and call `search` directly). B splits the knowledge across the seam for no gain; C reintroduces the trap under a new name and violates the "no parallel vocabulary" constraint.

Sacrificed: `MultiRetriever` must reach into `tier.filterable` to build the validation error's field list. Accepted — it is a read of a declared, immutable map, not a behavioral coupling.

### Depth Check

- Interface methods added to the public surface: **0 methods**; 2 struct fields (`Filter.Predicates`, `Filter.Sources`), 2 types (`FieldSpec`, `FilterableFields`), 2 changed signatures (`RegisterTier(name, src)`, `RegisterPostHook(name, h)`).
- Hidden details: which tier owns which field; how a predicate becomes an OpenSearch clause; that a `kind` clause must never reach the semantic index (mapping is `dynamic: strict` — the clause wouldn't just be inert, `kind` isn't in the semantic mapping at all); that ACL/tenancy/validity clauses and predicate clauses share one `filters` slice compiled *inside* the kNN clause.
- Common-case complexity: **simple** — `Filter{}` behaves exactly as today (nil `Predicates`, nil `Sources` = all sources), byte-identical query body.

### Key decisions

| Decision | Choice | Why |
|---|---|---|
| Where predicates are validated | `MultiRetriever.Search`, before any HTTP call (the barricade). Tiers re-check on compile (defense in depth, fail-closed). | cc-defensive-programming: `Filter` crosses a process boundary (MCP/gRPC → server → retrieval). "Internal team API is still external." |
| Field-existence scope | Against the **selected** tiers' union, not all tiers. `Sources:["semantic"] + kind` ⇒ error, not a silently-inert predicate. | Eliminates the last silent-drop path. Still satisfies DW-4.2 (with `Sources` nil, `kind` is declared by episodic ⇒ valid ⇒ routed to episodic only ⇒ semantic unconstrained). |
| `Sources` nil vs `[]` | nil ⇒ all sources. Non-nil empty ⇒ **error** naming valid sources. | Plan's explicit edge case: empty must not silently mean all. |
| Predicate value safety | `term`/`prefix` values must be scalars (string/bool/number); `range` bounds must be scalars. Values are placed into `map[string]any` clause structures and `json.Marshal`ed — never concatenated into a query string. | DW-4.8. A map/slice value could otherwise smuggle a nested DSL object into a clause position. |
| Predicate count cap | `MaxPredicates = 32`, error beyond. | External input bounding (mirrors `MaxK`'s rationale at `opensearch.go:45`). |
| Ops per type | keyword ⇒ `term`, `prefix`; date ⇒ `range`. | `filterClause` already implements all three; typed ops keep a `range` off a keyword and a `prefix` off a date. |
| Clause order | ACL → tenant → user → validity → predicates (caller order). | Appending last keeps the no-predicate body byte-identical (DW-4.3). |

### Registry contents (DW-4.1)

| Tier | Fields |
|---|---|
| episodic | `kind` (keyword), `occurred_at` (date), `created_at` (date) |
| semantic | `subject`, `predicate`, `object`, `extractor_version` (keyword), `valid_at`, `invalid_at`, `created_at`, `expired_at` (date) |

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-4.1 | Each tier declares its filterable fields; episodic `kind`, semantic `subject`/`predicate`/`object`/`extractor_version`, both their time fields | COVERED | `TestDW_4_1_TierFilterableFieldRegistry` (in-package: asserts each tier's declared set + type/ops), `TestDW_4_1_DeclaredFieldsExistInIndexTemplates` (cross-checks every declared field against `templates/{episodic,semantic}.json` — catches a registry entry the strict mapping would reject) |
| DW-4.2 | A `kind` predicate leaves the semantic tier's query unconstrained (not zeroed) | COVERED | `TestDW_4_2_KindPredicateRoutesToEpisodicOnly` — episodic body contains `{"term":{"kind":"decision"}}`; semantic body contains no `kind` at all AND is byte-identical to the no-predicate semantic body (this is the assertion a naive shared clause fails) |
| DW-4.3 | No predicates + no `Sources` ⇒ query body byte-identical to today | COVERED | `TestDW_4_3_GoldenQueryBodyUnchangedWithoutFilters` — golden bytes captured from the pre-change build for both tiers, hybrid + BM25-only |
| DW-4.4 | Predicate on an unknown field errors, naming valid filterable fields | COVERED | `TestDW_4_4_UnknownFieldErrorNamesValidFields` (+ subtest: field valid on another tier but not on any *selected* source) |
| DW-4.5 | `Sources: ["semantic"]` skips the graph post-hook and the episodic tier entirely | COVERED | `TestDW_4_5_SourcesSkipsEpisodicTierAndGraphPostHook` — zero HTTP requests to the episodic index; post-hook `Apply` never called; registered tier source not called |
| DW-4.6 | `extractor_version` appears on semantic hits | COVERED | `TestDW_4_6_ExtractorVersionOnSemanticHits` — projected `Fields` retain `extractor_version` |
| DW-4.7 | Unknown source name errors, naming valid sources | COVERED | `TestDW_4_7_UnknownSourceErrorNamesValidSources` (+ subtest: empty non-nil `Sources` errors) |
| DW-4.8 | Injection-shaped filter value is parameterized, not interpolated | COVERED | `TestDW_4_8_InjectionShapedValueIsParameterized` — value `"} },{"match_all":{}}"` round-trips as a *string* leaf under `term.kind` in the decoded body, and the body's clause tree is structurally unchanged; plus `TestPredicateValueMustBeScalar` (a map value is rejected, so no DSL object can occupy a value slot) |

**All items COVERED:** YES · **DW count: 8 in prompt / 8 in table**

### Tests beyond the DW floor
- `Sources: nil` vs `Sources: []` (nil = all, empty = error).
- Range with only `gte`, only `lte`, both, neither (neither ⇒ error).
- Bad op for a field's type (`range` on `kind`, `prefix` on `valid_at`).
- `Sources` naming a registered tier source (`"experience"`) runs it and skips both built-ins.
- Duplicate names in `Sources` are idempotent.
- `MaxPredicates` overflow errors.
- Validation errors are returned **before** any HTTP request is issued (fake server fails the test if called).
- Existing ACL tests still pass with named registration (ACL re-verification loops unchanged).

## Prerequisites
- [x] `Predicate`/`filterClause` exist in-package
- [x] Test fake server captures request bodies
- [x] Index templates confirm every declared field
- [x] No dependency on Phases 1–3 (they did not touch `internal/retrieval/**`)

## Recommendation

**BUILD** — the plan fits reality. Two notes carried into implementation: (1) there is no `Range` *type* to reuse (a range is a `map[string]any` value), and (2) the name-carrying registration signature forces mechanical updates to 9 test call sites in `internal/graph/**` and `internal/retrieval/**`, slightly outside the declared file scope but compile-forced and behavior-preserving.

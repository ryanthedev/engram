# Review: Phase 4 — Filter core (registry + predicate routing) — sample 1

## Executed Results (Step 0)
- Test suite: `make test` → all packages `ok` (0 failures). `go test ./internal/retrieval/... ./cmd/... -count=1` → `ok internal/retrieval 0.037s`, all cmd packages ok.
- Race: `go test ./internal/retrieval/ -race -count=1` → `ok 1.077s` (no data race in the new `Sources`-narrowed goroutine fan-out).
- Typecheck/Build: `make build` → `go build ./...` clean.
- Lint: `make lint` → `go vet ./...` + `revive -set_exit_status` clean.
- Named DW tests all PASS: `TestDW_4_1_TierFilterableFieldRegistry`, `TestDW_4_1_DeclaredFieldsExistInIndexTemplates`, `TestDW_4_2_KindPredicateRoutesToEpisodicOnly`, `TestDW_4_3_GoldenQueryBodyUnchangedWithoutFilters`, `TestDW_4_4_UnknownFieldErrorNamesValidFields`, `TestDW_4_5_SourcesSkipsEpisodicTierAndGraphPostHook`, `TestDW_4_6_ExtractorVersionOnSemanticHits`, `TestDW_4_7_UnknownSourceErrorNamesValidSources`, `TestDW_4_8_InjectionShapedValueIsParameterized`.

### Independent verification performed (not taken from the build's word)
1. **Golden bytes re-derived from the PRE-change code.** I extracted `HEAD` (`git archive HEAD`) into `/tmp/p4-sample-1/head`, added a probe test that runs the *unmodified* retriever over the same four inputs, and diffed its emitted bodies against the 8 golden strings hard-coded in `filter_routing_test.go`: **byte-identical, all 8**. The goldens are genuinely pre-change captures, not a re-derivation of the code under test.
2. **Five adversarial probes** written and run against a scratch copy of the tree (`/tmp/p4-sample-1/cur`), covering ACL×Sources interaction, ACL-clause survival under narrowing, predicate routing with only a tier-source selected, the injection surface, and the empty-text path. Results in the dimensions table below.

## Requirement Fulfillment

### DW-4.1
PREMISE:  "Each tier declares its filterable fields; episodic declares `kind`, semantic declares `subject`/`predicate`/`object`/`extractor_version`, both declare their time fields."
EVIDENCE: `internal/retrieval/filters.go:53-73` (`episodicFilterable`, `semanticFilterable`); wired at `internal/retrieval/opensearch.go:145,151` (`filterable:` on each `tierRetriever`).
TRACE:    `episodicFilterable.names()` → `[created_at kind occurred_at]`; `semanticFilterable.names()` → `[created_at expired_at extractor_version invalid_at object predicate subject valid_at]`. Asserted by `TestDW_4_1_TierFilterableFieldRegistry` (PASS), including the disjointness assertion that `semantic` does NOT declare `kind`. `TestDW_4_1_DeclaredFieldsExistInIndexTemplates` (PASS) additionally re-reads `internal/store/templates/{episodic,semantic}.json` and confirms every declared field is mapped with the declared type and the mapping is `dynamic:strict` — a real assumption guard, not a tautology.
VERDICT:  PASS

### DW-4.2
PREMISE:  "A `kind` predicate leaves the semantic tier's query unconstrained (not zeroed) — asserted by a test that would fail under a naive shared clause."
EVIDENCE: `internal/retrieval/filters.go:99-109` (`clauseFor` returns `declared=false, err=nil` for a field the tier does not own); `internal/retrieval/opensearch.go:582-591` (`filterClauses` `continue`s on `!declared`).
TRACE:    `Search(q, Filter{TenantID:"t1", Predicates:[{kind term "decision"}]})` → episodic body contains `{"term":{"kind":"decision"}}`; semantic body contains no `kind` substring **and is byte-equal to the semantic body of the same search with zero predicates**. `TestDW_4_2_KindPredicateRoutesToEpisodicOnly` (PASS). Under a naive shared clause the semantic body would carry the kind term and differ from the baseline — the test compares against the captured baseline body, so it would fail. Mirrored by `TestSemanticOnlyPredicateLeavesEpisodicUnconstrained` (PASS).
VERDICT:  PASS

### DW-4.3
PREMISE:  "With no predicates and no `Sources`, the emitted OpenSearch query body is byte-identical to today's (golden-byte regression test)."
EVIDENCE: `internal/retrieval/filter_routing_test.go:168-222`; implementation basis `opensearch.go:555-592` (predicates appended strictly LAST, only when declared) and `buildQuery` (`opensearch.go:611-649`, unchanged by the diff).
TRACE:    4 cases × 2 tiers = 8 bodies (hybrid/bm25 × zero-filter/tenancy+validity). `TestDW_4_3_GoldenQueryBodyUnchangedWithoutFilters` PASS. **Independently confirmed**: the 8 golden strings diff to zero against the bodies emitted by the pre-change `HEAD` retriever for the same inputs.
VERDICT:  PASS

### DW-4.4
PREMISE:  "A predicate on an unknown field errors, and the error names the valid filterable fields."
EVIDENCE: `internal/retrieval/filters.go:301-326` (`validatePredicates`), error at `filters.go:321-322` calling `filterableFieldNames(sel)` + `fieldListOrNone`.
TRACE:    `Search(q, Filter{Predicates:[{password term "x"}]})` → `retrieval: unknown or unfilterable field "password" for the selected sources; valid filterable fields: created_at, expired_at, extractor_version, invalid_at, kind, object, occurred_at, predicate, subject, valid_at`. `TestDW_4_4_UnknownFieldErrorNamesValidFields` (PASS) asserts the named fields AND that the error is raised before any HTTP call (the server is a `failingHandler` that fails the test on any request — validation precedes I/O, confirmed by `Search`'s ordering at `opensearch.go:264-271`, ahead of the ACL compile and the fan-out). Second sub-case proves the vocabulary follows the selection: `Sources:["semantic"] + kind` errors and does NOT offer `kind`. Bad-op variants covered by `TestUnsupportedOpForFieldTypeErrors` (PASS).
VERDICT:  PASS

### DW-4.5
PREMISE:  "`Sources: [\"semantic\"]` skips the graph post-hook and the episodic tier entirely."
EVIDENCE: `internal/retrieval/filters.go:240-263` (`selectSources` — the single place `Sources` is applied, partitioning all three registries at once); consumed at `opensearch.go:271, 291-308, 344`.
TRACE:    `Search(q, Filter{Sources:["semantic"]})` against a fake cluster with a registered `experience` tier source and `graph` post-hook → indices actually queried = `[sem-idx]` only (episodic issues **no HTTP request at all**, not a zeroed one), tier-source call count = 0, post-hook call count = 0. `TestDW_4_5_SourcesSkipsEpisodicTierAndGraphPostHook` (PASS). Converse covered by `TestSourcesSelectsRegisteredTierSourceAndPostHook` (PASS): `Sources:["experience","graph"]` queries neither built-in index.
VERDICT:  PASS

### DW-4.6
PREMISE:  "`extractor_version` appears on semantic hits."
EVIDENCE: `internal/retrieval/opensearch.go:378` (`allowedFields["semantic"]` now includes `extractor_version`); `projectFields` at `opensearch.go:392-407`.
TRACE:    Fake semantic response with `_source.extractor_version="v3"` and `_source.fact_embedding=[0.1]` → returned hit has `Fields["extractor_version"]=="v3"` and no `fact_embedding` (the projection still strips embeddings). `TestDW_4_6_ExtractorVersionOnSemanticHits` (PASS). `TestExtractorVersionPredicateFiltersSemanticTier` (PASS) closes the loop end-to-end: the field is filterable *and* visible, and the episodic body carries no `extractor_version`.
VERDICT:  PASS

### DW-4.7
PREMISE:  "An unknown source name errors, and the error names the valid sources."
EVIDENCE: `internal/retrieval/filters.go:209-229` (`resolveSources`), error at `filters.go:224`; vocabulary from `sourceNames()` (`filters.go:186-199`) which unions built-in tiers + registered tier sources + registered post-hooks.
TRACE:    `Search(q, Filter{Sources:["semantic","epsiodic"]})` (typo) → `retrieval: unknown source "epsiodic"; valid sources: episodic, experience, graph, semantic`; zero HTTP requests, tier-source and post-hook call counts both 0. `TestDW_4_7_UnknownSourceErrorNamesValidSources` (PASS), 4 sub-cases.
VERDICT:  PASS

### DW-4.8
PREMISE:  "An injection-shaped filter value (e.g. a string containing OpenSearch query DSL) is parameterized into a clause structure, not interpolated into the query body."
EVIDENCE: `internal/retrieval/knowledge.go:308-332` (`filterClause` — the value is placed as a leaf in a `map[string]any` and marshaled by `encoding/json`; no string concatenation anywhere on the query-body path); `internal/retrieval/filters.go:127-164` (`validatePredicateValue`/`isScalar` — the barricade that stops a map/slice value from becoming *structure*).
TRACE:    For `Value = "decision\"}},{\"match_all\":{}},{\"term\":{\"x\":\""` (and 3 other injection shapes): emitted episodic body parses as valid JSON; its BM25 `bool.filter` array has **exactly 2** clauses (tenant + kind) — no clause was injected; `filter[1].term.kind` is a *string leaf* equal to the caller's value verbatim, escaped. `TestDW_4_8_InjectionShapedValueIsParameterized` (PASS, 4 sub-cases). Structure-smuggling closed by `TestNonScalarPredicateValueRejected` (PASS: map / slice / nil all rejected with a "scalar" error). Field names cannot be injected either: `clauseFor` only reaches `filterClause` when `p.Field` is a key in the tier's registry, so the field position is always a whitelisted literal.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] All 8 DW items have a corresponding automated test that ran and passed in Step 0 (test names carry the DW id).
- [ ] **Test coverage does not fully meet the stated 100% level.** Three *new* branches are uncovered by the suite (verified with `go tool cover`):
  - `filters.go:144-146` — the "range bound must be a scalar" rejection (a `map`/slice inside `gte`/`lte`). This is part of the DW-4.8 injection barricade. I verified the branch is *correct* by executing it (`{"gte": {"match_all":{}}}` → `retrieval: range bound "gte" on field "valid_at" must be a scalar, got map[string]interface {}`), but no committed test pins it.
  - `opensearch.go:222-225, 226-228` — `checkSourceName`'s empty-name and duplicate-name ERROR-log branches (new code, no test).
  - `opensearch.go:346-348` — the post-hook error wrap (touched by this phase: the message gained `%q` with the hook name).
- Every edge case in the prompt's list IS covered by a passing test (see below), so no DW item lacks execution evidence.

## Edge cases (prompt-listed — same standing as DW)
| Edge case | Status | Evidence |
|---|---|---|
| Empty `Predicates` ⇒ body byte-identical to today | PASS | `TestDW_4_3_...` + my independent HEAD-vs-goldens byte diff (8/8 identical) |
| Predicate valid on one tier, absent on another (route, don't fail) | PASS | `TestPredicateValidOnBothTiersReachesBoth` (`created_at` reaches BOTH), `TestDW_4_2_...` and `TestSemanticOnlyPredicateLeavesEpisodicUnconstrained` (route, no error) |
| `Sources` naming an unknown source | PASS | `TestDW_4_7_.../unknown name` — errors, names all 4 valid sources, zero I/O |
| `Sources: []` vs `Sources: nil` (nil = all, empty = error, not silent-all) | PASS | `resolveSources` (`filters.go:210-216`): `nil` → `(nil, nil)` = all; `len==0` → explicit error. `TestDW_4_7_.../empty but non-nil is an error, not silent-all` and `/nil means every source` |
| Range with only `gte` or only `lte` | PASS | `TestRangeBounds` — gte-only, lte-only, both → correct clause; neither, `gt`-only, non-map → error |

## Dead Code
None found. No unused imports (`go vet` + `revive` clean), no unreachable code after early returns, no debug statements, no commented-out blocks in `filters.go`, `retriever.go`, `opensearch.go`, `stages_experience.go`, `stages_graph.go`. Every symbol in `filters.go` has a live caller.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | Adversarial case: `Sources` now sizes and indexes the shared `results` slice (`opensearch.go:291-308`). Traced: built-in tiers write `results[i]` for `i ∈ [0, len(tiers))`; tier sources are dispatched with `i = len(tiers)+j`, and `results` is `make([]outcome, len(tiers)+len(tierSrcs))` — every goroutine owns a distinct index, no overlap, no out-of-range even when `selectSources` shrinks both slices. `go test -race` on the package: clean. |
| Error Handling | PASS | Every new error path returns a wrapped, self-correcting error; no empty catch, no swallowed error. `filterClauses` propagates `clauseFor`'s error rather than emitting an unchecked clause (`opensearch.go:489-491, 583-586`) — defense in depth behind the `Search` barricade, asserted by `TestFilterClausesFailsClosedOnUnvalidatedPredicate`. ACL compile failure still fail-closed (`TestDW_4_4_CompilerErrorFailsClosed`). |
| Resources | N/A | The phase adds no file handles, connections, locks, threads or caches. `resp.Body.Close()` on the search path is unchanged (`opensearch.go:510`); `Sources` only shortens loops, so it strictly *reduces* the HTTP requests issued. |
| Boundaries | PASS | Probed: nil `Predicates` → `validatePredicates` returns early (`filters.go:302-304`); nil `sourceSet` → `selected()` returns true for everything and `selectSources` short-circuits to the full lists; `Sources: []` → error, not silent-all; `MaxPredicates+1` → rejected (`TestTooManyPredicatesRejected`); range with 0 bounds / unknown-only bounds → rejected; `filterableFieldNames` de-dupes `created_at` (declared by both tiers) to a single entry — asserted. |
| Security | PASS | Three probes, all executed. **(a) Interpolation:** no query-string concatenation exists on the body path; values are `map[string]any` leaves marshaled by `encoding/json`. Non-scalar values are rejected at the barricade; an *extra* key inside a range value (e.g. `"boost": {"match_all":{}}`) is silently **dropped** by `filterClause`'s explicit `gte`/`lte` copy loop — I confirmed the emitted clause is `{"range":{"valid_at":{"gte":"2026-01-01"}}}` with no `boost` key. **(b) `Sources` cannot ADMIT a denied hit:** I ran the ACL-enabled retriever with a tier source returning a high-scoring *unauthorized* hit and a hook appending an unauthorized hit, across `Sources` = `["stub"]`, `["stub","hook"]`, `["hook"]`, `nil` — the unauthorized hits were dropped in **all four**. Structurally: `aclClause` is derived from `m.acl`/`f.Identity` only (`opensearch.go:275-285`), never from `sel`; `filterAuthorized` runs whenever `m.acl != nil`, independent of `Sources`; `selectSources` only *removes* entries from the three registries. **(c) ACL ordering not weakened:** the diff shows authorize-before-truncate (`opensearch.go:332-338`) and re-authorize-after-post-hooks (`opensearch.go:351-353`) unmoved; the only change is `m.postHooks` → the selected `postHooks`, which can only shrink the set. I confirmed the ACL clause still lands in a `Sources`-narrowed tier's `filterClauses` output. `TestTierHitsAuthorizedBeforeTruncation`, `TestPostHookAdditionsReauthorizedWithoutLosingTopK`, `TestRegisteredSeamsReceiveIdentityAndAreACLFiltered` all still PASS. |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| aposd-designing-deep-modules | Information hiding — tier-specific field knowledge confined to one module | PASS | The fact that `kind` is episodic-only lives in exactly one place (`filters.go:53-73`). There is no second hand-written gate to forget: `clauseFor` derives routing from the registry, and `validatePredicates`/`filterableFieldNames` derive the caller-facing vocabulary from the same map. Adding a field is one entry. |
| aposd-designing-deep-modules | Deep interface / low caller cognitive load | PASS | The caller-facing surface grew by exactly two struct fields (`Filter.Predicates`, `Filter.Sources`); routing, per-tier compilation, op/type validation, source partitioning and the error vocabulary are all hidden behind them. No caller needs to know that `graph` is a post-hook rather than a tier (`filters.go:181-185`). |
| aposd-designing-deep-modules | Silent Failure red flag — module hides errors but gives callers no way to know | PASS | This is the red flag the phase exists to close, and it is closed: an unknown field, an unknown source, an empty `Sources`, a bad op, and a non-scalar value all return errors naming the valid vocabulary; a predicate that would be inert on *every* selected tier is rejected rather than silently dropped (`filters.go:319-323`). |
| aposd-designing-deep-modules | Information Leakage — same knowledge in two modules | Note (not a violation) | `allowedFields["semantic"]` (`opensearch.go:378`) had to be hand-edited to add `extractor_version` alongside the registry entry. "Filterable" and "displayable" are genuinely different properties, so this is not duplicated knowledge per se — but nothing enforces the "filterable ⇒ visible" invariant the comment states. Non-blocking; see Notes. |
| cc-defensive-programming | External input validated at entry (barricade) | PASS | `Sources`/`Predicates` arrive from the MCP caller across a process boundary and are validated in `Search` *first* — before the ACL compile, before any HTTP (`opensearch.go:258-271`). Proven by the `failingHandler` tests: an invalid filter costs zero cluster round-trips. Inside the barricade the tiers may assume well-formed input, yet still re-check (defense in depth) — exactly the skill's "barricade reduces redundant validation; it does not replace defense-in-depth" rule, correctly applied on a security-critical path. |
| cc-defensive-programming | Bound untrusted input | PASS | `MaxPredicates = 32` (`filters.go:15`), enforced at `filters.go:305-307`, tested. Mirrors the existing `MaxK` rationale. |
| cc-defensive-programming | No executable code in assertions / assertions for bugs only | PASS (N/A by construction) | The package contains no assertions and never panics. Programmer errors that *are* bugs (empty or duplicate source name at wiring time) are surfaced as ERROR logs rather than silently swallowed (`opensearch.go:217-229`) — a deliberate, documented choice consistent with the package's no-panic convention. |
| cc-defensive-programming | No empty catch / no swallowed error | PASS | Every `err` on the new paths is returned or wrapped. `clauseFor`'s `declared=false, err=nil` is *not* a swallowed error — it is a distinct, documented routing signal, and the "owned by no selected tier" case is turned into a hard error one level up. |
| cc-defensive-programming | Correctness over robustness on the security-critical path | PASS | Every ambiguous input fails closed: unvalidated predicate at a tier → error, not an unchecked clause; ACL compile error → zero results; unresolvable source → error, not "all". |

## Notes (non-blocking)
1. **Coverage shortfall vs the stated 100% level** (detailed above): `filters.go:144-146` (non-scalar range bound — part of the injection barricade), and `checkSourceName`'s two ERROR branches (`opensearch.go:222-228`). I executed the range-bound branch by hand and it behaves correctly, but it deserves a committed test given DW-4.8's security framing. Recommend three small tests.
2. **`Text: ""` short-circuits BEFORE filter validation.** `Search` returns `(nil, nil)` at `opensearch.go:253-255`, so `Search(Query{Text:""}, Filter{Sources:["nope"]})` returns no error — verified by probe. This yields no wrong results (an empty query has always returned zero hits) and cannot leak anything, so it is not a defect against any DW item. But it does mean the "unknown source errors" contract has one hole, and a caller debugging an empty-text search gets no signal about a typo'd source. Consider moving the `Text == ""` short-circuit *after* the barricade.
3. **`filterable ⇒ projected` is a convention, not an invariant.** `semanticFilterable` and `allowedFields["semantic"]` are two hand-maintained lists; a future filterable field added to the registry can be filtered on but invisible on the hits, silently. A one-line unit test asserting `for f := range semanticFilterable where keyword: slices.Contains(allowedFields["semantic"], f)` would pin the property the DW-4.6 comment claims. (Time fields would need an exception, so it's not free.)
4. **Predicates do not constrain registered tier sources.** With `Sources: nil` and a `kind` predicate, the `experience` tier source is still searched and its hits are merged *unfiltered by kind* (the `TierSource` interface takes no `Filter`). This is consistent with the declared routing semantics ("tiers that do not declare the field are left unconstrained") and identical to how the semantic tier behaves, so it is not a defect — but the resulting hit list can contain hits that do not satisfy the caller's predicate, which is worth stating explicitly in the `Filter.Predicates` doc comment.
5. **Duplicate source names are logged, not rejected.** `checkSourceName` logs at ERROR and registers anyway; a duplicated name would then select two sources at once. Wiring-time only, correctly surfaced, no runtime exposure.

## Issues (if FAIL)
None blocking.

**Verdict: PASS**

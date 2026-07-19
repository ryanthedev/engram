# Review: Phase 4 - Filter core (registry + predicate routing) — sample 2

## Executed Results (Step 0)
- Test suite: `make test` (plus `go test ./... -count=1` and `go test ./internal/retrieval/... -count=1 -race -v`) → all packages **ok**, 0 failures, race detector clean. All 21 Phase-4 tests PASS.
- Typecheck/build: `make build` (`go build ./...`) → clean.
- Lint: `make lint` (`go vet ./...` + `revive -set_exit_status`) → clean.
- Coverage (`go tool cover -func`): every new function in `filters.go` at 100% except `validatePredicateValue` at **94.4%**; `Search` 98.4%; `filterClauses` 100%.

## Requirement Fulfillment

### DW-4.1
PREMISE:  "Each tier declares its filterable fields; episodic declares `kind`, semantic declares `subject`/`predicate`/`object`/`extractor_version`, both declare their time fields."
EVIDENCE: internal/retrieval/filters.go:53-73 (`episodicFilterable`, `semanticFilterable`), wired at internal/retrieval/opensearch.go:145,151 (`filterable:` on each `tierRetriever`).
TRACE:    `episodicFilterable.names()` → [created_at kind occurred_at]; `semanticFilterable.names()` → [created_at expired_at extractor_version invalid_at object predicate subject valid_at]; `semanticFilterable["kind"]` → absent. Executed: TestDW_4_1_TierFilterableFieldRegistry PASS, and TestDW_4_1_DeclaredFieldsExistInIndexTemplates PASS (every declared field is mapped with the declared type in `internal/store/templates/{episodic,semantic}.json`, both `dynamic:strict`).
VERDICT:  PASS

### DW-4.2
PREMISE:  "A `kind` predicate leaves the semantic tier's query unconstrained (not zeroed) — asserted by a test that would fail under a naive shared clause."
EVIDENCE: internal/retrieval/filters.go:99-109 (`clauseFor` returns `declared=false, err=nil` for an unowned field), internal/retrieval/opensearch.go:582-591 (`filterClauses` `continue`s on `!declared`).
TRACE:    Filter{TenantID:"t1", Predicates:[{kind,term,"decision"}]} → episodic body contains `{"term":{"kind":"decision"}}`; semantic body contains no "kind" substring and is **byte-equal to the no-predicate baseline body**. Executed: TestDW_4_2_KindPredicateRoutesToEpisodicOnly PASS. The byte-equality assertion is exactly what a naive shared clause would fail (it would emit `{"term":{"kind":...}}` into `sem-idx`). Mirror case TestSemanticOnlyPredicateLeavesEpisodicUnconstrained PASS.
VERDICT:  PASS

### DW-4.3
PREMISE:  "With no predicates and no `Sources`, the emitted OpenSearch query body is byte-identical to today's (golden-byte regression test)."
EVIDENCE: internal/retrieval/filter_routing_test.go:168-222 (golden strings, 4 cases × 2 tiers); code path internal/retrieval/opensearch.go:555-593 + 611-649.
TRACE:    Independently verified the golden claim rather than trusting the comment: `git diff internal/retrieval/opensearch.go` shows `buildQuery` is **textually unchanged**, and `filterClauses`' ACL/tenancy/validity section is unchanged — the only addition is the predicate loop appended *after* it, which iterates `f.Predicates` (empty ⇒ zero iterations). With `Predicates == nil` the emitted bytes are produced by identical code to HEAD. Executed: TestDW_4_3_GoldenQueryBodyUnchangedWithoutFilters PASS (hybrid/bm25 × zero-filter/tenancy+validity), plus the pre-existing TestBuildQueryMemoryPathByteIdenticalWhenSortNil PASS.
VERDICT:  PASS

### DW-4.4
PREMISE:  "A predicate on an unknown field errors, and the error names the valid filterable fields."
EVIDENCE: internal/retrieval/filters.go:301-326 (`validatePredicates`), :268-284 (`filterableFieldNames`), called at internal/retrieval/opensearch.go:268 before any HTTP or ACL work.
TRACE:    Filter{Predicates:[{password,term,"x"}]} → `retrieval: unknown or unfilterable field "password" for the selected sources; valid filterable fields: created_at, expired_at, extractor_version, invalid_at, kind, object, occurred_at, predicate, subject, valid_at`, with zero cluster round-trips (the test wires a handler that fails the test on any request). Executed: TestDW_4_4_UnknownFieldErrorNamesValidFields PASS (incl. the `Sources:["semantic"] + kind` case, where the offered vocabulary correctly excludes episodic's fields). Related: TestUnsupportedOpForFieldTypeErrors PASS (bad op names the valid ops).
VERDICT:  PASS

### DW-4.5
PREMISE:  "`Sources: [\"semantic\"]` skips the graph post-hook and the episodic tier entirely."
EVIDENCE: internal/retrieval/filters.go:240-263 (`selectSources` — the single gate for all three fan-out sites), consumed at internal/retrieval/opensearch.go:271, 293, 301, 344, 351.
TRACE:    Filter{Sources:["semantic"]} → `resolveSources` → set{semantic} → `selectSources` returns tiers=[semantic], tierSrcs=[], postHooks=[]. Observed: the fake cluster saw exactly one request, to `sem-idx`; the registered "experience" TierSource ran 0 times; the "graph" PostHook ran 0 times. Executed: TestDW_4_5_SourcesSkipsEpisodicTierAndGraphPostHook PASS; TestSourcesSelectsRegisteredTierSourceAndPostHook PASS (converse — neither built-in index queried).
VERDICT:  PASS

### DW-4.6
PREMISE:  "`extractor_version` appears on semantic hits."
EVIDENCE: internal/retrieval/opensearch.go:378 (`allowedFields["semantic"]` now includes `extractor_version`; the projection is applied at :362).
TRACE:    A semantic `_source` of {statement, subject, extractor_version:"v3", fact_embedding:[...]} → projected Hit.Fields["extractor_version"] == "v3" and `fact_embedding` dropped. Executed: TestDW_4_6_ExtractorVersionOnSemanticHits PASS; TestExtractorVersionPredicateFiltersSemanticTier PASS (filterable end-to-end: the clause lands on `sem-idx` only).
VERDICT:  PASS

### DW-4.7
PREMISE:  "An unknown source name errors, and the error names the valid sources."
EVIDENCE: internal/retrieval/filters.go:209-229 (`resolveSources`), :186-199 (`sourceNames` — one namespace over tiers + tier sources + post-hooks).
TRACE:    Filter{Sources:["semantic","epsiodic"]} → `retrieval: unknown source "epsiodic"; valid sources: episodic, experience, graph, semantic`, with zero HTTP calls, zero tier-source calls, zero hook calls. Executed: TestDW_4_7_UnknownSourceErrorNamesValidSources PASS (4 subtests: unknown name, empty-non-nil, nil-means-all, duplicate names idempotent).
VERDICT:  PASS

### DW-4.8
PREMISE:  "An injection-shaped filter value (e.g. a string containing OpenSearch query DSL) is parameterized into a clause structure, not interpolated into the query body."
EVIDENCE: internal/retrieval/filters.go:120-164 (`validatePredicateValue` / `isScalar` — the scalar barricade), internal/retrieval/knowledge.go:308-320 (`filterClause` builds `map[string]any` structures), internal/retrieval/opensearch.go:647 (`json.Marshal` — the only serialization; no string concatenation anywhere in the body path).
TRACE:    Value `decision"}},{"match_all":{}},{"term":{"x":"` → emitted body decodes as valid JSON; the BM25 `bool.filter` array holds exactly 2 clauses (tenant, kind) — no injected clause — and `filter[1].term.kind` is a **string leaf** equal to the caller's value verbatim (escaped by the marshaler). Executed: TestDW_4_8_InjectionShapedValueIsParameterized PASS (4 injection shapes); TestNonScalarPredicateValueRejected PASS (map/slice/nil values rejected before any I/O, so a nested DSL object can never occupy a value slot).
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have corresponding automated tests, all of which ran and passed in Step 0 (test names carry the DW ids: `TestDW_4_1_…` … `TestDW_4_8_…`).
- [ ] Test coverage matches the stated level (100%) — **one gap**: `validatePredicateValue` is at 94.4%. The uncovered branch is internal/retrieval/filters.go:144-147 — a **non-scalar value in a range BOUND** (`{"gte": {"match_all":{}}}`), i.e. the range-op sibling of the DW-4.8 smuggling vector that `TestNonScalarPredicateValueRejected` covers only for `term`. I exercised it myself (temporary probe test, run then removed): it correctly returns `retrieval: range bound "gte" on field "valid_at" must be a scalar, got map[string]interface {}`. So the behavior is right; the repo suite just has no case for it. Recommend adding one row to `TestNonScalarPredicateValueRejected`.

## Edge Cases (plan-listed)
| Edge case | Status | Evidence |
|---|---|---|
| Empty `Predicates` ⇒ byte-identical query | PASS | TestDW_4_3_… PASS; diff shows the no-predicate emission path is unchanged code. |
| Predicate valid on one tier, absent on another (route, don't fail) | PASS | TestPredicateValidOnBothTiersReachesBoth PASS (`created_at` reaches both); TestDW_4_2_… / TestSemanticOnlyPredicate… PASS (routing miss ⇒ unconstrained, not zeroed, not an error). |
| `Sources` naming an unknown source | PASS | TestDW_4_7_…/unknown name PASS; error names the vocabulary, no I/O. |
| `Sources: []` vs `nil` (nil=all, empty=error) | PASS | filters.go:209-216; TestDW_4_7_…/"empty but non-nil is an error, not silent-all" and /"nil means every source" both PASS. |
| Range with only `gte` or only `lte` | PASS | TestRangeBounds PASS (gte-only, lte-only, both, neither⇒error, non-map⇒error). |

## Dead Code
None found. `go vet` and `revive` are clean; every new symbol (`MaxPredicates`, `fieldTypeDate`, `sourceSet.selected`, `selectSources`, …) has a live caller or a test consumer. No unreachable code after early returns, no debug statements, no commented-out blocks.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | The fan-out at opensearch.go:291-309 now sizes `results` to the *selected* lists (`len(tiers)+len(tierSrcs)`) and indexes tier sources at `len(tiers)+j` — I checked for the classic off-by-one/aliasing bug this refactor invites (a goroutine writing an index computed from the *unfiltered* slice): indices are disjoint and in range for every selection. `-race` over the whole package is clean. Registration is documented and used as wiring-time-only. |
| Error Handling | PASS | Validation precedes all I/O (opensearch.go:264-270); tier `filterClauses` now returns an error and `search` propagates it (:488-491) rather than emitting an unchecked clause; ACL compile failure still returns zero results fail-closed; post-hook errors now name the hook. No empty catch/ignored error introduced. |
| Resources | N/A | No new handles, connections, locks, or goroutine ownership; the response body close and the embed-timeout context are untouched by this phase. |
| Boundaries | PASS | Probed the adversarial inputs: `nil` value ⇒ rejected (`isScalar(nil)==false`); empty bounds map ⇒ error; `Predicates` of length `MaxPredicates+1` ⇒ error (TestTooManyPredicatesRejected); duplicate source names in `Sources` ⇒ idempotent (set semantics); `Sources` selecting only a post-hook ⇒ empty tier list, `results` len 0, `merged` nil, `filterAuthorized(nil)` safe. One matter-of-degree looseness found (extra bound keys) — see Notes; it is not a defect in any listed requirement. |
| Security | PASS | (1) No interpolation: the body is built exclusively as `map[string]any` and serialized once by `json.Marshal` (opensearch.go:647); grep confirms no string concatenation of caller values into the body. Injection-shaped values land as escaped string leaves (TestDW_4_8_…), and the only route by which a value could become *structure* — a map/slice — is rejected at the barricade for both term values and range bounds (probe above). (2) `Sources` can only REMOVE: `selectSources` filters the three registered slices; it never touches `aclClause` (compiled from `f.Identity` at :275-285 independently of the selection) and never skips `filterAuthorized`. There is no `Sources` value that turns ACL enforcement off — a source that runs, runs under the same clause + re-verification as before. (3) The ACL ordering is intact: `filterAuthorized` still runs BEFORE the score sort and top-k truncation (:332-338), and the post-hook re-authorization still runs after the hook chain (:351-353); the diff shows no reordering, only `m.postHooks` → the selected `postHooks` slice. `Sources` excluding "graph" means no hook runs, so skipping the re-auth pass in that case admits nothing. |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| aposd-designing-deep-modules | Deep interface / information hiding | PASS | The tier's filter surface is one declarative map (`FilterableFields`); callers of `Search` learn nothing about clause shapes, and adding a filterable field is a single registry entry — no second hand-written gate. `clauseFor`'s (clause, declared, err) triple is the module's way of hiding "which tier owns this field" from `filterClauses`. |
| aposd-designing-deep-modules | Information leakage / duplicated knowledge | PASS | Field ownership lives in exactly one place (filters.go:53-73); `filterableFieldNames`, `validatePredicates` and `filterClauses` all read it rather than restating it. `filterClause` is reused from the knowledge path rather than duplicated. |
| aposd-designing-deep-modules | Silent failure red flag | PASS | This is the phase's whole thesis and it holds: an unknown field, an unknown source, an empty `Sources`, a bad op, and a non-scalar value are all errors that name the valid vocabulary — none is a silently inert clause. The one *routing* silence (a predicate another tier owns) is the documented, tested contract, and is impossible to reach without at least one selected tier owning the field. |
| cc-defensive-programming | External input validated at entry (barricade) | PASS | `Search` validates `Sources` then `Predicates` before ACL compilation or any HTTP call (opensearch.go:264-270); count is bounded (`MaxPredicates`), field/op/type/value shape all checked. Downstream code may then assume well-formed input. |
| cc-defensive-programming | Defense in depth inside the barricade (security-critical path) | PASS | `filterClauses` re-validates through `clauseFor` and fails closed if an unvalidated predicate ever reaches a tier (TestFilterClausesFailsClosedOnUnvalidatedPredicate PASS) — the skill's "validate again on security-critical paths" rule, honored rather than assumed away. |
| cc-defensive-programming | No empty catch / no swallowed error; errors surfaced | PASS | Every error is returned; the only deliberate non-error swallow is the ACL fail-closed path (returns zero results + WARN log), which predates this phase. |
| cc-defensive-programming | Assertions used for bugs only; no executable code in assertions | PASS | No assertions/panics added. `checkSourceName` logs a wiring-time programmer error at ERROR without panicking, consistent with the package's no-panic convention. |
| cc-defensive-programming | Consistent handling of malformed external input | WARNING (Note) | `{"gt": x}` alone errors, but `{"gte": a, "gt": b}` silently drops `gt` — see Notes. A matter of strictness degree, not a demonstrated failure of any listed requirement, so it is a Note, not a FAIL. |

## Notes (non-blocking)
1. **Unrecognized range-bound keys are silently dropped when a valid bound is also present.** Demonstrated (probe, run then removed): `Predicate{Field:"valid_at", Op:"range", Value:{"gte":"2026-01-01","gt":"2027-01-01"}}` → clause `{"range":{"valid_at":{"gte":"2026-01-01"}}}` — the `gt` bound vanishes with no error, so the caller gets a *looser* filter than it asked for. Note the inconsistency: `{"gt":"..."}` alone *is* rejected (set==0). This is inherited from `filterClause` (internal/retrieval/knowledge.go:308-330), is not covered by any DW item or listed edge case, and cannot loosen the ACL (ACL clauses are built separately and are unaffected). Suggested hardening, in the spirit of the phase: reject bound keys outside {gte, lte} in `validatePredicateValue`.
2. **Coverage gap** (see Test-DW Coverage): add a `range`-op row with a map-valued bound to `TestNonScalarPredicateValueRejected` to close `validatePredicateValue` to 100%.
3. `validatePredicates` scopes the "valid fields" vocabulary to the *selected built-in tiers*, so `Sources:["graph"]` + any predicate errors with `valid filterable fields: (none declared)`. Correct and fail-closed, but the message is terse for that (unlikely) combination.
4. A duplicate registration name is logged at ERROR and still registered, so `Sources:["dup"]` would select both implementations. Harmless (both run ACL-enforced) and it is a wiring-time programmer error, but worth remembering if source names ever become dynamic.

## Issues (if FAIL)
None.

**Verdict: PASS**

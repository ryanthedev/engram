# Review: Phase 4 - Filter core (registry + predicate routing) — sample 3

## Executed Results (Step 0)
- Test suite: `make test` → all packages `ok` (no failures). Re-run fresh: `go test ./internal/retrieval/... ./internal/graph/... ./cmd/... -count=1` → all `ok`.
- Race: `go test ./internal/retrieval/ -race -count=1` → ok (1.07s).
- Typecheck/vet: `make lint` → `go vet ./...` + revive → clean, exit 0.
- Build: `make build` → `go build ./...` → clean.
- Coverage of new code (`go tool cover -func`): filters.go 94–100% per func (`validatePredicateValue` 94.4%, one uncovered branch — probed by hand below, see Notes); `filterClauses` 100%; `Search` 98.4%; `checkSourceName` 62.5% (the two error-log branches).
- Independent golden capture: extracted the pre-change tree (`git archive HEAD`) to `/tmp/p4-sample-3/head`, added a capture test, and diffed the emitted bodies against the goldens hard-coded in `filter_routing_test.go` — byte-identical for all 4 cases (see DW-4.3).
- Adversarial probes written and run against a copy of the current tree (`/tmp/p4-sample-3/cur/internal/retrieval/probe_test.go`): both PASS (see Correctness Dimensions / Security).

## Requirement Fulfillment

### DW-4.1
PREMISE:  "Each tier declares its filterable fields; episodic declares `kind`, semantic declares `subject`/`predicate`/`object`/`extractor_version`, both declare their time fields."
EVIDENCE: internal/retrieval/filters.go:53-73 (`episodicFilterable`, `semanticFilterable`); wired onto the tiers at internal/retrieval/opensearch.go:145,151 (`filterable:` field, declared at opensearch.go:449).
TRACE:    `episodicFilterable.names()` → [created_at kind occurred_at]; `semanticFilterable.names()` → [created_at expired_at extractor_version invalid_at object predicate subject valid_at]; each spec's Type/Ops asserted, and `semanticFilterable["kind"]` asserted absent. A second test parses `internal/store/templates/{episodic,semantic}.json` and confirms every declared field is mapped with the declared type under `"dynamic":"strict"`.
VERDICT:  PASS — `TestDW_4_1_TierFilterableFieldRegistry`, `TestDW_4_1_DeclaredFieldsExistInIndexTemplates` both PASS.

### DW-4.2
PREMISE:  "A `kind` predicate leaves the semantic tier's query unconstrained (not zeroed) — asserted by a test that would fail under a naive shared clause."
EVIDENCE: internal/retrieval/filters.go:99-109 (`clauseFor` returns declared=false, nil clause, nil err for an unowned field); internal/retrieval/opensearch.go:582-591 (`filterClauses` `continue`s on !declared).
TRACE:    Filter{TenantID:"t1", Predicates:[{kind,term,"decision"}]} → episodic body contains `{"term":{"kind":"decision"}}`; semantic body contains no "kind" substring AND is byte-equal to the semantic body emitted for the same query with zero predicates. Under a shared-clause implementation the semantic body would carry the kind term (a field `semantic.json` does not map, `dynamic:strict`) — the test fails in that world.
VERDICT:  PASS — `TestDW_4_2_KindPredicateRoutesToEpisodicOnly` PASS; mirror `TestSemanticOnlyPredicateLeavesEpisodicUnconstrained` PASS.

### DW-4.3
PREMISE:  "With no predicates and no `Sources`, the emitted OpenSearch query body is byte-identical to today's (golden-byte regression test)."
EVIDENCE: internal/retrieval/filter_routing_test.go:168-222 (goldens); code path: opensearch.go:555-592 appends predicates strictly AFTER acl/tenant/validity clauses, and `buildQuery` (opensearch.go:611) is untouched by the diff.
TRACE:    I did not take the test's word for the goldens. I re-extracted the tree at HEAD (pre-change), ran the same 4 inputs (hybrid/bm25 × zero-filter/tenancy+validity) through the UNMODIFIED retriever, and captured the request bodies: all 8 bodies are character-for-character identical to the `wantEpisodic`/`wantSemantic` strings in the test. Empty `Predicates` ⇒ zero clauses appended ⇒ same `filters` slice ⇒ same marshaled body.
VERDICT:  PASS — `TestDW_4_3_GoldenQueryBodyUnchangedWithoutFilters` (4 subtests) PASS, and the goldens are independently confirmed to be HEAD's bytes.

### DW-4.4
PREMISE:  "A predicate on an unknown field errors, and the error names the valid filterable fields."
EVIDENCE: internal/retrieval/filters.go:301-326 (`validatePredicates` → `fmt.Errorf("...unknown or unfilterable field %q...valid filterable fields: %s", ...)`, list from `filterableFieldNames(sel)`), called from Search at opensearch.go:268 BEFORE the ACL compile and before any HTTP call.
TRACE:    Filter{Predicates:[{password,term,"x"}]} → no tier declares "password" → error listing kind, subject, predicate, object, extractor_version, occurred_at, valid_at… The test's server is a `failingHandler` (any HTTP request fails the test), so validation is proven to precede I/O. Second subtest: Sources:["semantic"] + kind → error, and the valid-field list does not offer episodic-only fields.
VERDICT:  PASS — `TestDW_4_4_UnknownFieldErrorNamesValidFields` (2 subtests) PASS.

### DW-4.5
PREMISE:  "`Sources: [\"semantic\"]` skips the graph post-hook and the episodic tier entirely."
EVIDENCE: internal/retrieval/filters.go:240-263 (`selectSources` — one partition consumed by all three fan-out/chain sites); opensearch.go:271, 293/301 (tier + tier-source goroutines iterate the selected slices), 344 (post-hook chain iterates the selected hooks).
TRACE:    Sources:["semantic"] → sel={semantic} → selectSources returns [semantic tier], [], [] → exactly one HTTP search (`sem-idx`), registered tier source `experience` called 0 times, `graph` post-hook run 0 times. Verified against a fake cluster that records every request path.
VERDICT:  PASS — `TestDW_4_5_SourcesSkipsEpisodicTierAndGraphPostHook` PASS; `TestSourcesSelectsRegisteredTierSourceAndPostHook` PASS (inverse: named sources run, built-in tiers issue zero queries).

### DW-4.6
PREMISE:  "`extractor_version` appears on semantic hits."
EVIDENCE: internal/retrieval/opensearch.go:378 (`allowedFields["semantic"]` now includes `extractor_version`); filters.go:68 (declared filterable).
TRACE:    Fake cluster returns a semantic `_source` with extractor_version="v3" and fact_embedding → Search → hit.Fields["extractor_version"] == "v3", and fact_embedding is still stripped by the projection. Filter side: Predicates:[{extractor_version,term,"v3"}] → semantic body carries `{"term":{"extractor_version":"v3"}}`, episodic body carries no such field.
VERDICT:  PASS — `TestDW_4_6_ExtractorVersionOnSemanticHits`, `TestExtractorVersionPredicateFiltersSemanticTier` PASS.

### DW-4.7
PREMISE:  "An unknown source name errors, and the error names the valid sources."
EVIDENCE: internal/retrieval/filters.go:209-229 (`resolveSources`), called from opensearch.go:264 first thing after the empty-text short-circuit; vocabulary from `sourceNames()` (filters.go:186) = built-in tiers + registered tier sources + registered post-hooks.
TRACE:    Sources:["semantic","epsiodic"] (typo) → error naming episodic, experience, graph, semantic; zero HTTP requests, tier source and post-hook both uncalled. Sources:[] → error ("omit it to search every source…"), zero I/O. Sources:nil → both built-in tiers queried, tier source and hook each run once. Duplicate names in Sources are idempotent (sem-idx searched exactly once).
VERDICT:  PASS — `TestDW_4_7_UnknownSourceErrorNamesValidSources` (4 subtests) PASS.

### DW-4.8
PREMISE:  "An injection-shaped filter value (e.g. a string containing OpenSearch query DSL) is parameterized into a clause structure, not interpolated into the query body."
EVIDENCE: internal/retrieval/knowledge.go:308-332 (`filterClause` places the value as a Go map VALUE — `map[string]any{"term": map[string]any{field: value}}` — which `json.Marshal` (opensearch.go:647) escapes); internal/retrieval/filters.go:120-164 (`validatePredicateValue`/`isScalar` reject any map/slice value, closing the one path by which a value could become structure).
TRACE:    Value = `decision"}},{"match_all":{}},{"term":{"x":"` on field kind → emitted episodic body decodes as valid JSON; the BM25 bool's `filter` array has EXACTLY 2 clauses (tenant + kind); `filter[1].term.kind` is a string leaf equal to the caller's bytes verbatim. Same for `*`, `{"match_all":{}}`, `" OR 1==1 //`. Map/slice/nil values are rejected at the barricade with a "scalar" error.
VERDICT:  PASS — `TestDW_4_8_InjectionShapedValueIsParameterized` (4 subtests) and `TestNonScalarPredicateValueRejected` (3 subtests) PASS.

**All requirements met:** YES

## Edge cases (plan-listed — same standing as DW)
| Edge case | Handled | Evidence |
|---|---|---|
| Empty `Predicates` ⇒ body byte-identical to today | YES | DW-4.3 above; goldens independently re-captured from the pre-change tree |
| Predicate valid on one tier, absent on another (route, don't fail) | YES | `TestPredicateValidOnBothTiersReachesBoth` (created_at → both bodies), `TestClauseForRoutesRatherThanFails` (declared=false, no error) |
| `Sources` naming an unknown source | YES | `TestDW_4_7…/unknown name` — error names valid sources, zero I/O |
| `Sources: []` vs `nil` (nil = all, empty = error, not silent-all) | YES | filters.go:209-216; `TestDW_4_7…/empty but non-nil is an error` + `/nil means every source` |
| Range with only `gte` or only `lte` | YES | `TestRangeBounds` — gte-only, lte-only, both emit the expected clause; neither/`gt`-only/non-map error |

## Test-DW Coverage
- [x] Every DW item has at least one automated test that RAN in Step 0, named for its DW id (DW-4.1 … DW-4.8), plus supporting tests.
- [x] Coverage level: new logic is at or near 100% by `go tool cover` (see Executed Results). The two uncovered branches are `validatePredicateValue`'s non-scalar RANGE BOUND and `checkSourceName`'s two error-log branches. I exercised the first by hand (probe below): it fails closed. Neither is a DW item; noted, not blocking.

## Dead Code
None found. No unused imports (vet+revive clean), no unreachable code after early returns, no debug prints, no commented-out blocks. `MaxPredicates`, `FieldSpec`, `FilterableFields` are exported but only consumed in-package/tests today — see Notes.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | Adversarial case: the tier-source goroutines were re-indexed by this diff (`results[len(tiers)+j]` with the SELECTED slices). Traced: `results` is sized `len(tiers)+len(tierSrcs)` (opensearch.go:291) and each goroutine writes one distinct pre-assigned index (i, and len(tiers)+j) — no overlap, no append. `aclClause`/`f.Predicates` are read-only across goroutines (each tier builds its own clause slice). `go test -race` on the package: clean. |
| Error Handling | PASS | Every error path returns rather than swallowing: resolveSources/validatePredicates errors abort before I/O (opensearch.go:264-270); `filterClauses` now returns an error and `search` propagates it (opensearch.go:488-491); post-hook errors are wrapped with the hook name. No empty catch equivalents. ACL compile error still fail-closed (returns zero hits, logs denial). |
| Resources | PASS | No new handles/locks/goroutines beyond the existing per-tier fan-out, which is bounded by the SELECTED source count (≤ before). Response bodies still closed via defer (opensearch.go:510). Predicate count bounded by MaxPredicates=32, so a caller cannot inflate the query body without bound. |
| Boundaries | PASS | Probed: empty Predicates (no clauses), 33 predicates (rejected), empty range map (rejected), unrecognized-bound-only `{"gt":…}` (rejected — `set==0`), non-map range value (rejected), nil value (rejected), Sources nil vs [] vs duplicates. `sourceSet(nil).selected()` returns true by design (nil = all) and is only ever constructed from nil Sources. |
| Security | PASS | Three probes, all traced through the real code: (1) injection-shaped values stay string leaves (DW-4.8); (2) non-scalar values — including a DSL object smuggled into a RANGE BOUND, the one branch the suite misses — are rejected: `retrieval: range bound "gte" on field "valid_at" must be a scalar, got map[string]interface {}`, and the tier compiler independently fails closed on the same input; (3) `Sources` cannot ADMIT an ACL-denied hit — with a real `acl.Filter`, `Sources:["experience"]`, `["experience","graph"]`, and `nil` each returned 0 hits when the registered tier source and post-hook handed back cross-tenant/org documents. Order of enforcement is unchanged by the diff: authorize-before-truncate (opensearch.go:332-338) then re-authorize after post-hooks (opensearch.go:351-353); `selectSources` only removes sources and never touches `aclClause` or `filterAuthorized`. |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| aposd-designing-deep-modules | Information hiding / no leakage | PASS | Field→clause knowledge lives only in `FilterableFields` + `filterClause`; callers (Search, tiers, cmd wiring) know none of it. Adding a filterable field is one map entry — no second hand-written gate to forget. |
| aposd-designing-deep-modules | Deep interface (small surface, hidden implementation) | PASS | The caller-facing surface grows by exactly two struct fields (`Filter.Predicates`, `Filter.Sources`); routing, validation, clause compilation, and source partitioning are all behind it. |
| aposd-designing-deep-modules | No information leakage across module boundary (registration) | PASS | Source names live in `namedTierSource`/`namedPostHook` in the registry rather than forcing a `Name()` method on `TierSource`/`PostHook` — implementors (internal/experience, internal/graph) are unchanged. |
| aposd-designing-deep-modules | No silent failure (failures observable) | PASS | Every reject path returns an error naming the valid vocabulary; the one non-error path (routing miss) is the documented, tested contract, not a swallowed failure. Registration bugs are logged at ERROR (see Notes for the residual). |
| aposd-designing-deep-modules | Pass-through / shallow module | PASS | `filters.go` is not a pass-through: it owns validation, routing, and the source vocabulary rather than forwarding to another layer. |
| cc-defensive-programming | External input validated at entry (barricade) | PASS | `Search` validates Sources then Predicates before ACL compile and before any HTTP call — `failingHandler` tests prove zero I/O precedes a rejection. |
| cc-defensive-programming | Defense in depth inside the barricade (security-critical path) | PASS | `tierRetriever.filterClauses` re-validates each predicate rather than trusting the barricade (`TestFilterClausesFailsClosedOnUnvalidatedPredicate`, and my range-bound probe). ACL re-verification of tier-source/post-hook hits is retained. |
| cc-defensive-programming | No empty catch / no swallowed error | PASS | vet+revive clean; every `err` in the new code is returned or logged with a denial. |
| cc-defensive-programming | Assertions carry no side effects / bugs-only | N/A | No assertions introduced (Go; the package deliberately never panics). |
| cc-defensive-programming | Bounded external input | PASS | `MaxPredicates = 32` mirrors the `MaxK` rationale; `TestTooManyPredicatesRejected` PASS. |

## Notes (non-blocking)
1. **Registered tier sources receive no predicates.** `validatePredicates` and `filterClauses` only consult the BUILT-IN tiers. With `Sources: nil` and, say, `kind=decision`, the episodic/semantic tiers are constrained but a registered tier source (`experience`, wired in cmd/engram-server/stages_experience.go:65) returns hits that ignore the predicate entirely. Not a DW item, and not reachable today (no caller sets `Filter.Predicates` — the gRPC/MCP surface lands in Phase 5), but it is the same "silently unconstrained source" shape this phase set out to eliminate; worth an explicit decision in Phase 5.
2. **Predicate on a field owned only by an unselected tier + no built-in tier selected.** `Sources:["experience"]` + any predicate errors with `valid filterable fields: (none declared)` — technically correct and self-correcting, but the message reads oddly. Cosmetic.
3. **`checkSourceName` logs and proceeds.** A duplicate registration name makes one `Sources` token select two sources; an empty name makes a source unselectable. Both are logged at ERROR at wiring time and neither can admit an unauthorized hit (ACL runs regardless), so this is a startup-hygiene issue only. Untested (the 62.5% coverage on that function).
4. **Uncovered branch:** the non-scalar RANGE BOUND rejection in `validatePredicateValue` (filters.go:145) has no test in the suite. I verified by hand that it fails closed; given the 100% coverage target and that it is a security-relevant branch, a two-line table entry in `TestRangeBounds` would close it.
5. Exported-but-internal types (`FieldSpec`, `FilterableFields`, `MaxPredicates`) have no out-of-package consumer yet. Fine if Phase 5 uses them; otherwise they widen the package API for no caller.

## Issues (if FAIL)
None.

**Verdict: PASS**

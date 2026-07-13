# Review: Phase 5 - LLM-facing API (proto + flat MCP schema) — sample 1

## Executed Results (Step 0)
- Test suite: `make test` (`go test ./...`) → all packages **ok**, 0 failures. Re-ran the phase packages with `-count=1 -v`: `internal/mcp`, `internal/server`, `internal/engramclient`, `internal/retrieval` all PASS.
- Typecheck/vet: `make lint` (`go vet ./...` + revive) → **exit 0**, no findings.
- Build: `make build` (`go build ./...`) → **exit 0**.
- Proto: `make proto-check` (`./scripts/codegen.sh` → `codegen: ok`; `git diff --exit-code -- api/engrampb`) → **exit 0**.
- Coverage: `internal/mcp` 89.8%, `internal/server` 87.6%, `internal/engramclient` 70.3% of statements.

## Requirement Fulfillment

### DW-5.1
PREMISE:  "`memory_search` accepts every flat filter param; each maps to the correct internal predicate and tier."
EVIDENCE: `internal/mcp/searchargs.go:16-47` (closed `searchParams` allowlist + `searchArgs` struct), `internal/mcp/tools.go:66-88` (schema advertises all 11), `internal/engramclient/client.go:229-250` (flat wire mapping), `internal/server/searchfilter.go:52-73` (flat → `retrieval.Predicate`), `internal/retrieval/filters.go:100-122` (per-tier registry + `TimeField` alias).
TRACE:    `{"query":"q","kind":"conversation","subject":"s","predicate":"p","object":"o","extractor_version":"v2","since":"…","until":"…","include_superseded":true,"sources":["episodic","semantic"]}` → `parseSearchArgs` decodes to `SearchFilter` with parsed `time.Time` bounds → `engramclient.Search` copies each field flat onto `SearchRequest` (fields 6-14), `timestamppb` for since/until → `compileSearchFilter` emits `term` predicates for kind/subject/predicate/object/extractor_version and ONE `range` predicate on `retrieval.TimeField` ("time") → registry routes `kind`→episodic `kind`, triple+`extractor_version`→semantic only, `time`→episodic `occurred_at` AND semantic `valid_at` (`FieldSpec.Target`). A `kind` predicate is `declared == false` on semantic, so it is routed away rather than zeroing that tier (`filters.go:148-160`, `opensearch.go:592-601`).
VERDICT:  **PASS** — `TestDW_5_1_SearchAcceptsEveryFlatFilterParam`, `TestSearchSchemaAdvertisesEveryFilterParam` (mcp), `TestDW_5_1_SearchFilterTravelsFlatOnTheWire` (engramclient), `TestDW_5_1_FlatParamsCompileToPredicates`, `TestDW_5_1_TimeBoundsAreOneRangePredicate` (server), `TestDW_5_1_TimeAliasRoutesToPerTierField`, `TestDW_5_1_SemanticParamsRouteToSemanticOnly`, `TestDW_5_1_OpenTimeBoundsCompile` (retrieval) — all ran and passed.

### DW-5.2
PREMISE:  "`include_superseded: true` returns historical facts; absent/false preserves today's `ValidOnly` behavior."
EVIDENCE: `internal/server/server.go:152` (`ValidOnly: !req.GetIncludeSuperseded()`), `internal/retrieval/opensearch.go:579-590` (validity clause gated on `f.ValidOnly && t.supportsValidity`), `api/proto/engram.proto:158-162` (`reserved 5 / "valid_only"`).
TRACE:    absent → `IncludeSuperseded=false` → `ValidOnly=true` → the `expired_at`/`invalid_at` bi-temporal bool clause is emitted, byte-identical to the clause `engramclient` used to trigger with its hardcoded `ValidOnly: true`. `true` → `ValidOnly=false` → that one clause is omitted, so superseded/retracted versions match. Verified `grep -rn ValidOnly` that `server.go:152` is the **sole** producer on the request path and that `GetValidOnly` no longer exists in `engram.pb.go`.
VERDICT:  **PASS** — `TestDW_5_2_IncludeSupersededDrivesValidOnly` (both sub-cases), `TestDW_5_2_ValidOnlyGatesTheHistoryClause`, `TestDW_5_2_IncludeSupersededCannotBypassACL` — ran and passed.

### DW-5.3
PREMISE:  "`sources: [\"semantic\"]` excludes episodic and graph hits end-to-end from the MCP call."
EVIDENCE: `internal/mcp/searchargs.go:30,129-142` (source vocabulary + validation), `internal/engramclient/client.go:242` (`Sources: f.Sources`), `internal/server/server.go:154` (`Sources: req.GetSources()`), `internal/retrieval/filters.go:265-285, 349-384` (`resolveSources` → `selectSources`, the single gate for tiers, tier-sources AND post-hooks).
TRACE:    `sources:["semantic"]`, no predicates → `sel={semantic}`, `filtered=false` → `selectSources` bottom branch filters all three collections: episodic tier not selected, `experience` tier-source not selected, `graph` post-hook not selected. Only `sem-idx` is queried; the hook never runs, so no graph hit can enter the fused list.
VERDICT:  **PASS** — `TestDW_5_3_SourcesReachTheBackend` (mcp), `TestDW_5_3_SourcesReachTheFilter` (server), `TestDW_5_3_SemanticOnlySearchHasNoEpisodicOrGraphHits` (retrieval; asserts searched indices == `[sem-idx]`, `tier.calls==0`, `hook.calls==0`, and every surviving hit's `Source=="semantic"`) — ran and passed.

### DW-5.4
PREMISE:  "An invalid filter field or malformed time range is rejected at the MCP/gRPC entry with an error naming the valid fields — never reaching the retriever."
EVIDENCE: `internal/mcp/searchargs.go:62-100` (double-decode allowlist; unknown key named + full `searchParams` list; `since > until` rejected; sources validated), `internal/mcp/tools.go:286-296` (`parseSearchArgs` runs BEFORE `s.backend.Search`), `internal/server/searchfilter.go:40-50, 102-105` (`compileSearchFilter` runs at the top of `Server.Search` before `Retriever.Search`; `invalidFilter` appends `searchFilterFields`).
TRACE:    `{"query":"q","kinds":"conversation"}` → the first-pass key map finds `kinds ∉ searchParams` → `memory_search: unknown parameter "kinds"; valid parameters: query, k, kind, subject, predicate, object, extractor_version, since, until, include_superseded, sources` returned as a tool error; the fake backend's `searchCalls` is asserted `== 0`. On the gRPC wire an unknown filter field is **unrepresentable** (flat proto3 fields, `reserved 5`), so the only remaining shape is the malformed range, rejected by `compileSearchFilter:42-45` with `codes.InvalidArgument` before `retrieval.Search` is called.
VERDICT:  **PASS** — `TestDW_5_4_InvalidFiltersRejectedAtEntry` (8 sub-cases, each asserting `backend called 0 times`), `TestDW_5_4_MalformedTimeRangeRejectedAtEntry`, `TestDW_5_4_EmptySourceNameRejectedAtEntry`, `TestSearchArgsRejectNonObject` — ran and passed.

### DW-5.5
PREMISE:  "`make proto` is run and the regenerated `api/engrampb/*.pb.go` are committed; `make proto-check` passes."
EVIDENCE: `api/proto/engram.proto:137-188`, `api/engrampb/engram.pb.go:342-358, 470-472`.
TRACE:    Observed behavior — ran `make proto-check`: `./scripts/codegen.sh` → `codegen: ok`, then `git diff --exit-code -- api/engrampb` → **exit 0**, i.e. regeneration reproduces the checked-in `engram.pb.go` byte-for-byte. Confirmed the generated file carries the new fields (`ExtractorVersion` tag 10, `IncludeSuperseded` tag 13, `Sources` tag 14) and that `GetValidOnly` is gone. `api/engrampb/engram.pb.go` is staged in the index (`git status` → `M `), so it is in the tree the orchestrator commits.
VERDICT:  **PASS**

### DW-5.6
PREMISE:  "A call passing no filters behaves identically to today (end-to-end)."
EVIDENCE: `internal/mcp/searchargs.go:79-108` (zero `SearchFilter`, `k<=0` → `defaultRequestK`), `internal/engramclient/client.go:230-249` (unset fields → proto zero values; `Since/Until` omitted when `IsZero`), `internal/server/server.go:139-155` (`preds == nil`, `Sources == nil`, `ValidOnly == true`), `internal/retrieval/filters.go:350-352` (`sel == nil && !filtered` → every source, no work), `internal/cli/cli.go:270-272` (CLI passes `mcp.SearchFilter{}`).
TRACE:    `{"query":"anything"}` → zero `SearchFilter`, nil `Sources` (not an empty slice) → `SearchRequest` with only `Query`/`K` set → `retrieval.Filter{ValidOnly:true, Predicates:nil, Sources:nil}` → `selectSources` early-returns all sources untouched and `filterClauses` appends no predicate clause → the same query body as before the phase. The CLI, whose `ValidOnly:true` used to be hardcoded in `engramclient`, now gets the same `true` from the `include_superseded:false` default.
VERDICT:  **PASS** — `TestDW_5_6_NoFiltersSendsZeroFilter` (mcp), `TestDW_5_6_ZeroFilterSendsNoFilterFields` (engramclient), `TestDW_5_6_NoFiltersProducesTodaysFilter` (server), plus the carried-forward `TestDW_4_3_GoldenQueryBodyUnchangedWithoutFilters` (retrieval) — ran and passed.

### DW-5.7
PREMISE:  "An adversarial filter value (injection-shaped string) is safely parameterized into the query body, not interpolated."
EVIDENCE: `internal/server/searchfilter.go:61` (value carried as `Predicate.Value`, a Go `any`), `internal/retrieval/filters.go:178-220` (`validatePredicateValue` — scalars only, so a value can never become nested structure), `internal/retrieval/knowledge.go:308-330` (`filterClause` builds `map[string]any{"term": {field: value}}`; no string concatenation anywhere on the path).
TRACE:    `kind = "x\"}}]}},\"query\":{\"match_all\":{}},\"z\":{\"script\":{…}"` → placed as a leaf into `map[string]any{"term":{"kind": evil}}` → `json.Marshal` escapes it → the emitted body parses as valid JSON, has **no** top-level `z` key, and the decoded `term.kind` equals the caller's bytes exactly. It reaches the cluster as an inert keyword that matches nothing.
VERDICT:  **PASS** — `TestDW_5_7_AdversarialValueParameterizedIntoQueryBody` (retrieval; asserts on the *decoded* body, not on text), `TestDW_5_7_AdversarialFilterValueStaysData` (server) — ran and passed.

**All requirements met:** YES

## Test-DW Coverage
- [x] All 7 DW items have corresponding automated tests that ran in Step 0 (test names carry the DW-ID: `TestDW_5_1_*` … `TestDW_5_7_*`). DW-5.5 is covered by recorded observed behavior (`make proto-check` exit 0) — no automated Go test can exercise a codegen-freshness gate.
- [x] Coverage level: every DW item and every listed edge case has at least one passing test; the barricade tests additionally assert `backend called 0 times`, which is the load-bearing negative.
- Statement coverage of the new code is high but not literally 100% (`parseSearchArgs` 95.8%, `compileSearchFilter`/`timeBounds`/`invalidFilter`/`validateSources`/`parseBound` all 100%). The single uncovered block is `searchargs.go:91-93`, the `until` parse-error return — the exact symmetric twin of the `since` branch at :88-90, which *is* covered by `TestDW_5_4_InvalidFiltersRejectedAtEntry/malformed_time`. No DW item or listed edge case is uncovered. Recorded as a Note, not a blocker.

### Edge cases (explicit plan requirements)
| Edge case | Status | Evidence |
|---|---|---|
| Unknown filter field (error naming valid fields) | PASS | `TestDW_5_4_InvalidFiltersRejectedAtEntry/unknown_filter_field` + `/…looks_like_an_internal_one` (`valid_only` → told about `include_superseded`) |
| Malformed time (`since` > `until`) | PASS | `…/since_after_until` (mcp), `TestDW_5_4_MalformedTimeRangeRejectedAtEntry` (server) |
| `include_superseded` with no other filter | PASS | `TestIncludeSupersededAloneIsAValidRequest`; it sets no predicate, so `filtered=false` and all sources still run |
| `sources` naming an unknown tier | PASS | `…/unknown_source` (mcp allowlist), `resolveSources` (retrieval authority), `TestInvalidFilterFromRetrieverIsInvalidArgument` |
| Every filter absent ⇒ identical behavior | PASS | DW-5.6 tests across all four layers |
| `k` bounds still enforced | PASS | `TestSearchKBoundsStillEnforced` (`k<=0` → `defaultRequestK`; explicit `k` passes through) + `clampK` to `[1, MaxK=100]` at `opensearch.go:55-63`, unchanged by this phase |

## Dead Code
None found. No unused imports (`go vet` + revive clean), no unreachable code after early returns, no debug statements, no commented-out blocks in the reviewed files.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | The phase adds no shared mutable state. `compileSearchFilter` and `parseSearchArgs` are pure per-request functions over request-local values; the only map is a function-local literal, and its non-deterministic iteration order is neutralized by `sortPredicates` (`searchfilter.go:72,94-96`) so the emitted body is stable. `retrieval.Filter` is passed by value into the existing per-tier goroutine fan-out; `Predicates`/`Sources` are read-only after validation. Probed for a data race by re-running `internal/retrieval` and `internal/server` — no race reports, and the fan-out writes into disjoint `results[i]` slots as before. |
| Error Handling | PASS | Every parse/validate path returns an error; no empty catch equivalents (`_ =` swallows). `parseBound` distinguishes "absent" (zero time, legal open bound) from "malformed" (error). `server.go:166-171` correctly discriminates `retrieval.ErrInvalidFilter` (→ `InvalidArgument`) from everything else (→ `Internal`), proven both ways by `TestInvalidFilterFromRetrieverIsInvalidArgument` and `TestRetrieverInfrastructureErrorStaysInternal`. |
| Resources | N/A | The phase opens no files, connections, locks, or goroutines; it only shapes a request struct on an existing call path. |
| Boundaries | PASS | Probed the adversarial ends: `sources: []` (non-nil empty) is an explicit error rather than a silent "all" — the classic empty-vs-nil collapse — at both `searchargs.go:133` and `filters.go:271`; `since == until` is legal (only `After` errors); either bound alone yields a one-sided range (`TestDW_5_1_TimeBoundsAreOneRangePredicate`, both sub-cases); `k = -5` and `k = 0` both fall to `defaultRequestK`; `k = 10000` passes through and is clamped by `clampK` to `MaxK`. Predicate count is bounded by `MaxPredicates`, and the flat schema can emit at most 6 predicates anyway. |
| Security | PASS | See the dedicated section below — I attacked each stated vector and could not break one. |

### Security review (this phase is the untrusted-input barricade)
| Vector probed | Result | Trace |
|---|---|---|
| `include_superseded` widening authorization | **Cannot.** | `ValidOnly` is consumed at exactly one place, `opensearch.go:579`, and gates exactly one clause (the `expired_at`/`invalid_at` bi-temporal bool). The ACL clause is compiled independently from `f.Identity` via `m.acl.Enforce` (`opensearch.go:285-293`, fail-closed) and the tenant term from `f.TenantID` (`opensearch.go:572`); neither reads `ValidOnly`. Setting `ValidOnly=false` removes a clause from the `filter` array and adds nothing — it can only surface *additional versions of documents the ACL already admits*. `TestDW_5_2_IncludeSupersededCannotBypassACL` asserts the tenant term and the ACL scope clause are still in the emitted body for both indices under `ValidOnly:false`. |
| `sources` admitting an ACL-denied hit | **Cannot.** | `selectSources` only ever *removes* entries from `m.tiers`/`m.tierSrcs`/`m.postHooks`; it cannot synthesize a source. Every selected tier still gets `aclClause` in its filter array, every `TierSource` still receives `f.Identity` (`opensearch.go:314`), and every `PostHook` still receives it (`opensearch.go:354`). `resolveSources` rejects any name not in the registered vocabulary, so an unregistered index cannot be reached by name. Narrowing is monotonically subtractive. |
| Value interpolation into a query string | **None.** | Grepped the whole predicate path: no `fmt.Sprintf`, `+`, or template rendering ever touches a caller value on the way into the body. Values land as leaves in `map[string]any` and are escaped by `json.Marshal`. `validatePredicateValue` additionally rejects map/slice values, closing the only route by which a value could become *structure*. Demonstrated inert by `TestDW_5_7_AdversarialValueParameterizedIntoQueryBody`. |
| Validation before inner modules | **Confirmed.** | MCP: `parseSearchArgs` is the first statement of `callSearch`; the fake backend's `searchCalls` is asserted `0` on all 8 rejection cases — a rejected request costs zero backend round-trips. gRPC: `compileSearchFilter` is the first statement of `Server.Search`, before `retrieval.Query`/`Filter` are even constructed. Retrieval: `resolveSources`/`validatePredicates`/`validateFilterableSources` run before the embedder and before any HTTP request. Three concentric barricades, defense-in-depth as the skill prescribes for a security-critical path. |
| InvalidArgument vs Internal; no state leak | **Confirmed.** | `retrieval.ErrInvalidFilter` is the caller-fault sentinel; `server.go:169` maps it to `codes.InvalidArgument`, everything else to `Internal`. Both directions are tested. Error text names only the *caller's* vocabulary — `searchFilterFields` (`searchfilter.go:20`) deliberately lists the flat wire names, never the physical OpenSearch fields (`occurred_at`, `valid_at`, `owner_agent_id`); no index name, cluster address, stack, or internal predicate shape appears in any caller-facing message. Format strings are compile-time constants with caller data only in `%q`/`%v` args, so no format-string injection either. |

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| aposd-designing-deep-modules | Deep interface / information hiding | PASS | The generic `Predicate{Field,Op,Value}` form is hidden entirely inside retrieval; the caller sees 9 flat named fields and the server is the single translation point (`searchfilter.go:40`). The comment at `searchfilter.go:31-35` is explicit that field→tier ownership is *not* re-derived here — it lives once, in the registry. |
| aposd-designing-deep-modules | Information leakage (same knowledge in two modules) | PASS | Probed the one real risk: does `compileSearchFilter` duplicate the registry's tier knowledge? It does not — it emits `Field: "kind"` etc. and lets `FilterableFields` route. The `TimeField` alias + `FieldSpec.Target` indirection exist precisely so the server need not know that "time" means `occurred_at` here and `valid_at` there. |
| aposd-designing-deep-modules | False abstraction / silent failure | PASS | The phase's hardest call — a filtered search silently dropping unfilterable sources — is surfaced, not hidden: an *explicitly named* unfilterable source alongside a filter is an error (`validateFilterableSources`), and the implicit exclusion is documented in the tool description the LLM actually reads (`tools.go:86`). Failure is observable; only the mechanism is hidden. |
| aposd-designing-deep-modules | Pull complexity downward (caller ease of use) | PASS | The common case (`{"query":"x"}`) requires zero knowledge of tiers, predicates, or bi-temporality; `k<=0` means "server-chosen" rather than an error. Duplicate MCP-side source validation is a deliberate, documented usability trade (`searchargs.go:22-29`) that saves the caller a round-trip, not an ownership leak — retrieval remains the authority and would reject anything this list wrongly admits. |
| cc-defensive-programming | External input validated at entry (barricade) | PASS | Agent-supplied JSON is fully validated at `parseSearchArgs` before the backend seam; the proto request is validated at `compileSearchFilter` before the retriever. Both are true barricades: everything inside may assume a well-formed query, a bounded `k`, parsed time bounds, and a known source vocabulary. |
| cc-defensive-programming | Defense-in-depth on security-critical paths | PASS | The skill's explicit carve-out ("inside the barricade you may assume validated data — except for security-critical paths") is honored: retrieval re-validates every predicate and source even though MCP already did, and `filterClauses` fails closed on an unvalidated predicate reaching a tier (`TestFilterClausesFailsClosedOnUnvalidatedPredicate`). |
| cc-defensive-programming | No empty catch / no swallowed errors | PASS | Every error return on the new paths is propagated. No `_ =` discards, no bare `recover`, no default-to-empty on a parse failure. |
| cc-defensive-programming | Assertions for bugs only; anticipated bad input gets error handling | PASS | No assertions/panics anywhere on the request path — all caller-facing failures are returned errors (Go's idiom, consistent with the surrounding codebase). The one "should never happen" case (an unvalidated predicate reaching `filterClauses`) fails closed with an error rather than a panic, which is the stricter choice. |
| cc-defensive-programming | Correctness-vs-robustness posture appropriate to domain | PASS | Correctness-leaning throughout, correctly for a memory/ACL system: empty `sources` errors rather than defaulting to "all"; an unfilterable source named with a filter errors rather than half-honoring the request; ACL failure returns zero results (fail-closed). Nothing degrades silently toward a wider result set. |

## Notes (non-blocking)
1. **Stale docs.** `docs/architecture.md:193` and `docs/architecture-report.html:486,545` still describe the `valid_only` field, which this phase reserved. `docs/api.md` and `docs/mcp.md` were updated; these two were not. No DW item asked for them, so this is a Note.
2. **Coverage gap (cosmetic).** `internal/mcp/searchargs.go:91-93` — the `until` parse-error return — is the one uncovered block in the new code. Its `since` twin is covered; a second table row (`"until": "next week"`) in `TestDW_5_4_InvalidFiltersRejectedAtEntry` would close it.
3. **`int32(k)` narrowing.** `engramclient/client.go:236` converts `k int` → `int32`. A `k` above 2³¹ wraps negative, which `clampK` then normalizes to `DefaultK` — harmless, and the conversion predates this phase. Mentioned only because the phase touched the line.
4. **MCP `memorySources` must be hand-synced** with `cmd/engram-server`'s `RegisterTier`/`RegisterPostHook` calls (the code says so at `searchargs.go:28`). A deployment that registers a different source set would have the MCP list reject a name the server would accept. The retrieval registry remains the authority, so the failure mode is a spurious rejection, never an over-admission — but it is a real drift surface with no test pinning it.
5. **Duplicate `parseSearchArgs` decode.** The two-pass `json.Unmarshal` (key allowlist, then value shape) costs one extra decode per search. Deliberate and documented — it buys the "unknown parameter `kinds`" message that lets an LLM self-correct — and the cost is trivial next to the retrieval round-trip. Noted, not objected to.

## Issues (if FAIL)
None.

**Verdict: PASS**

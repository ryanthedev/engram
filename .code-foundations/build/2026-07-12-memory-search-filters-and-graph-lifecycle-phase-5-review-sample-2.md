# Review: Phase 5 - LLM-facing API (proto + flat MCP schema) — sample 2

## Executed Results (Step 0)
- Test suite: `make test` (`go test ./...`) → all packages ok, 0 failures. Re-run fresh: `go test -count=1 ./internal/mcp/... ./internal/server/... ./internal/engramclient/... ./internal/retrieval/...` → 4/4 ok.
- Typecheck/build: `make build` (`go build ./...`) → clean.
- Lint: `make lint` (`go vet ./...` + revive) → clean, no output.
- Proto: `make proto-check` (`./scripts/codegen.sh` + `git diff --exit-code -- api/engrampb`) → `codegen: ok`, no diff.

## Requirement Fulfillment

### DW-5.1
PREMISE:  "`memory_search` accepts every flat filter param; each maps to the correct internal predicate and tier."
EVIDENCE: internal/mcp/searchargs.go:16-47 (vocabulary + decode), internal/mcp/tools.go:61-88 (schema), internal/engramclient/client.go:232-249 (flat wire), internal/server/searchfilter.go:52-73 (predicate compile), internal/retrieval/filters.go:88-122 (tier registry + `TimeField` alias).
TRACE:    `{"query":"orders-svc leak","k":7,"kind":…,"subject":…,"predicate":…,"object":…,"extractor_version":…,"since":…,"until":…,"include_superseded":true,"sources":[…]}` → `parseSearchArgs` → `mcp.SearchFilter` → flat `engrampb.SearchRequest` → `compileSearchFilter` → 5 `term` predicates + 1 `range` on `retrieval.TimeField` → registry routes `kind`→episodic, triple+`extractor_version`→semantic, `time`→`occurred_at` (episodic) / `valid_at` (semantic) → emitted query bodies contain `"range":{"occurred_at":…}` and `"range":{"valid_at":…}`, and the alias name `"time"` appears in neither.
Tests run: `TestDW_5_1_SearchAcceptsEveryFlatFilterParam`, `TestSearchSchemaAdvertisesEveryFilterParam` (mcp); `TestDW_5_1_SearchFilterTravelsFlatOnTheWire` (engramclient); `TestDW_5_1_FlatParamsCompileToPredicates`, `TestDW_5_1_TimeBoundsAreOneRangePredicate` (server); `TestDW_5_1_TimeAliasRoutesToPerTierField`, `TestDW_5_1_OpenTimeBoundsCompile`, `TestDW_5_1_SemanticParamsRouteToSemanticOnly` (retrieval) — all PASS.
VERDICT:  PASS

### DW-5.2
PREMISE:  "`include_superseded: true` returns historical facts; absent/false preserves today's `ValidOnly` behavior."
EVIDENCE: internal/server/server.go:152 (`ValidOnly: !req.GetIncludeSuperseded()`), internal/retrieval/opensearch.go:579-590 (validity clause gated on `f.ValidOnly`), api/proto/engram.proto:161-162 + 172-176 (field 5 reserved; `include_superseded` = 13).
TRACE:    absent → `IncludeSuperseded=false` → `ValidOnly=true` → semantic body carries `must_not:{exists:expired_at}` + the `invalid_at` should-clause — byte-identical to the clause the old hardcoded `ValidOnly:true` produced. `true` → `ValidOnly=false` → that clause is omitted → superseded/retracted versions are no longer excluded.
Tests run: `TestDW_5_2_IncludeSupersededDrivesValidOnly` (server), `TestDW_5_2_ValidOnlyGatesTheHistoryClause` (retrieval, asserts the emitted query body both ways), `TestDW_5_6_ZeroFilterSendsNoFilterFields` (engramclient) — all PASS.
VERDICT:  PASS

### DW-5.3
PREMISE:  "`sources: [\"semantic\"]` excludes episodic and graph hits end-to-end from the MCP call."
EVIDENCE: internal/mcp/searchargs.go:86 + 129-142, internal/engramclient/client.go:241, internal/server/server.go:153, internal/retrieval/filters.go:349-386 (`selectSources`).
TRACE:    `sources:["semantic"]` → MCP `SearchFilter.Sources` → `SearchRequest.Sources` → `retrieval.Filter.Sources` → `resolveSources` → `sel={semantic}` → `selectSources` returns `tiers=[semantic]`, `tierSrcs=[]`, `postHooks=[]` → only `sem-idx` is queried, the experience tier source never runs, the graph post-hook never runs → no episodic/experience/graph hit can be in the result.
Tests run: `TestDW_5_3_SourcesReachTheBackend` (mcp), `TestDW_5_3_SourcesReachTheFilter` (server), `TestDW_5_3_SemanticOnlySearchHasNoEpisodicOrGraphHits` (retrieval — asserts searched indices == `[sem-idx]`, tier-source calls == 0, hook calls == 0, and that no non-semantic hit survives) — all PASS.
VERDICT:  PASS

### DW-5.4
PREMISE:  "An invalid filter field or malformed time range is rejected at the MCP/gRPC entry with an error naming the valid fields — never reaching the retriever."
EVIDENCE: internal/mcp/searchargs.go:62-100 (closed allowlist + RFC 3339 parse + since>until), internal/mcp/tools.go:286-295 (`parseSearchArgs` before `s.backend.Search`), internal/server/searchfilter.go:41-50 + 98-105 (`invalidFilter` → `codes.InvalidArgument`, message appends `searchFilterFields`), internal/server/server.go:139-143 (compile before `retrieval.Query` is built).
TRACE:    `{"query":"q","kinds":"conversation"}` → allowlist miss → tool error `memory_search: unknown parameter "kinds"; valid parameters: query, k, kind, subject, predicate, object, extractor_version, since, until, include_superseded, sources`; backend call count = 0. gRPC `Since=2026-06-01, Until=2026-01-01` → `compileSearchFilter` returns InvalidArgument `since (…) is after until (…); the time range is empty; valid filter fields: kind, subject, …, sources`; `capturingRetriever.calls == 0`.
Tests run: `TestDW_5_4_InvalidFiltersRejectedAtEntry` (8 sub-cases, each asserts `searchCalls == 0`), `TestSearchArgsRejectNonObject` (mcp); `TestDW_5_4_MalformedTimeRangeRejectedAtEntry`, `TestDW_5_4_EmptySourceNameRejectedAtEntry` (both assert `r.calls == 0`), `TestInvalidFilterFromRetrieverIsInvalidArgument`, `TestRetrieverInfrastructureErrorStaysInternal` (server) — all PASS.
VERDICT:  PASS

### DW-5.5
PREMISE:  "`make proto` is run and the regenerated `api/engrampb/*.pb.go` are committed; `make proto-check` passes."
EVIDENCE: api/proto/engram.proto:137-188 (fields 6-14, `reserved 5` / `reserved "valid_only"`), api/engrampb/engram.pb.go:342-352 + 449-481 (generated `ExtractorVersion`, `IncludeSuperseded` tag 13, `GetSince/GetUntil/GetSources`).
TRACE:    `make proto-check` → regenerates into the tree → `git diff --exit-code -- api/engrampb` → exit 0. The checked-in `.pb.go` is byte-identical to what the current `.proto` generates; the new getters are present and the old `ValidOnly` accessor is gone (`grep ValidOnly` over non-test source returns only comments and `retrieval.Filter`).
VERDICT:  PASS (the regenerated file is in the tree and staged; the git commit itself is the orchestrator's post-gate step, so "committed" is verified as "regenerated, in-tree, and in sync")

### DW-5.6
PREMISE:  "A call passing no filters behaves identically to today (end-to-end)."
EVIDENCE: internal/mcp/searchargs.go:102-108, internal/engramclient/client.go:232-249, internal/server/server.go:146-155, internal/retrieval/filters.go:358-360 (`sel == nil && !filtered` → every source, no work).
TRACE:    `{"query":"anything"}` → zero `SearchFilter`, `k=defaultRequestK(50)` → `SearchRequest{Query,K}` only (no term fields, `Since/Until` nil, `IncludeSuperseded` false, `Sources` nil) → `retrieval.Filter{TenantID, UserID, ValidOnly:true}` with **nil** Predicates and **nil** Sources → `selectSources(nil, false)` short-circuits to all tiers + all tier sources + all post-hooks. Identical to the pre-Phase-5 path, where the client hardcoded `ValidOnly:true`.
Tests run: `TestDW_5_6_NoFiltersSendsZeroFilter` (mcp), `TestDW_5_6_ZeroFilterSendsNoFilterFields` (engramclient), `TestDW_5_6_NoFiltersProducesTodaysFilter` (server — `reflect.DeepEqual` against `retrieval.Filter{TenantID:"t1", UserID:"agent-9", ValidOnly:true}` plus explicit nil-not-empty assertions), `TestFilteredSearchExcludesUnfilterableSources/unfiltered:_both_run` (retrieval) — all PASS.
VERDICT:  PASS

### DW-5.7
PREMISE:  "An adversarial filter value (injection-shaped string) is safely parameterized into the query body, not interpolated."
EVIDENCE: internal/server/searchfilter.go:61 (value carried as `any` into `retrieval.Predicate`), internal/retrieval/knowledge.go:308-332 (`filterClause` builds `map[string]any` clause structures), internal/retrieval/filters.go:178-212 (`validatePredicateValue` rejects non-scalars), internal/retrieval/opensearch.go:504-510 (`buildQuery` → `json.Marshal` → `bytes.NewReader`).
TRACE:    `kind = x"}}]}},"query":{"match_all":{}},"z":{"script":{"source":"ctx._source.remove('acl')"` → `Predicate{Field:"kind", Op:"term", Value:<evil>}` → `map[string]any{"term":{"kind":<evil>}}` → `json.Marshal` escapes the quotes → emitted body decodes as valid JSON, has no top-level `z` key, and the `term.kind` leaf is byte-for-byte the caller's string. No `fmt.Sprintf`/string concatenation exists anywhere on the value path.
Tests run: `TestDW_5_7_AdversarialFilterValueStaysData` (server), `TestDW_5_7_AdversarialValueParameterizedIntoQueryBody` (retrieval — decodes the emitted body and asserts leaf identity + no hijacked key) — both PASS.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have corresponding automated tests that ran in Step 0; test names carry DW IDs (`TestDW_5_1_…` … `TestDW_5_7_…`) across four packages.
- [x] Coverage matches the stated 100% level: each DW is asserted at every layer it crosses (mcp barricade → engramclient wire → server compile → retrieval query body), not just at one.
- DW-5.5 is the one item with no automated test *name*; its execution evidence is the `make proto-check` run in Step 0 (regeneration + `git diff --exit-code`), which is the requirement's own stated check. Recorded as observed behavior.

Every prompt-listed edge case has a named passing test:

| Edge case | Test | Result |
|---|---|---|
| Unknown filter field (error naming valid fields) | `TestDW_5_4_InvalidFiltersRejectedAtEntry/unknown_filter_field` + `/unknown_filter_field_that_looks_like_an_internal_one` (`valid_only` → told about `include_superseded`) | PASS |
| Malformed time (`since` > `until`) | `…/since_after_until` (mcp), `TestDW_5_4_MalformedTimeRangeRejectedAtEntry` (gRPC) | PASS |
| `include_superseded` with no other filter | `TestIncludeSupersededAloneIsAValidRequest`; traced: 0 predicates → `filtered=false` → no source is excluded | PASS |
| `sources` naming an unknown tier | `…/unknown_source`, `…/empty_sources` (mcp); `TestErrInvalidFilterWrapsCallerErrors/unknown_source` (retrieval → InvalidArgument via `TestInvalidFilterFromRetrieverIsInvalidArgument`) | PASS |
| Every filter absent ⇒ identical behavior | DW-5.6 trio above | PASS |
| `k` bounds still enforced | `TestSearchKBoundsStillEnforced`; retriever `clampK` (opensearch.go:58-67) still caps to `[1, MaxK=100]`, and `k<=0` still means server-chosen `defaultRequestK` | PASS |

## Dead Code
None found. No unused imports (`go vet` + revive clean), no unreachable code after early returns, no debug statements, no commented-out blocks. `strings` was newly imported in tools.go and is used (schema description).

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | The filter is compiled and validated *before* the goroutine fan-out (opensearch.go:266-280); `f` is read-only inside each tier goroutine, `aclClause` is built once and shared read-only, and results are written to pre-sized disjoint slots (`results[i]`). No new shared mutable state. Probed: nothing in the Phase-5 path writes to `Filter`, `Predicate.Value`, or the registry after `Search` begins. |
| Error Handling | PASS | Every caller-input failure is an explicit typed error; no empty catch, no silently-dropped error. `ErrInvalidFilter` cleanly separates caller fault (InvalidArgument) from infrastructure fault (Internal) — `TestRetrieverInfrastructureErrorStaysInternal` proves the sentinel check does not swallow real failures into "your fault". Probed adversarially: `{"kind":7}`, `{"sources":"semantic"}`, `{"k":"many"}`, `null`, and a bare JSON array all return errors, never a panic or a partial filter. |
| Resources | N/A | No new file handles, connections, locks, caches, or goroutines are introduced by this phase; the HTTP path and its `defer resp.Body.Close()` are untouched. |
| Boundaries | PASS | Probed the numeric boundary: `{"k": 99999999999999}` → `int32` truncation in the client → `clampK` still bounds the result into `[1, MaxK]` (a value truncating to 0 or negative falls to `DefaultK`), so no unbounded `k` can reach OpenSearch and no `k`-driven allocation exists in the MCP packer. Zero-time boundary: `since:"0001-01-01T00:00:00Z"` parses to the zero `time.Time` and is treated as "unset" — semantically identical (a year-1 lower bound constrains nothing), so no wrong result is produced. Empty vs nil `sources` is distinguished at both barricades. |
| Security | PASS | See below — no defect found on any of the five named checks. |

### Security checks (this phase is the untrusted-input barricade)

| Check | Status | Trace |
|---|---|---|
| `include_superseded` widens validity only, never authorization | PASS | `filterClauses` (opensearch.go:564-601) appends the ACL clause and the tenant term **unconditionally** and *before* the `if f.ValidOnly` branch; `ValidOnly` gates exactly one clause (the `expired_at`/`invalid_at` bool). Setting `IncludeSuperseded=true` therefore removes that clause and nothing else. `filterAuthorized` still runs post-fusion regardless. Proven by `TestDW_5_2_IncludeSupersededCannotBypassACL`, which wires the **production** `acl.Filter` and asserts the tenant term and the scope/`owner_agent_id` clause are still in both query bodies with `ValidOnly:false`. |
| `sources` cannot admit an ACL-denied hit | PASS | `selectSources` only ever *removes* entries from `m.tiers`/`m.tierSrcs`/`m.postHooks`; it never constructs a source. Whatever survives still runs under `aclClause`, and `filterAuthorized(merged, enf)` runs after fusion and again after post-hooks. Narrowing is monotonically subtractive. |
| No filter value interpolated into a query string | PASS | DW-5.7 trace above. Values travel as `any` into `map[string]any` clause structures and are serialized by `json.Marshal`. Non-scalar values are rejected before that (`validatePredicateValue`), so a caller cannot smuggle *structure* into a value position either (`TestErrInvalidFilterWrapsCallerErrors/non-scalar_value`). |
| Validation before inner modules are touched | PASS | MCP: `parseSearchArgs` runs before `s.backend.Search` (tools.go:293-296); every DW-5.4 sub-case asserts `searchCalls == 0`. gRPC: `compileSearchFilter` runs before `retrieval.Query` is even constructed (server.go:139-145); both server barricade tests assert `r.calls == 0`. Retrieval: `resolveSources`/`validatePredicates`/`validateFilterableSources` run before the empty-text short-circuit and before any HTTP round-trip (`TestFilteredSearchNamingAnUnfilterableSourceErrors` asserts 0 cluster round-trips). Three layers, defense in depth. |
| Caller error → InvalidArgument, not Internal | PASS | `invalidFilter` (searchfilter.go:102-105) emits `codes.InvalidArgument`; `retrieval.ErrInvalidFilter` is mapped to `InvalidArgument` at server.go:166-171 while everything else stays `Internal`. Both directions are pinned by tests. Messages name the caller-facing vocabulary (`kind, subject, …, sources`) and never the physical index fields. |

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| aposd-designing-deep-modules | Deep interface / information hiding | PASS | The generic `{field, op, value}` predicate form is fully hidden from the caller: the wire contract is flat and named, so "a filter on a field no tier owns" is *unrepresentable*, not merely rejected. `compileSearchFilter` is the single translation point (searchfilter.go:40) and deliberately does **not** re-derive tier ownership — that stays in the retrieval registry. Physical field names (`occurred_at`, `valid_at`, `invalid_at`) never appear in any caller-facing message or schema. |
| aposd-designing-deep-modules | Common case is simple | PASS | `{"query": "…"}` is still the whole call; every filter is optional with a no-constraint zero value, verified by DW-5.6 producing the byte-identical pre-filter `retrieval.Filter`. |
| aposd-designing-deep-modules | No information leakage / knowledge in one place | PASS (with one Note) | `TimeField` collapses what would otherwise be two physical time fields into one caller-facing bound; `FieldSpec.Target` keeps the alias→physical mapping inside the registry. The one duplication is `mcp.memorySources` mirroring the retrieval registry's source names — see Notes; it is a deliberate, documented fail-fast echo (the registry remains authoritative and re-validates), not a second source of truth. |
| aposd-designing-deep-modules | No silent failure | PASS | The two places this design could have gone quiet, it does not: an empty `sources` list is an error rather than a silent "all" (searchargs.go:129-142, filters.go:279-281), and an explicitly-named unfilterable source alongside a filter errors rather than being silently honored or silently dropped (`validateFilterableSources`, filters.go:305-317). |
| cc-defensive-programming | External input validated at entry (barricade) | PASS | Three barricades, each proven to run before the next layer is touched — see the Security table row above. |
| cc-defensive-programming | "Internal team API" is still external | PASS | The gRPC entry validates independently of the MCP entry (`compileSearchFilter` re-checks the time range and source names even though `parseSearchArgs` already did), rather than trusting its own client. |
| cc-defensive-programming | Security-critical paths validate again inside the barricade | PASS | The retrieval layer re-validates predicates, ops, value scalarity, and source names, and re-authorizes hits post-fusion and post-hook — it does not assume the entry barricades did their job. |
| cc-defensive-programming | No empty catch / no swallowed errors | PASS | Every `err` on the Phase-5 path is returned or wrapped; `go vet` and revive are clean. |
| cc-defensive-programming | Assertions contain no executable code / bugs-only | N/A | No assertions are used; Go error returns are the codebase's uniform, correctly-followed convention. |
| cc-defensive-programming | Error message does not leak security info | PASS (with one Note) | Caller-facing messages name only the filter vocabulary. One message passes `encoding/json`'s text through — see Notes; it exposes a Go type name, not server state, credentials, data, or authorization detail. |

## Notes (non-blocking)

1. **`encoding/json` error text reaches the caller verbatim.** `internal/mcp/searchargs.go:73` wraps the decode error as-is, so `{"query":"q","kind":7}` returns:
   `memory_search: invalid argument value: json: cannot unmarshal number into Go struct field searchArgs.kind of type string`
   That leaks the internal Go struct name (`searchArgs`) and Go type names, and — unlike every other rejection in this file — it does not name the valid vocabulary. It is caller-actionable and leaks no server state, credentials, or authorization detail, so it is not a security defect and no DW item or edge case covers a wrong-*typed* value. Still, it is the one message in the barricade that breaks the file's own stated contract ("the returned error is caller-facing text: it names the valid vocabulary"). Cheap fix: name the offending key and its expected JSON type instead of interpolating the decoder's message.

2. **`mcp.memorySources` duplicates the retrieval source vocabulary** (searchargs.go:30). The comment discloses this and correctly names retrieval as the authority, and the pattern matches the existing `readSources`. The residual risk is real but bounded: registering a new source in `cmd/engram-server` without adding it here makes it unaddressable from `memory_search` (a false rejection, never a false admission). A registry-driven `tools/list` or a cross-package test asserting the two lists agree would close it.

3. **`engram-perf` and `engram-loadtest` change behavior slightly.** Both send `SearchRequest` without any validity field (cmd/engram-perf/main.go:97,126; cmd/engram-loadtest/phase.go:123). Before, that meant `ValidOnly=false` (all facts); now the server derives `ValidOnly=true`. Benchmark queries therefore become current-state-only. Harmless — arguably a correction, since it aligns them with the real MCP path — but it is a behavior change outside the reviewed files that no DW item names.

4. **Wire compatibility of `reserved 5` is sound.** An old client that sent `valid_only=true` now has that field ignored and gets `ValidOnly=true` derived from the `include_superseded` default — identical behavior. One that sent `valid_only=false` now gets the *narrower* current-state filter. Narrowing, never widening: no security regression.

## Issues (if FAIL)
None.

**Verdict: PASS**

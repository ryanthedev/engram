# Review: Phase 5 - LLM-facing API (proto + flat MCP schema) — sample 3

## Executed Results (Step 0)
- Test suite: `make test` (`go test ./...`) → all packages **ok**, 0 failures. Targeted re-run with `-count=1` of `./internal/mcp ./internal/server ./internal/retrieval ./internal/engramclient` → **ok** (61 `--- PASS` subtests matching `DW_5|Search|Filter|IncludeSuperseded|ErrInvalid`, 0 FAIL).
- Typecheck/build: `make build` (`go build ./...`) → exit 0.
- Lint: `make lint` (`go vet ./...` + revive) → exit 0, no findings.
- Proto: `make proto-check` (`./scripts/codegen.sh` → `codegen: ok`; `git diff --exit-code -- api/engrampb`) → exit 0.
- `make integration` NOT run (per dispatch). Scratch artifacts under `/tmp/p5-sample-3/`.

## Requirement Fulfillment

### DW-5.1
PREMISE:  "`memory_search` accepts every flat filter param; each maps to the correct internal predicate and tier."
EVIDENCE: `internal/mcp/searchargs.go:16-20` (closed param allowlist), `:79-100` (decode → `SearchFilter`); `internal/mcp/tools.go:61-88` (schema); `internal/engramclient/client.go:230-250` (flat wire translation); `internal/server/searchfilter.go:52-73` (flat → `retrieval.Predicate`); `internal/retrieval/filters.go:99-121` (`TimeField` alias → `occurred_at` / `valid_at`), `:156` (`spec.field(p.Field)` compiles against the physical target).
TRACE:    `{query,k,kind,subject,predicate,object,extractor_version,since,until,include_superseded,sources}` → `parseSearchArgs` (all 11 accepted; `since/until` parsed to `time.Time`) → `Client.Search` sets the 9 named `SearchRequest` fields + 2 `Timestamp`s → `compileSearchFilter` emits 5 `term` predicates + ONE `range` on `retrieval.TimeField` → registry routes: `kind`→episodic body `{"range":{"occurred_at":…}}`/`{"term":{"kind":…}}`, the triple + `extractor_version`→semantic body only, `time`→episodic `occurred_at` AND semantic `valid_at`, and the alias name never appears in any emitted body.
BACKING:  `TestDW_5_1_SearchAcceptsEveryFlatFilterParam`, `TestSearchSchemaAdvertisesEveryFilterParam` (mcp); `TestDW_5_1_SearchFilterTravelsFlatOnTheWire` (engramclient); `TestDW_5_1_FlatParamsCompileToPredicates`, `TestDW_5_1_TimeBoundsAreOneRangePredicate` (server); `TestDW_5_1_TimeAliasRoutesToPerTierField`, `TestDW_5_1_OpenTimeBoundsCompile`, `TestDW_5_1_SemanticParamsRouteToSemanticOnly` (retrieval) — all PASS.
VERDICT:  **PASS**

### DW-5.2
PREMISE:  "`include_superseded: true` returns historical facts; absent/false preserves today's `ValidOnly` behavior."
EVIDENCE: `internal/server/server.go:152` (`ValidOnly: !req.GetIncludeSuperseded()` — verified the SOLE producer on the gRPC path by grep: no other non-test, non-retrieval producer); `internal/retrieval/opensearch.go:579-586` (validity clause gated on `f.ValidOnly && t.supportsValidity`); `api/proto/engram.proto` (`reserved 5; reserved "valid_only";`).
TRACE:    `include_superseded` absent → `ValidOnly=true` → semantic body carries `"must_not":[{"exists":{"field":"expired_at"}}]` + the `invalid_at` clause (byte-identical to the pre-phase hardcoded `ValidOnly:true`). `include_superseded=true` → `ValidOnly=false` → that clause is omitted → superseded/retracted versions surface.
BACKING:  `TestDW_5_2_IncludeSupersededDrivesValidOnly` (server), `TestDW_5_2_ValidOnlyGatesTheHistoryClause` (retrieval, asserts the clause on/off in the emitted body) — PASS.
VERDICT:  **PASS**

### DW-5.3
PREMISE:  "`sources: [\"semantic\"]` excludes episodic and graph hits end-to-end from the MCP call."
EVIDENCE: `internal/mcp/searchargs.go:86,129-142`; `internal/engramclient/client.go:239`; `internal/server/server.go:154`; `internal/retrieval/opensearch.go:279` → `selectSources` (`internal/retrieval/filters.go:344-...`), which is the single gate for built-in tiers, registered tier sources, AND post-hooks.
TRACE:    MCP `sources:["semantic"]` → `SearchFilter.Sources=["semantic"]` → `SearchRequest.Sources` → `retrieval.Filter.Sources` → `resolveSources` → `selectSources` returns only the semantic tier, zero tier-sources, zero post-hooks → exactly one cluster request (`sem-idx`); the experience tier and the graph post-hook are never invoked, so no hit of either source can exist in the result.
BACKING:  `TestDW_5_3_SourcesReachTheBackend` (mcp seam), `TestDW_5_1_SearchFilterTravelsFlatOnTheWire` (wire), `TestDW_5_3_SourcesReachTheFilter` (server), `TestDW_5_3_SemanticOnlySearchHasNoEpisodicOrGraphHits` (retrieval — asserts searched indices == `[sem-idx]`, `tier.calls==0`, `hook.calls==0`, and that a seeded experience hit does not survive) — all PASS. Each link of the MCP→cluster chain is pinned by an executed test; see Notes on the absence of one single spanning test.
VERDICT:  **PASS**

### DW-5.4
PREMISE:  "An invalid filter field or malformed time range is rejected at the MCP/gRPC entry with an error naming the valid fields — never reaching the retriever."
EVIDENCE: `internal/mcp/searchargs.go:62-70` (unknown-key allowlist, names the offending key + full vocabulary), `:88-97` (RFC 3339 parse + `since > until`), `:129-142` (source vocabulary); `internal/mcp/tools.go:286-294` (barricade runs before `s.backend.Search`); `internal/server/searchfilter.go:40-50` + `:102-105` (`invalidFilter` → `codes.InvalidArgument` + `searchFilterFields`); `internal/server/server.go:139-142` (compile before `retrieval.Filter` is built or the retriever called).
TRACE:    MCP `{"query":"q","kinds":"conversation"}` → first-pass key allowlist → error `memory_search: unknown parameter "kinds"; valid parameters: query, k, kind, subject, …, sources` → returned as a tool error; `backend.searchCalls == 0`. gRPC `Since=2026-06-01, Until=2026-01-01` → `compileSearchFilter` → `InvalidArgument: since (…) is after until (…); the time range is empty; valid filter fields: kind, subject, …, sources`; `retriever.calls == 0`. An unknown filter FIELD is structurally unrepresentable on the gRPC wire (flat proto), so it cannot reach the retriever at all.
BACKING:  `TestDW_5_4_InvalidFiltersRejectedAtEntry` (8 subcases: unknown field, `valid_only` lookalike, malformed time, since>until, unknown source, empty sources, empty query, wrong value type — each asserts `searchCalls == 0`); `TestDW_5_4_MalformedTimeRangeRejectedAtEntry`, `TestDW_5_4_EmptySourceNameRejectedAtEntry` (assert `calls == 0` + `InvalidArgument`); `TestInvalidFilterFromRetrieverIsInvalidArgument`, `TestRetrieverInfrastructureErrorStaysInternal` — all PASS.
VERDICT:  **PASS**

### DW-5.5
PREMISE:  "`make proto` is run and the regenerated `api/engrampb/*.pb.go` are committed; `make proto-check` passes."
EVIDENCE: `api/proto/engram.proto` (fields 6-14 added, `reserved 5`/`reserved "valid_only"`); `api/engrampb/engram.pb.go:342,352,449,470,477` (generated `ExtractorVersion`, `IncludeSuperseded`, `GetSources`, …).
TRACE:    Ran `make proto-check` → `./scripts/codegen.sh` regenerated into the worktree → `codegen: ok` → `git diff --exit-code -- api/engrampb` → **exit 0**, i.e. the on-disk generated code is byte-identical to a fresh regeneration from the current `.proto` (a stale checked-in `.pb.go` would have produced a diff and a non-zero exit). `git status` shows `api/engrampb/engram.pb.go` staged (`M `) alongside the proto; the commit itself is the orchestrator's post-gate step.
VERDICT:  **PASS**

### DW-5.6
PREMISE:  "A call passing no filters behaves identically to today (end-to-end)."
EVIDENCE: `internal/mcp/searchargs.go:79-108` (zero `SearchFilter`, nil `Sources`, `k<=0` → `defaultRequestK`); `internal/engramclient/client.go:230-248` (no `Since`/`Until` set when zero); `internal/server/server.go:139-155` (nil `Predicates`, `ValidOnly: !false == true`); `internal/retrieval/opensearch.go:279` (`selectSources(nil, false)` short-circuits to every source, "no filtering work at all"); `internal/cli/cli.go:270` (CLI passes `mcp.SearchFilter{}`).
TRACE:    `{"query":"anything"}` → zero `SearchFilter` + `k=defaultRequestK` → wire carries only query/k (no term fields, no timestamps, `include_superseded=false`, nil sources) → `retrieval.Filter{TenantID, UserID, ValidOnly:true}` with **nil** (not empty) `Predicates` and `Sources` → every source runs, unfilterable-source exclusion not triggered → identical to the pre-phase request.
BACKING:  `TestDW_5_6_NoFiltersSendsZeroFilter` (mcp), `TestDW_5_6_ZeroFilterSendsNoFilterFields` (engramclient), `TestDW_5_6_NoFiltersProducesTodaysFilter` (server — `reflect.DeepEqual` against `Filter{TenantID, UserID, ValidOnly:true}` plus explicit nil-vs-empty assertions), `TestFilteredSearchExcludesUnfilterableSources/unfiltered: both run` (retrieval) — all PASS.
VERDICT:  **PASS**

### DW-5.7
PREMISE:  "An adversarial filter value (injection-shaped string) is safely parameterized into the query body, not interpolated."
EVIDENCE: `internal/server/searchfilter.go:61` (value carried as `Predicate.Value any`, never concatenated); `internal/retrieval/filters.go:156` (`filterClause(spec.field(p.Field), p.Op, p.Value)` builds a `map[string]any` clause); the body is produced by `json.Marshal` of that structure. No `fmt.Sprintf`/string concatenation of any caller value into a query body exists on this path.
TRACE:    `kind = x"}}]}},"query":{"match_all":{}},"z":{"script":{"source":"ctx._source.remove('acl')"` → `Predicate{Field:"kind", Op:"term", Value:<evil>}` → clause `{"term":{"kind":<evil>}}` → marshaled → the emitted body **decodes as valid JSON**, has no injected top-level `z` key, and the `term.kind` leaf equals the caller string byte for byte (an inert keyword that matches nothing). The value is never re-parsed and never becomes structure.
BACKING:  `TestDW_5_7_AdversarialFilterValueStaysData` (server), `TestDW_5_7_AdversarialValueParameterizedIntoQueryBody` (retrieval — asserts on the *decoded* body, not on text) — PASS.
VERDICT:  **PASS**

**All requirements met:** YES

## Test-DW Coverage
- [x] All 7 DW items have automated tests that ran in Step 0; test names carry the DW ids (`TestDW_5_1_*` … `TestDW_5_7_*`). DW-5.5 is covered by the executed `make proto-check` gate (no automated test is possible for a codegen-freshness property; observed behavior recorded above).
- [x] Coverage matches the stated 100% level: every filter param, both `include_superseded` states, the source-narrowing path, all 8 MCP rejection shapes + 3 gRPC rejection shapes, the no-filter baseline at all three seams, and the injection value at two seams.
- [x] Every prompt-listed edge case is covered by a named, executed test:

| Edge case | Executed test | Result |
|---|---|---|
| Unknown filter field (error names valid fields) | `TestDW_5_4_InvalidFiltersRejectedAtEntry/unknown_filter_field` (+ the `valid_only` lookalike case) | PASS |
| Malformed time (`since` > `until`) | `TestDW_5_4_InvalidFiltersRejectedAtEntry/{malformed_time,since_after_until}`, `TestDW_5_4_MalformedTimeRangeRejectedAtEntry` | PASS |
| `include_superseded` with no other filter | `TestIncludeSupersededAloneIsAValidRequest` | PASS |
| `sources` naming an unknown tier | `TestDW_5_4_InvalidFiltersRejectedAtEntry/unknown_source`, `TestDW_5_4_EmptySourceNameRejectedAtEntry`, `TestErrInvalidFilterWrapsCallerErrors/unknown_source` | PASS |
| Every filter absent ⇒ identical behavior | `TestDW_5_6_*` at all three seams | PASS |
| `k` bounds still enforced | `TestSearchKBoundsStillEnforced` (k≤0 → `defaultRequestK`; explicit k passes through) + `clampK` clamp to `[1, MaxK=100]` at `internal/retrieval/opensearch.go:58-67` | PASS |

## Dead Code
None found. `go vet` + revive are clean (both would flag unused imports/vars). Every new symbol has a live caller: `sortPredicates` (`searchfilter.go:72`), `timeBounds` (`:67`), `invalidFilter` (`:43,48`), `parseBound`/`validateSources` (`searchargs.go:88,91,98`), `unfilterableSourceNames` (`filters.go`, twice), `FieldSpec.field` (`filters.go:156`), `invalidFilterf` (10 call sites). No unreachable code after early returns, no debug statements, no commented-out blocks.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | Probed the tier fan-out under the new `filtered` path: `selectSources(sel, true)` returns `nil` tierSrcs, and `results` is sized `len(tiers)+len(tierSrcs)` at `opensearch.go:298` — the index math holds (each goroutine writes a disjoint `results[i]`). `sel`, `f.Predicates`, and `aclClause` are read-only across the goroutines; no shared mutable state was introduced. Tests run with the race-free default; `go test ./...` is green. |
| Error Handling | PASS | Every error path returns; no swallowed errors in the new code. Caller errors are classified via the `ErrInvalidFilter` sentinel and mapped to `InvalidArgument` (`server.go:170`), infrastructure errors stay `Internal` — both directions are pinned by executed tests. Adversarial probe: a nil/`"null"` arguments payload decodes to a zero `searchArgs`, which trips the `query == ""` guard rather than panicking; a non-object payload (`["query"]`) is rejected with the vocabulary (`TestSearchArgsRejectNonObject`). |
| Resources | N/A | This phase adds no file handles, connections, locks, caches, or goroutines; it reuses the existing retriever fan-out. |
| Boundaries | PASS | Probed each: `since == until` is accepted (`After` is strict — a legal instant-wide range); a single bound is a legal open interval (`TestDW_5_1_OpenTimeBoundsCompile`); `sources: []` (empty, non-nil) is an explicit error at both barricades, never a silent "all"; nil `Sources` → `sourceSet.selected` returns `true` for a nil set (`filters.go:229-235`), so the `filtered && sel == nil` combination keeps both built-in tiers rather than searching nothing — confirmed by `TestFilteredSearchExcludesUnfilterableSources/filtered: neither runs`, which asserts both indices are still searched. `k` upper bound survives at `clampK`. |
| Security | PASS | See the barricade/ACL analysis below; no defect demonstrated. |

### Security review (this phase is the untrusted-input barricade)
| Check | Status | Evidence |
|---|---|---|
| `include_superseded` widens ONLY the validity window, never authorization | PASS | TRACE: `f.ValidOnly` is consumed at exactly one place — `filterClauses` (`opensearch.go:579`), which gates the `invalid_at`/`expired_at` clause. The ACL clause is appended independently at `:566-567` from `m.acl.Enforce(ctx, f.Identity)` (`:286-293`), and `filterAuthorized(merged, enf)` re-checks every hit at `:342` regardless of `ValidOnly`. There is no code path by which `ValidOnly=false` removes, weakens, or short-circuits either. Backed by `TestDW_5_2_IncludeSupersededCannotBypassACL`, which runs the **production** `acl.Filter` and asserts the tenant term AND the `scope`/`owner_agent_id` clause are present in both index bodies under `ValidOnly:false`. |
| `sources` cannot admit an ACL-denied hit | PASS | TRACE: `sel` (the resolved source set) is consumed only by `selectSources` and `validateFilterableSources`; it never reaches `aclClause` construction or `filterAuthorized`. `selectSources` can only *remove* entries from `m.tiers`/`m.tierSrcs`/`m.postHooks` — it cannot add a source, relax a clause, or skip the authorization pass, which runs unconditionally whenever `m.acl != nil`. Every tier that does run still receives `aclClause`, and post-hook output is re-authorized at `:361`. Narrowing is monotonically result-reducing. |
| No filter value interpolated into a query string | PASS | Values travel as `any` inside `Predicate` → `map[string]any` clause → `json.Marshal`. `TestDW_5_7_AdversarialValueParameterizedIntoQueryBody` decodes the emitted body and proves the DSL-shaped value is a leaf string, not structure. Also confirmed the *field name* is not caller-controlled: it comes from a closed literal set in `compileSearchFilter` and is validated against the registry. |
| Validation at MCP/gRPC entry, before inner modules | PASS | `parseSearchArgs` runs before `s.backend.Search` (`tools.go:290-295`); `compileSearchFilter` runs before `retrieval.Filter` is constructed or `s.Retriever.Search` is called (`server.go:139-166`). Eleven executed rejection cases assert `searchCalls == 0` / `calls == 0`. Defense in depth is intact: the retrieval registry re-validates independently (`validatePredicates`, `resolveSources`) rather than trusting the barricade. |
| Caller error → `InvalidArgument`, not `Internal`; no internal-state leak | PASS | `invalidFilter` → `codes.InvalidArgument` (`searchfilter.go:102-105`); the retriever's own `ErrInvalidFilter` is remapped to `InvalidArgument` at `server.go:167-171`, while a bare infrastructure error stays `Internal` — both pinned by executed tests. Messages carry only the caller-facing vocabulary (`kind, subject, …, sources`) and the caller's own offending value; the physical field names (`occurred_at`, `valid_at`, `invalid_at`), index names, and cluster details never appear in a client-facing message. One cosmetic exception is recorded under Notes. |

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| aposd-designing-deep-modules | Deep interface / information hiding | PASS | The generic `{field, op, value}` predicate vocabulary and the physical document fields are fully hidden behind a flat, named wire contract. `compileSearchFilter` is the single translation point and explicitly does *not* re-derive tier ownership — that knowledge stays in the retrieval registry alone. |
| aposd-designing-deep-modules | Information leakage (same knowledge in multiple modules) | PASS (with Note) | Field *ownership* lives in exactly one place (the registry). Two vocabulary *lists* are restated for caller-facing error text (`mcp.searchParams`/`memorySources`, `server.searchFilterFields`); both are deliberate, documented barricade allowlists whose authority remains the registry, and `TestSearchSchemaAdvertisesEveryFilterParam` pins the MCP pair against each other. Not a demonstrable defect — see Notes. |
| aposd-designing-deep-modules | False abstraction / silent failure | PASS | The unfilterable-source rule is the notable case: a filtered search that *implicitly* excludes `experience`/`graph` only narrows results (documented), while an *explicit* `sources:["graph"] + filter` contradiction is surfaced as an error rather than silently honoring one half. No failure is hidden from the caller. |
| aposd-designing-deep-modules | Granularity mismatch / caller ease of use | PASS | The common case (no filters) requires zero caller knowledge and is byte-identical to the prior request (DW-5.6). Callers never see predicates, ops, or physical fields. |
| cc-defensive-programming | External input validated at entry (barricade) | PASS | Both entries validate in full before any inner module is touched; 11 executed rejection cases assert zero downstream calls. Closed allowlist (not a denylist) for both params and sources. |
| cc-defensive-programming | Barricade does not replace defense-in-depth on security paths | PASS | The retrieval layer re-validates predicates/sources independently, and ACL enforcement is applied twice (query clause + post-merge `filterAuthorized`, plus a third pass after post-hooks) irrespective of any caller filter. |
| cc-defensive-programming | No empty catch / swallowed errors | PASS | Every error in the new code is returned or mapped; no `_ = err` discards on this path. |
| cc-defensive-programming | Assertions carry no executable code / assertions for bugs only | N/A | Go; no assertion mechanism used. Anticipated bad input is handled with error returns, which is the rule's prescription. |
| cc-defensive-programming | Correctness-over-robustness for a security barricade | PASS | Ambiguous input is rejected rather than coerced: empty `sources` errors instead of defaulting to "all"; an unparseable time errors instead of being dropped; an unknown key errors instead of being ignored. |
| cc-defensive-programming | Error messages do not leak security-relevant info | PASS (with Note) | Client-facing messages name only the caller's vocabulary and the caller's own value. The one leak is a Go struct name in a stdlib JSON type error — no state, no topology, no secrets. See Notes. |

## Notes (non-blocking)
1. **Go struct name in the type-mismatch error.** `searchargs.go:73` wraps the raw `encoding/json` error, so `{"query":"q","kind":7}` returns `memory_search: invalid argument value: json: cannot unmarshal number into Go struct field searchArgs.kind of type string` (reproduced with a scratch program in `/tmp/p5-sample-3/`). It leaks the internal struct name `searchArgs` — no server state, credentials, or topology, and the message is genuinely useful for self-correction. Cosmetic; consider naming the field and expected type yourself.
2. **Two hand-maintained vocabulary lists.** `mcp.memorySources` (`searchargs.go:30`) is a hardcoded copy of what `cmd/engram-server` registers, and `server.searchFilterFields` (`searchfilter.go:20`) is a hand-written string. Both are documented as convenience allowlists with the registry/proto as the authority, and the MCP list is pinned to the tool schema by a test — but nothing pins `searchFilterFields` to the proto's actual filter fields, so a future proto field could be omitted from the error text. A reflection-over-descriptor test would close it.
3. **DW-5.3 is proven by a chain, not a single spanning test.** Each seam (MCP → wire → server → retrieval) has its own executed assertion and the chain is complete, but there is no one test that drives a `sources:["semantic"]` MCP call all the way to the emitted cluster body. That end-to-end shape presumably lives in `e2e/` (`make integration`, not run here per dispatch).
4. **`int32(k)` narrowing in `engramclient` (pre-existing).** A `k` above 2^31 wraps negative on the wire and is then clamped by `clampK` to `DefaultK` rather than `MaxK`. Bounds are still enforced (nothing above `MaxK=100` is ever searched), so no requirement is violated; the behavior predates this phase.
5. **`callSearch` now returns argument errors as a tool error rather than a JSON-RPC `codeInvalidParams`.** Deliberate and documented (the agent must be able to *read* the correction), and no DW item constrains the code — worth knowing if a strict MCP client keys off the protocol error.

## Issues (if FAIL)
None.

**Verdict: PASS**

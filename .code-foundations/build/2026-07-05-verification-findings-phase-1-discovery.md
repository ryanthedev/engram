# Discovery + Design: Phase 1 - Read-path index robustness

## Files Found
- `internal/store/opensearch.go` — `OpenSearchStore`, the shared `doJSON` HTTP primitive, `WithEpisodicIndex`/`WithSemanticIndex` options.
- `internal/store/facts.go` — `Candidates`, `ValidTimeNeighbors`, `LiveSuperseders`, `DuplicateLiveContentKeys`, `LiveByContentKey`, `FindByEventID`, `ClosedOverlapChainKeys`, `ChainVersions`, and the shared `searchFacts` helper.
- `internal/store/ledger.go` — `ScanIncomplete` (inline `_search`), `getLedger`/`ClaimLedger` (`_doc/{id}` GET — distinct 404 handling, NOT touched).
- `internal/store/outbox.go` — `ClaimBatch` (inline `_search`).
- `internal/store/enrich.go` — `FindUnembedded` (inline `_search`, line 39).
- `internal/store/counts.go` — `countTenant` (`_count`, line 43).
- `internal/store/lag.go` — `PendingBacklog` (`_count` line 25 + `_search` line 48), `DeadLetteredCount` (`_count` line 91).
- `internal/store/apply.go` — `Apply` (PUTs templates + creates the 5 hardcoded default indices), `indexExists`, `do`, `clusterVersion`.
- `internal/store/templates.go` — index/template name constants; template `index_patterns` are `engram-<tier>*`.
- `cmd/engram-server/main.go` — boot: calls `store.Apply` (line 92) then constructs the store with the CONFIGURED `-episodic/-semantic/-ledger-index` names (line 114).
- `internal/store/acledges.go` (line 171), `internal/store/auth.go` (lines 143/167) — ACL-edge and auth-token `_search` reads. Deliberately OUT of scope (see Gaps).

## Current State
- `doJSON` is the shared read/write HTTP primitive; a `_search`/`_count` returning HTTP 404 currently falls through each site's `status != http.StatusOK` branch and is returned as an `unexpected status` error.
- `searchFacts` is the shared semantic-index search helper behind `Candidates`, `ValidTimeNeighbors`, `LiveSuperseders`, `LiveByContentKey`, `ChainVersions`.
- Inline (non-`searchFacts`) read sites: `DuplicateLiveContentKeys`, `FindByEventID`, `ClosedOverlapChainKeys` (facts.go); `ScanIncomplete` (ledger.go); `ClaimBatch` (outbox.go); `FindUnembedded` (enrich.go); `countTenant` (counts.go); `PendingBacklog` count + oldest-search, `DeadLetteredCount` (lag.go). Guarding `searchFacts` alone is NOT sufficient — each inline site needs the detector.
- `store.Apply` creates only the hardcoded defaults (`EpisodicIndex` = `engram-episodic-000001`, etc.). It does NOT create the configured override names. `main.go` calls `Apply` before constructing the store, so a `-semantic-index=engram-semantic-scratch` override is never materialized on boot → the first reconcile `Candidates()` 404s forever.

## Verified error shape (live cluster, OpenSearch 3.1.0)
Both `_search` and `_count` against a missing index return **HTTP 404** with body `{"error":{"type":"index_not_found_exception", ...}, "status":404}`. The detector matches EXACTLY status 404 AND `error.type == "index_not_found_exception"` — never a blanket 404.

## Gaps
- No read path treats `index_not_found_exception` as empty — this is the 🔴 bug.
- `Apply` materializes only default index names, not configured overrides — the latent second half of the bug.
- `acledges.go` / `auth.go` reads are intentionally left erroring on a missing index: their indices (`engram-acl-edges*`, `engram-auth-tokens*`) are non-flag-overridable Apply defaults, always created on boot, and they sit on the security barricade (auth/ACL). Per cc-defensive-programming, a missing auth/ACL index must stay a LOUD failure, not silently degrade to "no tokens / no grants". The plan enumerates only the semantic/episodic/ledger read paths; these two are correctly excluded.

## Code Standards
`docs/code-standards.md` present. Applied: wrapped errors with `%w` + sentinels; `context.Context` first; deep modules / consumer-defined interfaces; no OpenSearch types in public signatures; table-driven tests + ≥1 dirty test; `log/slog`. The 404 detector is an internal predicate (no vendor types leak); `EnsureIndices` returns only stdlib types.

## Test Infrastructure
- Unit: `internal/store/opensearch_test.go` uses a `fakeOS` httptest server (returns 200 on `_search`, never 404) — existing unit tests are unaffected by the status-404 guard.
- Integration (`//go:build integration`): `liveStore`/`liveOutboxStore` helpers apply the contract and point the store at per-test scratch indices via `testutil.ScratchIndexName` (matches the `engram-<tier>*` template pattern). `make integration` runs the store package with `-tags=integration` against the live pinned cluster.

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-1.1 | Regression: reconcile/candidate path against a missing semantic index returns empty (not error); fails before, passes after | COVERED | Unit `TestDW_1_1_CandidatesMissingIndexEmpty` (custom handler returns the real index_not_found 404 body → `Candidates` returns empty, nil). Provable fail-before by reverting the guard. |
| DW-1.2 | Integration: overridden never-seen semantic index, ingest one event → derived fact retrievable with no manual index PUT | COVERED | Integration `TestDW_1_2_FreshSemanticIndexReconcilesAndRetrieves`: fresh semantic name, NO manual PUT / NO EnsureIndices; reconcile order `Candidates` (empty) → `Create` (auto-creates index) → `Candidates`/`GetFact` finds the fact. Proves the 404-guard belt alone closes the loop. |
| DW-1.3 | Server boot ensures configured `-semantic/-episodic/-ledger-index` names exist (scratch); re-run idempotent | COVERED | Integration `TestDW_1_3_EnsureIndicesCreatesConfiguredAndIdempotent`: `EnsureIndices` creates the 3 configured scratch names (inherit template), second run all `unchanged`. Plus `TestDW_1_3_EnsureIndicesRejectsOffPatternName`: a name not matching `engram-<tier>*` returns an error (loud typo surfacing). |
| DW-1.4 | Dirty: guard distinguishes index_not_found from other errors — a genuine transport/other error still propagates | COVERED | Unit `TestDW_1_4_NonIndexNotFoundErrorsPropagate`: (a) a 404 with a different `error.type` → `Candidates` errors; (b) a 500 → errors; (c) a transport failure (closed server) → errors. Plus direct `TestIsIndexNotFound` table test. |
| DW-1.5 | `make integration` green incl. new tests; no regression in store/worker tests | COVERED | Full `make integration` run + `go test ./internal/store` unit run; baseline was 18 unit / 29 integration green. |

**All items COVERED:** YES (5 DW-IDs in prompt, 5 mapped)

## Design Decisions (design-it-twice, aposd-designing-deep-modules)

### Component 1: the 404-as-empty detector

**Approaches**
1. **Predicate helper** `isIndexNotFound(status int, decoded map[string]any) bool` called at each read site; each site returns its own natural empty value (`nil` slice / `0` count).
2. **Read wrapper** `doRead` that returns a sentinel `errIndexNotFound`; callers `errors.Is` and translate to empty.
3. **Fold into `doJSON`** — return a typed `ErrIndexNotFound` from the shared primitive; every caller checks it.

**Comparison**

| Criterion | A (predicate) | B (wrapper) | C (fold into doJSON) |
|-----------|---|---|---|
| Interface simplicity | 1 pure predicate | new wrapper + sentinel | changes shared primitive contract |
| Information hiding | error shape in ONE place | shape in ONE place | shape in ONE place |
| Caller ease of use | 1 guard line, natural empty per site | forces one return shape (bad for int64 vs []) | every caller must branch |
| Write-path safety | untouched | untouched | **RISK: writes (`_create`, guarded `_doc/{id}`, partial `_update`) share doJSON; a 404 there is already handled distinctly (ErrNotFound / won=false) — folding would blur it** |

**Choice: A.** It is the deepest for this job: one pure predicate hides the exact `{404, error.type=="index_not_found_exception"}` shape; each read site keeps its natural empty value; write paths and the distinct `_doc/{id}` GET 404 handling (`GetFact` ok=false, `getLedger`) are untouched — satisfying the constraint "match ONLY the specific shape, never a blanket any-404". C is rejected because `doJSON` is shared with writes where 404 means something else. B is a shallow wrapper / granularity mismatch (can't unify int64-count and slice-search returns).

**Depth check** — interface methods: 1. Hidden details: the 404 status check, the `error.type` dig, the `"index_not_found_exception"` literal. Common-case complexity: simple (`if isIndexNotFound(status, decoded) { return empty, nil }`).

### Component 2: boot-ensure configured indices

**Approaches**
1. **Package func** `EnsureIndices(ctx, client, base, names...)` — main.go re-passes the configured names.
2. **Extend `Apply`** to take the configured names.
3. **Method** `(s *OpenSearchStore) EnsureIndices(ctx) error` — the store already holds `episodicIndex/semanticIndex/ledgerIndex`; it creates exactly those three, each validated against its `engram-<tier>*` template prefix, tolerating `resource_already_exists`.

**Comparison**

| Criterion | A (pkg func) | B (extend Apply) | C (method) |
|-----------|---|---|---|
| Interface simplicity | needs names re-passed | bloats Apply's contract | 1 zero-arg method |
| Information hiding | names leak back to caller | Apply learns override concept | store owns its own names |
| Caller ease of use | 3 args at call site | changes every Apply caller | `st.EnsureIndices(ctx)` |
| Ordering correctness | must run after Apply (templates) | single call | runs after Apply; documented |

**Choice: C.** The store already knows its configured names (set via options); a zero-arg method keeps that knowledge where it lives instead of leaking index names back into `main.go`. It validates each configured name against the required `engram-<tier>*` prefix (defensive: boot config is external input — a typo like `-semantic-index=engram-episodic-x` that would inherit the WRONG template is rejected loudly at boot, per the plan's correctness-vs-robustness note), then creates it idempotently. `Apply`'s existing index-create body (`indexExists` → PUT → tolerate `resource_already_exists`) is factored into a shared `ensureIndex` helper reused by both `Apply` and `EnsureIndices` (consolidate duplicated knowledge). Called in `main.go` right after the store is constructed, after `Apply` has PUT the templates.

**Depth check** — interface methods: 1. Hidden details: which three indices, the template-prefix validation, the PUT, the `resource_already_exists` race tolerance. Common-case complexity: simple.

### Belt and suspenders
The 404-guard (Component 1) makes any read on a not-yet-created index return empty — the durable defence covering all future indices. `EnsureIndices` (Component 2) materializes the configured names on boot so a mistyped override surfaces as a visible wrong-but-loud created index / a boot error, not silent empty reads. Both land, per the plan Decision Log.

## Prerequisites
- [x] Required files exist.
- [x] Dev cluster up (green, OpenSearch 3.1.0 at localhost:9200 — confirmed via `_cluster/health`).
- [x] Baseline store tests green (18 unit, 29 integration).

## Recommendation
BUILD — the plan fits reality. Implement the `isIndexNotFound` predicate at `searchFacts` + every enumerated inline read site, and add `EnsureIndices` (with template-prefix validation) wired into server boot.

# Review: Phase 3 - Collection registry (re-verification after path-traversal fix)

## Executed Results (Step 0)
- `go build ./...` → success.
- `make test` (full repo) → all packages pass, no failures.
- Store-package tests forced fresh, `go test -count=1 -v ./internal/store/ -run 'Traversal|MetaDoc|DW_3|Duplicate|Provision|Update|Create|Get'` → all pass, including both new security regression tests.
- `make lint` (`go vet ./...` + `revive`) → clean, no findings.
- Integration against live OpenSearch, `ENGRAM_OPENSEARCH_URL=http://localhost:9200 go test -tags=integration -count=1 -v ./internal/store/ -run 'Registry|Collection|DW_3|Traversal|MetaDoc'` → all pass (5 live DW-3 tests + `TestProvisionRejectsPathTraversalName` + `TestGetMetaDocRejectsPathTraversalName`).
- Live attack reproduction (temporary probe, deleted after run): `Provision(ctx, "../../engram-episodic-000001/_search")` against the real cluster returned `store: invalid collection name "../../engram-episodic-000001/_search": must match ^[a-z][a-z0-9_]*$` — gated before any HTTP request; no episodic documents reachable.
- `gofmt -l` on `registry.go`/`registry_test.go` → clean; no debug artifacts.

## Fix verification (previously the sole blocker)

### The exact prior repro is now closed — with live evidence
PREMISE:  The Phase-3 finding was that `Provision(ctx, name)` embedded a raw caller-supplied name into an unescaped request path, letting `name = "../../engram-episodic-000001/_search"` read live episodic-memory documents through the registry.
EVIDENCE: internal/store/registry.go:335-337 (top-level `validateCollectionName(name)` in `Provision`), :539-541 (defense-in-depth `validateCollectionName(name)` inside `getMetaDoc`), :196-204 (`validateCollectionName` helper).
TRACE:    `Provision(ctx, "../../engram-episodic-000001/_search")` → line 335 `validateCollectionName` runs FIRST → `collectionNameRE` (`^[a-z][a-z0-9_]*$`) does not match (slashes/dots absent from the grammar) → returns the invalid-name error at line 335 → `getMetaDoc` is never reached, no URL is built, no HTTP request is issued. Confirmed live against http://localhost:9200: the call returns the validation error (NOT `ErrNotFound`, NOT an "unexpected status" — i.e. it never touched the HTTP layer). The unit regression `TestProvisionRejectsPathTraversalName` asserts a recording server receives **0** requests across 6 traversal/malformed names; `TestGetMetaDocRejectsPathTraversalName` asserts the same for the helper directly.
VERDICT:  FIXED (execution-verified)

### Independent sibling re-audit — every name-into-path site
I re-derived, not accepted, the "only `Provision` was vulnerable" claim by enumerating every URL-building site in registry.go (`grep -nE 'baseURL[^,]*\+|Sprintf\("%s/'`) and tracing each argument to its trust boundary:

| Site (registry.go) | Interpolated value | Gate | Safe? |
|--------------------|--------------------|------|-------|
| :311 Create `_create/{name}` | `spec.Name` | `normalizeSpec`→`validateCollectionName` at :303 | Yes |
| :407 Update meta `_doc/{name}` | `spec.Name` | `normalizeSpec` at :375, plus `getMetaDoc` re-gate at :379 | Yes |
| :395 Update `{index}/_mapping` | `spec.Index` = `aliasFor(validatedName)` (:236) | registry-assigned, never raw input | Yes |
| :506 `currentPhysical` `_alias/{alias}` | `spec.Index` from a validated spec or from the stored meta doc (written only by validated Create/Update) | registry-assigned | Yes |
| :542 `getMetaDoc` `_doc/{name}` | `name` | `validateCollectionName` at :539 | Yes (the added barricade) |
| :475, :493 `_reindex` / `_aliases` | static paths; names ride in the JSON body, not the URL, and derive from validated `spec.Name`/`spec.Index` | n/a | Yes |
| :605 `loadAll` `{metaIndex}/_search` | `r.metaIndex` (constructor config) | not caller-supplied | Yes |

Exported-method entry points: `Get` (cache lookup, builds no name-path), `List` (no name), `Create`/`Update` (`normalizeSpec`-gated), `Provision` (now top-level gated + helper gated). `normalizeSpec` was refactored to delegate to the same `validateCollectionName` helper (:210), so there is a single source of truth for the grammar+reserved-name rule. The audit holds: `getMetaDoc` is the one helper interpolating a caller-supplied name into a path, and no other unescaped-name-into-path site exists. No site was missed by the fix.

## Requirement Fulfillment (regression re-check — all 4 still PASS)

### DW-3.1
PREMISE:  `CollectionRegistry.Create` writes a meta-index doc AND provisions the live index/alias in one call; a follow-up `Get` returns the spec with no process restart.
EVIDENCE: internal/store/registry.go:302-328 (`Create`), :334-352 (`Provision`), :270-280 (`Get`)
TRACE:    `Create` PUTs the meta row then `r.provision(...)` stands up `knowledge-<name>-v1` with its alias in the same call; a cold second registry instance `Get`s the full spec back. Unchanged by the fix (validation gate is additive, on the pre-existing `normalizeSpec` path).
VERDICT:  PASS — `TestDW_3_1_CreateWritesMetaAndProvisionsInOneCall` (fake) and `TestDW_3_1_CreateProvisionsAndGetReturnsSpec_Live` (live) both ran and passed.

### DW-3.2
PREMISE:  `Update` adds a mapping field via live `PUT mapping`; a field-type change provisions a `-v2` index + swaps the alias.
EVIDENCE: internal/store/registry.go:374-421 (`Update`), :446-501 (`reindexSwap`)
TRACE:    Additive field → `PUT {alias}/_mapping`, no reindex; type change → create `-v2`, `_reindex`, atomic `_aliases` swap. Unchanged by the fix.
VERDICT:  PASS — `TestDW_3_2_UpdateAddsFieldViaLiveMapping`, `TestDW_3_2_TypeChangeProvisionsV2AndSwapsAlias` (fake); `TestDW_3_2_UpdateAddsMappingField_Live`, `TestDW_3_2_TypeChangeReindexesAndSwapsAlias_Live` (live) all ran and passed.

### DW-3.3
PREMISE:  registry reads hit the in-process cache; any write invalidates it (create → list reflects it without re-reading the index directly).
EVIDENCE: internal/store/registry.go:571-599 (`cached`/`invalidate`)
TRACE:    Three reads issue one `_search`; `Create` invalidates; next `List` reloads and reflects the new collection. Unchanged by the fix.
VERDICT:  PASS — `TestDW_3_3_ReadsHitCacheWritesInvalidate` (fake), `TestDW_3_3_CreateReflectsInListWithoutDirectRead_Live` (live) ran and passed.

### DW-3.4
PREMISE:  YAML boot-seed applied twice is idempotent (second run makes no changes).
EVIDENCE: internal/knowledge/seed.go:61-84 (`Seed`) — unchanged by the fix (fix touched only registry.go/registry_test.go).
TRACE:    Second `Seed` finds every name present via `Get` → zero `Create` calls, `created=nil`; live meta-row `_seq_no` unchanged across runs.
VERDICT:  PASS — `TestDW_3_4_SeedTwiceIsIdempotent` (fake), `TestDW_3_4_SeedIdempotent_Live` (live) ran and passed.

**All requirements met:** YES

## Test-DW Coverage
- [x] DW-3.1..3.4 each covered by a fake-cluster unit test AND a live-cluster integration test naming it (`DW_3_N` prefix on both sides) — all ran in this session.
- [x] Security regression covered by `TestProvisionRejectsPathTraversalName` (6 traversal/malformed names, 0 HTTP requests to a recording server) and `TestGetMetaDocRejectsPathTraversalName` — both ran and passed.
- [x] Test coverage matches the stated 100% level.

## Dead Code
None found. `gofmt -l` clean on the changed files; no unreachable code, debug prints, TODO/FIXME markers, or commented-out blocks introduced by the fix.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | Cache generation-counter guard, `op_type=create` for Create races (`TestCreateDuplicateAndConcurrentRace_Live`), `if_seq_no`/`if_primary_term` guard on Update (`TestUpdateConcurrentWriteIsErrConflict`). Unchanged by the fix. The lesser concurrent-Update reindex error-classification rough edge remains a non-blocking Note (below). |
| Error Handling | PASS | Sentinel wrapping (`ErrConflict`/`ErrNotFound`), 404-as-empty for a not-yet-created meta index, `Seed`'s parse/notfound/conflict/other branching. Unchanged. |
| Resources | PASS | All HTTP flows go through `doJSON`/`do` which `defer resp.Body.Close()` in every path. Unchanged. |
| Boundaries | PASS | `normalizeSpec` rejects empty/hyphenated/reserved/clashing/bad-type before any write (`TestSpecValidationRejectsBadInput`, 8 sub-cases). Version arithmetic guarded in `reindexSwap`. Unchanged. |
| Security | **PASS** (was FAIL) | The path-traversal blocker is closed: `validateCollectionName` gates `Provision` at the top and `getMetaDoc` as defense-in-depth; the grammar admits no `/`, `.`, or path metacharacters. Re-audit found no other unescaped-name-into-path site. Live reproduction of the exact prior attack now returns a validation error with zero cluster contact. |

## Loaded-Skill Criteria
N/A — no skills loaded (dispatch prompt had no `## Additional Skills` section).

## Notes (non-blocking)
- **Concurrent-`Update` reindex error classification** (carried over, still non-blocking): two concurrent type-change `Update`s can have the loser's `_aliases` "remove old alias" action 404 and surface as a generic error rather than `ErrConflict`. Final cluster state stays correct (one meta write wins; alias ends at the winner's version). No DW item or listed edge case exercises concurrent `Update`s; worth hardening before Phase 6 exposes concurrent collection admins.
- `validateCollectionName` is now the single source of truth for the name grammar + reserved-name rule, shared by `normalizeSpec`, `Provision`, and `getMetaDoc` — a clean consolidation that removes the possibility of a future path diverging from the rule.

## Issues (if FAIL)
None. The sole prior blocker is resolved and execution-verified.

**Verdict: PASS — path-traversal blocker fixed and confirmed live (Provision + getMetaDoc gated by validateCollectionName, registry.go:335-337/:539-541); independent sibling re-audit found no missed name-into-path site; DW-3.1..3.4 all still pass on fake and live clusters; build, full test suite, and lint all clean.**

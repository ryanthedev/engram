# Review: Phase 1 - 404-as-empty guard for fresh-index reads

## Executed Results (Step 0)
- Build: `go build ./...` → success.
- Unit tests: `make test` → all packages `ok` (0 failures).
- Lint: `make lint` (`go vet ./...` + revive) → exit 0, no warnings.
- Integration: `make integration` (OpenSearch 3.1.0, podman `engram-dev-os`, already running) → all 13 integration packages `ok`, including `internal/store`, `internal/worker`, `internal/experience`, `internal/graph` (bi-temporal/supersession-adjacent packages).
- Targeted DW-1 run: `go test -tags=integration -v -run 'TestDW_1_|TestIsIndexNotFound' ./internal/store/...` → 7 top-level tests / 18 total (with subtests), all PASS.
- Fail-before/pass-after: temporarily replaced `isIndexNotFound` with `return false` in `internal/store/opensearch.go`, reran `TestDW_1_1_CandidatesMissingIndexEmpty` and `TestDW_1_1_AllReadPathsEmptyOnMissingIndex` → both FAIL, every one of the 14 enumerated read call sites (Candidates, ValidTimeNeighbors, LiveSuperseders, LiveByContentKey, ChainVersions, DuplicateLiveContentKeys, ClosedOverlapChainKeys, FindByEventID, ScanIncomplete, ClaimBatch, FindUnembedded, Counts, PendingBacklog, DeadLetteredCount) surfaced the raw 404 as an error. Restored the guard, reran → both PASS, `git diff --stat` on the file matches the original 19-line insertion exactly (no residual change).

## Requirement Fulfillment

### DW-1.1
PREMISE:  candidate path against a non-existent semantic index returns empty candidates, not error (test fails before the guard, passes after).
EVIDENCE: internal/store/facts.go:379-388 (`searchFacts`, called by `Candidates` at facts.go:78); internal/store/opensearch.go:208-215 (`isIndexNotFound`); internal/store/robustness_test.go:59-68 (`TestDW_1_1_CandidatesMissingIndexEmpty`), robustness_test.go:74-128 (`TestDW_1_1_AllReadPathsEmptyOnMissingIndex`).
TRACE:    `Candidates` builds a bool query → `searchFacts` POSTs `/{semanticIndex}/_search` against an index that does not exist → OpenSearch 3.1.0 returns HTTP 404 body `{"error":{"type":"index_not_found_exception",...}}` → `isIndexNotFound(404, decoded)` matches → `searchFacts` returns `(nil, nil)` → `Candidates` returns `([]VersionedFact{}, nil)`, not an error.
VERDICT:  PASS (fail-before/pass-after independently reproduced above, not just accepted from the shipped test).

### DW-1.2
PREMISE:  with an overridden never-before-seen semantic index name, ingest one event → derived fact retrievable without any manual index PUT.
EVIDENCE: internal/store/robustness_integration_test.go:23-80 (`TestDW_1_2_FreshSemanticIndexReconcilesAndRetrieves`).
TRACE:    Test applies only the cluster contract (templates + default indices), explicitly deletes/never-creates the scratch semantic+episodic indices, runs `Candidates` on the fresh index (empty, nil err — the bug fix), then `Create` writes the fact (auto-creating the index, inheriting the `engram-semantic*` template via Apply's prior PUT), refreshes, and re-runs `Candidates`, asserting the new fact's ID is present in the results.
VERDICT:  PASS — ran live against OpenSearch 3.1.0 in `make integration`; observed PASS with no manual index PUT anywhere in the test body.

### DW-1.3
PREMISE:  server boot ensures the configured -semantic/-episodic/-ledger-index names exist; idempotent re-run.
EVIDENCE: internal/store/apply.go:113-149 (`EnsureIndices`); cmd/engram-server/main.go:113-125 (boot call, placed after `store.Apply` at main.go:92 and before ACL/retriever wiring); internal/store/robustness_integration_test.go:86-128 (`TestDW_1_3_EnsureIndicesCreatesConfiguredAndIdempotent`); robustness_test.go:171-177 (`TestDW_1_3_EnsureIndicesRejectsOffPatternName`).
TRACE:    `EnsureIndices` iterates the store's configured `episodicIndex`/`semanticIndex`/`ledgerIndex`, validates each against its `engram-<tier>*` template prefix (apply.go:139-141), then calls the shared `ensureIndex` (idempotent create). Integration test creates scratch names, calls `EnsureIndices` once → each reports `"created"` and a HEAD confirms 200; calls again → each reports `"unchanged"`.
VERDICT:  PASS.

### DW-1.4
PREMISE:  dirty test — guard distinguishes `index_not_found_exception` from other error shapes (a genuine other error still propagates).
EVIDENCE: internal/store/opensearch.go:208-215; internal/store/robustness_test.go:134-163 (`TestDW_1_4_NonIndexNotFoundErrorsPropagate`, three subtests: different 404 error.type, 500 status, transport failure); internal/store/robustness_internal_test.go:10-33 (`TestIsIndexNotFound`, 8 table cases covering status≠404, decoded=nil, error not a map, error map without `type`, wrong `type` string).
TRACE:    `errorServer(t, 404, "some_other_exception")` → `Candidates` → `isIndexNotFound` returns false (type mismatch) → `searchFacts` falls through to the `status != http.StatusOK` branch → returns non-nil error. Same for 500 and for a closed-connection transport failure (`decoded` is nil, `status` is 0 — first check `status != 404` short-circuits to false).
VERDICT:  PASS.

### DW-1.5
PREMISE:  `make integration` green incl. new tests; no regression in existing store/worker tests.
EVIDENCE: /tmp/fixp1-review/integration2.log (full run, exit 0); make test output (all packages ok).
TRACE:    Full `make integration` invocation ran 13 packages including `internal/store` (new + existing tests) and `internal/worker` (existing outbox/ledger/repair tests exercising bi-temporal supersession) — both `ok`.
VERDICT:  PASS.

**All requirements met:** YES

## Test-DW Coverage
- [x] DW-1.1 — `TestDW_1_1_CandidatesMissingIndexEmpty`, `TestDW_1_1_AllReadPathsEmptyOnMissingIndex` (ran in Step 0, independently reproduced fail-before/pass-after).
- [x] DW-1.2 — `TestDW_1_2_FreshSemanticIndexReconcilesAndRetrieves` (integration-tagged, ran live).
- [x] DW-1.3 — `TestDW_1_3_EnsureIndicesCreatesConfiguredAndIdempotent` (integration) + `TestDW_1_3_EnsureIndicesRejectsOffPatternName` (unit, no cluster needed).
- [x] DW-1.4 — `TestDW_1_4_NonIndexNotFoundErrorsPropagate` (3 subtests) + `TestIsIndexNotFound` (8 table cases) — this is the required dirty test.
- [x] DW-1.5 — the full `make integration` + `make test` runs themselves are the evidence.

All DW items covered by automated tests that ran in Step 0. Dirty-test requirement (≥1) satisfied by DW-1.4 alone, several times over.

## Dead Code
None found. The diff is additive-only (19+3+3+12+9+3+3+10 lines across 8 files); no unreachable code, no leftover debug statements, no commented-out blocks. `isIndexNotFound` is referenced from every file it's declared for; no unused imports introduced (confirmed via `go vet` and `go build` both clean).

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | No new concurrent state; guard is a pure function of (status, decoded) evaluated inline in existing single-request read paths. |
| Error Handling | PASS | Traced the three failure shapes the guard must reject (wrong error.type, non-404 status, transport failure/nil decoded) through `isIndexNotFound`'s three-line body: `status != 404` short-circuits transport failures (status=0) and other statuses; the type-assertion chain (`decoded["error"].(map[string]any)` then `errObj["type"].(string)`) safely zero-values through a nil map, a non-map `error` field, or a missing `type` key without panicking — confirmed by the 8 `TestIsIndexNotFound` table cases including exactly those shapes. |
| Resources | N/A | No new file handles/connections/locks; reuses the existing `doJSON` HTTP primitive. |
| Boundaries | PASS | Traced `decoded == nil` (transport failure case): `errObj, _ := decoded["error"].(map[string]any)` on a nil map returns zero value + ok=false in Go (no panic), `typ` stays `""`, comparison to `"index_not_found_exception"` is false — correct rejection, verified by the "transport failure, nil decoded" test case (PASS) and independently by the transport-failure subtest in `TestDW_1_4`. |
| Security | N/A | No untrusted external input newly parsed; `decoded` already flows through the existing `doJSON`/JSON-decode path used everywhere else in the store package — this guard only inspects two already-decoded fields more precisely than before. |

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | No executable code in assertions | N/A | No assertions introduced; this is error-handling code (external I/O boundary), not assertion code — correctly categorized per the skill's own table (external input / anticipated runtime condition → error handling, not assertion). |
| cc-defensive-programming | No empty catch blocks | PASS | Every `isIndexNotFound` call site either returns a defined empty-result value with an explaining comment (e.g. `counts.go:48` `return 0, nil // index not created yet: count 0`) or falls through to the existing `status != http.StatusOK` error branch — never a bare swallow. |
| cc-defensive-programming | External input validated at entry / barricade correctly scoped | PASS | The barricade is scoped precisely to "OpenSearch read response shape," not blanket "any 404." Verified `GetFact` (facts.go:26-43, a `_doc/{id}` GET) does NOT call `isIndexNotFound` — its own pre-existing switch treats 404 as "no such doc" via a structurally separate branch, so the new guard is never blurred into document-level absence. Verified `acledges.go:171-177` and `auth.go:167-173` (auth/ACL `_search` reads) do not reference `isIndexNotFound` at all — any non-200 status there, including a missing-index 404, still returns a hard error, confirmed by tracing both functions' bodies (no guard present) and by the fact that neither file appears in the `grep -n isIndexNotFound` result set. |
| cc-defensive-programming | Assertions for bugs only / correctness vs robustness | PASS | This is a robustness choice (treat a specific, well-understood absent-resource condition as empty) applied only at the exact boundary condition it targets (index_not_found_exception), leaving every other fault path (auth, ACL, non-404, wrong error.type, transport failure) on the strict "shut down / propagate" correctness path — appropriate given auth/ACL is the security-critical path this codebase must never silently soften. |
| aposd-designing-deep-modules | Interface depth / information hiding | PASS | `isIndexNotFound(status, decoded)` is a single small function reused across 12 call sites (facts.go×4, counts.go, enrich.go, lag.go×3, ledger.go, outbox.go) — a deep, narrow interface hiding the full OpenSearch error-body shape behind one boolean predicate. No caller re-implements the type assertion. |
| aposd-designing-deep-modules | No information leakage / silent failure | PASS | `EnsureIndices` surfaces failure loudly (returns an error, does not silently create under the wrong template) when a configured name doesn't match its tier prefix — satisfies the skill's "Silent Failure" red flag check: the module could have silently created the index anyway and hidden the mismatch, but it doesn't. |

## Notes (non-blocking)
- `updateByEventID` (outbox.go:129-152, backing `Complete`/`DeadLetter`) is not guarded and was not required to be — it targets an event that by construction was already ingested into the episodic index, so a missing index there would indicate a different, more serious problem (not the "fresh index, first write hasn't happened yet" case this fix addresses). Not in the DW list or the additional-checks list; noting only as a design observation, not a defect.
- The dispatch prompt's file list did not mention `internal/testutil/testutil.go`; confirmed via `git status`/`git diff --stat` that this file is pre-existing and untouched by this change (the new integration test simply consumes its existing `ScratchIndexName`/`DeleteIndex`/`RefreshIndex`/`Call`/`OpenSearchURL`/`HTTPClient` helpers).
- `docs/eval/dashboard.md` and `eval/gate-runs/history.jsonl` also show as modified in `git status` but are unrelated eval-gate artifacts, not part of this fix's files list; not reviewed as part of this scope.

## Issues (if FAIL)
None.

**Verdict: PASS**

# Review: Phase 3 - Seed the curated_notes knowledge collection

## Executed Results (Step 0)
- `go build ./...` → exit 0, no output.
- `go clean -testcache && go test ./...` → all packages `ok`, including `internal/cli`, `internal/engramclient`, `cmd/...`, `internal/importlint` (33 tested packages, 0 failures).
- `go test ./internal/cli/ ./cmd/... ./internal/engramclient/ -v` → all tests PASS, including every `TestDW_3_*` name in `internal/cli/seedknowledge_test.go`.
- `make lint` (go vet + revive v1.12.0) → exit 0, no findings.
- `go vet ./internal/cli/... ./cmd/engram-seed-knowledge/... ./internal/engramclient/...` → no output (clean).

## Requirement Fulfillment

### DW-3.1
PREMISE:  against a stub server, the seed routine issues one create-collection (with the `memory_ref` mapping) and one ingest batch of the demo docs; a re-run issues ingest again without a duplicate-create failure (idempotency — stub returns already-exists on the 2nd create and the routine tolerates it).
EVIDENCE: `internal/cli/seedknowledge.go:164-186` (`seedKnowledge`, `createSeedCollection`); `internal/cli/seedknowledge_test.go:90-147` (`TestDW_3_1_SeedIssuesCreateThenIngest`, `TestDW_3_1_RerunToleratesAlreadyExistsAndReingests`)
TRACE:    1st run against `seedStub{}` → `createSeedCollection` calls `CreateCollection` (stub records `lastSpec`, `createCalls=1`) → `err==nil` so no wrap → `KnowledgeIngest` called once (`ingestCalls=1`, `lastIngest.Docs` = 5 = `len(seedDemoDocs)`, `Source="curated-demo"`, `HarvestId="curated-demo-seed"`). 2nd run with `createErrs=[nil, AlreadyExists]` → 2nd `CreateCollection` returns `status.Error(codes.AlreadyExists,...)` → `engramclient.IsAlreadyExists(err)` true → `createSeedCollection` returns nil → ingest proceeds → `ingestCalls=2`, no error returned from `seedKnowledge`. Ran both tests: PASS.
VERDICT:  PASS

### DW-3.2
PREMISE:  the demo-doc set is defined in one place, each doc carrying a non-empty `memory_ref` + `memory_ref_name`; a table test asserts every doc has a non-empty `memory_ref`.
EVIDENCE: `internal/cli/seedknowledge.go:52-119` (single `var seedDemoDocs = []mcp.KnowledgeDoc{...}`); `internal/cli/seedknowledge_test.go:151-178` (`TestDW_3_2_DemoDocsCarryMemoryRefAndName`, table-driven via `t.Run(doc.ID, ...)` over all 6 docs)
TRACE:    For each of the 6 entries in `seedDemoDocs`, the test asserts `doc.ID != ""`, no duplicate id, `doc.Fields["memory_ref"].(string) != ""`, `doc.Fields["memory_ref_name"].(string) != ""`, `doc.Text != ""`. Ran: all 6 subtests PASS (including `curated-unresolved-demo`, whose `memory_ref` is deliberately a non-existent id but is still a non-empty string, satisfying the literal DW wording).
VERDICT:  PASS

### DW-3.3
PREMISE:  a missing/role-less token path surfaces the server's PermissionDenied with a message naming the `--roles admin` remedy (dirty test).
EVIDENCE: `internal/cli/seedknowledge.go:193-198` (`wrapPermissionDenied`); `internal/cli/seedknowledge_test.go:194-233` (`TestDW_3_3_PermissionDeniedOnCreateNamesRolesRemedy`, `TestDW_3_3_PermissionDeniedOnIngestNamesRolesRemedy`)
TRACE:    Stub `CreateCollection` returns `status.Error(codes.PermissionDenied, "not authorized...")` → `createSeedCollection`: not nil, `IsAlreadyExists`=false → `wrapPermissionDenied` → `IsPermissionDenied(err)`=true → returns `fmt.Errorf("seed-knowledge: creating the curated_notes collection: permission denied -- mint an admin token first: \`engram token create --roles admin\` (%w)", err)`. Test asserts `status.Code(err)==PermissionDenied` (verified `%w`-wrapping preserves the gRPC status through `status.Code`) and `strings.Contains(err.Error(), "engram token create --roles admin")`, and `ingestCalls==0` (create failure short-circuits ingest). Second test repeats the same assertion for a PermissionDenied on `KnowledgeIngest` instead. Ran both: PASS.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] DW-3.1 — `TestDW_3_1_SeedIssuesCreateThenIngest`, `TestDW_3_1_RerunToleratesAlreadyExistsAndReingests` (ran in Step 0, PASS)
- [x] DW-3.2 — `TestDW_3_2_DemoDocsCarryMemoryRefAndName` (table test, PASS), plus `TestDW_3_2_UnresolvedDemoDocPresentForThePhase2LivePath`
- [x] DW-3.3 — `TestDW_3_3_PermissionDeniedOnCreateNamesRolesRemedy`, `TestDW_3_3_PermissionDeniedOnIngestNamesRolesRemedy` (PASS)
- Function-level coverage (via `go test -coverpkg`): `seedCollectionSpec` 100%, `seedKnowledge` 100%, `createSeedCollection` 100%, `wrapPermissionDenied` 100%, `RunSeedKnowledge` 84.6% (the `flag.ContinueOnError` parse-error return branch is unexercised — not a DW item or listed edge case; see Notes), `IsAlreadyExists` 100%, `IsPermissionDenied` 100% (exercised indirectly, through real gRPC status codes returned by the stub server over the wire and decoded by `status.Code`, from `internal/cli` tests — genuine cross-package execution, not a mock of the predicate itself).
- Matches stated level ("100% of new/changed functions") at the function-exercised level; one branch inside `RunSeedKnowledge` is below 100% statement coverage (noted, not a DW/edge-case violation).

## Dead Code
None found. No unused imports, no unreachable code after early returns, no commented-out blocks, no debug prints in `seedknowledge.go`, `seedknowledge_test.go`, `cmd/engram-seed-knowledge/main.go`, `internal/engramclient/knowledge.go` diff, or `scripts/seed-curated-notes.sh`.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | No shared mutable state, goroutines, or async paths introduced by this diff; `seedKnowledge` runs a strictly sequential two-call sequence on a single CLI invocation. |
| Error Handling | PASS | Traced the adversarial case: a non-`AlreadyExists`, non-`PermissionDenied` create error (`codes.Internal`, "opensearch is on fire") — `createSeedCollection`: `err!=nil`, `IsAlreadyExists`=false → falls into `wrapPermissionDenied`, `IsPermissionDenied`=false → returns `err` unchanged. Confirmed live via `TestCreateCollectionNonAlreadyExistsErrorPropagates`: `status.Code(err)==Internal`, message does not contain `--roles admin`. Matches the prompt's explicit "NON-AlreadyExists create error still propagate as a hard failure" check. |
| Resources | N/A | No file handles, connections, locks beyond the single `*engramclient.Client`, which is opened and `defer client.Close()`'d exactly once in `RunSeedKnowledge`; `seedKnowledge` itself takes an already-dialed client and does not own its lifecycle (test-injectable by design). |
| Boundaries | PASS | `seedDemoDocs` is a fixed, authored, compile-time literal set (not runtime/external input) — traced `wrapPermissionDenied(action, err)` is only ever invoked from the two `err != nil` guards in `seedKnowledge`/`createSeedCollection`, so its "never receives nil" comment holds by inspection; confirmed `status.Code(nil) == codes.OK` (verified interactively), so even a hypothetical nil-err call to `IsAlreadyExists`/`IsPermissionDenied` would not false-positive. |
| Security | PASS | Token is passed straight to the pre-existing, shared `dialClient` (unchanged in this diff) — no new credential handling added. `scripts/seed-curated-notes.sh` passes `"$@"` quoted (no unquoted expansion / injection), uses `set -euo pipefail`, and requires `ENGRAM_TOKEN` non-empty before running. `internal/importlint` (re-run cold, not cached) confirms `internal/cli` does not import `google.golang.org/grpc/codes`/`status` directly — the `IsAlreadyExists`/`IsPermissionDenied` predicates are the sole edge where `internal/engramclient` is allowed to know gRPC status codes, matching the doc comment's claimed boundary. |

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | GC-1: routine protects itself from bad input data | PASS | The only true external input at this boundary is the CLI `-addr`/`-token` flags, handled by the shared, pre-existing `dialClient` (unchanged). `seedDemoDocs` is authored literal data, not external input (the file's own header comment at seedknowledge.go:49-51 correctly reasons about this distinction). |
| cc-defensive-programming | EC-3 / RF-2: no empty catch blocks | PASS | Every `err != nil` branch in `seedknowledge.go` either returns the error, wraps it, or (in `createSeedCollection`) explicitly tolerates one classified case (`AlreadyExists`) before returning nil — no silent swallow. |
| cc-defensive-programming | SO-2 / RF-10: all error-return codes checked | PASS | Both `client.CreateCollection` and `client.KnowledgeIngest` return values are checked; no ignored `_ = err` anywhere in the diff. |
| cc-defensive-programming | RF-12: fallback masking failure | PASS | `createSeedCollection` tolerates only the one specific classified code (`AlreadyExists`) via `IsAlreadyExists`, not a blanket "swallow and continue" — traced the `codes.Internal` adversarial case above and confirmed it still propagates as a hard failure rather than being silently absorbed. |
| cc-defensive-programming | GH-3: barricades contain error-handling concerns | PASS | gRPC-status classification is centralized in two named predicates (`IsAlreadyExists`, `IsPermissionDenied`) in `internal/engramclient`, the transport boundary package, rather than scattered `status.Code(err)==codes.X` checks at each call site — confirmed by a cold (non-cached) run of `internal/importlint`, which asserts `internal/cli` does not import `google.golang.org/grpc/{codes,status}` directly. |

## Notes (non-blocking)
- `RunSeedKnowledge`'s `flag.ContinueOnError` parse-error branch (an unrecognized flag, e.g. `-bogus-flag`) is reachable and correct (manually verified it returns a `flag: not defined` error) but is not exercised by any test in `seedknowledge_test.go`, leaving that one function at 84.6% statement coverage rather than 100%. Not a DW item, not a listed edge case, and not a demonstrated defect — the branch behaves correctly when manually exercised — so this is a coverage gap note, not a FAIL.
- `seedCollectionSpec`'s doc comment says `memory_ref` is declared "filterable/sortable (future structured lookups)" — that's accurate to the code and to the DW-3.1 test's own assertion, which checks both `Filterable` and `Sortable` are true on the wire.
- The dispatch's edge case "re-run → docs upsert by id in place, no duplicates" is satisfied structurally rather than via a new integration test: `seedDemoDocs` uses the same fixed IDs on every invocation (verified no duplicate IDs within the set, `TestDW_3_2_DemoDocsCarryMemoryRefAndName`), and the actual upsert-by-id dedup happens in the pre-existing, unchanged server-side `KnowledgeIngest` handler, which is outside this phase's file list.

## Issues (if FAIL)
None.

**Verdict: PASS**

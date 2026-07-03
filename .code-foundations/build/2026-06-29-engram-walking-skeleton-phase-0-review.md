# Review: Phase 0 - Engram Walking Skeleton

## Executed Results (Step 0)
- Build: `go build ./...` → Success (all packages, including generated `api/engrampb`)
- Unit tests: `rtk proxy go test ./... -v` → 37/37 tests PASS across 8 packages with test files (`internal/contracts`, `internal/embed`, `internal/eval`, `internal/eval/goldgen`, `internal/memory`, `internal/store`); 0 failures
- Lint: `make lint` (`go vet ./...` + `revive -config revive.toml -set_exit_status -exclude ./api/engrampb/... ./...`) → exit 0, no findings
- Proto codegen check: `make proto-check` (`scripts/codegen.sh` → buf lint + generate, then `git diff --exit-code -- api/engrampb`) → "codegen: ok", exit 0, no diff
- Integration/spike suite: `make apply-templates` then `make integration` against live `engram-dev-os` (podman, OpenSearch 3.1.0, verified via `curl localhost:9200/`) → `ENGRAM_OPENSEARCH_URL=http://localhost:9200 go test -tags=integration -count=1 -v ./internal/spike/ ./internal/store/` → 9/9 tests PASS (3 spikes + `TestDW_0_4_ApplyTemplatesIdempotent` + 5 store idempotency/contract tests), 0 failures

## Requirement Fulfillment

### DW-0.1
PREMISE:  clean checkout runs `go build ./...` and `go test ./...` green in CI.
EVIDENCE: `.github/workflows/ci.yml:20-23` (`make build`, `make test`, `make lint`, `make proto-check` steps); `internal/contracts/contracts_test.go:26-40` (`TestDW_0_1_CIPipelineDefinesBuildTestLintCodegen`)
TRACE:    `go build ./...` → "Go build: Success"; `go test ./...` → 37/37 pass; `TestDW_0_1_CIPipelineDefinesBuildTestLintCodegen` reads `ci.yml` and asserts it contains all four `make` steps and the pinned `opensearch:3.1.0` image → PASS
VERDICT:  PASS

### DW-0.2
PREMISE:  `Store`, `Retriever`, `Extractor`, `Reconciler`, `Embedder` interfaces compile with doc-comment contracts (enforced by `revive`'s exported-comment rule).
EVIDENCE: `internal/store/store.go:70-121` (Store), `internal/retrieval/retriever.go:39-42` (Retriever), `internal/ingest/ingest.go:60-64` (Extractor) and `:69-74` (Reconciler), `internal/embed/embedder.go:24-30` (Embedder); `revive.toml:7-8` (`[rule.exported]`); `internal/contracts/contracts_test.go:47-74` (`TestDW_0_2_SeamInterfacesHaveDocCommentContracts`)
TRACE:    `go build ./...` succeeds; `make lint` runs `revive` with the exported rule over all five packages → exit 0; `TestDW_0_2_SeamInterfacesHaveDocCommentContracts` AST-parses each interface + every named method and asserts a non-empty doc comment → PASS for all five
VERDICT:  PASS

### DW-0.3
PREMISE:  Episodic + SemanticFact structs + matching OpenSearch index templates checked in; a test asserts knn_vector 1024-dim, `engine: faiss`, HNSW `m`/`ef_construction`, SQfp16 encoder, BM25 text, bi-temporal date fields, tenancy/provenance fields, and episodic outbox fields.
EVIDENCE: `internal/memory/record.go:9-47` (Episodic), `:53-95` (SemanticFact); `internal/store/templates/episodic.json`, `internal/store/templates/semantic.json`; `internal/store/templates_test.go:92-138` (`TestDW_0_3_EpisodicTemplateContract`, `TestDW_0_3_SemanticTemplateContract`)
TRACE:    `requireKNNVector` (templates_test.go:30-73) parses `text_embedding`/`fact_embedding` and asserts `dimension=1024`, `method.engine=faiss`, `method.name=hnsw`, `parameters.m=16`, `parameters.ef_construction=128`, `parameters.encoder={name:sq,type:fp16}`; `requireFieldTypes` asserts `text`/`statement` are `"text"` (BM25), `valid_at/invalid_at/created_at/expired_at/invalidated_tx_at` and `occurred_at/created_at/processed_at/claim_lease_until` are `"date"`, `tenant_id/team_id/scope/owner_agent_id/source_ids` are `"keyword"`, and `attempts`/`dead_lettered` outbox fields are present → both tests PASS
VERDICT:  PASS

### DW-0.4
PREMISE:  apply-templates script creates indices + the RRF pipeline on a dev OpenSearch idempotently; re-running is a no-op.
EVIDENCE: `internal/store/apply.go:40-95` (`Apply`); `cmd/engram-apply-templates/main.go`; `internal/store/apply_test.go:76-122` (`TestDW_0_4_ApplyIsIdempotentAgainstFakeCluster`); `internal/store/apply_integration_test.go:17-44` (`TestDW_0_4_ApplyTemplatesIdempotent`, `//go:build integration`)
TRACE:    Live run: `make apply-templates` against `engram-dev-os` (3.1.0) → "result: no-op (already up to date)"; `make integration` ran `TestDW_0_4_ApplyTemplatesIdempotent` live — first apply then second apply, second run logs `unchanged` for both indices and `res2.Changed()==false` asserted → PASS; fake-cluster test asserts first run creates both indices (`Changed()==true`), second run reports `unchanged` for both and never re-PUTs an index (`fake.puts` after first run contains no `index:` entries) → PASS
VERDICT:  PASS

### DW-0.5
PREMISE:  eval-harness skeleton loads a seed gold set (query → expected ids) and emits recall@k (returns 0 until Phase 1 — harness exists and runs).
EVIDENCE: `internal/eval/harness.go:53-102` (`LoadGoldSet`, `Validate`), `:142-177` (`Run`), `:179-186` (`NullRetriever`); `eval/goldset/seed.json` (checked in, 30 facts × 4 query classes); `internal/eval/harness_test.go:28-44` (`TestDW_0_5_HarnessLoadsSeedAndEmitsRecallAtK`)
TRACE:    `eval.LoadGoldSet("eval/goldset/seed.json")` → loads successfully; `eval.Run(ctx, NullRetriever{}, gs, 10, "")` → `rep.Queries == len(gs.Queries)`, `rep.RecallAtK == 0`, `rep.MRR == 0`, `rep.NDCGAtK == 0` (NullRetriever returns no hits by construction) → PASS. `cmd/engram-eval/main.go` runs the same path as a CLI.
VERDICT:  PASS

### DW-0.6
PREMISE:  D0 (Go) explicitly confirmed or flipped; language locked in this plan's Decision Log.
EVIDENCE: `.code-foundations/plans/2026-06-29-engram-walking-skeleton.md:70` (Decision Log row `| D0 | **Go**, not Rust — **CONFIRMED 2026-07-03 (Phase 0): language locked.** ...`); `internal/contracts/contracts_test.go:111-128` (`TestDW_0_6_D0ConfirmedInDecisionLog`)
TRACE:    Read the plan file directly — line 70 begins `| D0 ` and contains both the literal substrings `CONFIRMED` and `language locked`; the test reads the same file, scans for the `| D0 ` row, and asserts both substrings are present → PASS
VERDICT:  PASS

### DW-0.7
PREMISE:  `api/proto/engram.proto` (`Ingest` with required `event_id`, `Search` RPCs) compiles via codegen in CI; generated Go builds.
EVIDENCE: `api/proto/engram.proto:20-30` (`service Engram { rpc Ingest(...); rpc Search(...); }`), `:34` (`string event_id = 1;` documented Required); `api/engrampb/engram.pb.go`, `api/engrampb/engram_grpc.pb.go` (generated, checked in); `.github/workflows/ci.yml:26-27` (`make proto-check`); `internal/contracts/contracts_test.go:130-157` (`TestDW_0_7_GeneratedProtoBuildsAndCarriesEventID`)
TRACE:    `make proto-check` → `scripts/codegen.sh` runs `buf lint` + `buf generate` (pinned `protoc-gen-go@v1.36.6`, `protoc-gen-go-grpc@v1.5.1`) → "codegen: ok"; `git diff --exit-code -- api/engrampb` → exit 0 (no diff, generated output matches checked-in bytes); `go build ./...` builds `api/engrampb` cleanly; `TestDW_0_7_GeneratedProtoBuildsAndCarriesEventID` compiles a `engrampb.EngramClient` interface assertion covering both RPCs and round-trips `IngestRequest.EventId` → PASS
VERDICT:  PASS

### DW-0.8
PREMISE:  the ID & idempotency contract (content-key scheme, doc-`_id` format, `supersedes` chain, claim-first ledger design — D11/D13) is documented in the code and reflected in the record structs.
EVIDENCE: `internal/memory/doc.go:1-45` (package doc: content-key formula, `_id` formula, `op_type=create`, `supersedes` semantics, claim-first ledger); `internal/memory/ids.go:19-49` (`ContentKey`, `FactDocID`, `LedgerKey.DocID`); `internal/memory/record.go:65-74` (`SemanticFact.ContentKey/Supersedes/ExtractorVersion`); `internal/memory/ids_test.go:18-144`
TRACE:    `TestDW_0_8_ContentKeyAndDocIDContract` proves `ContentKey`/`FactDocID`/`LedgerKey.DocID` are deterministic, field-boundary-unambiguous (separator prevents `("a","bc")`/`("ab","c")` collision), object-sensitive, `valid_at`-sensitive, and timezone-canonical (NYC vs UTC same instant → same id) → PASS; `TestDW_0_8_StructsCarryChainAndLedgerFields` reflects `SemanticFact`/`Episodic`/`LedgerKey` for the exact field/json-tag set → PASS; `TestDW_0_8_ContractDocumentedInCode` greps `doc.go` for the 8 required contract phrases → PASS
VERDICT:  PASS

### DW-0.9
PREMISE:  the three live-cluster spikes run green against the pinned cluster and their findings are logged (op_type=create 409 flow · RRF pipeline behavior · filtered-kNN recall).
EVIDENCE: `internal/spike/op_create_test.go` (`TestDW_0_9a_OpTypeCreate409Flow`), `internal/spike/rrf_test.go` (`TestDW_0_9b_RRFPipelineShape`), `internal/spike/filtered_knn_test.go` (`TestDW_0_9c_FilteredKNNRecall`); `docs/spikes/phase-0-findings.md` (full findings write-up, dated 2026-07-03, "all three spikes PASS")
TRACE:    `make integration` against live `engram-dev-os` (OpenSearch 3.1.0) → all three spike tests PASS with observed findings logged via `t.Logf`: spike 1 shows `201`→`409` on duplicate `_create`, guarded update `200`→stale-guard `409`; spike 2 shows fused RRF list `[both, bm25-only, knn-only, neither]` with descending rank-based scores (`0.0328` ≈ `2/61`); spike 3 shows `recall@10=1.00` at 40%/8%/2% filter selectivity with zero filter leakage. `docs/spikes/phase-0-findings.md` transcribes all three findings with the same numbers → PASS
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] All 9 DW items have corresponding automated tests that ran in Step 0 (test names reference `DW_0_N` directly, except DW-0.4 and DW-0.9 which are covered by explicitly DW-referenced tests plus supporting fake-cluster/live-cluster tests)
- [x] Test coverage matches the stated level: every code-touching phase has ≥1 dirty/error-path test — `TestApplyRejectsWrongClusterVersion` + `TestApplyReportsUnreachableCluster` + `TestApplyToleratesConcurrentCreateRace` (store/apply), `TestRunRejectsBadK` + `TestGoldSetValidateRejectsCorruptSets` (eval), `TestValidateInfoRejectsDimMismatch` + `TestValidateInfoRejectsUnpinnedModel` (embed)
- [x] Integration tests run against a disposable OpenSearch behind a build tag (`//go:build integration`, `make integration`, live `engram-dev-os` podman container)

No gaps found.

## Dead Code
None found. Scanned all implementation files in scope for unused imports, unreachable code after early returns, debug prints, and commented-out blocks — none present. All `return` statements found via search are ordinary early returns inside test table-loops (`apply_test.go:62,160,165`, `contracts_test.go:124`), not unreachable code.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | `internal/store/apply_test.go:149-180` (`TestApplyToleratesConcurrentCreateRace`) demonstrates the HEAD-then-PUT non-atomic race: a simulated cluster where HEAD always 404s but PUT returns `resource_already_exists_exception` (the loser of a concurrent create). Traced `apply.go:83-92`: `do()` error is checked with `strings.Contains(err.Error(), "resource_already_exists_exception")` and mapped to `"unchanged"` rather than propagated as an error — verified live by spike 1's "Bonus finding" in `docs/spikes/phase-0-findings.md:26-31`, which is the actual mechanism that motivated this handling. |
| Error Handling | PASS | Traced `store.Apply`: unreachable cluster → `clusterVersion` wraps `client.Do` error as `"store: cluster unreachable at %s: %w"` (apply.go:104-106), proven by `TestApplyReportsUnreachableCluster` hitting `127.0.0.1:1`. Wrong-version cluster → single early-return with both actual and pinned version named in the message (apply.go:52-54), proven for 3 versions by `TestApplyRejectsWrongClusterVersion`. `eval.LoadGoldSet` wraps read/parse/validate errors distinctly (harness.go:53-66). |
| Resources | PASS | Traced every HTTP call site in `internal/store/apply.go` (`clusterVersion:107`, `indexExists:134`, `do:161`) and `internal/spike/spike_test.go:71` (`call`) — each uses `defer resp.Body.Close()` immediately after a successful `client.Do`, so no leaked connections on any return path including early error returns. |
| Boundaries | PASS | Traced `PinnedVersionPrefix = "3.1."` against `strings.HasPrefix`: a version string shorter than the 4-char prefix (e.g. `"3.1"`) correctly fails the prefix check (Go's `HasPrefix` returns false when `len(s) < len(prefix)`); `"3.10.0"` does not match `"3.1."` because its 4th byte is `'0'` not `'.'`, so a hypothetical future 3.10.x line would correctly be rejected, not falsely accepted. `eval.RecallAtK`/`NDCGAtK` guard `len(relevant)==0` before any division, and `top()` clamps `k` against `len(ranked)` before slicing — no out-of-range slice or divide-by-zero found. |
| Security | N/A — Phase 0 ships no live input-handling server; the proto documents the `event_id` required-field contract but no gRPC service implementation exists yet (explicitly deferred to a later phase per `api/proto/engram.proto:5-7`). No untrusted-input surface exists in the reviewed code to probe. |

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| ca-architecture-boundaries | Dependency Rule: inner rings (business types) import nothing from outer rings (infra/frameworks) | PASS | Traced imports of all five seam files: `internal/memory/{doc,record,ids}.go` import only `crypto/sha256`, `encoding/hex`, `strings`, `time` (stdlib only — the innermost ring); `internal/store/store.go` (interface only) imports `context`, `errors`, `time`, and `internal/memory` — no `net/http` or OpenSearch types in the interface file itself; `internal/retrieval/retriever.go` imports only `context`; `internal/ingest/ingest.go` imports `context` + `internal/memory`; `internal/embed/embedder.go` imports `context`, `fmt`. No interface signature anywhere names an OpenSearch/HTTP type. |
| ca-architecture-boundaries | Business rules vs. infrastructure separated via dependency inversion (interface in business layer, impl in infra layer) | PASS | `Store`/`Retriever`/`Extractor`/`Reconciler`/`Embedder` are pure interfaces over domain types (`memory.Episodic`, `memory.SemanticFact`); no concrete OpenSearch-backed implementation of any of them exists yet in this phase (correctly deferred — Phase 1 per package doc comments, e.g. `store.go:1-5`, `retriever.go:1-4`). `internal/store/apply.go` (the one piece of code that does speak raw HTTP to OpenSearch) is bootstrap/ops tooling, not a Store implementation, and lives behind its own free function (`Apply`), not behind the `Store` interface — it does not leak into the interface's dependency direction. |
| aposd-designing-deep-modules | Interface depth: hidden complexity vs. interface surface size | PASS (Note) | `Store` has 9 methods, the largest of the five seams. Traced against the depth test: each method's doc comment (store.go:70-121) names a distinct, substantial hidden mechanism (optimistic-concurrency mapping to `ErrConflict`, scan-and-claim outbox leasing, claim-first extraction ledger) that a real OpenSearch-backed implementation would require hundreds of lines to satisfy — the interface is not shallow relative to that implementation. Flagged as a Note (not a FAIL) below: the 9-method surface bundles three logically distinct concerns (episodic outbox, semantic write protocol, extraction ledger) that a future iteration could consider splitting; no DW item or edge case requires this split, so it does not meet the FAIL bar. |
| aposd-designing-deep-modules | Information hiding: no OpenSearch-specific knowledge (mapping shapes, HTTP paths, query DSL) leaks into interface signatures | PASS | All five interfaces' method signatures use only domain types (`memory.Episodic`, `memory.SemanticFact`, `retrieval.Query/Filter/Hit`, `embed.ModelInfo`) plus stdlib types (`context.Context`, `time.Duration`, `int64` for seq/term guards) — no `json.RawMessage`-shaped OpenSearch response type, HTTP status code, or query-DSL fragment appears in any interface method. |

## Notes (non-blocking)
- `Store` interface (9 methods) bundles three concerns — episodic outbox, semantic write protocol, extraction ledger — that could be split into smaller seams in a later phase; not required by any DW item, flagged only as a design consideration (see Loaded-Skill Criteria above).
- `embed.ValidateInfo` (the embedding-dimension-mismatch startup guard, DW edge case 2) is implemented and unit-tested (`TestValidateInfoRejectsDimMismatch`, `TestValidateInfoAcceptsPinnedMatch`) but is not yet called from any `main.go` — there is no service entrypoint to wire it into yet, since no concrete `Embedder` implementation exists until Phase 1. The guard mechanism itself satisfies the edge case at the Phase-0 skeleton level; wiring it into an actual startup path is implicitly Phase 1 scope.
- `docs/spikes/phase-0-findings.md` findings are dated 2026-07-03, matching the actual run captured in this review's Step 0 execution (spikes re-ran clean during this review).

## Issues (if FAIL)
None.

**Verdict: PASS**

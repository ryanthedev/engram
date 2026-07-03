# Discovery + Design: Phase 0 - Foundations & Contracts

## Files Found
- `go.mod` — module `github.com/ryanthedev/engram`, `go 1.23` directive only; no Go source exists yet.
- `docs/code-standards.md` — intended conventions (std layout, sentinel errors, ctx-first, deep modules, table-driven tests, integration behind build tag, slog).
- `.code-foundations/plans/2026-06-29-engram-walking-skeleton.md` — the plan (read in full, incl. Decision Log D0–D16, Bi-temporal semantics, Write protocol).
- `.code-foundations/research/REFERENCE-ARCHITECTURE.md` + `GREENFIELD-BUILD-PLAN.md` — backbone config: 1024-dim + SQfp16, Faiss HNSW `m=16–24` / `ef_construction=128`, `score-ranker-processor` RRF `rank_constant=60`, OpenSearch pinned 3.1.
- `.gitignore`, `README.md`, `docs/vision/index.html` — no impact.

## Current State
Greenfield: zero Go source files, no CI, no proto, no templates. Everything in this phase is net-new. Branch `feature/engram-walking-skeleton` in the build worktree.

## Gaps
| # | Gap (plan assumption vs reality) | Resolution |
|---|----------------------------------|------------|
| 1 | Dispatch says "local OpenSearch 3.1 container (docker)"; **no docker CLI on this machine** | `podman` 5.x is installed (`/opt/homebrew/bin/podman`); machine `podman-machine-default` (4 GiB) exists and is being started. Podman is CLI-compatible with docker; the dev-cluster script tries `docker` first, falls back to `podman`. Pinned image: `opensearchproject/opensearch:3.1.0`. |
| 2 | No `protoc`/`buf`/`revive` binaries installed | Run all three as version-pinned Go tools (`go run <module>@<version>`) — reproducible in CI without host installs; generated code checked in and diff-verified in CI. |
| 3 | `go.mod` says `go 1.23`; toolchain is 1.26.3 | Keep `go 1.23` floor (raise only if a dependency forces it). |
| 4 | CI (GitHub Actions) cannot be executed from this machine | `ci.yml` runs exactly the commands run green locally (build/test/lint/codegen-diff); integration/spike job uses the pinned image as a service. Local green run is the Phase-0 evidence; first push proves the runner. |
| 5 | Plan's `Store.Append(ctx, Record)` — `Record` type unnamed | `Record` = `memory.Episodic` (the only appendable record in the skeleton). Noted as interpretation, not deviation. |
| 6 | Plan's `Reconciler` returns `Op ∈ {Add, Update, Invalidate, Noop}` — bare enum can't identify the predecessor an UPDATE closes | `Op` is a struct `{Kind OpKind; PredecessorID string}`; Kind is the plan's 4-way enum. Elaboration required to make the seam consumable in Phase 2 (D10 step 4 needs the predecessor). |

## Code Standards
`docs/code-standards.md` exists and is followed: std layout (`cmd/`, `internal/`, `api/`), one package per concern (`memory`, `store`, `retrieval`, `ingest`, `embed`, `eval`), sentinel errors (`store.ErrConflict`), `context.Context` first param on all I/O, vendor types kept out of public signatures (interfaces use `memory.*` types, not OpenSearch client types), table-driven tests, integration tests behind `//go:build integration`.

## Test Infrastructure
None exists. Established this phase: stdlib `testing` + table-driven style; `net/http/httptest` for unit-testing the apply tool against a fake cluster; live-cluster integration/spike tests behind `//go:build integration` using `ENGRAM_OPENSEARCH_URL` (default `http://localhost:9200`) against the pinned 3.1.0 container.

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-0.1 | clean checkout runs `go build ./...` + `go test ./...` green in CI | COVERED | whole suite green locally + `TestDW_0_1_CIPipelineDefinesBuildTestLintCodegen` asserts `.github/workflows/ci.yml` wires build/test/lint/codegen steps |
| DW-0.2 | 5 interfaces compile with doc-comment contracts (revive exported rule) | COVERED | `TestDW_0_2_ExportedDeclsHaveDocComments` (go/ast sweep over the 5 interface packages) + revive `exported` rule run in lint step |
| DW-0.3 | record structs + index templates checked in; test asserts knn 1024/faiss/HNSW m,ef/SQfp16/BM25/bi-temporal/tenancy/outbox fields | COVERED | `TestDW_0_3_EpisodicTemplateContract`, `TestDW_0_3_SemanticTemplateContract`, `TestDW_0_3_RRFPipelineContract` |
| DW-0.4 | apply-templates script idempotent against dev cluster; re-run is a no-op | COVERED | unit: `TestDW_0_4_ApplyIsIdempotentAgainstFakeCluster` (httptest); live: `TestDW_0_4_ApplyTemplatesIdempotent` (integration tag, pinned 3.1.0 container) |
| DW-0.5 | eval harness loads seed gold set (pre-registered split) and emits recall@k (0 until Phase 1) | COVERED | `TestDW_0_5_HarnessLoadsSeedAndEmitsRecallAtK`, `TestDW_0_5_SeedSplitPreRegistered` (holdout ≥ 50, split fixed in file) |
| DW-0.6 | D0 (Go) explicitly confirmed; locked in plan Decision Log | COVERED | `TestDW_0_6_D0ConfirmedInDecisionLog` (plan file D0 row carries the confirmation) |
| DW-0.7 | `engram.proto` (Ingest w/ required event_id, Search) compiles via codegen in CI; generated Go builds | COVERED | `TestDW_0_7_GeneratedProtoBuildsAndCarriesEventID` (imports `api/engrampb`, constructs `IngestRequest.EventId`); CI codegen + `git diff --exit-code` step |
| DW-0.8 | ID & idempotency contract (content key, doc-_id, supersedes, claim-first ledger — D11/D13) documented in code + reflected in structs | COVERED | `TestDW_0_8_ContentKeyAndDocIDContract` (determinism, canonical form, valid_at sensitivity), `TestDW_0_8_StructsCarryChainAndLedgerFields` |
| DW-0.9 | three live-cluster spikes green vs pinned cluster; findings logged | COVERED | integration tag: `TestDW_0_9a_OpTypeCreate409Flow`, `TestDW_0_9b_RRFPipelineShape`, `TestDW_0_9c_FilteredKNNRecall`; findings written to `docs/spikes/phase-0-findings.md` |

Edge cases (plan): `TestApplyRejectsWrongClusterVersion` (≠3.1 → clear pinned-version error, one code path — D14); `TestValidateInfoRejectsDimMismatch` (embedder dim ≠ template dim → startup error).

**All items COVERED:** YES

## Design Decisions

### Boundary check (ca-architecture-boundaries)
Dependency arrows all point inward: `internal/memory` (record types + ID contract — the entities) imports only stdlib. `store`, `retrieval`, `ingest`, `embed` define consumer-side interfaces over `memory` types; no OpenSearch/vendor type appears in any public signature. `cmd/*` and the future OpenSearch impl are the outer ring. Actors: write-path (ingest/store seams), read-path (retrieval), eval (harness) — separate packages per actor, so Phase-1 retrieval changes can't break Phase-2 write seams.

### Design: index-template source of truth (design-it-twice)
1. **A** — Go structs serialized to JSON at apply time. 2. **B** — JSON files as source, `go:embed`ed; tests parse the same bytes the cluster receives. 3. **C** — templates created imperatively by client code.

| Criterion | A | B | C |
|-----------|---|---|---|
| Drift risk vs what cluster gets | med (marshal quirks) | **none** | high |
| Reviewability / diffability | poor | **excellent** | poor |
| Testability of DW-0.3 field asserts | ok | **direct** | hard |

**Choice: B.** JSON under `internal/store/templates/`, embedded; the apply tool PUTs the exact bytes; DW-0.3 tests parse the same embed. Sacrifice: no compile-time typing of mappings — recovered by the contract tests.

### Design: apply-templates "script" (design-it-twice)
1. **A** — bash + curl. 2. **B** — Go command `cmd/engram-apply-templates` sharing `internal/store` apply logic.

**Choice: B.** Version guard (D14), idempotency detection, and clear errors are logic that deserves unit tests (httptest); bash hides it. `PUT _index_template` / `PUT _search/pipeline` are natively idempotent; index creation is HEAD-then-create. Re-run reports `unchanged` per resource. The same `Apply` function will back testcontainers setup later. HTTP via stdlib `net/http` — no OpenSearch client dependency in Phase 0 (keeps vendor types out, per standards; Phase 1 picks the client behind `Store`).

### Design: codegen strategy
`buf` v2 (pinned, via `go run github.com/bufbuild/buf/cmd/buf@<ver>`) with `protoc-gen-go` / `protoc-gen-go-grpc` invoked as pinned `go run` local plugins — zero host installs, identical in CI. Generated code checked in under `api/engrampb/`; CI regenerates and `git diff --exit-code`s (DW-0.7). `google.golang.org/protobuf` + `grpc` become real go.mod deps (generated code imports them).

### Design: eval harness + seed gold set
`internal/eval`: `Harness` loads a JSON gold set `{corpus[], queries[{id,text,expected_ids[],split}]}` and computes recall@k, MRR, nDCG@k against any `retrieval.Retriever` (metrics added now — pure math, saves Phase-1 churn; DW-1.3 needs them). `cmd/engram-eval` runs it with a `NullRetriever` (returns nothing) → recall 0, proving the harness runs (DW-0.5). Seed set generated deterministically by `cmd/engram-goldgen` (checked-in generator + checked-in output `eval/goldset/seed.json`); split assigned by hash of query id at generation time and **frozen in the file** — pre-registered (DW-1.3 holdout ≥ 50 seeded now).

### Interface depth check (aposd)
`Store` is 9 methods — pinned verbatim by the plan's Produces contract (append + create/update + outbox 3 + ledger 2 + repair scan); each hides an OpenSearch protocol detail (op_type=create, seq_no guards, scan-and-claim, ledger claim). `Retriever`/`Extractor`/`Reconciler` are 1 method, `Embedder` 2 (`Embed` + `Info` for the D15 model/dim/revision startup validation). Common case (append an event; search) needs one call and no OpenSearch knowledge.

### Other pinned choices
- Content key / doc-id canonical form: `sha256` over fields joined with `0x1F` (unit separator — unambiguous vs plain concat); `valid_at` canonicalized `UTC RFC3339Nano` so identical concurrent extractions collide (D11). Documented in `internal/memory/ids.go`.
- kNN space_type `innerproduct` (BGE-M3 vectors are L2-normalized; Faiss engine has no cosinesimil) — documented in template + doc.go; spike 3 exercises it live.
- Episodic mapping adds `dead_lettered`/`dead_letter_reason` alongside the three planned outbox fields — the `DeadLetter` seam needs a durable marker.
- Semantic mapping carries `invalidated_tx_at` (the plan's documented deviation for auditable in-place close).
- Dev cluster: `scripts/dev-cluster.sh` runs `opensearchproject/opensearch:3.1.0` single-node, security plugin disabled, 1 GiB heap (fits 4 GiB podman VM), port 9200.

## Prerequisites
- [x] Go toolchain (1.26.3) and network (Go proxy reachable)
- [x] Container runtime: podman (machine starting); docker absent — noted in Gaps
- [x] Plan + research docs present with backbone config values
- [x] OpenSearch 3.1.0 image pull + container boot — completed; container `engram-dev-os` up on :9200, `version.number=3.1.0` verified

## Recommendation
**BUILD.** Greenfield, contracts fully specified by the plan; the only reality gap (docker→podman) has a drop-in resolution. If the 3.1.0 image cannot be pulled or the podman VM cannot boot, DW-0.4 (live half) and DW-0.9 fail-fast → report per dispatch instructions.

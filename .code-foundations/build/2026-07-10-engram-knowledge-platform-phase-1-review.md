# Review: Phase 1 - Proto contract & Backend seam

## Executed Results (Step 0)
- `go build ./...` → success, no errors.
- `make test` (`go test ./...`) → all packages `ok` (or `[no test files]`), no failures.
- `make lint` (`go vet ./...` + revive) → clean, no findings.
- `make proto` (`./scripts/codegen.sh`, buf via `go run`) → run twice back to back; `api/engrampb/engram.pb.go` and `api/engrampb/engram_grpc.pb.go` were byte-identical before run 1, after run 1, and after run 2 (diffed with `diff`, all three identical). `git diff --stat api/engrampb` shows the same uncommitted diff throughout (211-line proto + generated code are staged as working-tree changes, not yet committed — consistent with the DW-1.1 note that `proto-check` only passes once committed).
- Targeted suite: `go test ./internal/server/ ./internal/engramclient/ ./internal/mcp/ -run 'DW_1|ValueOneof' -v -count=1` → `TestDW_1_1_KnowledgeRPCsUnimplementedByDefault` (6 subtests), `TestValueOneofRoundTrip` (5 subtests), `TestDW_1_3_NoArxivFieldNamesInProto`, `TestDW_1_2_KnowledgeStubsReturnNotImplemented` (6 subtests) — all PASS.

## Requirement Fulfillment

### DW-1.1
PREMISE:  `api/proto/engram.proto` defines all 6 RPCs (KnowledgeIngest, KnowledgeSearch, KnowledgeCollections, KnowledgeDelete, CreateCollection, UpdateCollection) + their messages; `make proto` regenerates `api/engrampb` with no diff drift (deterministic across two runs).
EVIDENCE: `api/proto/engram.proto:72,76,81,86,90,94` (six `rpc` declarations on the `Engram` service); messages at `api/proto/engram.proto:344-425` (`KnowledgeIngestRequest/Response`, `KnowledgeSearchRequest/Response`, `KnowledgeCollectionsRequest/Response`, `KnowledgeDeleteRequest/Response`, `CreateCollectionRequest/Response`, `UpdateCollectionRequest/Response`); generated code at `api/engrampb/engram_grpc.pb.go:169-228` (`engramClient.KnowledgeIngest/KnowledgeSearch/KnowledgeCollections/KnowledgeDelete/CreateCollection/UpdateCollection`); test at `internal/server/knowledge_proto_test.go:29-36` (compile-time `_ = engrampb.EngramClient.Knowledge*` proof).
TRACE:    `go run buf generate` (via `make proto`) reads `api/proto/engram.proto` → emits `api/engrampb/*.pb.go` → ran twice, `diff` on both generated files between run 1 and run 2 output nothing (`RUN2 IDENTICAL TO RUN1`), and run 1's output was also identical to the pre-run working-tree copy → deterministic, no drift.
VERDICT:  PASS

### DW-1.2
PREMISE:  `mcp.Backend` interface lists all 6 knowledge methods; the repo compiles (`go build ./...`) with stub/Unimplemented returns — including the `engramclient` adapter and the `fakeBackend` test double.
EVIDENCE: `internal/mcp/mcp.go:115-142` (`Backend` interface, six `Knowledge*`/`CreateCollection`/`UpdateCollection` methods added alongside `Ingest`/`Search`/`Status`); `internal/engramclient/knowledge.go:22-49` (adapter methods, each returning `errKnowledgeUnimplemented`); `internal/mcp/mcp_test.go:40-66` (`knowledgeStubs` embedded by `fakeBackend`, `var _ Backend = (*fakeBackend)(nil)` compile-time proof); `internal/mcp/budget_test.go:15-18,371-374` (`fixedHitsBackend` and `recordingKBackend` both embed `knowledgeStubs` too).
TRACE:    `go build ./...` → exit 0 (ran directly, output "Go build: Success"). `TestDW_1_2_KnowledgeStubsReturnNotImplemented` calls all 6 `engramclient.Client{}` knowledge methods on a zero-value client (no dial) → each returns a non-nil error containing "not implemented" and the op name (e.g. `engramclient: KnowledgeIngest: not implemented (knowledge platform Phase 6)`) → test PASS, no panic.
VERDICT:  PASS

### DW-1.3
PREMISE:  filter/sort/value shapes are generic (no arXiv field names in the proto), enforced by a test/guard.
EVIDENCE: `api/proto/engram.proto:285-298` (`Predicate{field, op, oneof value}`, `SortKey{field, order}` — no hardcoded field names); `internal/server/knowledge_proto_test.go:144-162` (`TestDW_1_3_NoArxivFieldNamesInProto`, regex-scans `engram.proto` for `arxiv|papers?|abstracts?|authors?|categor(y|ies)|doi|journal`, word-bounded with underscore-splitting so snake_case leaks are caught too).
TRACE:    Test reads `api/proto/engram.proto` line by line, applies the banned-word regex to each line → no match found → test PASS. Manually re-scanned the proto text above (lines 1-444) for the same terms; none present outside comments describing the generic-surface design (e.g. line 62-66 explicitly documents "no arXiv field names" as a design intent, itself matched and excluded correctly since the regex is checked and passes, i.e. that comment line does not contain the banned words literally — confirmed by re-running the regex against the file content directly).
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] DW-1.1 — `TestDW_1_1_KnowledgeRPCsUnimplementedByDefault` (ran in Step 0, PASS) + manual `make proto` x2 determinism check (recorded above).
- [x] DW-1.2 — `TestDW_1_2_KnowledgeStubsReturnNotImplemented` (ran in Step 0, PASS) + `go build ./...` (ran, PASS).
- [x] DW-1.3 — `TestDW_1_3_NoArxivFieldNamesInProto` (ran in Step 0, PASS).
- [x] Edge case: Value oneof round-trip — `TestValueOneofRoundTrip`, 5 subtests (scalar string/number/bool, range both bounds, range open bound), all PASS.
- [x] Edge case: op/order enums — asserted via generated code inspection (`PredicateOp_PREDICATE_OP_TERM/RANGE/PREFIX`, `SortOrder_SORT_ORDER_ASC/DESC` all present in `api/engrampb/engram.pb.go:53-109`) and exercised by `TestValueOneofRoundTrip`'s use of `PREDICATE_OP_TERM/RANGE/PREFIX`.
- [x] Edge case: Provenance.roles — confirmed field present (`api/proto/engram.proto:172`, generated getter `api/engrampb/engram.pb.go:656`); no dedicated round-trip test exists for this field, but this is a plain scalar/repeated-string field on a proto3 message with no custom logic — protobuf's own marshal/unmarshal guarantees the round-trip (the same generated-code path exercised and proven correct by `TestValueOneofRoundTrip` for the sibling `Predicate`/`Range` messages). No DW item nor edge case demanded a dedicated roles-round-trip test, so this is not a coverage gap against the stated requirements.
- [x] Test coverage matches the stated 100% level for the phase's DW items and listed edge cases — all covered by executed tests above.

## Dead Code
None found. The diff to `internal/mcp/mcp.go`, `internal/mcp/mcp_test.go`, `internal/mcp/budget_test.go`, `internal/engramclient/knowledge.go`, and `internal/server/knowledge_proto_test.go` is purely additive (new types, new interface methods, new stub bodies, new tests); no unreachable code, no leftover debug statements, no unused imports (`go vet` and `go build` both clean).

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | Phase 1 is a proto contract + stub seam; no goroutines, shared state, or async paths introduced. |
| Error Handling | PASS | `engramclient` stubs return descriptive errors instead of panicking or silently no-op'ing; verified `&engramclient.Client{}` (zero-value, no dial) does not panic on any of the 6 calls (`TestDW_1_2...` ran clean). |
| Resources | N/A | No file handles, connections, locks, or caches introduced in this phase — stubs allocate nothing. |
| Boundaries | PASS | Traced the oneof discriminator logic in `TestValueOneofRoundTrip` (`internal/server/knowledge_proto_test.go:136-139`): `gotScalar, gotRange := got.GetScalar() != nil, got.GetRange() != nil; gotRange != wantRange \|\| gotScalar == wantRange` — for every one of the 5 cases (3 scalar arms, 2 range arms) this correctly asserts exactly one arm is populated after marshal/unmarshal; traced the "range open upper bound" case (`Lte` unset) specifically: marshaled `Range{Gte: 10}` → unmarshaled back to `Range{Gte: 10, Lte: nil}`, `proto.Equal` PASS, confirming the open-interval boundary (nil `Lte`) survives round-trip rather than being coerced to a zero value. |
| Security | N/A | No auth/authz logic touched in this phase (Provenance.roles is a plain repeated-string field on the wire; the proto's own doc-comment at `engram.proto:170-172` notes it is populated server-side from the verified token only in later phases — not something Phase 1's proto-contract scope can enforce or that any DW item asked this phase to enforce). |

## Loaded-Skill Criteria
N/A — no skills loaded (dispatch prompt had no `## Additional Skills` section).

## Notes (non-blocking)
- `docs/code-standards.md` also shows a diff in `git status`/`git diff --stat` but was not in the "Files to review" list for this phase — not assessed here.
- The generated `api/engrampb` files and `api/proto/engram.proto` are currently uncommitted working-tree changes; `make proto-check` (which does `git diff --exit-code`) would therefore currently fail until these are committed — this matches the DW-1.1 premise's own caveat ("`make proto-check` only passes once changes are committed") and is not a defect.
- `internal/server/server.go`'s `Server` struct embeds `engrampb.UnimplementedEngramServer` (pre-existing from Phase 0), which is what makes the six new RPCs return `codes.Unimplemented` for free — no new server-side wiring was needed or added in this phase, consistent with the Phase-1 stub posture.

**Verdict: PASS**

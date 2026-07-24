# Review: Phase 1 - Proto & query-builder foundation

## Executed Results (Step 0)
- Test suite: `go test -count=1 ./...` → all packages `ok` (0 failures). Re-ran targeted `-v -run` for every `TestDW_1_*` across all packages plus `TestBuildQueryMemoryPathByteIdenticalWhenSortNil` — all PASS.
- Typecheck/Build: `go build ./...` → exit 0, no errors.
- Vet: `go vet ./...` → exit 0, no errors.
- Lint: `make lint` (`go vet ./...` + `revive -set_exit_status`) → exit 0, no findings.
- Proto drift: `make proto` (buf lint + generate) → `codegen: ok`. Ran twice back-to-back and diffed the resulting `git diff -- api/engrampb/engram.pb.go` against itself both times — byte-identical, confirming codegen is idempotent and the checked-in (currently uncommitted, pre-commit-gate) `engram.pb.go` is exactly what `buf generate` produces from `api/proto/engram.proto` today — no hand-edits, no staleness. (Note: `git diff --exit-code -- api/engrampb` against `HEAD` is non-empty only because this phase's proto changes are legitimately uncommitted at review time — that diff is the new feature, not drift.)

## Requirement Fulfillment

### DW-1.1
PREMISE:  `KnowledgeHit` message exists in the proto and regenerated Go; `KnowledgeSearchResponse` uses `repeated KnowledgeHit` + `total`; `KnowledgeSearchRequest` carries `offset` + `full_body`.
EVIDENCE: `api/proto/engram.proto:519-543` (KnowledgeHit, KnowledgeSearchResponse), `api/proto/engram.proto:500-517` (KnowledgeSearchRequest.offset/full_body); regenerated Go at `api/engrampb/engram.pb.go:2416-2431` (KnowledgeHit struct), `:2499-2507` (KnowledgeSearchResponse.Hits []*KnowledgeHit + Total int64), `:2312-2331` (KnowledgeSearchRequest.Offset int32 + FullBody bool).
TRACE:    `TestDW_1_1_KnowledgeHitProtoRoundTrip` (internal/engramclient/knowledgehit_test.go) constructs a `KnowledgeSearchRequest{Offset:32, FullBody:true}` and a `KnowledgeSearchResponse{Hits:[]*KnowledgeHit{...Fragments:[...]}, Total:1234}` → `proto.Marshal`/`Unmarshal` round trip → asserts `proto.Equal` and each field individually (offset, full_body, collection, fragments, total) survives. Ran: PASS.
VERDICT:  PASS

### DW-1.2
PREMISE:  `CollectionSpec` (proto + both Go structs `internal/mcp/mcp.go` and `internal/knowledge/knowledge.go`) carries `fragment_size`, `number_of_fragments`, `highlight_pre_tag`, `highlight_post_tag`, round-tripping through `collectionSpecProto`/`collectionSpecFromProto` without loss; absent sizing → global fallback (240/3), not zero.
EVIDENCE: `internal/mcp/mcp.go:141-146`, `internal/knowledge/knowledge.go:61-68` (fields on both domain structs); `internal/knowledge/knowledge.go:85-94` (`FragmentSizing()`); two independent translation pairs — `internal/engramclient/knowledge.go:211-249` (mcp↔proto) and `internal/server/knowledge.go:398-438` (knowledge↔proto).
TRACE:    Client pair — `TestDW_1_2_CollectionSpecSizingRoundTrip` drives an `mcp.CollectionSpec` with all four fields set through `CreateCollection`→`collectionSpecProto`→wire, then wire→`collectionSpecFromProto`→`KnowledgeCollections`, asserting `reflect.DeepEqual` equality: PASS. `TestDW_1_2_CollectionSpecZeroSizingStaysZeroOnWire` proves unset stays `0`/`""` on the wire (fallback is NOT applied at translation time): PASS. Server pair — `TestDW_1_2_ServerCollectionSpecSizingRoundTrip` drives the same round trip through `collectionSpecFromProto`(decode)/`collectionSpecProto`(encode) on the server side, plus the unset-stays-zero-on-wire case: PASS. Fallback — `TestDW_1_2_FragmentSizingFallback` (internal/knowledge/collectionspec_test.go) covers both-absent→(240,3), both-set→passthrough, one-set-one-absent→independent fallback per knob, and negative→(240,3) (never zero-size): PASS, 5/5 subtests.
VERDICT:  PASS

### DW-1.3
PREMISE:  `buildQuery` takes an options struct; a golden-body matrix — {BM25Only, KNNOnly, hybrid} × {filters nil/set} × {sort nil/set} — asserts BOTH byte-identical body AND identical `usePipeline`; all memory-path call sites compile and their tests pass unchanged.
EVIDENCE: `internal/retrieval/opensearch.go:657-684` (`queryOpts` struct), `:702-740` (`buildQuery(opts queryOpts)`); golden matrix at `internal/retrieval/buildquery_golden_test.go:14-108`; call sites at `internal/retrieval/opensearch.go:549-552` (memory tier path) and `internal/retrieval/knowledge.go:118-121` (knowledge path).
TRACE:    Independently verified the golden matrix is NOT circular: I extracted the pre-refactor `buildQuery` function verbatim from `git show HEAD~1:internal/retrieval/opensearch.go` (8-positional-parameter form) into a standalone Go program and ran it against the exact same 15 input cells the golden test uses (BM25Only×{filters,sort}×{nil,set} = 4, KNNOnly×same = 4, Hybrid×same = 4, plus 3 edge cells: hybrid-no-vector, knn-no-vector, empty-text-filter-only). Every body string and `usePipeline` bool produced by the ACTUAL pre-refactor code matched the golden `want`/`wantPipeline` values byte-for-byte across all 15 cells. Ran `TestDW_1_3_BuildQueryGoldenMatrix`: PASS, 15/15 subtests (all three modes × filters nil/set × sort nil/set are present: e.g. `Hybrid/filters/sort` asserts both the fused `hybrid.queries` body AND `wantPipeline:true`). Ran the pre-existing `TestBuildQueryMemoryPathByteIdenticalWhenSortNil` (updated only to use `queryOpts{}` instead of positional args, same assertions): PASS, 6/6 subtests. Both memory-path call sites (`tierRetriever.search`, and the pre-existing `KnowledgeRetriever.Search`) compile and their surrounding package tests (`internal/retrieval`, `internal/server`, `internal/engramclient`, `internal/cli`) all pass under `go test -count=1 ./...`.
VERDICT:  PASS

### DW-1.4
PREMISE:  `make proto` codegen is clean (`git diff --exit-code -- api/engrampb` passes after regen) and `go build ./...` + `go vet ./...` pass.
EVIDENCE: `Makefile:36-41` (proto/proto-check targets), `scripts/codegen.sh`.
TRACE:    Ran `make proto` → `codegen: ok`. Ran it a second time and diffed the working-tree `api/engrampb/engram.pb.go` diff-against-HEAD before and after the second run: identical byte-for-byte, proving regeneration is a no-op against the current tree (no drift, no hand-edits). `go build ./...` → exit 0. `go vet ./...` → exit 0. (The literal `git diff --exit-code -- api/engrampb` against `HEAD` is non-zero only because this phase's proto additions are legitimately uncommitted — that is the feature diff, not codegen drift.)
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] DW-1.1 → `TestDW_1_1_KnowledgeHitProtoRoundTrip` (internal/engramclient/knowledgehit_test.go), ran in Step 0.
- [x] DW-1.2 → `TestDW_1_2_CollectionSpecSizingRoundTrip`, `TestDW_1_2_CollectionSpecZeroSizingStaysZeroOnWire` (engramclient), `TestDW_1_2_ServerCollectionSpecSizingRoundTrip` (server), `TestDW_1_2_FragmentSizingFallback` (knowledge), all ran in Step 0.
- [x] DW-1.3 → `TestDW_1_3_BuildQueryGoldenMatrix` (retrieval) + `TestBuildQueryMemoryPathByteIdenticalWhenSortNil` (retrieval), ran in Step 0.
- [x] DW-1.4 → build/lint/proto are process checks, not unit-testable; covered by recorded observed behavior (`go build`, `go vet`, `make proto` + idempotency diff), all executed directly in Step 0.
- [x] Test coverage matches the stated 100% level — every DW item traces to either an automated test or (for DW-1.4, a non-testable build/codegen check) directly observed command output.

## Dead Code
None found. `queryOpts.offset/fragmentSize/numberOfFragments/highlightPreTag/highlightPostTag` are declared but not yet read inside `buildQuery` — this is explicitly documented as intentional forward-declaration for Phases 2–3 ("INERT this phase... zero-value-off by design"), not unreachable code or a leftover; Go's compiler does not flag unused struct fields, and `go vet`/`revive` both pass clean. No unused imports, no commented-out blocks, no debug statements found in any reviewed file.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | No concurrent code paths touched by this phase's diff (buildQuery/queryOpts and the CollectionSpec/KnowledgeHit translation helpers are pure, synchronous, non-shared-state functions). |
| Error Handling | PASS | `collectionSpecFromProto` (server) rejects an empty `spec.name` (`internal/server/knowledge.go:399-401`); translation functions elsewhere are lossless field copies with no fallible step. Traced: a `CreateCollectionRequest{Spec:{Name:""}}` → `status.Error(InvalidArgument, "spec.name is required")`, not a panic or silent no-op. |
| Resources | N/A | No new file handles, connections, locks, or caches introduced. |
| Boundaries | PASS | Traced the adversarial boundary explicitly named in the DW/edge-case list: `FragmentSize/NumberOfFragments <= 0` (zero AND negative) → `FragmentSizing()` returns `(240, 3)`, never a zero-size or zero-count fragment (`internal/knowledge/knowledge.go:85-94`, confirmed by `TestDW_1_2_FragmentSizingFallback`'s "negative values fall back" subtest). `offset == 0` (proto default) is semantically identical to "page one" by construction — no separate fallback needed, and none was claimed. |
| Security | N/A | No new untrusted-input parsing paths in this diff; existing filter/predicate validation (`predicatesFromProto`, `sortKeysFromProto`) is unchanged. |

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| aposd-designing-deep-modules | Interface depth / no over-specialization: `buildQuery(opts queryOpts)` | PASS | Single-parameter interface hiding per-mode BM25/kNN/hybrid JSON construction; the struct-of-options replaces an 8-positional-parameter signature specifically to keep the interface manageable as Phase 2/3 add fields — matches the "push complexity into the module" direction, not shallow pass-through. |
| aposd-designing-deep-modules | Silent Failure red flag: `offset`/`full_body` accepted on the wire but not yet acted on | PASS (no violation) | Traced: a caller setting `offset=10` today gets page-one results with no error and no field signaling non-application. This is NOT a hidden-error case (no failure occurred to hide) — it is a documented, in-progress multi-phase rollout (`internal/server/knowledge.go:177-181`: "req.offset/full_body are accepted but inert until then"; `internal/retrieval/opensearch.go:667-671` mirrors this on the query-builder side), with paging wiring explicitly scoped to a later phase. No DW item in this phase claims offset is honored, so there is no unmet requirement, and no error state is being suppressed. |
| cc-routine-and-class-design | Parameter count (buildQuery refactor target) | PASS | `buildQuery` now takes exactly 1 parameter (the `queryOpts` struct) — the refactor's entire purpose was collapsing what would have grown past 8 positional params into a parameter object, the textbook fix the checklist prescribes. |
| cc-routine-and-class-design | Functional cohesion: `collectionSpecProto`/`collectionSpecFromProto` (both pairs) | PASS | Each function performs exactly one operation (encode-or-decode one spec), verb-named accordingly; no "and"/"then" compound responsibility. |
| cc-routine-and-class-design | Inheritance / LSP | N/A | No inheritance introduced anywhere in this diff (Go has none; no interface-substitution concerns raised by the changes). |

## Notes (non-blocking)
- `int32(spec.FragmentSize)` / `int32(spec.NumberOfFragments)` conversions in both `collectionSpecProto` implementations would silently truncate a caller-supplied value larger than `math.MaxInt32`. Not a listed edge case and not exercised by any DW item or the plan's edge-case list — noting for awareness only, not a blocker.
- `internal/knowledge/knowledge.go`'s `HighlightPreTag`/`HighlightPostTag` have no `FragmentSizing`-style fallback resolver (by design, per the code comment: "no global marker default exists") — consistent with the DW-1.2 premise, which only requires fallback for `fragment_size`/`number_of_fragments`.

## Issues (if FAIL)
None.

**Verdict: PASS**

# Review: Phase 2 - Graph dev-embedder clustering

## Executed Results (Step 0)
- Unit suite (graph pkg): `go test ./internal/graph/... -v -count=1` → 52/52 PASS (0.013s)
- Full unit suite: `make test` → all packages ok, no failures
- Typecheck/vet: `go vet ./...` (part of `make lint`) → clean, no output
- Lint: `make lint` (revive v1.12.0, `-set_exit_status`) → exit code 0, no findings
- Build: `go build ./...` → Success
- Integration (graph pkg): `ENGRAM_OPENSEARCH_URL=http://localhost:9200 go test -tags=integration -v ./internal/graph/...` → 20/20 PASS (3.278s), against live OpenSearch 3.1.0 (podman `engram-dev-os`, already running)
- Full integration suite: `make integration` → exit code 0, 272 `--- PASS`, 0 `--- FAIL` across spike/store/retrieval/server/eval/worker/ingest/experience/graph/reindex
- Fail-before/pass-after proof: temporarily neutralized the embedding-swap line in `internal/graph/store.go` (`if false && s.nameKeyedDedup {...}`), re-ran DW-2.1 and DW-2.2 → both FAIL as expected (embed_sim=-0.554 / 0.0126, both below split threshold, entity count 4 not 3); reverted the edit — `git diff internal/graph/store.go` empty afterward, and the full re-run reconfirmed 52/52 PASS.
- Side effects: `make integration` auto-regenerated `docs/eval/dashboard.md` and `eval/gate-runs/history.jsonl` (expected per dispatch note, not part of this phase, left as-is).

## Requirement Fulfillment

### DW-2.1
PREMISE:  under the deterministic dev embedder, the same entity name in two facts with different fact context → exactly ONE entity doc (fails before the fix, passes after — verify by reverting the embedding-swap line).
EVIDENCE: internal/graph/store.go:260-274 (`embed`, the swap); internal/graph/store_test.go:140-168 (`TestDW_2_1_NameKeyedDedup_SameEntityDifferentFactContextMerges`)
TRACE:    Two `UpsertMention` calls with Name="Acme Corp", differing Context, under `newNameKeyedTestStore` (WithNameKeyedDedup) → `embed()` ignores `m.Context`, embeds `normalizeName("Acme Corp")` for both → identical vectors → `Decide` scores combined=1.0 (embed_sim≈1.0, lex_sim=1.0) → merge=true, same entity ID both times → `CountEntities` = 1. Confirmed via live test run (see log: `merge=true ... combined=1 embed_sim=1.0000000000000002`). Fail-before reproduced by reverting the swap: embed_sim drops to -0.554/0.0126 (orthogonal hash vectors), decision flips to `Merge: false`, count would be 2.
VERDICT:  PASS

### DW-2.2
PREMISE:  integration — a 3-fact A→B→C chain answers a 2-hop connect-the-dots query returning the C node from an A-anchored query, driven through the real graph Stage.Process (not hand-built ids).
EVIDENCE: internal/graph/opensearch_integration_test.go:232-312 (`TestDW_2_2_Integration_NameKeyedDedup_TwoHopThroughRealStage`)
TRACE:    `stage.Process` ingests fact1 (A works_at B) then fact2 (B located_in C) — B is independently mentioned twice (once as fact1's object, once as fact2's subject), through the real `graph.Stage` (not hand-built `UpsertMention`/`UpsertEdge` calls with pre-known ids). Under `newLiveNameKeyedStore` (name-keyed dedup), B's second mention merges into the same entity id → 3 entities total (A,B,C) → `Expander` (depth 2) + `acl.Filter` + `retrieval.Search` anchored on a seed hit for fact "A works_at B" → hop-2 traversal reaches C, found in `hits[].Fields["object"]=="C"`. Ran live against OpenSearch 3.1.0: `--- PASS: TestDW_2_2_Integration_NameKeyedDedup_TwoHopThroughRealStage (0.09s)`. Reverting the embedding-swap line reproduces the pre-fix failure: entity count = 4 (B fragmented), test fails at the count assertion before even reaching the traversal check.
VERDICT:  PASS

### DW-2.3
PREMISE:  entity count stable across 10 re-ingests (DW-6.3 preserved).
EVIDENCE: internal/graph/store_test.go:194-224 (`TestDW_2_3_NameKeyedDedup_RepeatedIngestEntityCountStable`, name-keyed mode); internal/graph/store_test.go:62-97 (`TestDW_6_3_RepeatedIngestEntityCountStable`, default mode, untouched); internal/graph/opensearch_integration_test.go:142-168 (`TestDW_6_3_Integration_RepeatedIngestEntityCountStable`, live cluster)
TRACE:    Identical mention re-ingested 10x under `newNameKeyedTestStore` → iteration 0 creates, iterations 1-9 each re-embed the same normalized name → identical vector → combined=1.0 → merge into the same id every time → `CountEntities("t1")` = 1 after all 10. Ran live: `--- PASS: TestDW_2_3_NameKeyedDedup_RepeatedIngestEntityCountStable (0.00s)`. The pre-existing (non-name-keyed) DW-6.3 test and its live-cluster counterpart both still pass unmodified, confirming no regression to the default path's stability guarantee.
VERDICT:  PASS

### DW-2.4
PREMISE:  `TestHomonym_SameNameDifferentEntityStaysUnmerged` passes UNCHANGED (confirm the TEST SOURCE is not modified — git diff dedup_test.go); a test asserts private vs shared same-name entities stay separate.
EVIDENCE: internal/graph/dedup_test.go:83-108 (test source, byte-identical); `git diff HEAD~1 HEAD -- internal/graph/dedup_test.go` → empty; `git log --oneline -- internal/graph/dedup_test.go` → last touched at commit 4e22b24 (phase 6), untouched since; internal/graph/store_test.go:226-260 (`TestScopePreFilter_PrivateAndTeamSameNameStaySeparate`)
TRACE:    `dedup_test.go` has zero diff against both `HEAD~1` and the entire history back to phase 6 — the Phase-2 commit (4c1973a) touches only store.go/graph.go/stages_graph.go/store_test.go/opensearch_integration_test.go per `git show --stat HEAD`. Ran the homonym test directly: `--- PASS: TestHomonym_SameNameDifferentEntityStaysUnmerged (0.00s)`, `j.calls=0` (no judge invoked, embedding distance alone resolved it), `LexSim=1.0` as asserted. Separately, `TestScopePreFilter_PrivateAndTeamSameNameStaySeparate` upserts Scope="private" then Scope="team" mentions with the IDENTICAL name AND identical context (which would otherwise score embed_sim=lex_sim=1.0, clearing the merge threshold) — the (tenant,scope) pre-filter in `UpsertMention` (store.go:140-149, `if e.Scope != m.Scope { continue }`) excludes the cross-scope candidate before `Decide` ever sees it → `dec.Merge=false`, two distinct entity ids, `CountEntities`=2. Ran live: PASS. (The codebase's canonical scope constants are `acl.ScopePrivate`="private" and `acl.ScopeTeam`="team" — there is no literal "shared" scope value in this codebase; the test's private-vs-team boundary is the concrete instance of the "private+shared" boundary named in the dispatch.)
VERDICT:  PASS

### DW-2.5
PREMISE:  ACL still honored at expansion (no regression to DW-6.4); make integration green.
EVIDENCE: internal/graph/expand_test.go (`TestDW_6_4_ExpansionACLBlocked`, unmodified — not in this phase's file list); `make integration` full run
TRACE:    `TestDW_6_4_ExpansionACLBlocked` ran unmodified in both the unit suite (`go test ./internal/graph/...`) and the `-tags=integration` suite — `--- PASS: TestDW_6_4_ExpansionACLBlocked (0.00s)` in both. `make integration` (the full command from the dispatch's "How to run") exited 0 with 272 `--- PASS` / 0 `--- FAIL` across every integration-tagged package, including `internal/graph`.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] DW-2.1: `TestDW_2_1_NameKeyedDedup_SameEntityDifferentFactContextMerges` (ran, PASS) + `TestNameKeyedDedup_IsOptInOnly_DefaultStoreStillFragments` pins the opt-in-only contract
- [x] DW-2.2: `TestDW_2_2_Integration_NameKeyedDedup_TwoHopThroughRealStage` (ran live, PASS)
- [x] DW-2.3: `TestDW_2_3_NameKeyedDedup_RepeatedIngestEntityCountStable` (ran, PASS) + pre-existing `TestDW_6_3_*` (both unit and live) still pass
- [x] DW-2.4: `TestHomonym_SameNameDifferentEntityStaysUnmerged` (ran unchanged, PASS) + `TestScopePreFilter_PrivateAndTeamSameNameStaySeparate` (ran, PASS)
- [x] DW-2.5: `TestDW_6_4_ExpansionACLBlocked` (ran unit + integration, PASS) + full `make integration` green
- [x] Dirty/boundary test present: `TestUpsertMention_RequiresTenantAndName` (pre-existing, still exercised); the DW-2.1 fail-before/pass-after revert constitutes an additional dirty-path proof specific to this phase.
- [x] Coverage matches the stated level (100% of DW items, ≥1 dirty test) — met.

## Dead Code
None found. Reviewed the full diff (`git show HEAD -- internal/graph/store.go internal/graph/graph.go cmd/engram-server/stages_graph.go`) for unused imports, unreachable code, debug prints, commented-out blocks — none present. `go vet` and `revive` both clean.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | `nameKeyedDedup` is a `bool` set once at construction time (`WithNameKeyedDedup` in `NewStore`'s option loop) and never mutated afterward; every `Store` method only reads it. No new shared mutable state introduced. `MemBackend` already guards its map with `sync.Mutex`, untouched by this phase. |
| Error Handling | PASS | `embed()`'s degraded-not-fatal contract preserved for both modes (nil embedder or empty text → nil vector, dedup falls back to lexical-only); traced `normalizeName("")` (an all-whitespace or empty Name) → `strings.TrimSpace(text) == ""` → returns nil, same degrade path as the Context case, not a crash. `UpsertMention`'s existing tenant/name validation (store.go:125-127) still runs before `embed` is called, guarding against the boundary case. |
| Resources | N/A | No new file handles, connections, locks, or caches introduced; `WithNameKeyedDedup` only flips a bool. |
| Boundaries | PASS | Traced empty `Scope` on both sides (`e.Scope != m.Scope` when both are `""`, e.g. legacy pre-scope entities) → filter passes both through unchanged (`"" != ""` is false) — matches existing behavior for any entity that predates scope tagging, not a new gap this phase introduces. Traced a candidate list where every entity is scope-excluded → `candidates` stays an empty slice → `Decide` hits its existing `len(candidates)==0` branch → new entity, no panic. |
| Security | N/A — untrusted input surface (mention Name/Context) is unchanged by this phase; the existing tenant/name validation and ACL re-verification at expansion (`acl.Filter`, unmodified) are the enforcement points, both exercised green above. |

## Loaded-Skill Criteria

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-routine-and-class-design | Parameter count (`NewStore`) | PASS | `NewStore(backend, dedup, embedder, logger, opts ...StoreOption)` = 5 counted params (variadic counts as 1) — well under the 7-max threshold; the functional-options pattern keeps the dev-only knob out of the required-argument list entirely. |
| cc-routine-and-class-design | Functional cohesion (`embed`, `WithNameKeyedDedup`) | PASS | `embed` does exactly one operation ("produce the dedup-similarity embedding for this mention") with a single internal branch on which text to embed — not two operations glued together. `WithNameKeyedDedup` does exactly one thing (set a flag), matching the existing `ExpanderOption`/`BackendOption` convention cited in its own doc comment. |
| cc-routine-and-class-design | No inheritance/LSP concerns | N/A | This change adds no new types with inheritance-like relationships; `StoreOption` is a plain function type (the standard Go functional-options idiom), not a class hierarchy. |
| aposd-designing-deep-modules | Interface depth preserved | PASS | `Store`'s five public intent methods (`UpsertMention`, `UpsertEdge`, `GetEntity`, `Neighbors`, `CountEntities`) are unchanged; the new complexity (name-vs-context embedding choice, scope pre-filter) is hidden entirely inside `UpsertMention`/`embed`, invisible to every existing caller that doesn't pass `WithNameKeyedDedup`. Depth check: interface surface unchanged, hidden detail added (the embedder-input decision + scope-exclusion), common case (production, no opts) unchanged in complexity. |
| aposd-designing-deep-modules | Information hiding / no leakage | PASS | The (tenant,scope) merge boundary is enforced entirely inside `UpsertMention` (store.go:140-149) before `Decide` ever sees excluded candidates — `Candidate`'s struct shape and `Deduper.Decide`'s signature are untouched, so the scoping knowledge is not duplicated into the dedup algorithm. Traced the alternative (teaching `Decide` about `Scope`) and confirmed the chosen design avoids that information leakage. |
| aposd-designing-deep-modules | No silent failure | PASS | The dev/e2e-only gate is not silent: `wireGraph` logs `"graph dedup: name-keyed mention embedding enabled..."` when it activates (stages_graph.go:66), and every `UpsertMention` call logs its full `Decision` (name, merge, combined/embed/lex scores, reason) regardless of mode — an operator can observe which path fired, not just infer it. |

## Notes (non-blocking)
- The dispatch's DW-2.4 wording ("private vs shared") doesn't match a literal scope value in this codebase (`acl.ScopePrivate`/`acl.ScopeTeam`/`acl.ScopeOrg` — no `"shared"`); `TestScopePreFilter_PrivateAndTeamSameNameStaySeparate` covers the private-vs-non-private boundary the requirement is evidently pointing at (worst-case: private vs. team, same tenant). Flagging for awareness, not treating as a gap, since the concrete private/non-private separation is exactly what's being asked for and it's tested and green.
- `TestDW_6_2_TwoHopConnectTheDots` (pre-existing, hand-built ids) and `TestDW_2_2_Integration_...` (new, real-Stage-driven) both exist side by side; the new test's doc comment explicitly notes the old one "only ever mentions B ONCE, so it never actually exercises the fragmentation bug" — a good catch by the implementer, and the reason DW-2.2 asked for a Stage-driven test specifically.
- Log volume: every `UpsertMention` call emits an `INFO` line with the full decision breakdown; fine for dev/test, worth keeping an eye on if this fires at production ingest volume, but that log statement predates this phase (unchanged) and is out of scope here.

## Issues (if FAIL)
None.

**Verdict: PASS**

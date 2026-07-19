# Discovery + Design: Phase 3 - Graph rebuild command

## Files Found
- `internal/graph/graph.go`, `store.go`, `stage.go`, `opensearch.go` — the T4 graph tier (Phase 1/2 already landed: `Store.CloseEdge`, `FactOutcome`-driven `Stage.Process`, `graph.Apply` for idempotent template+index creation, `graph.MemBackend` for unit tests).
- `internal/store/facts.go` — `OpenSearchStore`'s read paths over the semantic index (`Candidates`, `LiveSuperseders`, `LiveByContentKey`, `ChainVersions`, …). All bounded by `limit`/`size`; **no unbounded/paginated scan exists**.
- `internal/store/robustness_test.go` — the hermetic `httptest.Server` pattern for unit-testing `OpenSearchStore` methods without a live cluster (`errorServer`/`indexNotFoundServer`). This is the precedent I follow for `facts_test.go`.
- `internal/graph/opensearch_integration_test.go`, `internal/store/apply_integration_test.go` — `//go:build integration` precedent (real OpenSearch, run via `make integration`, not `make test`).
- `cmd/engram-apply-templates/main.go` — the CLI shape precedent (flag parsing, `os.Exit(1)` on error).
- `scripts/backfill-reextract-rtd.sh` — a destructive-op script precedent (snapshot-before-mutate); not directly reusable (bash, not Go), but confirms the project's "operator confirms explicitly" convention.
- `cmd/engram-server/stages_graph.go` (`wireGraph`) — the production wiring shape for `graph.Store`/`graph.Stage`/dedup/judge/embedder, mirrored (trimmed) for the standalone command.
- `internal/graph/templates/{entities,edges}.json`, `EntityIndex`/`EdgeIndex`/`EntityTemplateName`/`EdgeTemplateName` constants in `opensearch.go`.

## Current State
- `Edge.InvalidAt` is now set correctly by the live write path (Phase 2). But every edge written **before** that fix stays live forever — nothing before this phase can remove a zombie edge already sitting in the `engram-graph-edges-*` index.
- `graph.Apply` is idempotent-create only (PUT template, PUT index, tolerate `resource_already_exists_exception`). It never drops anything — there is no "wipe" primitive anywhere in the graph package.
- No live-fact scan exists in `internal/store`. The closest are `LiveSuperseders`/`LiveByContentKey` (both `size`-bounded, no pagination) and `ChainVersions` (size 1000, unbounded-but-capped, single query).

## Gaps
- Need a paginated live-fact scan (`internal/store/facts.go`).
- Need an index-drop primitive for the graph tier (`internal/graph`).
- Need an orchestration entry point that sequences drop → recreate → scan → replay, and a CLI wrapper with a required `-confirm` flag.
- **Architecture boundary constraint** (`graph.go`'s package doc): `internal/graph` may import `internal/{auth, memory, embed, retrieval, worker, store-independent HTTP helpers}` but explicitly **not** `internal/store`. So the live-fact scan cannot be consumed directly by `internal/graph` — it needs a narrow interface (`graph.FactScanner`) that the command's wiring layer (`cmd/engram-graph-rebuild`, which is free to import both) adapts `store.OpenSearchStore.ScanLiveFacts` into. This mirrors the existing `worker.SweepStore` narrow-interface pattern used for the same reason (repair sweep reads facts.go methods through an interface, not a direct import).

## Code Standards
No `docs/code-standards.md` found in the repo. Followed the codebase's own observed conventions instead: doc-comment-first exported symbols (revive's exported-comment lint rule enforces this per the Makefile), narrow interfaces at package boundaries (`worker.SweepStore`, `graph.Backend`), idempotent-by-construction writes/closes, `httptest.Server`-backed hermetic unit tests for OpenSearch HTTP surfaces, `//go:build integration` for real-cluster tests, `fmt.Errorf("<pkg>: <verb>: %w", …)` error wrapping with package-name prefix.

## Test Infrastructure
- Unit: plain `go test ./...` (the `make test` gate). `internal/store` unit-tests OpenSearch HTTP behavior via `httptest.Server` fakes (no live cluster needed) — see `robustness_test.go`. `internal/graph` unit-tests orchestration/business logic via `graph.MemBackend` (see `lifecycle_test.go`, `store_test.go`).
- Integration: `//go:build integration`, run via `make integration` against a live OpenSearch (confirmed reachable at `localhost:9200` in this environment, but **not** part of the `make test` gate this dispatch specifies). I will add these for real end-to-end confidence but they are supplementary, not gating.

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-3.1 | Command drops+recreates graph indices, then replays every live semantic fact through the graph stage | COVERED | `TestRebuild_DropsBeforeScanning` (order), `TestRebuild_ReplaysEveryScannedFact` (all pages processed) — `internal/graph/rebuild_test.go`; `TestRebuild_Integration_DropAndReplay` — `internal/graph/rebuild_integration_test.go` (real OpenSearch) |
| DW-3.2 | After a rebuild against a store containing a superseded fact, no edge for the superseded version is `Live()` | COVERED | `TestRebuild_SupersededFactNeverGetsAnEdge` (unit: scanner returns only the live successor, mirroring `ScanLiveFacts`'s live-only contract; assert no edge exists for the predecessor triple) — `internal/graph/rebuild_test.go`; `TestRebuild_Integration_ZombieEdgeRemovedByDrop` — `internal/graph/rebuild_integration_test.go` (seeds a real zombie edge doc directly, asserts the index-level DROP erases it) |
| DW-3.3 | Command refuses to run without `-confirm` | COVERED | `TestRun_RefusesWithoutConfirm` — `cmd/engram-graph-rebuild/main_test.go` |
| DW-3.4 | Command never writes to episodic/semantic indices (asserted in test) | COVERED | `TestRun_NeverWritesEpisodicOrSemantic` — hermetic `httptest.Server` that `t.Fatal`s on any non-`_search` request to an episodic/semantic path — `cmd/engram-graph-rebuild/main_test.go`. Structurally reinforced: `graph.FactScanner`'s one method (`ScanLiveFacts`) is read-only by construction — the orchestration has no writer-shaped dependency to misuse |
| DW-3.5 | Re-running is idempotent — a second run produces the same graph | COVERED | `TestRebuild_IdempotentAcrossTwoRuns` (drives `graph.Rebuild` twice against the same fixed fact set + a fresh `MemBackend` each time via the fake dropper, asserts identical entity/edge sets) — `internal/graph/rebuild_test.go` |

**All items COVERED:** YES

## Design Decisions

**1. `graph.FactScanner` interface, not a direct `internal/store` import.** `internal/graph`'s package doc explicitly excludes `internal/store` from its allowed import set (the same boundary Phase 1 protected by siting `FactOutcome` in `internal/ingest` rather than `internal/worker`). `internal/graph/rebuild.go` declares:
```go
type FactScanner interface {
    ScanLiveFacts(ctx context.Context, tenantID string, cursor FactCursor) ([]memory.SemanticFact, FactCursor, error)
}
type FactCursor struct { CreatedAtUnixMilli int64; ContentKey string }
```
`cmd/engram-graph-rebuild` (free to import both packages, being the wiring layer — same convention as `cmd/engram-server/stages_graph.go`) adapts `store.OpenSearchStore.ScanLiveFacts` to this shape. Rejected alternative: relax the boundary and let `graph` import `store` directly — rejected because it would be a new, undocumented dependency arrow the package doc explicitly disclaims, for a benefit (avoiding one small adapter type) that doesn't outweigh the architectural cost.

**2. `IndexDropper` interface for the wipe half**, implemented in production by `OpenSearchIndexDropper` (`internal/graph/rebuild_opensearch.go`, DELETE-then-`Apply`, tolerating 404 via the existing `isIndexNotFound` helper) and faked in unit tests. Keeping this behind an interface (rather than hard-wiring `graph.Rebuild` to HTTP calls) is what makes `TestRebuild_DropsBeforeScanning` and `TestRebuild_IdempotentAcrossTwoRuns` unit-testable without a live cluster.

**3. Live-fact scan pagination key: `(created_at, content_key)`, not `_id`.** Verified via OpenSearch docs (search, this session): the `_id` metadata field has no doc-values, so sorting on it forces the whole field into heap-resident fielddata — an operational hazard OpenSearch's own docs warn against, and something no other scan in this codebase does (`graph.ScanEntities`/`ScanEdges` sort on a real *mapped* `id` field stored in `_source`; semantic facts have no such field, and the plan's file scope forbids editing the semantic template to add one). `created_at` (mapped `date`) plus `content_key` (mapped `keyword`) as a compound `search_after` sort is the built-from-what's-already-mapped equivalent, using epoch-millis in `search_after` (not the RFC3339Nano-with-nanoseconds Go's default JSON marshaling would produce, which risks failing OpenSearch's default `date` parser). Documented as a deliberate, bounded trade in `facts.go`'s doc comment: a true tie on both fields is possible in principle (not primary-key-unique) but vanishingly unlikely in practice, and the command is safely re-runnable if it ever matters.

**4. Replay always emits `ingest.OpAdd` with a nil `Predecessor`.** The scan's own live-only filter (`invalid_at`/`expired_at` both unset — the same `liveFilterClauses()` every other read path in `facts.go` already uses) is what gives DW-3.2 its guarantee: a superseded fact is never in the scanned set, so it is never replayed, so no edge is ever written for it post-rebuild — not merely closed-but-present, genuinely absent. This is simpler and stronger than reconstructing supersession history during replay (which would require re-deriving `Predecessor` links the scan doesn't carry) and matches the plan's edge case "entity merges must be re-derived through `UpsertMention`, not assumed" — every entity/edge in the rebuilt graph is freshly resolved through the real dedup path, nothing is copied forward.

**5. Tenant scope of the wipe vs. the replay.** Per the plan's literal `Produces` line, `-tenant` scopes the **replay** (`ScanLiveFacts` is tenant-filtered), but "drops and recreates the graph indices" (DW-3.1's own wording, plural/index-level) is a whole-index operation — `EntityIndex`/`EdgeIndex` are not partitioned per tenant, and OpenSearch has no way to "drop only tenant A's docs" via `DELETE /index` (that would require `_delete_by_query`, a different and non-idempotent-in-the-same-way operation). This is consistent with the plan's explicit constraint ("Not in production; no users; no backward compatibility required") — flagging it here rather than quietly narrowing it, since in a genuinely multi-tenant deployment this would wipe every other tenant's graph too until each is separately rebuilt. Not treated as a defect to fix silently; if it matters later it's a plan-level scope question, not a phase-3 implementation choice.

**6. No integration test drives `OpenSearchIndexDropper.DropAndRecreate` against the real cluster.** Discovered during implementation, not anticipated at discovery time: this environment's dev OpenSearch (`localhost:9200`, reachable) already carries live data under the FIXED production index names (`engram-graph-entities-000001`: 142 docs, `engram-graph-edges-000001`: 71 docs — presumably from Phase 1/2's own integration runs). `OpenSearchIndexDropper` is deliberately hardcoded to those same fixed names (matching `graph.Apply`'s own convention and DW-3.1's literal "drops and recreates the graph indices" — there is only one set of graph indices, not one per tenant). An integration test exercising it for real would therefore destroy that existing data as a side effect of running `make test`/`make integration` — unacceptable. I verified the counts were unchanged (142/71) after running the one integration test I did add. DW-3.1/DW-3.2/DW-3.5 stay COVERED via the unit-test suite's interface-segregated fakes (`fakeDropper`), which is the same standard `robustness_test.go` already applies to OpenSearch-HTTP-shaped logic elsewhere in this codebase. What I DID integration-test for real: `store.ScanLiveFacts`'s (created_at, content_key) `search_after` pagination and live-filter exclusion (`internal/store/facts_integration_test.go`, scratch-indexed, confirmed not to touch the production-named indices) — the plan's own stated "Uncertainty: fact-scan pagination shape" concern, and the one piece of new logic a hermetic httptest fake can't fully de-risk (real OpenSearch date/search_after parsing semantics).

## Prerequisites
- [x] Phase 2 complete (`graph.Store.CloseEdge`, `FactOutcome`-driven `Stage.Process`) — confirmed via `git log` (commit `89e9300`) and by reading the current `stage.go`/`store.go`.
- [x] `graph.Apply`, `graph.MemBackend`, `graph.NewStore`/`NewStage` all exist and are usable as-is.
- [x] Dependencies available (`internal/embed`, `internal/ingest`, `internal/memory` all present and unchanged).

## Recommendation
BUILD. Both plan assumptions verified true (see below) — no UPDATE_PLAN needed.

**Assumption verification:**
- "The graph can be wiped and rebuilt with no permanent loss (derived data)" — CONFIRMED. `EntityIndex`/`EdgeIndex` carry no data any other tier depends on; every entity/edge is re-derivable from live semantic facts via `UpsertMention`/`UpsertEdge`, which is exactly this phase's replay mechanism.
- "No live-fact scan exists in `internal/store` and one must be written" — CONFIRMED. Grepped `internal/store/facts.go` and every caller; the only paginated scans anywhere in the codebase are `graph.ScanEntities`/`ScanEdges` (a different package, different index, different sort key available).

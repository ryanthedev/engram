# Discovery + Design: Phase 3 - Durable + complete telemetry

## Files Found
- `internal/telemetry/gauges.go` — `Gauges` struct + `NewGauges`; 10 existing DW-7.8 instruments, including `GateAdmitRate/QuarantineRate/RejectRate` whose descriptions currently say only "fraction of experience-gate verdicts that were X" (no restart caveat — misstates durability).
- `internal/telemetry/recorder.go` — `Recorder` with nil-safe duck-typed `*Source` interfaces (`OutboxSource`, `DLQSource`, `RepairSource`, `GateSource`, `CostSource`) satisfied structurally by concrete production types; `Poll` reads each configured source once.
- `internal/telemetry/telemetry.go`, `internal/telemetry/recorder_test.go` — Provider/scrape test harness (`newTestProvider`, `scrape`, `gaugeValue`, `wantGauge`) I reuse for new gauge assertions.
- `cmd/engram-server/main.go` — wires `telemetryProvider`/`gauges`/`recorder`; `recorder.Gate = experienceStore` already; `wireGraph` currently returns only `error` (no `*graph.Store` handed back), so main.go has no graph source to wire in.
- `cmd/engram-server/stages_graph.go` / `stages_experience.go` — `wireExperience` already returns `*experience.Store` (precedent for `wireGraph` to follow); both apply the T3/T4 templates and build the `Store`/`OpenSearchBackend` pair.
- `internal/experience/store.go` — `Store.VerdictCounts()` reads three `atomic.Int64` process-lifetime counters; `Backend` interface has no count method at all (writes/reads only: `PutAdmitted`, `GetAdmitted`, `SearchAdmitted`, `ScanPrunable`, `SoftExpire`, `PutQuarantine`, `ListQuarantine`, `GetQuarantine`, `DeleteQuarantine`). `Admit`'s `Reject` branch (line ~143-147) logs and increments the in-process counter only — confirmed no write to any backend on reject, so there genuinely is no durable reject trace.
- `internal/experience/opensearch.go` — `OpenSearchBackend` over `admittedIndex`/`quarantineIdx`; has its own self-contained `osDo`/`osJSON`/`osSearchHits`/`osDecodeSource` copies (package doc precedent: "internal/experience carries its own thin copies" rather than importing `internal/store`). **No `isIndexNotFound` guard anywhere in this file** — Phase 1's file scope was `internal/store/**` only, so experience's reads were never guarded.
- `internal/graph/store.go` — `Backend.CountEntities(ctx, tenantID) (int, error)` is per-tenant only (confirmed: filters `tenant_id` term in both `MemBackend` and `OpenSearchBackend` implementations); no all-tenant variant exists.
- `internal/graph/opensearch.go` — `OpenSearchBackend.CountEntities` (line 170) runs an unguarded `_count` — same gap as experience: no `isIndexNotFound` check, would error (not 0) against a missing index today.
- `internal/store/opensearch.go` (line 198) / `internal/store/counts.go` — the reference `isIndexNotFound` + `_count`-with-guard pattern Phase 1 established; I mirror its shape (not its code — self-contained copies per package, matching the existing convention) in `graph` and `experience`.
- `internal/store/robustness_test.go` — the `httptest.NewServer`-based fake-error-server pattern (`errorServer`, `indexNotFoundServer`) used for DW-1.x; I reuse the same technique for DW-3.4 without needing a live cluster.
- `internal/experience/opensearch_integration_test.go`, `internal/graph/opensearch_integration_test.go` — live-cluster integration test precedent (`testutil.OpenSearchURL()`, `CreateScratchIndex`, `sanitize(t.Name())`) for the DW-3.3 `/metrics` integration assertion.
- `Makefile` — `integration` target's package list does **not** include `internal/telemetry` today.

## Current State
Gate-verdict-rate gauges (`engram_gate_admit_rate` / `_quarantine_rate` / `_reject_rate`) are computed purely from `experience.Store`'s in-process atomic counters (finding #3) — a restart zeroes them regardless of what's actually durable in OpenSearch. There is no graph gauge registered anywhere (finding #4): `graph.Store.CountEntities` exists and is exercised by DW-6.3 tests directly, but nothing wires it into `telemetry.Recorder`/`Gauges`, and it is per-tenant only (no cluster-wide reading is even possible with the current signature). Neither `internal/experience` nor `internal/graph`'s OpenSearch backends guard `_count` reads against `index_not_found_exception` — that guard exists only in `internal/store` (Phase 1's scope).

## Gaps
- Plan assumes "Phase 1's index_not_found-as-empty (already committed)" covers the new gauges' reads, but Phase 1's file scope was `internal/store/**` exclusively — `internal/graph` and `internal/experience` were never touched. Each package keeps its own self-contained OpenSearch helper copies (documented convention, not an oversight), so the guard must be duplicated into both packages' `opensearch.go`, not imported from `internal/store`.
- `Backend` interfaces in both `experience` and `graph` have no count-style method for the data Phase 3 needs (admitted/quarantine document counts; all-tenant entity count) — new interface methods are required, not just new call sites.
- `wireGraph` (`cmd/engram-server/stages_graph.go`) returns only `error`; it must return `(*graph.Store, error)` (mirroring `wireExperience`'s existing shape) so `main.go` has a handle to wire into the recorder.
- `Makefile`'s `integration` target omits `internal/telemetry`, so a genuine live-cluster `/metrics` integration test (DW-3.3) would not run under `make integration` unless the package list is extended.

## Code Standards
`docs/code-standards.md` conventions applied: wrapped errors with `%w` and package-prefixed messages (`"experience: counting admitted inventory: %w"`); `context.Context` first parameter; consumer-defined narrow interfaces (`GateInventorySource`, `GraphSource` duck-typed, not exported from the producing package); OpenSearch types never appear in `telemetry` package's public signatures (only `int64`/`int`/`error`); table-driven tests where the existing file already uses that shape (`recorder_test.go`'s fake-source table); ≥1 dirty test per phase (DW-3.4 satisfies this); `log/slog` via `Recorder.logger()`, unchanged.

## Test Infrastructure
- Unit-level: `internal/telemetry/recorder_test.go`'s `newTestProvider`/`scrape`/`gaugeValue`/`wantGauge` harness — extend with new fake sources (`fakeGateInventory`, `fakeGraph`) matching the existing `fakeGate`/`fakeCost` shape.
- Dirty/robustness: `internal/store/robustness_test.go`'s `httptest.NewServer` fake-error-server pattern, reproduced independently in `internal/experience` and `internal/graph` (each package is self-contained, matching their existing `osJSON` duplication) to prove a missing index reads 0 through the new count methods without needing a live cluster.
- Live-cluster integration (DW-3.3): mirrors `internal/experience/opensearch_integration_test.go` / `internal/graph/opensearch_integration_test.go`'s `scratchBackend` + `testutil.CreateScratchIndex`/`DeleteIndex` pattern; new file `internal/telemetry/metrics_integration_test.go` (`//go:build integration`, `package telemetry`) builds real `experience.Store`/`graph.Store` over scratch indices, wires a `Recorder`, polls once against the live dev cluster (localhost:9200, confirmed up/green), and scrapes `/metrics` via `httptest`. Requires adding `internal/telemetry` to the `Makefile` `integration` target's package list (currently omitted) — a minimal, necessary companion edit within this phase's intent (DW-3.3 explicitly requires this to run, DW-3.5 requires `make integration` green).

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-3.1 | Durable experience-inventory gauges reflect admitted/quarantined `_count` after a simulated restart, not 0; in-process rate gauges' descriptions state they reset on restart | COVERED | `TestDW_3_1_InventoryCountsSurviveRestart` (fresh `*experience.Store` over a `MemBackend` pre-seeded with admitted+quarantined docs — the "restart" — asserts `InventoryCounts` reflects real data while `VerdictCounts()` reads 0,0,0); `TestDW_3_1_GateRateGaugeDescriptionsStateResetOnRestart` (scrapes `/metrics` HELP lines for `engram_gate_admit_rate`/`_quarantine_rate`/`_reject_rate`, asserts "resets on restart" substring) |
| DW-3.2 | A graph entity gauge is registered and its value tracks the (all-tenant) entity count (increments as entities are added) | COVERED | `TestDW_3_2_GraphEntityGaugeTracksCount` (fake `GraphSource` returning increasing counts across two polls; scrape asserts `engram_graph_entity_count` moves) |
| DW-3.3 | `/metrics` integration assertion — both the durable inventory gauge(s) and the graph gauge appear with non-garbage values | COVERED | `TestIntegration_MetricsExposesDurableInventoryAndGraphGauges` (live cluster: scratch experience+graph indices, admit one experience + upsert one entity, real `Recorder.Poll`, scrape, assert all three new gauge names present with values matching what was written) |
| DW-3.4 | Dirty test — gauge poll against an empty/missing index yields 0, not an error or crash | COVERED | `TestDW_3_4_MissingIndexCountsZero` in `internal/experience` (httptest fake server returning `index_not_found_exception` for `CountAdmitted`/`CountQuarantine`) and `internal/graph` (`CountAllEntities` against the same fake-server pattern) — both assert `(0, nil)`, not an error |
| DW-3.5 | `make integration` green; no regression to existing telemetry (DW-7.8) gauges | COVERED | Existing `TestRecorder_GaugesMoveOnPoll` / `TestRecorder_NilSourcesSkipped` / `TestRecorder_SourceErrorDoesNotBlockOthers` re-run unchanged (anchored); full `make integration` run after implementation |

**All items COVERED:** YES

## Design Decisions

### Design: Durable experience-inventory + graph-entity telemetry sources

**Approaches Considered**
1. **Duck-typed Store-level source methods (chosen)** — add `InventoryCounts(ctx) (admitted, quarantined int64, err error)` to `*experience.Store` and `CountAllEntities(ctx) (int, error)` to `*graph.Store`, each delegating to two new `Backend` interface methods (`CountAdmitted`/`CountQuarantine` on `experience.Backend`; `CountAllEntities` on `graph.Backend`). `telemetry.Recorder` gains two new duck-typed interfaces (`GateInventorySource`, `GraphSource`) satisfied structurally, exactly like `GateSource`/`OutboxSource` today.
2. **Duck-type directly against the concrete `OpenSearchBackend` types**, bypassing `Store` — `telemetry.Recorder.Graph` would hold a `*graph.OpenSearchBackend` instead of `*graph.Store`. Rejected: it breaks the existing convention that business-logic-adjacent counters (`GateSource` is `*experience.Store.VerdictCounts`, not the backend) are exposed through the deep module, not its storage port; it would also require `main.go` to keep a second handle (the raw backend) around alongside the `Store` it already threads through `wireGraph`/`wireExperience`, for no benefit.
3. **A single combined `TelemetrySource` interface** bundling gate-inventory + graph-entity + verdict-rate into one wide interface. Rejected: `Recorder`'s whole design is one-interface-per-gauge-family, independently nilable (a unit test wiring only `Gate` must not need to also stub `GraphSource`); a combined interface would force every test and every wiring site to satisfy fields it doesn't care about, and would preclude `experience.Store` and `graph.Store` (two unrelated concrete types) both partially satisfying it.

**Comparison**

| Criterion | 1 (Store-level, chosen) | 2 (raw backend) | 3 (combined interface) |
|---|---|---|---|
| Interface simplicity | One new narrow method per source | Same method count, but couples Recorder to storage internals | Fewer named interfaces, but each is wide |
| Information hiding | Backend storage strategy (OpenSearch vs Mem) stays hidden behind Store | Leaks which concrete backend type is in play to `main.go`/tests | Hides nothing extra; conflates unrelated concerns |
| Caller ease of use | `main.go` already holds `*graph.Store`/`*experience.Store` post-wiring; zero new plumbing | Requires threading a second value out of `wireGraph` | Every partial implementer must stub unrelated methods |
| Consistency with existing code | Matches `GateSource`/`RepairSource` precedent exactly | Diverges from that precedent for no stated reason | No precedent in this codebase for combined telemetry interfaces |

**Choice:** 1 (Store-level duck-typed sources)
Rationale: matches the codebase's existing `telemetry.Recorder` idiom exactly (narrow interface per gauge family, satisfied structurally by the domain-layer type that already owns the concept), keeps storage strategy hidden inside `Backend`, and requires the smallest, least surprising diff to `main.go` (one new return value from `wireGraph`, one new field wired on `recorder`).

**Depth Check**
- Interface methods added: `GateInventorySource` (1 method), `GraphSource` (1 method) — mirrors the existing one-method-per-source shape.
- Hidden details: which OpenSearch queries compute "live" vs "all" counts; the `index_not_found_exception` guard; the difference between `experience.Backend`'s MemBackend/OpenSearch implementations.
- Common-case complexity for `main.go`: two one-line assignments (`recorder.GateInventory = experienceStore`, `recorder.Graph = graphStore`) — no new caller-side branching.

### Design: All-tenant graph entity count (new method vs. reusing `CountEntities("")`)

**Approaches Considered**
1. **New dedicated method `CountAllEntities(ctx) (int, error)` (chosen)** on `Backend` and `Store`, alongside the existing per-tenant `CountEntities(ctx, tenantID)`.
2. **Overload the empty-tenantID sentinel** on the existing `CountEntities(ctx, tenantID)` — mirrors `internal/store/counts.go`'s `countTenant` convention ("an empty tenantID matches all").

**Comparison**

| Criterion | 1 (new method) | 2 (blank-tenant sentinel) |
|---|---|---|
| Interface simplicity | +1 method | 0 new methods |
| Safety / surprise | No behavior change to any existing call | Silently redefines what `CountEntities(ctx, "")` returns — today it returns ~0 (a `term` filter on an empty string matches nothing); an existing caller relying on that would now get "everything" instead, a latent correctness change disguised as an addition |
| Self-documentation | Name states scope explicitly (`CountAllEntities`) | Caller must know the "" convention from a doc comment; the plan's own wording ("an all-tenant count is a source edit") reads as adding a capability, not redefining one |

**Choice:** 1 (new dedicated method)
Rationale: the plan's constraint is additive ("no public signature change" is the spirit carried over from Phase 1/2's contracts; here it's an explicit new capability, not a redefinition), and reusing the blank-tenant sentinel would silently change `CountEntities("")`'s existing meaning — an invisible behavior change is worse than one extra interface method. `CountAllEntities` is also the plan's own emphasis: "the DW-6.3 stability signal," a purpose distinct enough from per-tenant counting to earn its own name.

**Depth Check**
- Interface methods: `Backend` grows from 6 to 7 methods (`CountEntities` unchanged, `CountAllEntities` added); `Store` grows one delegating method.
- Hidden details: the OpenSearch query drops the `tenant_id` term filter but keeps the `expired_at`-exists exclusion (live-only, matching `CountEntities`'s semantics minus the tenant scope) — callers never see the query shape.
- Common case: `Store.CountAllEntities(ctx)` — one call, no tenant plumbing.

## Prerequisites
- [x] Required files exist (`internal/telemetry/{gauges,recorder}.go`, `internal/experience/{store,opensearch}.go`, `internal/graph/{store,opensearch}.go`, `cmd/engram-server/{main,stages_graph,stages_experience}.go`)
- [x] Dependencies available (OTel/Prometheus toolchain already vendored; dev cluster up/green at localhost:9200 per task context)
- [x] Phase 1's `isIndexNotFound` pattern available as a reference shape in `internal/store/opensearch.go` (not imported — each package keeps a self-contained copy, per existing convention)
- [ ] `Makefile`'s `integration` target does not yet include `internal/telemetry` — will be added as part of this phase's implementation (required for DW-3.3/DW-3.5)

## Recommendation
BUILD.

What actually needs to be done, in order:
1. `internal/experience`: add `isIndexNotFound` (opensearch.go); add `CountAdmitted`/`CountQuarantine` to `Backend`, `MemBackend`, `OpenSearchBackend`; add `Store.InventoryCounts`.
2. `internal/graph`: add `isIndexNotFound` (opensearch.go); add `CountAllEntities` to `Backend`, `MemBackend`, `OpenSearchBackend`; guard the existing `CountEntities` with the same helper (same file, same failure mode, zero behavior change on the happy path — closing an identical latent gap while already touching this exact code); add `Store.CountAllEntities`.
3. `internal/telemetry/gauges.go`: add three gauges (`engram_experience_admitted_count`, `engram_experience_quarantined_count`, `engram_graph_entity_count`); correct the three `Gate*Rate` descriptions to state "in-process since start; resets on restart."
4. `internal/telemetry/recorder.go`: add `GateInventorySource`/`GraphSource` interfaces + `Recorder.GateInventory`/`Recorder.Graph` fields; extend `Poll` with two new nil-safe, error-logged blocks.
5. `cmd/engram-server/stages_graph.go`: `wireGraph` returns `(*graph.Store, error)`.
6. `cmd/engram-server/main.go`: capture the returned `*graph.Store`; wire `recorder.GateInventory = experienceStore` and `recorder.Graph = graphStore`.
7. `Makefile`: add `./internal/telemetry` to the `integration` target's package list.
8. Tests per the DW table above, plus the existing telemetry/experience/graph suites re-run to confirm anchoring.

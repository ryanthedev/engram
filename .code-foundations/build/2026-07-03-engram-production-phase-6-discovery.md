# Discovery + Design: Phase 6 - Incremental Graph (T4)

## Files Found
- `internal/retrieval/retriever.go`, `opensearch.go`, `acl_test.go` — `PostHook`/`TierSource` seams already live and exercised (`MultiRetriever.RegisterPostHook`); `Search` re-authorizes post-hook additions through `filterAuthorized` BEFORE returning (ordering-correct — confirmed by `TestPostHookAdditionsReauthorizedWithoutLosingTopK` and `TestRegisteredSeamsReceiveIdentityAndAreACLFiltered`). No retrieval-package edits needed.
- `internal/acl/{acl,edge,filter}.go` — `Record`, `Enforcer.Authorize`, `WriteGuard` seam; scopes `{private,team,org}`.
- `internal/worker/worker.go` — `Stage` interface + `RegisterStage`, at-least-once/idempotent contract, runs after facts land, before ledger-complete.
- `internal/experience/**` + `cmd/engram-server/stages_experience.go` — the closest structural precedent for this phase: `Gatekeeper`/`RuleGatekeeper`/`HTTPGatekeeper` (fail-closed judge, config-selected), `Store` as sole gated writer, `Tier` as `TierSource`, `OpenSearchBackend` with self-contained `osDo/osJSON/osSearchHits/osDecodeSource` helpers (internal/store's are unexported), own `Apply` + embedded templates, own `cmd/engram-server/stages_*.go` wiring file called from `main.go`.
- `internal/ingest/extraction.go`, `rule.go` — `memory.SemanticFact{Subject,Predicate,Object,Statement}` triples; `RuleExtractor` directive syntax (`fact: s | p | o`) — the deterministic fixture path e2e/tests will reuse.
- `internal/embed/{embedder,fake}.go` — `Embedder` interface; `FakeEmbedder` with a fixture-key table so two different mention *contexts* can be forced to embed far apart even when their surface name is identical (needed for the homonym edge case).
- `internal/store/{templates,apply,acledges}.go` — index-template + idempotent-`Apply` + deterministic-doc-id conventions to mirror.
- `cmd/engram-server/main.go` — composition root; Phases 4/5 both edited it despite it not being listed in their `File scope` (it is the shared wiring point, touched by necessity every phase). This phase does the same for `wireGraph`.
- `e2e/{registry,harness,scenarios_experience,scenarios_acl}.go` — `RegisterScenario` extension point (zero core edits), MCP/CLI harness driving the compose stack; hit shape exposes `source`/`fields_json` (`internal/mcp/mcp.go`, `internal/server/server.go`).
- `Makefile` — `integration` target lists packages explicitly; `internal/graph/` is not yet in it (per the dispatch note, must add).
- No `internal/graph/**` exists yet — greenfield within this phase's scope.

## Current State
Phases 3–5 are complete and committed. The four registration seams (`RegisterStage`, `RegisterTier`, `RegisterPostHook`, `RegisterWriteGuard`) are real and already exercised by ACL + experience. Extraction never emits a separate "entity mention" record — only Subject/Predicate/Object triples on `SemanticFact`. Per the dispatch's environment note, Phase 6 derives entity mentions from those triples rather than editing `internal/ingest` (keeps this phase's file scope self-contained, matching how Phase 5 kept its own OpenSearch backend rather than reusing `internal/store`'s unexported helpers).

## Gaps
- Plan's file-scope literal for the worker-stage file says `cmd/engramd/stages_graph.go`; the binary is actually `cmd/engram-server` (Phases 3–5 all used this path). The dispatch prompt's own `File scope` line already corrects this to `cmd/engram-server/stages_graph.go` — no plan conflict, just the plan body's stale filename (pre-existing typo carried from the skeleton plan's original binary name). No update-plan needed.
- No conflict found between "IN: extraction stage extended to emit entity mentions" and the file-scope restriction to `internal/graph/**` — the environment note explicitly resolves this by deriving mentions from existing fact triples, so no `internal/ingest` edit is required. Recommendation: proceed as designed, note it here rather than filing UPDATE_PLAN.

## Code Standards
`docs/code-standards.md` (present, though written pre-code and generic): errors wrapped with `%w`, sentinel errors for control flow, `context.Context` first param, OCC (`if_seq_no`/`if_primary_term`) for shared-state writes, deep modules / consumer-defined interfaces, table-driven tests with a build-tag-gated live-cluster integration tier, `log/slog` structured logs. All followed; the codebase's actual established convention (seen directly in Phases 3–5) additionally establishes: package-private OpenSearch HTTP helpers per capability package (not reused across packages), a `Backend` interface + `Store` deep-module split, fail-closed judges config-selected by an empty/non-empty `-*-url` flag, and `RegisterScenario`/`RegisterStage`/`RegisterPostHook` as the only cross-phase touch points.

## Test Infrastructure
- Unit tests: table-driven, in-memory backends (`MemBackend` pattern from `experience/store.go`).
- Integration tests: `//go:build integration`, live OpenSearch at `ENGRAM_OPENSEARCH_URL` (dev cluster confirmed running at `:9200`, pinned 3.1.0), run via `make integration` (package list will gain `./internal/graph/`).
- E2E: `//go:build e2e`, `RegisterScenario` from an `init()`, drives the compose stack via MCP/CLI (`make e2e`).
- Perf-flavored latency assertion (DW-6.5) belongs in an `internal/graph` integration test (live cluster, real `MultiRetriever` + `GraphExpander` wired) rather than editing the out-of-scope `cmd/engram-perf` tool.

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-6.1 | single-episode ingest adds/updates entities + edges with zero recompute of existing graph state | COVERED | `TestDW_6_1_IngestTouchesOnlyItsOwnEntities` (store_test.go): ingest episode A, snapshot entity/edge docs; ingest unrelated episode B; assert A's docs byte-identical (untouched) |
| DW-6.2 | 2-hop connect-the-dots e2e query on the fixture KB returns the documented answer path (A→B→C) | COVERED | `TestDW_6_2_TwoHopConnectTheDots` (opensearch_integration_test.go, live cluster, real retriever+expander) + e2e scenario `graph/connect-the-dots` (scenarios_graph.go, full MCP pipeline) |
| DW-6.3 | entity count stable (±0) across 10 re-ingests of the same fact set; dedup decisions logged | COVERED | `TestDW_6_3_RepeatedIngestEntityCountStable` (store_test.go): 10x `UpsertMention` of an identical mention set, assert `CountEntities` unchanged after the first; dedup decisions asserted via a logging spy |
| DW-6.4 | expansion honors ACL — dirty test: a hit reachable only through an unauthorized edge is absent | COVERED | `TestDW_6_4_ExpansionACLBlocked` (expand_test.go, unit, via `retrieval.MultiRetriever` wired with a real `acl.Filter` + `GraphExpander`): an edge whose owner is unreachable is expanded but dropped by the retriever's post-hook re-authorization; e2e scenario `graph/acl-blocked-edge` |
| DW-6.5 | p95 search latency with expansion enabled ≤ 250 ms in the perf harness (base 150 ms + expansion budget 100 ms) | COVERED | `TestDW_6_5_ExpansionLatencyP95` (opensearch_integration_test.go, live cluster): seeds a bounded fixture graph, runs N searches through `MultiRetriever` with `GraphExpander` registered, asserts p95 ≤ 250ms |
| DW-6.6 | decision-gate memo written with measured hop-depth distribution; D8 confirmed or flipped in the Decision Log | COVERED | `internal/graph/DECISION_GATE.md` written from the DW-6.5 test's measured latency/hop data; plan's Decision Log gets a D8-confirmation entry appended during commit |

**All items COVERED:** YES

## Design Decisions

### Design: Dedup Decision Routine

#### Approaches Considered
1. **LLM-judge-on-every-mention** — call the judge for every extracted mention, no local scoring.
2. **Threshold-only (embed + lexical, no judge)** — pure deterministic scoring, no adjudication.
3. **Tri-tier: fast-path threshold + judge tie-break only in the ambiguous band** — embed-similarity + BM25-style lexical score settle the clear cases (near-1.0 combined → merge; near-0 → distinct, including the homonym case where lexical match is perfect but embedding context diverges); only the middle band pays for a judge call. (LiCoMemory hyperlink-not-duplicate: keep the duplicate as a linked mention rather than physically merging/deleting.)

#### Comparison
| Criterion | 1: judge-every-mention | 2: threshold-only | 3: tri-tier |
|---|---|---|---|
| Interface simplicity | 1 method, trivial | 1 method, trivial | 1 method (`Decide`), same caller-facing shape |
| Cost/latency per mention | one judge call always (violates the extraction cost budget at scale — plan's own uncertainty note) | zero | judge only on the ambiguous minority |
| Correctness on the homonym edge case | good (judge sees full context) | poor — a naive name-only or over-weighted-lexical score merges homonyms | good — embedding distance alone routes homonyms to "distinct" before any judge call, exactly the edge case's documented resolution |
| Determinism for tests/e2e | needs a mocked judge everywhere | fully deterministic | fully deterministic in the common+edge cases; judge is fixture-deterministic (`RuleJudge`) in tests, real LLM in prod — same shape as `experience.Gatekeeper` |

#### Choice: 3 (tri-tier), Hybrid of the "one routine" requirement
Rationale: matches the plan's explicit instruction ("embed-similarity + BM25 + LLM-judge tie-break ... ONE decision routine, functional cohesion") and the fixture-judge/HTTP-judge split already proven at Phase 5 for exactly this fail-closed-judge shape. The three signals are computed and consumed inside a single `Deduper.Decide` method — one operation ("decide whether this mention is the same entity as its best existing match"), not three separate routines glued by a caller. Judge failure resolves to "distinct" (favor precision: an incorrectly split entity is cheap to re-merge on the next mention once the judge is healthy again; an incorrectly merged entity conflates two identities and is comparatively expensive to unwind) — the fail-*safe* direction for a data-quality decision, deliberately distinguished from the ACL judge's fail-*closed*-to-deny (there is no confidentiality being protected here, so "safe" is defined by reversibility cost, not by authorization).

#### Depth Check
- Interface methods: 1 (`Deduper.Decide`); `Judge.SameEntity` is a single-method tie-break seam (mirrors `Gatekeeper.Evaluate`).
- Hidden details: token/BM25 scoring, cosine similarity, threshold bands, judge-call decision (when to even ask) — none of this leaks to `Store.UpsertMention`, which just calls `Decide` and acts on `Decision{Merge, MatchID}`.
- Common case complexity: simple — the overwhelming majority of mentions (identical repeats, or clearly-new names) never touch the judge.

### Design: Graph Storage + Traversal

#### Approaches Considered
1. **Reuse the T2 semantic index** with a `kind` discriminator field for entity/edge docs.
2. **Dedicated `entities`/`edges` OpenSearch indices**, own templates, own package-private HTTP helpers (mirrors `experience`'s `OpenSearchBackend`).
3. **Graph-native store (Neo4j/FalkorDB) from day one.**

#### Comparison
| Criterion | 1: shared T2 index | 2: dedicated indices | 3: graph-native |
|---|---|---|---|
| Interface simplicity | leaks a discriminator into T2's `dynamic: strict` mapping (mapping churn risk to a phase-1 index) | clean, isolated contract | clean but a new subsystem |
| Information hiding | poor — T2's mapping now encodes graph concerns | good — graph owns its own mapping | good |
| Matches D8 scope (≤2-hop, OpenSearch edges; graph DB is the decision-gate's *output*, not an input) | — | yes | pre-empts the decision gate this phase is supposed to produce |

#### Choice: 2 (dedicated indices)
Rationale: D8 says "graph DB only for >2-3 hops" and this phase's own job is to gather the hop-depth evidence that decides that — building on Neo4j now would make DW-6.6 a foregone conclusion instead of a measured one. Dedicated indices keep T2's mapping untouched and mirror the proven `experience` package shape.

### Design: GraphExpander / PostHook Seam
`GraphExpander` satisfies `retrieval.PostHook` (`Apply(ctx, id, hits)`) by delegating to an exported `Expand(ctx, hits, depth)` (the plan's literal contract, also directly unit-testable for the depth-2-honored/depth-3-rejected boundary). `Apply` calls `Expand(ctx, hits, g.depth)` with `g.depth` fixed at construction (validated ≤2, `ErrDepthExceeded` otherwise — ADR: OUT is >2-hop, so the constructor and `Expand` both refuse >2 rather than silently clamping, since silent clamping would hide a misconfiguration). Every hit `Expand` adds carries the traversed edge's OWN provenance fields (`tenant_id`, `team_id`, `scope`, `owner_agent_id`) — never inherited from the seed hit — so `MultiRetriever`'s existing post-hook re-authorization (`filterAuthorized`, already proven correct by `TestPostHookAdditionsReauthorizedWithoutLosingTopK`) drops anything reached only through an edge the caller cannot read. This phase adds zero lines to `internal/retrieval`.

Traversal is bounded (`maxFanout` per node per hop, `maxAdded` total) — an engineering default to keep the ≤250ms latency budget predictable regardless of graph density; documented, not derived from a DW item, but directly informs the DW-6.6 memo.

## Prerequisites
- [x] Required files exist (or will be created) — `internal/graph/**` is new; every seam it plugs into (`RegisterStage`, `RegisterPostHook`) already exists and is tested.
- [x] Dependencies available — dev OpenSearch 3.1.0 running (`engram-dev-os`, `:9200`); Go 1.26.3 toolchain.
- [x] No missing prerequisites.

## Recommendation
BUILD. No plan conflicts requiring UPDATE_PLAN; the one apparent tension (entity-mention emission vs. file-scope boundary) is already resolved by the dispatch prompt's environment note.

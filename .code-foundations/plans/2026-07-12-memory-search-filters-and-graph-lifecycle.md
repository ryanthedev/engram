# Plan: memory_search filters + graph tier lifecycle
**Created:** 2026-07-12
**Status:** in-progress
**Started:** 2026-07-12
**Current Phase:** 1
**Complexity:** complex
---
## Context

Engram's graph tier has a lifecycle that was designed but never wired up. `Edge.InvalidAt` is declared (`internal/graph/graph.go:114`) and read by `Edge.Live()` (`:121`), but **no non-test code ever sets it**. The worker computes a reconciliation decision per fact (`internal/worker/worker.go:319`) and then discards it: `reconcileFact` takes the fact **by value** (`:377`) and stamps `Supersedes` on its local copy, so the slice handed to `runStages` (`:332`) never carries the decision. `graph.Stage.Process` (`internal/graph/stage.go:45`) therefore blindly upserts every triple. When a semantic fact is superseded, the semantic tier correctly hides the old version (`ValidOnly`), but its graph edge stays live forever and keeps being served — stale results ranked beside the correction that replaced them.

Two adjacent defects compound it. The expander's seed-echo guard seeds `visitedEdges` with semantic **document IDs** (`internal/graph/expand.go:111`) and compares them against graph **edge fingerprints** (`:134`) — two ID spaces that can never collide, making the guard structurally inert, so a seed fact's own edge is re-served as an "expanded" hit. And `MultiRetriever.Search` truncates to `q.K` (`internal/retrieval/opensearch.go:268-271`) and *then* runs post-hooks that append up to 20 more hits with no re-sort or re-truncate (`:277-283`), so `k=20` can return 40. Graph expansion is on by default (`cmd/engram-server/main.go:79`, depth = `MaxDepth` = 2).

Separately, `memory_search` exposes no filters at all — `retrieval.Query` is `{Text, K}` (`internal/retrieval/retriever.go:16-19`) and `Filter` (`:28-35`) is internal (tenant/ACL/`ValidOnly`, the last hardcoded `true` at `internal/engramclient/client.go:226`). A caller cannot narrow by event kind, fact subject/predicate/object, time, or extractor version, nor exclude a tier — even though every one of those fields is already mapped `keyword` or `date` in `internal/store/templates/{episodic,semantic}.json`. No reindex is needed; only a query surface is missing.

## Constraints

- **Not in production; no users; no backward compatibility required.** Data loss is acceptable (preferably avoided). This licenses breaking the `worker.Stage` interface and wiping/rebuilding the graph indices outright.
- **Optimization target: the most token-efficient, lowest-error API surface for an LLM caller, at low cost.** Filters are exposed as flat named params (`kind`, `subject`, `since`, …) — not a generic `filters:[{field,op,value}]` array, which costs more tokens to emit and gives a model more ways to be wrong. The generic predicate form remains the *internal* representation.
- Append-only store: no deletes of episodic/semantic records, no destructive extraction-time filters. Only *derived* data (the graph) may be rebuilt.
- Absent filters ⇒ emitted OpenSearch query body **byte-identical** to today's (regression-pinned; mirrors the knowledge-platform's `buildQuery` precedent).
- Filters must be **tier-gated by construction**. `kind` exists only on episodic; `subject`/`predicate`/`object`/`extractor_version` only on semantic. An ungated clause silently zeroes out the other tier — this is the single most likely way the feature ships broken, so the routing must make it impossible, not merely discouraged.
- `k` means k **for matched hits**. Graph expansions return in a separate labeled `expanded` block: nothing is evicted, nothing is smuggled in. This deliberately reverses the prior "Phase 4 design" decision documented at `internal/graph/expand.go:239` and re-affirmed as out-of-scope by the 2026-07-09 response-shaping plan (see Decision Log).
- Graph expansion stays enabled at depth 2. The feature is wanted; the implementation is what is broken.
- Do not couple the memory and knowledge retrievers into a shared filter package — they are different actors (SRP-by-actor; the 2026-07-10 knowledge-platform plan's Decision Log already rejected this).
- `make proto` regenerates checked-in `api/engrampb/*.pb.go`; CI's `proto-check` target fails if they are not committed.

---
## Chosen Approach

**A1 (widen the reconciliation seam) + B2-internal/flat-external (registry-routed predicates behind a flat LLM-facing schema).**

The reconciler is the single owner of fact lifecycle; the graph is a *derived projection* of it. The correct fix is to feed the graph the owner's decision rather than let it re-derive or guess one. Widening `worker.Stage` to carry `[]FactOutcome{Fact, Decision, Predecessor}` makes that dependency explicit and compiler-enforced, and with only two stage implementors (graph, experience) the interface break is two call sites. For filters, each tier *declares* the fields it can filter on, and the router drops predicates no tier owns — so the tier-gating trap becomes structurally impossible rather than dependent on someone remembering an `if`. The LLM-facing MCP schema stays flat and named, compiling down to that internal predicate form.

**Fallback:** A4 — delete stored edges entirely and derive graph neighbors at query time from the already-keyword-mapped semantic index. This eliminates the bug class rather than fixing it, at the cost of entity dedup/homonym merging. Adopt if the rebuild (Phase 3) proves uglier than expected.

## Rejected Approaches

- **A2 (write reconciled facts back into the slice):** avoids the interface change, but forces the graph stage to fetch each predecessor fact from the semantic store to recompute its edge fingerprint — a new inward coupling and an extra read per supersession, for a value the worker already held.
- **A3 (reconciliation outbox):** fully decoupled and replayable, but substantial new machinery and cross-tier eventual consistency for a system with exactly one consumer.
- **B1 (typed filter fields on `retrieval.Filter`):** every new filter costs a struct field, a proto field, and a hand-written tier gate — grows linearly and leaves the zeroing trap open.
- **B3 (shared filter package across knowledge + memory):** looks DRY, but couples two actors across a boundary the knowledge plan already drew deliberately.
- **Generic `filters:[{field,op,value}]` as the MCP surface:** the safe internal shape, but token-expensive for an LLM to emit and error-prone to construct. Kept internally, hidden externally.

---
## Implementation Phases

### Phase 1: Reconciliation-outcome seam
**Model:** opus
**Skills:** code-foundations:aposd-designing-deep-modules, code-foundations:cc-routine-and-class-design
**Gate:** Full

**Goal:** Stop discarding the reconciler's decision: make `worker.Stage` receive what actually happened to each fact, so derived projections can react to supersession.

**Scope:**
- IN: new `ingest.FactOutcome` type; `reconcileFact` returns its decision + predecessor; `runStages` passes `[]ingest.FactOutcome`; update both existing implementors (`graph.Stage`, `experience.DistillStage`) to the new signature — signature change only, no edge-closing logic.
- OUT: any graph edge-closing behavior (Phase 2 owns it — this phase only makes `graph.Stage` compile against the new signature); any retrieval change.

**Constraints:** No backward compatibility required — change the interface outright rather than adding a parallel one. `reconcileFact` currently takes the fact by value and mutates a copy; it must surrender the decision it already computes rather than re-deriving it. **`FactOutcome` lives in `internal/ingest`, not `internal/worker`** — it is reconciliation vocabulary (it carries `ingest.OpKind`), and siting it there avoids forcing a new `internal/graph → internal/worker` import arrow.
**Edge cases:** NOOP (fact already live — no outcome to act on); resumed events where `CompletedActions` already contains the docID (`worker.go:315`) and reconciliation is skipped — the outcome must still be reconstructible or explicitly marked as replayed; in-batch contradictory facts resolved by the `(valid_at, content_key)` sort (`:301-306`).
**Depends on:** none | **Unlocks:** Phase 2
**File scope:** `internal/worker/**`, `internal/experience/**`, `internal/ingest/**`, `internal/graph/stage.go`, `internal/graph/stage_test.go`
**Produces:** `type ingest.FactOutcome struct { Fact memory.SemanticFact; Decision ingest.OpKind; Predecessor *memory.SemanticFact }` and `worker.Stage.Process(ctx context.Context, ev memory.Episodic, outcomes []ingest.FactOutcome) error`

**Approach notes:** User chose A1 explicitly over A2 — the graph must be *fed* the decision, not re-derive it by reading the semantic store.
**File hints:** `internal/worker/worker.go` — `reconcileFact:377`, `runStages:153`, call site `:332`. `internal/experience/` — the other `Stage` implementor. `internal/graph/stage.go:45` — must be edited to compile against the new signature (behavior stays in Phase 2). `internal/graph/stage_test.go:15` — carries the only existing `var _ worker.Stage` compile assertion; `experience.DistillStage` has none and needs one added.

**Done when:**
- [ ] DW-1.1: `worker.Stage.Process` takes `[]ingest.FactOutcome`; the old `[]memory.SemanticFact` signature no longer exists, and the `var _ worker.Stage` compile assertions for both implementors still hold.
- [ ] DW-1.2: `FactOutcome.Decision` is populated from the reconciler's actual `Op.Kind` for every fact, and `Predecessor` is non-nil for exactly the UPDATE and INVALIDATE cases.
- [ ] DW-1.3: `experience.DistillStage` compiles and its existing tests pass against the new signature.
- [ ] DW-1.4: A resumed event (docID already in `CompletedActions`) produces a well-defined outcome rather than a zero-value one.

**Difficulty:** MEDIUM
**Uncertainty:** Resolved during CHECK — `reconcileFact` already holds the candidate set from `w.candidates(...)`, so `Predecessor` is populated by matching `op.PredecessorID` in memory, with no extra read. The replay path (DW-1.4) remains the open question; mark replayed outcomes explicitly and let Phase 2 treat them as no-ops.

### Phase 2: Graph edge lifecycle + echo dedup
**Model:** opus
**Skills:** code-foundations:cc-debugging, code-foundations:aposd-verifying-correctness
**Gate:** Full

**Goal:** Make `Edge.InvalidAt` a field that production code actually sets — closing a predecessor edge when its fact is superseded — and repair the structurally-inert seed-echo guard.

**Scope:**
- IN: `graph.Store.CloseEdge` (sets `InvalidAt`); `graph.Stage` closes the predecessor's edge on UPDATE/INVALIDATE using the predecessor triple now carried in `FactOutcome`; fix `expand.go`'s visited-set ID-space mismatch by seeding it with edge fingerprints derived from each seed hit's own triple.
- OUT: the k/`expanded` response shape (Phase 6); rebuilding existing bad data (Phase 3).

**Constraints:** Edge closing is bi-temporal and soft — set `InvalidAt`, never hard-delete (`graph.go:90-91` documents this intent). The retraction convention (empty Object ⇒ no edge, `stage.go:57-58`) must be preserved.
**Edge cases:** predecessor whose edge never existed (retraction fact, or entity soft-expired) — close must be idempotent, not an error; an UPDATE whose new object resolves to the *same* entity (edge fingerprint unchanged) — must not close the edge it just wrote; replayed events must not re-close an already-closed edge; a seed hit from a tier with no subject/object fields (episodic) contributes no fingerprint.
**Depends on:** Phase 1 | **Unlocks:** Phase 3, Phase 6
**File scope:** `internal/graph/**`
**Produces:** `graph.Store.CloseEdge(ctx, tenantID, edgeID string) error` setting `InvalidAt`; `Neighbors` no longer returns edges whose fact was superseded.

**File hints:** `internal/graph/stage.go:45` — `Process`. `internal/graph/store.go:264` — `UpsertEdge`, `edgeFingerprint`. `internal/graph/expand.go:107-113` vs `:134` — the ID-space mismatch. `internal/graph/graph.go:114,121` — `InvalidAt`, `Live()`.

**Done when:**
- [ ] DW-2.1: Superseding a fact sets `InvalidAt` on the predecessor's edge; a test asserts the edge is no longer `Live()`.
- [ ] DW-2.2: A superseded edge is absent from `Store.Neighbors` and from search results.
- [ ] DW-2.3: A seed fact's own edge is never returned as an expanded hit (the visited set is seeded with fingerprints, not doc IDs).
- [ ] DW-2.4: Closing an edge is idempotent — replaying an event does not error or double-close.
- [ ] DW-2.5: An UPDATE whose new object resolves to the same entity does not close the edge it just wrote.

**Difficulty:** HIGH
**Uncertainty:** Whether an entity-merge can make a predecessor's fingerprint irrecoverable after the fact; if so, close by (from, predicate) lookup rather than exact fingerprint.

### Phase 3: Graph rebuild command
**Model:** sonnet
**Skills:** code-foundations:cc-defensive-programming
**Gate:** Standard

**Goal:** Ship a command that wipes and rebuilds the graph indices from current live semantic facts, so the zombie edges already in the store are gone — the write-path fix alone is not self-healing.

**Scope:**
- IN: `cmd/engram-graph-rebuild`; a paginated scan over live semantic facts (none exists today); replay through the fixed `graph.Stage` path; drop-and-recreate the graph indices from their templates.
- OUT: any change to graph write logic (Phase 2 owns it); rebuilding episodic/semantic (never — append-only).

**Constraints:** The graph is **derived** data — wiping it is safe and reversible by re-running this command. It must never touch the episodic or semantic indices. Requires an explicit confirmation flag; refuses to run without it.
**Edge cases:** empty store; a tenant with facts but no graphable triples (all retractions); interruption mid-rebuild (must be re-runnable from scratch, not resume-corrupted); entity merges must be re-derived through `UpsertMention`, not assumed.
**Depends on:** Phase 2 | **Unlocks:** none
**File scope:** `cmd/engram-graph-rebuild/**`, `internal/graph/rebuild*.go`, `internal/store/facts.go`
**Produces:** `engram-graph-rebuild --tenant <id> --confirm` — drops graph indices, replays live semantic facts, exits non-zero on partial failure.
**Rollback:** The graph is derived data — re-run the command to rebuild. Point of no return only if the semantic tier is simultaneously corrupt, which this command cannot cause (it never writes there).

**File hints:** `internal/store/facts.go` — needs a new live-fact scan/scroll; none exists. `internal/graph/templates/` — index templates to recreate from. `scripts/backfill-reextract-rtd.sh` — precedent for a backfill script's shape.

**Done when:**
- [ ] DW-3.1: The command drops and recreates the graph indices, then replays every live semantic fact through the graph stage.
- [ ] DW-3.2: After a rebuild against a store containing a superseded fact, no edge for the superseded version is `Live()`.
- [ ] DW-3.3: The command refuses to run without an explicit `--confirm` flag.
- [ ] DW-3.4: The command never issues a write to the episodic or semantic indices (asserted in test).
- [ ] DW-3.5: Re-running the command is idempotent — a second run produces the same graph.

**Difficulty:** MEDIUM
**Uncertainty:** Fact-scan pagination shape; OpenSearch scroll vs search_after. Prefer `search_after` (no server-side cursor state).

### Phase 4: Filter core — per-tier field registry + predicate routing
**Model:** opus
**Skills:** code-foundations:aposd-designing-deep-modules, code-foundations:cc-defensive-programming
**Gate:** Full

**Goal:** Give each retrieval tier a declared set of filterable fields, and route predicates only to tiers that own them — making the "a `kind` clause silently zeroes the semantic tier" trap structurally impossible.

**Scope:**
- IN: a per-tier filterable-field registry (field → type/op); `retrieval.Filter` gains `Predicates []Predicate` and `Sources []string`; `tierRetriever.filterClauses` compiles only the predicates its tier declares; `MultiRetriever.Search` honors `Sources` at its three unconditional loop sites; add `extractor_version` to `allowedFields["semantic"]`.
- OUT: proto/MCP/client surface (Phase 5); the `expanded` response block (Phase 6).

**Constraints:** Reuse the existing `Predicate{Field, Op, Value}` / `Range` shapes from `internal/retrieval/knowledge.go:25-37` — do not invent a parallel vocabulary, and do not extract a shared package (different actors). A predicate naming a field no tier declares is a **validation error naming the valid fields** (usability, not a security boundary — the knowledge plan's Phase 6 precedent), not a silent drop. Caller-supplied filter values are compiled into an OpenSearch query body here: they must be parameterized into clause structures, never interpolated into a query string.
**Edge cases:** empty `Predicates` ⇒ query body byte-identical to today (regression-pinned); a predicate valid on one tier and absent on another (route, don't fail); `Sources` naming an unknown source; `Sources: []` vs `Sources: nil` (nil = all sources, empty = error, not silent-all); a range with only `gte` or only `lte`.
**Depends on:** none | **Unlocks:** Phase 5
**File scope:** `internal/retrieval/**`, `cmd/engram-server/**`
**Produces:**
- `retrieval.Filter{..., Predicates []Predicate, Sources []string}`; `type FilterableFields map[string]FieldSpec` declared per tier.
- **Name-carrying registration:** `RegisterTier(name string, src TierSource)` and `RegisterPostHook(name string, h PostHook)` — today neither the `TierSource` nor the `PostHook` interface exposes a name (`retriever.go:50,58`) and neither register function takes one (`opensearch.go:177,184`), so `MultiRetriever` currently *cannot* skip a registered source by name. Carry the name at registration rather than adding `Name()` to the interfaces — a `Name()` method would drag `internal/experience/**` and `internal/graph/expand.go` into this phase's scope and collide with Phases 1, 2 and 6.
- `MultiRetriever.Search` skips tiers and post-hooks not named in `Sources`. **`Sources` vocabulary is `{"episodic", "semantic", "graph"} ∪ registered tier-source names`** — note `"graph"` names a PostHook, not a tier, so the router resolves both namespaces from one list.
**Security-sensitive:** yes — this is where caller-supplied filter values are compiled into the OpenSearch query body (`filterClauses`, `opensearch.go:469-497`), and where `Sources` edits `MultiRetriever.Search`'s authorize/truncate/post-hook loops (`:261-285`), which carry the ACL enforcement.

**Approach notes:** The registry is the tier-gating mechanism the user's constraint demands — hand-written `if t.supportsX` gates (mirroring `:484`) were rejected as too easy to forget when the next filter is added.
**File hints:** `internal/retrieval/opensearch.go:469-497` — `filterClauses`, the clause injection point. `:200-301` — `MultiRetriever.Search`, the three unconditional loops. `:177,184` — `RegisterTier`/`RegisterPostHook`, which must start carrying a name. `:307-309` — `allowedFields`. `internal/retrieval/knowledge.go:286-332` — `buildFilterClauses`/`filterClause`, the compiler to mirror. `cmd/engram-server/stages_experience.go:65` and `stages_graph.go:86` — the two registration call sites to update.

**Done when:**
- [ ] DW-4.1: Each tier declares its filterable fields; episodic declares `kind`, semantic declares `subject`/`predicate`/`object`/`extractor_version`, both declare their time fields.
- [ ] DW-4.2: A `kind` predicate leaves the semantic tier's query unconstrained (not zeroed) — asserted by a test that would fail under a naive shared clause.
- [ ] DW-4.3: With no predicates and no `Sources`, the emitted OpenSearch query body is byte-identical to today's (golden-byte regression test).
- [ ] DW-4.4: A predicate on an unknown field errors, and the error names the valid filterable fields.
- [ ] DW-4.5: `Sources: ["semantic"]` skips the graph post-hook and the episodic tier entirely.
- [ ] DW-4.6: `extractor_version` appears on semantic hits.
- [ ] DW-4.7: An unknown source name errors, and the error names the valid sources (mirrors DW-4.4's contract for the `Sources` namespace).
- [ ] DW-4.8: An injection-shaped filter value (e.g. a string containing OpenSearch query DSL) is parameterized into a clause structure, not interpolated into the query body.

**Difficulty:** HIGH
**Uncertainty:** Whether `Sources` belongs on `Filter` or `Query`. `Filter` is the scoping type, so it goes there — but it is the one seam Phase 5 and 6 both consume.

### Phase 5: LLM-facing API surface — proto + flat MCP schema
**Model:** opus
**Skills:** code-foundations:aposd-designing-deep-modules, code-foundations:cc-defensive-programming
**Gate:** Full

**Goal:** Expose the filters to callers through the cheapest, least error-prone schema an LLM can emit — flat named params, compiled down to the internal predicate form.

**Scope:**
- IN: `SearchRequest` proto fields; `make proto` + commit regenerated `.pb.go`; the `memory_search` MCP tool schema gains flat optional params (`kind`, `subject`, `predicate`, `object`, `extractor_version`, `since`, `until`, `include_superseded`, `sources`); `Backend.Search` signature; `engramclient.Search` (dropping the hardcoded `ValidOnly: true`); `server.Search` request→`Query`/`Filter` mapping; entry-point validation.
- OUT: the retrieval internals (Phase 4); the `expanded` block (Phase 6).

**Constraints:** **Flat named params, not a generic `filters` array** — the user's optimization target is LLM token cost and low error rate; the generic form stays internal. No backward compatibility: change `Backend.Search` outright. Validate at the MCP/gRPC barricade before touching inner modules; inner modules may then assume validated input. Tool-description text is itself a token cost — keep it terse but unambiguous.
**Edge cases:** unknown filter field (error naming valid fields); malformed time (`since` > `until`); `include_superseded` with no other filter; `sources` naming an unknown tier; every filter absent ⇒ identical behavior to today; `k` bounds still enforced.
**Depends on:** Phase 4 | **Unlocks:** Phase 6
**File scope:** `api/proto/**`, `api/engrampb/**`, `internal/mcp/**`, `internal/engramclient/**`, `internal/server/**`
**Produces:** `memory_search(query, k, kind?, subject?, predicate?, object?, extractor_version?, since?, until?, include_superseded?, sources?)`; `mcp.Backend.Search(ctx, query string, k int, f mcp.SearchFilter) ([]Hit, error)`.
**Security-sensitive:** yes — this is the untrusted-input barricade; filter values reach an OpenSearch query body, and `sources`/`include_superseded` interact with the ACL and validity filters.

**File hints:** `api/proto/engram.proto:137-148` — `SearchRequest`; `:334-350` — `Range`/`Predicate` precedent. `internal/mcp/tools.go:59-70` — schema; `:264-291` — `callSearch`. `internal/mcp/mcp.go:138` — `Backend.Search`. `internal/engramclient/client.go:225` — hardcoded `ValidOnly`. `internal/server/server.go:138-172`. `Makefile:36,40` — `proto`, `proto-check`.

**Done when:**
- [ ] DW-5.1: `memory_search` accepts every flat filter param; each maps to the correct internal predicate and tier.
- [ ] DW-5.2: `include_superseded: true` returns historical facts; absent/false preserves today's `ValidOnly` behavior.
- [ ] DW-5.3: `sources: ["semantic"]` excludes episodic and graph hits end-to-end from the MCP call.
- [ ] DW-5.4: An invalid filter field or malformed time range is rejected at the MCP/gRPC entry with an error naming the valid fields — never reaching the retriever.
- [ ] DW-5.5: `make proto` is run and the regenerated `api/engrampb/*.pb.go` are committed; `make proto-check` passes.
- [ ] DW-5.6: A call passing no filters behaves identically to today (end-to-end).
- [ ] DW-5.7: An adversarial filter value (injection-shaped string) is safely parameterized into the query body, not interpolated.

**Difficulty:** HIGH
**Uncertainty:** Whether `sources` should also accept `"graph"` as an explicit opt-*in* rather than only an opt-out. Default to: `sources` absent = all tiers (today's behavior).

### Phase 6: Honest `k` — separate `expanded` block
**Model:** opus
**Skills:** code-foundations:aposd-designing-deep-modules
**Gate:** Full

**Goal:** Make `k` mean k for matched hits, returning graph expansions in their own labeled block so nothing is evicted and nothing is silently smuggled into the result array.

**Scope:**
- IN: split post-hook (graph) additions out of the matched-hit array at the `server.Search` boundary by `Hit.Source == "graph"`; `SearchResponse` gains a repeated `expanded` field; `engramclient` and `mcp.Backend.Search` carry both blocks; MCP render/budget treat `expanded` as a distinct, separately-budgeted section; `expand.go:239`'s "does not re-truncate" comment corrected to state the new contract.
- OUT: changing expansion depth or fanout defaults (stays 2); changing the `retrieval.Retriever` interface (see Constraints).

**Constraints:** This **reverses a deliberate prior decision** (`internal/graph/expand.go:239`, "Phase 4 design"; re-affirmed out-of-scope by the 2026-07-09 response-shaping plan). Record the reversal in the Decision Log. **Do not change `retrieval.Retriever.Search`'s `([]Hit, error)` signature** — `eval.NullRetriever` (`cmd/engram-eval/main.go:38`) also implements it, and the split is cleanly derivable from `Hit.Source` at the server boundary without touching the interface. This phase touches seams owned by the response-shaping and drilldown plans (`internal/mcp/render.go`, `budget.go`) — respect their byte-budget, projection, and spill contracts rather than bypassing them. Expanded hits are a *token cost* on every LLM call: they must be clearly delimited and independently budgetable.
**Edge cases:** zero expanded hits (omit the block, don't emit an empty one); expansion interacting with the MCP byte budget and spill-to-disk path (matched hits pack first; expansions are the first thing dropped under pressure); `sources` excluding graph (⇒ no block at all); a registered post-hook that is not the expander (its hits are not `Source == "graph"` and must not be misfiled into `expanded`).
**Depends on:** Phase 2, Phase 5 | **Unlocks:** none
**File scope:** `internal/retrieval/opensearch.go`, `internal/graph/expand.go`, `internal/server/**`, `internal/engramclient/**`, `internal/mcp/**`, `api/proto/**`, `api/engrampb/**`
**Produces:**
- proto: `SearchResponse { repeated Hit hits = 1; repeated Hit expanded = 2; }`
- Go: `type mcp.SearchResult struct { Hits []Hit; Expanded []Hit }`, returned by `mcp.Backend.Search(ctx, query string, k int, f mcp.SearchFilter) (SearchResult, error)`
- contract: `len(SearchResult.Hits) <= k`, and no element of `Hits` has `Source == "graph"`.

**Approach notes:** User chose "keep additive, but bound + label it" over strict re-truncation — expansion is bonus context and should not evict a direct match, but it must not inflate `k` either.

**Done when:**
- [ ] DW-6.1: The returned `hits` array contains no element with `Source == "graph"` and has length ≤ k, with expansion enabled at depth 2.
- [ ] DW-6.2: Graph expansions are returned in a distinct `expanded` block, never mixed into `hits`.
- [ ] DW-6.3: Zero expansions ⇒ no `expanded` block emitted.
- [ ] DW-6.4: The MCP byte budget accounts for the `expanded` block separately; matched hits pack first and overflow spills via the existing path.
- [ ] DW-6.5: The stale comment at `expand.go:239` is corrected to state the new contract.
- [ ] DW-6.6: `make proto` is run, the regenerated `api/engrampb/*.pb.go` are committed, and `make proto-check` passes.

**Difficulty:** HIGH
**Uncertainty:** How the byte-budget packer prioritizes `hits` vs `expanded` under pressure. Default: matched hits are packed first; expansions are the first thing dropped.

---
## Test Coverage
**Level:** 100%

## Test Plan
- [ ] Unit: the existing compile assertion in `internal/graph/stage_test.go:15` is updated to the `[]ingest.FactOutcome` signature and still holds; a new `var _ worker.Stage = (*experience.DistillStage)(nil)` assertion is ADDED (none exists today) (DW-1.1)
- [ ] Unit: `FactOutcome` populated with correct `Decision` for each of ADD / NOOP / UPDATE / INVALIDATE (DW-1.2)
- [ ] Unit: `Predecessor` non-nil exactly for UPDATE and INVALIDATE; nil otherwise (DW-1.2)
- [ ] Dirty (P1): resumed event whose docID is already in `CompletedActions` yields a well-defined, non-zero-value outcome (DW-1.4)
- [ ] Integration: `experience.DistillStage` passes its existing suite against the new signature (DW-1.3)
- [ ] Unit: superseding a fact sets `InvalidAt` on the predecessor edge; edge is no longer `Live()` (DW-2.1)
- [ ] Integration: superseded edge absent from `Neighbors` and from a live search (DW-2.2)
- [ ] Unit: seed fact's own edge is not returned as an expanded hit (DW-2.3)
- [ ] Dirty (P2): close an already-closed edge — idempotent, no error (DW-2.4)
- [ ] Dirty (P2): UPDATE whose new object resolves to the same entity — edge just written is not closed (DW-2.5)
- [ ] Dirty (P2): close an edge that never existed (retraction / soft-expired entity) — no error (Edge cases)
- [ ] Integration: rebuild drops + recreates graph indices and replays live facts (DW-3.1)
- [ ] Integration: post-rebuild, no edge for a superseded fact is `Live()` (DW-3.2)
- [ ] Dirty (P3): command without `--confirm` refuses and exits non-zero (DW-3.3)
- [ ] Dirty (P3): assert zero writes to episodic/semantic indices during rebuild (DW-3.4)
- [ ] Integration: rebuild is idempotent across two runs (DW-3.5)
- [ ] Dirty (P3): rebuild against an empty store; rebuild against a tenant whose facts are all retractions (Edge cases)
- [ ] Unit: each tier's declared filterable fields match its index mapping (DW-4.1)
- [ ] Unit: a `kind` predicate does NOT constrain the semantic tier's query (DW-4.2) — the trap test
- [ ] Regression: golden-byte — no predicates, no sources ⇒ query body byte-identical to today (DW-4.3)
- [ ] Dirty (P4): predicate on an unknown field errors and names the valid fields (DW-4.4)
- [ ] Boundary (P4): `Sources: nil` = all tiers; `Sources: []` = error, not silent-all (Edge cases)
- [ ] Boundary (P4): range with only `gte`; only `lte`; `gte` > `lte` (Edge cases)
- [ ] Unit: `Sources: ["semantic"]` skips episodic tier and graph post-hook (DW-4.5)
- [ ] Unit: `extractor_version` present on semantic hits (DW-4.6)
- [ ] Dirty (P4): unknown source name errors and names the valid sources (DW-4.7)
- [ ] Dirty (P4): injection-shaped filter value is parameterized into a clause, not interpolated into the query body (DW-4.8)
- [ ] Integration: every flat MCP filter param maps to the correct predicate and tier (DW-5.1)
- [ ] Integration: `include_superseded: true` returns historical facts; absent preserves `ValidOnly` (DW-5.2)
- [ ] Integration: `sources: ["semantic"]` excludes episodic + graph end-to-end from MCP (DW-5.3)
- [ ] Dirty (P5): invalid filter field rejected at entry, error names valid fields, retriever never called (DW-5.4)
- [ ] Dirty (P5): `since` > `until` rejected at entry (Edge cases)
- [ ] Dirty (P5): injection-shaped filter value is parameterized, not interpolated (DW-5.7)
- [ ] Manual: `make proto && make proto-check` passes with regenerated `.pb.go` committed (DW-5.5)
- [ ] Regression: MCP call with no filters behaves identically to today, end-to-end (DW-5.6)
- [ ] Unit: with expansion on at depth 2, `hits` contains no `Source == "graph"` element and `len(hits) <= k` (DW-6.1)
- [ ] Unit: expansions land in `expanded`, never in `hits` (DW-6.2)
- [ ] Boundary (P6): zero expansions ⇒ no `expanded` block (DW-6.3)
- [ ] Integration: byte budget accounts for `expanded` separately; matched hits pack first; overflow spills (DW-6.4)
- [ ] Dirty (P6): `sources` excluding graph ⇒ no `expanded` block at all (Edge cases)
- [ ] Dirty (P6): a non-expander post-hook's hits are NOT misfiled into `expanded` (Edge cases)
- [ ] Manual (P6): `expand.go`'s post-hook comment states the new "expansions land in a separate `expanded` block" contract (DW-6.5)
- [ ] Manual: `make proto && make proto-check` passes with regenerated `.pb.go` committed after the `expanded` field lands (DW-6.6)

---
## Assumptions
| Assumption | Confidence | Verify Before Phase | Fallback If Wrong |
|---|---|---|---|
| Only two `worker.Stage` implementors exist (graph, experience) | HIGH | Phase 1 | Update each additional implementor; interface break is still compile-caught |
| The predecessor fact's triple is enough to recompute its edge fingerprint | MED | Phase 2 | Close by `(from_entity, predicate)` lookup instead of exact fingerprint |
| The graph can be wiped and rebuilt with no permanent loss (derived data) | HIGH | Phase 3 | User accepted data loss; worst case is re-running extraction |
| No live-fact scan exists in `internal/store` and one must be written | HIGH | Phase 3 | Confirmed absent by grep; use `search_after` pagination |
| All target filter fields are already `keyword`/`date` — no reindex | HIGH | Phase 4 | Confirmed in both index templates; a miss would force a mapping change + reindex |
| Flat named params beat a generic filter array for LLM token cost + error rate | MED | Phase 5 | Internal predicate form is unchanged; a generic array can be added later as a second surface |
| `internal/mcp/budget.go`'s packer can be extended to a second section without redesign | MED | Phase 6 | Budget `hits` first and drop `expanded` wholesale under pressure |

## Decision Log
| Decision | Alternatives Considered | Rationale | Phase |
|---|---|---|---|
| Widen `worker.Stage` to carry `FactOutcome` | A2 write-back into the slice; A3 outbox; A4 delete stored edges | The reconciler owns lifecycle; a derived projection must be fed the owner's decision, not re-derive it. Only 2 implementors, so the break is compiler-caught and cheap | 1 |
| Keep stored edges + entity merging (reject A4) | A4: derive neighbors at query time from the semantic index | A4 eliminates the bug class but discards entity dedup/homonym merging, which is live functionality. Held as the documented fallback | 2 |
| Wipe + rebuild the graph rather than fix-forward only | Fix-forward only; incremental repair job | The write-path fix is not self-healing — existing zombie edges would be served forever. Graph is derived data and the user accepted data loss | 3 |
| Per-tier field registry, not hand-written tier gates | B1 typed fields + `if t.supportsX` gates (mirroring `:484`) | The zeroing trap is the most likely way this ships broken; a registry makes it structurally impossible rather than dependent on memory | 4 |
| Flat named MCP params; generic predicates internal only | Generic `filters:[{field,op,value}]` as the public surface (knowledge_search's shape) | User's optimization target is LLM token cost and error rate. Flat params are cheaper to emit and harder to get wrong; the safe generic form is retained internally | 5 |
| Do not share a filter package with knowledge | B3: extract one compiler both retrievers import | SRP-by-actor — memory and knowledge are different actors; the knowledge plan's own Decision Log already drew this boundary | 4 |
| `k` bounds matched hits; expansions in a separate `expanded` block | Strict re-truncation to k (evicts expansions); leave as-is (k=20 returns 40) | Reverses the deliberate "Phase 4 design" at `expand.go:239` and the response-shaping plan's explicit OUT. Expansion is bonus context: it should neither evict a match nor inflate k | 6 |
| `FactOutcome` lives in `internal/ingest`, not `internal/worker` | Siting it in `worker` alongside the `Stage` interface | It carries `ingest.OpKind` — it is reconciliation vocabulary. Siting it in `ingest` avoids forcing a new `internal/graph → internal/worker` import arrow that only the interface's shape would have required | 1 |
| Do not change `retrieval.Retriever.Search`'s signature to carry `expanded` | Widening the interface to return two blocks | `eval.NullRetriever` (`cmd/engram-eval/main.go:38`) also implements `Retriever`; the split is cleanly derivable from `Hit.Source == "graph"` at the `server.Search` boundary, so the interface stays untouched and the blast radius shrinks to one layer | 6 |

---
## Notes
- The k-truncation behavior was **not** an accidental bug: it is documented as intentional at `internal/graph/expand.go:239` and was explicitly preserved as out-of-scope by the 2026-07-09 response-shaping plan. Phase 6 knowingly reverses it.
- The `Edge.InvalidAt`-never-set defect and the seed-echo ID-space mismatch are **new findings** with no prior plan or doc coverage.
- The original audit that triggered this work estimated "~45% of the semantic tier is junk" by sampling via hybrid search. That estimate is **not trustworthy** — a retrieval-conditioned sample cannot estimate inventory distribution. Left deliberately out of scope; if inventory quality is ever measured, draw a uniform random sample (`match_all` + `sort: _doc`), not a search-conditioned one.
- Extraction-prompt hardening (durability, self-containment) was proposed and is likely still worthwhile, but is **not in this plan** — it is a separate, unmeasured concern and both external reviewers argued no extraction change should ship before retrieval quality is actually measured.
- Phases 1→2→3 (graph) and Phase 4 (filter core) are independent and can run as parallel waves; Phase 5 depends on 4, and Phase 6 joins both branches.
---
## Execution Log
_To be filled during /code-foundations:build_

### Phase 1: Reconciliation-outcome seam (Gate: Full)
- [x] BUILD: Discovery + design + implementation complete
- [x] REVIEW: Verification passed
- [x] Committed
Commit: ad2cb0c
Summary: `worker.Stage.Process` now receives `[]ingest.FactOutcome{Fact, Decision, Predecessor}` instead of raw extracted facts, so derived projections can react to supersession. Replay is a fifth `OpKind` (`OpReplayed`). `graph.Stage` and `experience.DistillStage` compile against the new seam but carry no new behavior yet. Gotcha for Phase 2: the late-arrival path reports UPDATE with a non-nil Predecessor but must NOT have its edge closed — detect via `Fact.ValidAt.Before(Predecessor.ValidAt)`.

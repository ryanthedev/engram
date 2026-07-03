# Greenfield Agent-Memory Platform — Phased Build Plan

**Context:** large-scale, mission-critical, greenfield. Three memory jobs: **task/episodic experience**, **domain knowledge base (multi-hop)**, **multi-agent shared memory**. Backbone: **OpenSearch** (BM25 + Faiss kNN + RRF). Grounded in deep-reads of Zep/Graphiti, Mem0, Letta, A-MEM, GraphRAG, MemOS, Mem-α, MUSE, Collaborative Memory, G-Memory, MIRIX, GeAR, SGMem, LiCoMemory + OpenSearch docs.

---

## Target architecture

| Layer / index | Holds | Retrieval | Mutability | Volume |
|---|---|---|---|---|
| **T0 Working** (app state/Redis) | current goals, agent scratch, core blocks | none (in-context) | agent self-edit | tiny |
| **T1 Episodic** (OS index) | raw events + task trajectories | hybrid BM25+kNN | append-only, decayable | very high |
| **T2 Semantic** (OS index) | extracted facts | hybrid, RRF | **bi-temporal** (append+invalidate) | high |
| **T3 Experience/Skill** (OS index) | distilled lessons/SOPs | hybrid, gated | curated, scored, pruned | low (curated) |
| **T4 Graph** (OS edges/nodes; Neo4j later) | entities + relationships | BM25+kNN + ≤2-hop expansion | incremental upsert, bi-temporal | high |
| **Cross-cutting** | scope+ACL, version chain, audit, eval | — | — | — |

**Backbone config (locked from research):** 1024-dim embeddings (BGE-M3 class, matches Graphiti) + **SQfp16** quantization; Faiss HNSW (`m=16–24`, `ef_construction=128`); fusion via **`score-ranker-processor` RRF** (`rank_constant=60`; OpenSearch **pinned 3.1** — one code path, no `normalization-processor` fallback; decision D14 in the walking-skeleton plan). Shards 10–50 GB. Size off-heap RAM for `1.1*(2*dim+8*m)` bytes/vector with SQfp16 (`4*dim` only if fp32) **× replicas**, stay under `circuit_breaker_limit`.

---

## Locked decisions (with grounding)

| # | Decision | Why / source |
|---|----------|--------------|
| D1 | Hybrid BM25+kNN, RRF — not RAG-vs-FTS | Anthropic Contextual Retrieval (BM25 adds 35%→49%); OpenSearch/Elastic default; Zep/Letta/MemOS all hybrid |
| D2 | **Incremental** graph upsert, not batch | Zep/Graphiti per-episode pipeline; GraphRAG batch-recompute is a non-starter at scale |
| D3 | **Bi-temporal** invalidation (append, never delete) + version chain | Zep 4-timestamp model; MemOS Version Chain; gives audit/point-in-time — mandatory at these stakes |
| D4 | Async, **LLM-gated** write path | Mem0 two-phase; extraction is the cost center; gating controls hallucination |
| D5 | **Write-gating for experience memory is mandatory** | Experience-Following (arXiv 2505.16067): agents OBEY retrieved experience (r≈1) → bad records actively harm. Curated-small beats large-noisy (acc 38.7%→42.1% while shrinking memory) |
| D6 | **Provenance-as-ACL**, enforced at **query time** | Collaborative Memory bipartite reachability + subset semantics; dynamic revocation requires query-time filter, not index-time |
| D7 | Concurrent writes → OpenSearch **optimistic concurrency** (`if_seq_no`/`if_primary_term`) + version-as-new-row | Papers ALL punt on concurrent-write merge — this is our contribution |
| D8 | Graph DB (Neo4j/FalkorDB) only for **>2–3 hop** traversal | OpenSearch has no native traversal/joins; GeAR-style triple expansion covers ≤2 hops on OpenSearch |
| D9 | Continuous **eval harness** from day one | HaluMem (memory hallucination); experience-following health metric (input↔output similarity correlation) |

---

## Phased build

Each phase has a **done-when** gate. Phases are roughly sequential; T3/T4/ACL can parallelize after Phase 2.

### Phase 0 — Foundations & contracts
- Lock embedding model+dim, OpenSearch version (pinned 3.1 — plan D14), record schemas (T1–T4), scope/ACL field contract, ID strategy.
- Stand up eval-harness skeleton + a seed gold set (queries → expected memories).
- **Done-when:** schemas reviewed; OpenSearch cluster up with kNN enabled; RRF pipeline returns results on dummy data; eval harness runs green on the seed set.

### Phase 1 — Hybrid retrieval backbone (T1 + T2)
- Episodic + semantic indices; ingest API (sync append); query API (BM25+kNN → RRF → optional rerank); query-time validity + scope filters.
- **Done-when:** hybrid search beats BM25-only and kNN-only on the gold set; p95 latency target met; filtered-kNN recall verified (no post-filter collapse).

### Phase 2 — Write / extraction pipeline (async)
- Event ingest (sync) → queue → async LLM extract facts → reconcile: **ADD / UPDATE / INVALIDATE / NOOP** (Mem0 decision) writing **bi-temporally** (Zep: new edge sets old `invalid_at=new.valid_at`).
- Idempotency + replay; cheap extractor model + batching for cost.
- **Done-when:** contradictions invalidate (not delete); re-running ingest is idempotent; extraction cost/1k events measured and within budget.

### Phase 3 — Experience / skill memory (T3)
- Experience record `{task, context, trajectory, outcome, utility_score Φ, distilled_skill, retrieval_count, last_retrieved}`; distillation (MUSE reflect: sync per sub-task + async global merge/generalize).
- **Write-gating evaluator** (fine-tuned/LLM judge on outcome) before indexing; background prune job (low `retrieval_count` + low Φ).
- **Done-when:** bad experiences are gated out (measure on injected-bad-record test); curated-small-beats-large A/B reproduced; experience-following correlation tracked as a health metric.

### Phase 4 — Multi-agent scope + ACL
- Scope tiers `{private, team, org, shared, global}`; per-record provenance `{owner_user_id, contributing_agent_ids[], resource_ids[], created_at}`.
- Two bipartite reachability stores (user↔agent, agent↔resource) → translate to OpenSearch `bool` filter; subset (⊆) semantics via `terms_set`/precomputed `max_required_clearance`.
- Concurrent writes via optimistic concurrency; on 409 → re-read + merge or append-as-version; version chain (`parent_id` + monotonic `version`); audit log.
- **Done-when:** ACL revocation hides fragments at query time (cache-invalidation verified); concurrent-write conflict resolves without loss; full provenance/audit trail per record.

### Phase 5 — Incremental graph layer (T4)
- Entity + edge indices; incremental upsert with embed+BM25+LLM-judge dedup (LiCoMemory hyperlink-not-duplicate); GeAR-style triple expansion for ≤2-hop; bi-temporal edges.
- Decide graph DB adoption for deep traversal; periodic community-summary refresh (incremental drifts).
- **Done-when:** single-episode ingest adds entities/edges with no recompute; 2-hop "connect-the-dots" query works; dedup keeps entity count stable on repeated facts.

### Phase 6 — Scale & ops hardening
- Sharding plan; off-heap RAM sizing + circuit-breaker headroom (account for replicas); quantization (SQfp16/byte/PQ); hot/cold tiering (searchable snapshots); `force_merge` + warmup for read-heavy; streaming ingestion.
- Cost controls: selective extraction gate, cheap models, batching.
- **Done-when:** load test at target scale within latency + RAM budget; cost/write within budget; failover/restore drill passes.

### Phase 7 — Eval & safety gates (continuous)
- HaluMem-style memory-hallucination suite; experience-following health metric; retrieval-quality regression set; wire into CI/CD as release gates; dashboards.
- **Done-when:** hallucination rate measured and gated; dashboards live; a bad release is actually blocked by the gate in a drill.

---

## Top risks / open problems (honest)

| 🔴/🟡 | Risk | Mitigation / status |
|---|------|---------------------|
| 🔴 | **Concurrent-write merge** in shared memory | No paper solves it — optimistic concurrency + version chain + LLM-merge is our design; needs hardening |
| 🔴 | **Experience poisoning** (one bad lesson propagates via following) | Mandatory write-gate + utility scoring + prune + quarantine tier (quarantine is unsolved in literature — we invent) |
| 🔴 | **Extraction cost at billions scale** | Selective "worth remembering" gate, cheap extractor, batching; this is the dominant cost line |
| 🟡 | **Subset-ACL vs kNN** (post-filter recall collapse) | OpenSearch `efficient_filter`/filtered-kNN; precompute clearance; verify recall under filter |
| 🟡 | **Off-heap vector RAM / circuit breaker** | Quantization + shard sizing + replica-aware capacity; verify warm/cold-tier kNN for your version (UNVERIFIED) |
| 🟡 | **Community/graph drift** under incremental updates | Scheduled refresh job (Zep notes communities "gradually diverge") |
| 🟡 | **Dynamic ACL revocation leaking via caches** | Query-time enforcement only; invalidate ANN/result caches on edge drop |

## Don't reinvent
Greenfield the **storage/scale/ACL/concurrency** layers (where OSS is weak and your scale is unique). **Lift the solved parts**: Mem0/Letta reconciliation logic (ADD/UPDATE/DELETE), Graphiti's incremental ingestion + bi-temporal model, GeAR's triple-expansion — check licenses, port the patterns rather than rebuilding from zero.

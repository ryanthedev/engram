# Agent Memory: Reference-Architecture Synthesis

Source: deep-read of 6 production/research memory systems (paper + open-source repo each), 2026-06-29.
Systems: **Zep/Graphiti, Mem0, MemGPT/Letta, A-MEM, Microsoft GraphRAG, MemOS.**

---

## Part 1 — Comparison Matrix

### 1a. Data structures

| System | Core unit | Concrete schema (verified from repo) | Backend | Graph? |
|--------|-----------|--------------------------------------|---------|--------|
| **Zep/Graphiti** | Bi-temporal KG node/edge | `EpisodicNode`(uuid,content,valid_at,source) → `EntityNode`(name_embedding,summary,attributes) → `CommunityNode`. `EntityEdge` = fact triplet w/ `valid_at`/`invalid_at`/`created_at`/`expired_at`+`fact_embedding` | Neo4j (+FalkorDB/Neptune) | Native, primary |
| **Mem0** | NL fact string | `{id, memory(text), event, metadata, user_id}`. Mem0ᵍ adds entity nodes (type,embedding,timestamp) + relationship triplets (vₛ,r,v_d) | Qdrant (+Neo4j for graph) | Optional (Mem0ᵍ) |
| **MemGPT/Letta** | Memory block + Passage | Block `{label, value, limit(2000ch), description, read_only}`; recall=message store; archival=`Passage`(text+embedding) | pgvector (Postgres) | No (filesystem variant) |
| **A-MEM** | Zettelkasten note | `MemoryNote{content, keywords[], context, tags[], category, links[], evolution_history[], timestamp, retrieval_count}` | ChromaDB | Soft (note links, underused) |
| **GraphRAG** | 7 Parquet tables | `entities`, `relationships(source,target,description,weight)`, `communities(level,parent,children)`, `community_reports(summary,findings,rank)`, `text_units`, `documents`, `covariates` | Parquet + LanceDB | Native, primary |
| **MemOS** | MemCube | Metadata header (Timestamp, Access Control, **Lifespan Policy/TTL**, Priority, **Version Chain**) + payload. 3 types: plaintext / activation(KV-cache) / parametric(LoRA) | Neo4j + Qdrant + FTS5 | Native (plaintext tier) |

### 1b. Operations (the lifecycle — the part that actually matters)

| Op | Zep/Graphiti | Mem0 | MemGPT/Letta | A-MEM | GraphRAG | MemOS |
|----|-------------|------|--------------|-------|----------|-------|
| **Write/extract** | LLM extracts entities+facts per episode (async) | LLM extracts facts from last 10 msgs (Phase 1) | Agent self-writes via `archival_memory_insert` (sync) | LLM generates keywords/tags/context per note (sync) | LLM gleaning per text-unit (batch) | MemReader parses NL→MemoryCall (async) |
| **Consolidate/merge** | LLM entity+fact dedup vs existing | part of UPDATE decision | recursive summarization on context pressure | re-index every `evo_threshold` (full re-embed) | Leiden clustering + bottom-up report gen (batch) | MemLifecycle "Merged" by access/decay |
| **Update/conflict** | **bi-temporal invalidation** (set `invalid_at`, never delete) | **LLM ADD/UPDATE/DELETE/NOOP** decision (Algorithm 1) | `core_memory_replace` overwrites block | **memory evolution** — new note rewrites neighbors' context/tags | batch recompute (`graphrag update`) | versioned diffs (append/merge/overwrite) |
| **Forget/evict** | soft (temporal supersession only) | DELETE on contradiction | FIFO eviction→recall when context full | none (tracked, unused) | **none** (no delete primitive) | **TTL/Lifespan → Archived → Expired** |
| **Retrieve** | hybrid BM25+vector+graph BFS, RRF rerank (sync, no LLM) | vector top-10 (Mem0ᵍ +graph traversal) | hybrid vector+FTS, RRF | vector cosine top-k (no lexical) | local(vector+graph) / global(map-reduce reports) | MemScheduler: type+injection by similarity/freq/decay |
| **LLM on write path?** | Yes (heavy) | Yes | Yes (agent) | Yes (2 calls/insert) | Yes (heavy, batch) | Yes |
| **LLM on read path?** | No | No | Yes (agent calls tools) | No | Yes (global) | No |

### 1c. Retrieval config + benchmarks

| System | Modes | Embedding | Fusion | Key numbers |
|--------|-------|-----------|--------|-------------|
| Zep | BM25 + vector + graph | text-embedding-3-small (1536) | RRF/MMR/node-distance/cross-encoder | DMR 94.8%; LongMemEval +18.5% acc, −90% latency |
| Mem0 | vector (+graph) | text-embedding-3-small | none | LoCoMo 66.9% / Mem0ᵍ 68.4%; ~7k tok, p95 1.44s |
| Letta | vector + FTS | text-embedding-3-small | RRF | **LoCoMo 74.0%** (filesystem); beats Mem0 graph |
| A-MEM | vector only | all-MiniLM-L6-v2 (384) | none | ≥2 LLM calls/write; O(N) re-index |
| GraphRAG | graph + community-summ + vector | text-embedding-3-large (3072) | n/a (map-reduce) | global sensemaking; expensive to build, not incremental |
| MemOS | FTS5 + vector + graph + KV | configurable | hybrid filters+vector+graph | LoCoMo +38.97% acc, +159% temporal, −60.95% tokens |

### 1d. Cross-cutting patterns (what's TRUE across all 6)

1. **Write is LLM-heavy, read is not.** Every system uses an LLM to turn raw turns into structured memories; retrieval itself is mostly a plain search call (except agentic/global).
2. **Conflict resolution is THE differentiator**, and there are exactly two philosophies: **mutate** (Mem0 UPDATE/DELETE, Letta replace, A-MEM evolve) vs **append + invalidate** (Zep/MemOS bi-temporal — never lose history). Bi-temporal wins for auditability; mutation wins for storage cost.
3. **Forgetting is rare and almost always soft.** Only MemOS has real TTL/lifecycle; most just invalidate or never forget. Storage grows monotonically — plan for it.
4. **Hierarchy is the norm, flat is the exception.** Letta (core/recall/archival), Zep (episodic/entity/community), GraphRAG (Leiden levels), MemOS (type lifecycle) all tier. Only A-MEM and base Mem0 are flat.
5. **Simplicity is competitive.** Letta's filesystem+grep (74%) beat Mem0's graph (68.5%). Don't reach for a graph until a query type forces it.
6. **Hybrid retrieval is the default backbone**; graph is an *added* signal for multi-hop, not a replacement for lexical+vector.

---

## Part 2 — Synthesized Reference Architecture (OpenSearch-centered)

Design target: hybrid backbone + hierarchy + optional graph, on OpenSearch (which does BM25 + kNN + RRF natively).

### Tier model (4 tiers)

| Tier | Holds | Store | Retrieval | Mutability |
|------|-------|-------|-----------|------------|
| **T0 Working/Core** | current goals, user profile, scratch | App state / Redis (in-context) | none (always present) | agent self-edit (Letta blocks) |
| **T1 Episodic** | raw events/turns, immutable log | OpenSearch index `episodic` | hybrid BM25+kNN | append-only |
| **T2 Semantic** | extracted facts/entities | OpenSearch index `semantic` | hybrid BM25+kNN, RRF | bi-temporal (append+invalidate) |
| **T3 Graph (optional)** | entity→relation→entity edges | OpenSearch `edges` index OR Neo4j | graph expansion on T2 hits | bi-temporal |

### Concrete schemas

**T0 — Core memory block** (Letta-style, in app state, not OpenSearch):
```json
{ "block_id": "uuid", "label": "user_profile", "value": "Name: Ryan...",
  "char_limit": 2000, "read_only": false }
```

**T1 — Episodic event** (OpenSearch `episodic`):
```json
{ "event_id": "uuid", "session_id": "...", "user_id": "...",
  "role": "user|assistant|tool", "text": "...", "text_embedding": [/*1024*/],
  "created_at": "2026-06-29T...", "source": "chat|doc|tool" }
```

**T2 — Semantic fact** (OpenSearch `semantic`; Zep bi-temporal + Mem0 fact + provenance):
```json
{ "fact_id": "uuid", "fact": "Ryan prefers OpenSearch over Pinecone",
  "fact_embedding": [/*1024*/],
  "subject": "Ryan", "predicate": "prefers", "object": "OpenSearch",
  "entity_ids": ["ent:ryan","ent:opensearch"],
  "source_episode_ids": ["uuid"],
  "valid_at": "2026-06-29", "invalid_at": null,
  "created_at": "2026-06-29", "expired_at": null,
  "user_id": "...", "importance": 0.8, "access_count": 0, "last_accessed": null }
```

**T3 — Graph edge** (only if multi-hop needed):
```json
{ "edge_id":"uuid", "source":"ent:ryan", "relation":"prefers",
  "target":"ent:opensearch", "fact_id":"uuid",
  "valid_at":"2026-06-29", "invalid_at":null, "weight":1.0 }
```

### OpenSearch index mapping (T2 semantic — the hybrid backbone)
```json
PUT /semantic
{
  "settings": { "index.knn": true },
  "mappings": { "properties": {
    "fact":           { "type": "text", "analyzer": "english" },          // BM25 / FTS
    "fact_embedding": { "type": "knn_vector", "dimension": 1024,
                        "method": { "name":"hnsw", "space_type":"cosinesimil", "engine":"faiss",
                                    "parameters": { "m": 16, "ef_construction": 128,
                                                    "encoder": { "name": "sq", "parameters": { "type": "fp16", "clip": true } } } } },
    "subject": {"type":"keyword"}, "predicate": {"type":"keyword"}, "object": {"type":"keyword"},
    "entity_ids": {"type":"keyword"}, "source_episode_ids": {"type":"keyword"},
    "valid_at": {"type":"date"}, "invalid_at": {"type":"date"},
    "created_at": {"type":"date"}, "expired_at": {"type":"date"},
    "user_id": {"type":"keyword"}, "importance": {"type":"float"},
    "access_count": {"type":"integer"}, "last_accessed": {"type":"date"}
  }}
}
```

**Hybrid query** = OpenSearch `hybrid` query (one BM25 clause + one kNN clause) run through a search pipeline using the **score-ranker/RRF processor** (rank-based — zero-tuning, robust across scales). *Engram pins OpenSearch 3.1 and uses RRF only (plan D14); the normalization-processor alternative was dropped for a single code path.* Always filter `invalid_at: null AND expired_at: null AND user_id = X`. Optional cross-encoder rerank on the fused top-k.

### Operations API (pseudocode)

```
write(event):
    append(episodic, event)                      # T1, sync, cheap, no LLM
    enqueue_async(extract_and_reconcile, event)  # T2, async (Zep/MemOS pattern)

extract_and_reconcile(event):                    # async worker
    facts = LLM.extract(recent_window(event))    # Mem0 Phase 1
    for f in facts:
        cands = hybrid_search(semantic, f, top_k=10, filter=valid)
        action = LLM.decide(f, cands)             # Mem0: ADD|UPDATE|INVALIDATE|NOOP
        match action:
          ADD        -> index(semantic, f)
          UPDATE     -> index new f; set old.invalid_at=now (Zep bi-temporal, keep history)
          INVALIDATE -> set old.invalid_at=now
          NOOP       -> skip
        if graph_enabled: upsert_edges(f.subject, f.predicate, f.object)

retrieve(query, user_id, tiers=[T2,T1]):
    hits = hybrid_search(tier, query, filter=valid & user_id)   # BM25+kNN, RRF
    if graph_enabled and is_multihop(query):
        hits += graph_expand(hits, depth=1)                     # Zep node-distance
    hits = rerank(hits)                                         # optional cross-encoder
    bump(access_count, last_accessed)                           # for forgetting
    return hits

consolidate():                                   # scheduled / threshold (A-MEM, Letta)
    cluster near-duplicate facts -> LLM merge/summarize -> invalidate originals

forget():                                        # scheduled (MemOS lifecycle)
    set expired_at where importance low AND access_count low AND age > TTL
    hard-purge episodic older than retention window
```

### Decision rules

| Question | Rule |
|----------|------|
| Lexical, vector, or both? | **Both** — hybrid is the backbone. BM25 for IDs/names/exact terms, kNN for semantic recall. |
| When add the graph (T3)? | Only when you have **multi-hop / relationship / "global" queries**. Start without it (Letta lesson). |
| Mutate or bi-temporal? | **Bi-temporal** (append + `invalid_at`) if you need audit/point-in-time; mutate if storage-constrained. |
| Sync or async writes? | Episodic append **sync**; fact extraction + reconciliation **async** (keeps latency low). |
| When consolidate / forget? | Scheduled job + thresholds; soft-invalidate first, hard-purge only past a retention window. |

**The 80/20:** OpenSearch hybrid (T1+T2) + bi-temporal reconciliation gets you most of the value. Graph (T3) and parametric/KV tiers (MemOS) are advanced add-ons, not the starting point.

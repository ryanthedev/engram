# Engram

Agent memory platform — hybrid retrieval (OpenSearch **BM25 + kNN + RRF**), tiered memory
(**working / episodic / semantic / experience / graph**), async **LLM-gated bi-temporal** writes,
and multi-agent **provenance-as-ACL**.

**Status:** pre-build. Walking-skeleton plan approved; Phases 0–2 next.

## Why

Long context ≠ memory (BEAM: structured memory beats a 1M-token window by ~75%). Agents need
durable, retrievable, *trustworthy* memory: task experience they can reuse, a multi-hop knowledge
base, and shared memory across agents with access control and concurrent-write safety.

## Where this is going

- **Vision site (the map):** [`docs/vision/index.html`](docs/vision/index.html) — architecture,
  data flow, ACL model, and the full 8-phase roadmap as mermaid diagrams.
- **Build plan (the contract):** [`.code-foundations/plans/2026-06-29-engram-walking-skeleton.md`](.code-foundations/plans/2026-06-29-engram-walking-skeleton.md)
- **Research (the grounding):** [`.code-foundations/research/`](.code-foundations/research/) —
  `REFERENCE-ARCHITECTURE.md`, `GREENFIELD-BUILD-PLAN.md` (deep-reads of Zep, Mem0, Letta, A-MEM,
  GraphRAG, MemOS, Mem-α, MUSE, Collaborative Memory, G-Memory, GeAR …).

## Stack

Go · OpenSearch 3.1 (pinned; Faiss HNSW kNN + BM25 + RRF) · BGE-M3 1024-dim embeddings (co-located) ·
gRPC/protobuf · Neo4j/FalkorDB later (only for >2-hop graph traversal).

## Layout

```
cmd/engramd        # service entrypoint
internal/
  memory/          # tier models + record schemas
  retrieval/       # hybrid search (BM25 + kNN + RRF)
  ingest/          # async write + extraction + reconciliation
  acl/             # provenance-as-ACL (Phase 4)
  graph/           # incremental entity/edge graph (Phase 5)
  store/           # OpenSearch client + index templates
  api/             # gRPC service
api/proto          # protobuf contracts
deploy/            # cluster + service deploy
docs/vision/       # the roadmap website
```

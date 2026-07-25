# Engram

Agent memory platform — hybrid retrieval (OpenSearch **BM25 + kNN + RRF**), tiered memory
(**working / episodic / semantic / experience / graph**), async **LLM-gated bi-temporal** writes,
and multi-agent **provenance-as-ACL**.

**Status:** built and running. The gRPC service, the `engram` CLI, and the MCP server are in daily
local use — the MCP surface backs a live Claude Code client over a compose stack. Deploy targets:
local compose (`deploy/local`), LocalStack, and AWS (`deploy/aws`).

## Why

Long context ≠ memory (BEAM: structured memory beats a 1M-token window by ~75%). Agents need
durable, retrievable, *trustworthy* memory: task experience they can reuse, a multi-hop knowledge
base, and shared memory across agents with access control and concurrent-write safety.

## Docs

- **Architecture (as built):** [`docs/architecture.md`](docs/architecture.md) — components,
  service assembly, the write/read paths, memory tiers, ACL, the knowledge platform, and
  deployment topology.
- **API surface reference:** [`docs/api.md`](docs/api.md) — gRPC RPCs, the `engram` CLI, MCP
  tools, the harvester, auth/tokens, config, and HTTP endpoints.
- **Runbooks:** [`docs/runbooks/`](docs/runbooks/) — worker lag, DLQ, node failure, snapshot
  restore, secrets rotation, rollback, and local-stack restart durability on macOS.

## MCP

Ten tools over stdio: `memory_ingest` / `memory_search` / `memory_read` / `memory_status`, and
`knowledge_ingest` / `knowledge_search` / `knowledge_collections` / `knowledge_create_collection` /
`knowledge_update_collection` / `knowledge_delete`.

Search results are **text-only** — three tab-separated columns (id, date, matched text) under a
source header, ripgrep-shaped. The id is self-addressing (`<source>:<id>`); pass it to
`memory_read` for the full record. `memory_read` is the one tool that still returns structured
JSON, because it returns a record to parse rather than a list to scan.

The client needs two environment variables:

```
ENGRAM_ADDR=localhost:7071   # gRPC address of the server
ENGRAM_TOKEN=egm_…           # bearer token, minted per store
```

**`ENGRAM_ADDR` is not optional in practice.** `engram-mcp` falls back to `localhost:7070` when it
is unset, and the local compose stack listens on **7071** — so an unset address silently reaches a
different server whose token store has never seen your token, and every call fails as
`Unauthenticated`. Tokens are per-store: mint against the OpenSearch backing the server you intend
to talk to (`engram token create --url http://localhost:9201` for the compose stack), not the one
you happen to have open.

## Stack

Go · OpenSearch 3.1 (pinned; Faiss HNSW kNN + BM25 + RRF) · **BGE-M3** 1024-dim embeddings ·
gRPC/protobuf · Neo4j/FalkorDB later (only for >2-hop graph traversal).

Embeddings run out-of-process. The default compose stack uses a deterministic **fake embedder** —
fine for tests and `make e2e`, useless for real retrieval. Real vectors come from
`deploy/local/embed-real/run-host.sh`, which runs `BAAI/bge-m3` **natively on the host** with
`EMBED_DEVICE=mps` and proxies it into the containers: macOS passes no Metal into the podman VM, so
an in-container embedder can only run CPU and gets OOM-killed in a 4 GB VM. Point the server at it
with `-embed-url`.

## Layout

```
cmd/
  engram-server/     # the service entrypoint
  engram/            # CLI: search, ingest, status, export, purge, audit,
                     #      plus token / acl / quarantine / knowledge admin
  engram-mcp/        # MCP server over stdio
  engram-harvester/  # knowledge ingestion (arXiv, markdown dirs)
  engram-embed-server/, engram-extract-shim/, engram-eval/, engram-deploy/, …
internal/
  memory/            # tier models + record schemas
  retrieval/         # hybrid search (BM25 + kNN + RRF)
  ingest/ enrich/    # async write, extraction, reconciliation, embedding backfill
  experience/ graph/ # experience tier + incremental entity/edge graph
  knowledge/         # collections, fragments, vault export
  auth/ authgrpc/    # token barricade + gRPC interceptor
  acl/ knowledgeauth/# provenance-as-ACL + per-collection RBAC
  store/             # OpenSearch client + index templates
  api/ server/       # gRPC service
  mcp/               # MCP tool surface + response rendering
api/proto            # protobuf contracts
deploy/local, deploy/aws
docs/vision/         # the roadmap website
```

## Where this is going

- **Vision site (the map):** [`docs/vision/index.html`](docs/vision/index.html) — architecture,
  data flow, ACL model, and the full 8-phase roadmap as mermaid diagrams.
- **Build plan (the contract):** [`.code-foundations/plans/2026-06-29-engram-walking-skeleton.md`](.code-foundations/plans/2026-06-29-engram-walking-skeleton.md)
- **Research (the grounding):** [`.code-foundations/research/`](.code-foundations/research/) —
  `REFERENCE-ARCHITECTURE.md`, `GREENFIELD-BUILD-PLAN.md` (deep-reads of Zep, Mem0, Letta, A-MEM,
  GraphRAG, MemOS, Mem-α, MUSE, Collaborative Memory, G-Memory, GeAR …).

# Engram — API Surface Reference

Engram exposes one gRPC contract and several clients over it. This document is the
authoritative catalogue of every outward-facing surface: gRPC RPCs, the `engram` CLI,
the MCP tools, the harvester, auth/tokens, configuration, and HTTP endpoints.

- **Architecture:** [`docs/architecture.md`](./architecture.md).
- **Proto contract:** [`api/proto/engram.proto`](../api/proto/engram.proto).

Everything routes through the single `engram.v1.Engram` gRPC service. Every RPC is
authenticated by a bearer token in the gRPC `authorization` metadata header
(`Bearer <raw-token>`); the server derives tenancy and provenance from the resolved
identity, never from client-supplied fields.

---

## 1. gRPC service — `engram.v1.Engram`

### Memory RPCs

| RPC | Purpose | Notes |
|---|---|---|
| `Ingest` | Durably append one episodic event; the append is the enqueue for async extraction. | `event_id` required (idempotency, D13); empty → `INVALID_ARGUMENT`. |
| `Search` | One hybrid query (BM25 + kNN → RRF), tenancy- and validity-filtered. | `valid_only` restricts to currently-valid facts; `k` clamped server-side. |
| `Status` | Server health, resolved identity, coarse per-tier counts. | Backs `engram status` / `memory_status`. |
| `Audit` | A fact's provenance + full bi-temporal version history. | Unauthorized/unknown id → `NOT_FOUND` (no existence oracle). |
| `Read` | One record's full content by `(id, source)`. | `source` ∈ `episodic`\|`semantic`; `graph` → `UNIMPLEMENTED`. Fail-closed by id. |
| `Export` | One bounded page of the tenant's live, ACL-filtered graph (entities then edges) + opaque cursor. | Cursor carries no tenancy; empty `next_cursor` = exhausted. |

### Knowledge RPCs

Document collections: BM25/FTS only, no fact extraction, no embeddings. Upsert-by-id +
mark-and-sweep delete. Filters and sort name **registry-mapped** fields only.

| RPC | Purpose | Notes |
|---|---|---|
| `KnowledgeIngest` | Bulk-upsert one harvest batch into a collection. | Rows stamped with `harvest_id`/`harvested_at`; whole-batch retry is idempotent. Requires `harvester`/`admin` role. |
| `KnowledgeSearch` | BM25 query over a collection with generic field filters + sort. | No kNN/RRF/tier fusion. |
| `KnowledgeCollections` | List readable collections with mappings, count, staleness. | Caller sees only collections it may read. |
| `KnowledgeDelete` | Mark-and-sweep: hard-delete rows from a source whose `harvest_id != current_harvest_id`. | Empty `current_harvest_id` refused (would wipe the collection). |
| `CreateCollection` | Register a new collection, provision its index live. | Duplicate name → `ALREADY_EXISTS`. Requires `admin`. |
| `UpdateCollection` | Amend a collection's spec (fields, access policy); mappings applied live. | Requires `admin`. |

Generic filter vocabulary (`Predicate`): `TERM` (exact), `RANGE` (gte/lte), `PREFIX`
(leading-substring) on a declared-filterable field; `SortKey` orders by a
declared-sortable field. An unknown field → `INVALID_ARGUMENT` naming the valid fields.

---

## 2. CLI — the `engram` binary

Entry `cmd/engram/main.go` → `internal/cli`. Two backend classes: **OpenSearch-direct
admin** (token/acl/quarantine — bypass gRPC so bootstrapping a token doesn't itself
need one) and **authenticated gRPC** (ingest/search/status/audit/export).

Address/token resolution: `-addr` → `$ENGRAM_ADDR` → `localhost:7070`; `-token` →
`$ENGRAM_TOKEN`. Admin `--url` → `$ENGRAM_OPENSEARCH_URL` → `http://localhost:9200`.

| Command | Key flags | Backs onto |
|---|---|---|
| `engram token create` | `--tenant`* `--user`* `--agent` `--ttl` (720h) `--url` | `TokenIssuer.Issue` (OpenSearch) |
| `engram token list` | `--tenant`* `--user`* `--url` | `TokenIssuer.List` |
| `engram token revoke <handle>` | `--url` | `TokenIssuer.Revoke` |
| `engram acl grant` | `--tenant --user` + one of `--agent`/`--team`/`--org`, `--url` | `ACLEdgeStore.PutEdge` |
| `engram acl revoke` | (same as grant) | `ACLEdgeStore.DeleteEdge` |
| `engram acl list` | `--tenant --user --url` | `ACLEdgeStore.ListEdges` |
| `engram quarantine list` | `--tenant`* `--url` | `experience.Store.ListQuarantine` |
| `engram quarantine release <fingerprint>` | `--tenant`* `--url` | `experience.Store.Release` |
| `engram ingest` | `--event-id`* `--text`* `--source` `--scope` `--team` `-addr` `-token` | gRPC **Ingest** |
| `engram search <query>` | `-k` (10) `-addr` `-token` | gRPC **Search** (`valid_only=true`) |
| `engram status` | `-addr` `-token` | gRPC **Status** |
| `engram audit <fact-id>` | `-addr` `-token` | gRPC **Audit** |
| `engram export <dir>` | `--force` `-addr` `-token` | gRPC **Export** (paginated → Obsidian vault) |

(`*` = required.) The CLI exposes **no `read` or `knowledge*` subcommands** — those are
MCP-only / harvester-only surfaces. `ingest --text` accepts optional pipe-delimited
`fact:`/`retract:`/`experience:` directive lines for deterministic fixtures.

### Other binaries

| Binary | Role |
|---|---|
| `engram-server` | The production gRPC daemon (see architecture doc). |
| `engramd` | Empty placeholder directory — no code. |
| `engram-harvester` | One-shot knowledge harvester driven by a YAML manifest (§4). |
| `engram-mcp` | MCP (JSON-RPC/stdio) server mapping tools onto the gRPC API (§3). |
| `engram-apply-templates` | Idempotently applies the OpenSearch cluster contract; refuses non-3.1. |
| `engram-embed-server` | Deterministic TEI-style embedding stub (`POST /embed`) for e2e. |
| `engram-extract-shim` | Host-side OpenAI-compatible extraction endpoint delegating to a headless agent CLI. |
| `engram-stub-llm` | Deterministic OpenAI-compatible chat-completions stub for extraction in e2e. |
| `engram-eval` | Retrieval eval harness (recall@k, MRR, nDCG@k) over a gold set. |
| `engram-goldgen` | Regenerates the checked-in seed gold set (train/holdout split). |
| `engram-perf` | Perf harness: seed N docs, drive concurrent Search, report p50/p95/p99. |
| `engram-loadtest` | Load test at S1 pace; reports latency, worker lag, vector RAM. |
| `engram-deploy` | Idempotent Go AWS deploy CLI (`-env staging\|prod`, `-rollback`). |

---

## 3. MCP tools — `engram-mcp`

JSON-RPC 2.0 over stdio (protocol `2024-11-05`, server `engram-mcp` v0.1.0). Each tool
maps 1:1 to a gRPC RPC via the shared client. Authenticated by `$ENGRAM_TOKEN`.

> **`docs/mcp.md` is stale** — it documents 3 tools; the code exposes **10** (4 memory
> + 6 knowledge).

| Tool | Args | Returns | RPC |
|---|---|---|---|
| `memory_ingest` | `event_id`* `text`* `source` | `{id}` | Ingest |
| `memory_search` | `query`* `k` | budget-packed ranked hits (spills oversized sets to `overflow_path`) | Search |
| `memory_read` | `id`* `source`* (`episodic`\|`semantic`; `graph` rejected) | full record (episodic text; or fact + provenance + history) | Read |
| `memory_status` | — | `{healthy, tenant/user/agent, episodic_count, semantic_count, opensearch_version}` | Status |
| `knowledge_ingest` | `collection`* `source`* `harvest_id`* `docs[]`* | `{indexed}` (harvester/admin) | KnowledgeIngest |
| `knowledge_search` | `collection`* `query` `filters[]` `sort[]` `k` | budget-packed hits, spill to `overflow_path` | KnowledgeSearch |
| `knowledge_collections` | — | `{collections[]}` (mappings, count, staleness) | KnowledgeCollections |
| `knowledge_delete` | `collection`* `source`* `current_harvest_id`* | `{deleted}` (harvester/admin) | KnowledgeDelete |
| `knowledge_create_collection` | `name`* `text_field` `mappings` `public` `roles[]` | `{created}` (admin) | CreateCollection |
| `knowledge_update_collection` | (same spec) | `{updated}` (admin) | UpdateCollection |

Tool-level failures return an `isError:true` result; protocol misuse returns a JSON-RPC
error. Tuning env: `$ENGRAM_MCP_SEARCH_BUDGET_BYTES`, `$ENGRAM_MCP_SPILL_DIR`.

---

## 4. Harvester — `engram-harvester`

A one-shot process (cron/systemd-driven) that reads a YAML manifest and feeds documents
into knowledge collections. It calls **only** the knowledge RPCs:
`KnowledgeCollections` (validation), `KnowledgeIngest` (per batch), `KnowledgeDelete`
(post-run mark-and-sweep). Token from **`$ENGRAM_HARVESTER_TOKEN`** (env only).

**CLI flags:** `--manifest`* `--collection` (repeatable/CSV filter) `--source`
(repeatable/CSV filter) `--addr` (:7070) `--batch-size` (500) `--timeout` (6h).
`--once`/`--backfill` are no-ops (always one-shot).

**Manifest:** top-level `collections:` list; each `{name, sources: [{type, ...}]}`.
Collection names must match `^[a-z0-9][a-z0-9_-]*$` and exist live on the server.

**Source types:**

| type | required / optional keys |
|---|---|
| `arxiv-kaggle` | `path`* · `filter`, `dump_date` |
| `arxiv-oaipmh` | `base_url`, `set`, `metadata_prefix`, `lookback` |
| `github-repos` | `repos`* · `files`, `base_url`, `max_file_bytes` |
| `web-crawl` | `seeds`* · `max_pages` (100), `max_page_bytes`, `delay`, `max_frontier`, `user_agent` |

`web-crawl` is **SSRF-guarded**: a dialer control hook blocks private/dangerous IPs on
every dial (loopback, RFC1918, link-local, IPv6 ULA/link-local, multicast,
unspecified); redirects are capped at 10 and cross-host redirects checked; robots.txt
and a per-host politeness delay are honored.

---

## 5. Auth & tokens

Opaque **256-bit** bearer tokens, prefix **`egm_`** + base64url. Only the SHA-256 hash
is stored; the raw token is shown once at issuance. Verify = hash → point-lookup →
constant-time compare → revocation/TTL check.

- **Identity:** `{TenantID, UserID, AgentID, Roles[]}`. `Roles` is the RBAC claim set,
  normalized at mint and derived **only** from the verified token record — never from
  request fields.
- **Roles in use:** `harvester` and `admin`. `harvester` or `admin` gates knowledge
  ingest/delete; `admin` gates collection create/update. Memory-path tokens carry no
  roles (scope/ACL-based instead).
- The barricade is the gRPC unary interceptor `authgrpc.UnaryServerInterceptor`; all
  typed rejections collapse to one opaque "unauthenticated" (no detail leak).

Tokens are minted with `engram token create` (OpenSearch-direct, so it does not itself
need a token).

---

## 6. Configuration

### `engram-server` flags

| Group | Flags (defaults) |
|---|---|
| Core | `-addr` (:7070) · `-url` (`$ENGRAM_OPENSEARCH_URL`) · `-metrics-addr` (:9464) |
| Embedding | `-embed-url` (empty→fake) · `-embed-model` (BAAI/bge-m3) · `-embed-revision` · `-enrich-interval` (2s) · `-enrich-batch` (50) |
| Extraction | `-extract-url` (empty→rule) · `-extract-model` · `-extractor-version` (v1) · `-extract-price-in/-out` · `-budget-per-1k-usd` (5.0, kill-switch) |
| Outbox worker | `-workers` (2) · `-claim-batch` (16) · `-claim-lease` (1m) · `-poll-interval` (2s) · `-max-attempts` (5) · `-sweep-interval` (30s) |
| Experience | `-gate-url` · `-gate-model` (gpt-4o-mini) · `-prune-phi-max` (0.2) · `-prune-retrieval-max` (0) · `-prune-interval` (5m) |
| Graph | `-graph-judge-url` · `-graph-judge-model` (gpt-4o-mini) · `-graph-expand-depth` (≤2) |
| Telemetry / indices | `-metrics-interval` (5s) · `-episodic-index` · `-semantic-index` · `-ledger-index` |

### Environment variables

| Var | Used by | Meaning |
|---|---|---|
| `ENGRAM_TOKEN` | CLI, engram-mcp | Client bearer token |
| `ENGRAM_ADDR` | CLI, harvester | gRPC address (default `localhost:7070`) |
| `ENGRAM_OPENSEARCH_URL` | CLI admin, server | OpenSearch base URL (default `http://localhost:9200`) |
| `ENGRAM_HARVESTER_TOKEN` | harvester | Harvester's bearer token (required) |
| `ENGRAM_MCP_SEARCH_BUDGET_BYTES`, `ENGRAM_MCP_SPILL_DIR` | engram-mcp | Search budget / overflow spill dir |
| `ENGRAM_HALU_JUDGE_URL`, `ENGRAM_HALU_JUDGE_MODEL` | engram-eval | Hallucination judge |
| `SHIM_BACKEND`, `SHIM_MODEL`, `SHIM_TIMEOUT` | engram-extract-shim | Backend agent CLI config |

---

## 7. HTTP endpoints

There is **no HTTP API for memory or knowledge** — those are gRPC-only. HTTP exists
only for telemetry and the stub/shim sidecars.

| Endpoint | Port | Where |
|---|---|---|
| `GET /metrics` (Prometheus) | :9464 (`-metrics-addr`) | engram-server |
| `POST /embed`, `GET /health` | :8081 | engram-embed-server |
| `POST /chat/completions`, `GET /health` | :8082 | engram-stub-llm |
| `POST /chat/completions`, `GET /health` | :8088 | engram-extract-shim |

The main daemon has no `/health` endpoint — health is the gRPC `Status` RPC.

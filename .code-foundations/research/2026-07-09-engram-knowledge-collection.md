# Engram Knowledge Collection — arXiv metadata as first corpus

**Summary:** Extend engram from a memory-only store into a knowledge platform by adding a second, document-oriented **collection** type (structured **full-text / BM25** search over a bulk-ingested corpus), with arXiv CS paper *metadata* as the first corpus and paper-grabber as the first consumer.

**Date:** 2026-07-09
**Status:** confirmed (grill + cold-read verification passed 2026-07-10)

**Still open:**
- Where the Layer-2 sources manifest + harvester live — inside paper-grabber, or a dedicated small harvester tool shared by future consumers (see Two-layer config).

**Decided since first draft (2026-07-09):**
- **Retrieval is FTS / BM25 only in v1 — no embeddings.** Embeddings are a deferred, additive backfill layer that reads docs already in the store and adds vectors with no re-harvest (see Retrieval Model).
- **Build the collection abstraction generically in v1** — a lightweight registry — because the store must hold other document types (websites, notes) beyond papers.
- Chunking of long documents is deferred along with embeddings (FTS indexes long text directly).
- **Nightly OAI-PMH incremental ships in v1** (not deferred to periodic full re-dumps) — freshness is the reason to localize arXiv, and the incremental is cheap (`from=<yesterday>` + resumption tokens). Paired with a **staleness signal**: `knowledge_collections` exposes the newest `harvested_at` / paper date per collection, so a *stalled* harvest is visible rather than silently serving stale results.
- **Mark-and-sweep deletion ships in v1** — upsert-by-id handles changed docs, a per-run `harvest_id` stamp + `knowledge_delete` sweep removes orphaned/withdrawn ones (incremental SHA-diff harvests carry explicit per-file deletes). Knowledge is the mutable+deletable store, in contrast to append-only memory.
- **Query API is shaped for an LLM caller** — generic registry-validated predicate filters + sort, optional filters, on-demand schema discovery via `knowledge_collections`, self-correcting errors, and lean responses reusing memory_search's budget-packer + `overflow_path` spill (`internal/mcp/budget.go`).
- **Memory boundary is behavioral only** — no code/API/reconciliation change; transient resource contention during bulk ops is acceptable (no hard isolation in v1).
- **Never-restart registry (Option A)** — Layer-1 registry's source of truth is an OpenSearch `knowledge-collections` **meta-index** (YAML is an optional idempotent boot seed), with runtime `knowledge_create_collection` / `knowledge_update_collection` admin RPCs. No engram restart for *any* collection change: create, add-field, field-type change (new index + reindex + alias swap), or access change — all live REST. Layer-2 sources manifest (harvester YAML) unchanged; adding repos/seeds = Layer-2 only.
- **Per-collection RBAC access** — `public` tier or `roles:[…]`; writes require a harvester/admin role. Planning must verify whether caller identity carries a role claim (memory ACL is tenant/team-based today).
- **Re-ingest is upsert-by-id** — index with `_id = <doc id>` (e.g. `arxiv_id`, `repo+path`) so re-delivery/edits overwrite in place. Idempotent, handles version updates, no dupes. Latest wins; no per-doc content history (see Versioning).
- **Two-layer config, not one file** (generalizes the "startup-config registration" call): engram owns a thin **collection registry** (name → index → mapping → text_field), loaded at boot; the **sources manifest** (repos, crawl seeds, dumps) lives on the harvester/consumer side and feeds engram via `knowledge_ingest`. Keeps engram a search platform, not a crawler (see Two-layer config).

---

## Motivation

paper-grabber searches arXiv, Semantic Scholar (S2), and OpenAlex. The **arXiv public API is fragile under the discovery fan-out**: its rate limiter is per-process (`tools/lib/rate-limit.ts`), each `bun tools/search.ts --source arxiv` is a separate OS process, so parallel invocations burst → HTTP 429 → arXiv "poisons the window" and the whole run comes back arXiv-empty. arXiv is exactly the source with the freshest CS/LLM preprints, so losing it hurts most.

The durable fix is to **own the arXiv metadata locally** and search it offline, removing arXiv's live query API from the hot path entirely. Engram already runs the ideal engine for this — a local OpenSearch 3.1 cluster whose **BM25 full-text search is all this corpus needs** (its kNN vector path, BGE-M3 1024-dim, stays reserved for memory) — but only exposes it as a *memory* API. This project generalizes engram to serve a document corpus too.

## Goal

Add a knowledge/document collection type to engram: bulk-ingest a corpus of records (**no fact-extraction, no embeddings**), then serve **structured full-text search** over it (BM25 with field filters and sort). Prove it end-to-end with arXiv CS metadata (~904k papers, ~2 GB). The memory path is untouched *behaviorally* (no code/API/reconciliation change); transient resource contention during bulk ops is acceptable — see Out of scope.

## Corpus & fields

First corpus: **arXiv CS metadata only — no PDFs, no full text.** ~904,275 papers (verified: deduplicated union of all `cs.*` categories via the arXiv API, 2026-07-09).

Fields to store (all present in the free Kaggle arXiv metadata dump, so zero extra acquisition cost):

| Field | Role |
|---|---|
| `arxiv_id` | primary id |
| `title` | BM25 text |
| `abstract` | BM25 text |
| `authors` / `authors_parsed` | display (author filtering out of v1) |
| `categories` | filter (equality / term) |
| `published_date` / `update_date` / version history | recency filter + sort; first-submitted vs latest-revised; dedup |
| `doi` | **high value** — precise join key to S2/OpenAlex/Crossref for query-time enrichment (avoids fuzzy title-matching) |
| `journal-ref` | presence ≈ "was published somewhere" — cheap peer-review proxy |
| `comments` | often venue signal ("accepted at NeurIPS 2025") |
| `license` | only matters if full text is ever touched (not in scope) |

## Data acquisition ("downloading the files" = the metadata dumps)

- **One-time backfill:** the Kaggle arXiv metadata dump (~1.5 GB gzipped, all fields, CC0 metadata), filtered to CS rows, bulk-loaded into the papers index.
- **Incremental freshness (v1):** nightly OAI-PMH harvest (`oaipmh.arxiv.org`, moved from export.arxiv.org Mar 2025; `from=<yesterday>`, `set=cs`, resumption tokens) pulls only new papers in a handful of polite requests. Ships in v1 — a stalled run is surfaced via the collection's staleness signal (newest `harvested_at` / paper date on `knowledge_collections`).
- These metadata downloads are the only "files" in scope. **No PDF downloading at any point.**

## Engram extension — the first slice

Grounded in the current engram code (Go / gRPC / OpenSearch 3.1; MCP is a thin stdio↔gRPC bridge). The two things one would expect to fight are already easy:

- **Skipping fact-extraction is natural** — extraction/reconciliation live *only* in the async worker (`internal/worker/worker.go`), downstream of the outbox; the synchronous ingest path is a plain index append (`internal/server/server.go:78` → `internal/store/opensearch.go:121`). `internal/eval/seed/seed.go` shows the closest existing pattern — indexing documents straight into an index outside the extraction/reconciliation worker (it uses a per-doc `put` loop and *does* embed; the knowledge path will batch via `_bulk` and skip embedding). No bulk-write helper exists in the repo today, so `BulkIndex` is written from scratch.
- **Multi-index is native** — 5 indices already exist as named constants (`internal/store/templates.go` ~17-44); adding one more is additive.
- **The query builder is a reusable free function** — `internal/retrieval/opensearch.go:buildQuery` (:499, BM25 / kNN / hybrid+RRF) can be called directly. Caveats a planner must know: `filterClauses` is a **method on the memory-specific `*tierRetriever`** (:466), not a free helper, and `buildQuery` **never emits a `sort` block today** — so filter-by-arbitrary-field + sort is net-new work.

Slice (memory path untouched):
1. A lightweight **collection registry** + per-collection index template with a strict mapping (the fields above; text fields for BM25, **no `knn_vector` in v1**).
2. A `KnowledgeStore.BulkIndex` that writes text + fields via the OpenSearch `_bulk` API with **no embedding call and no worker involvement** — a plain, fast index path (written from scratch; no bulk helper exists today).
3. A dedicated knowledge retriever built directly on the `buildQuery` free function in **BM25 mode** — **not** the memory `MultiRetriever` (`internal/retrieval/opensearch.go:163`), which is hard-wired to two memory tiers + memory ACL and calls the embedder to build a kNN query vector (`opensearch.go:400`). The knowledge retriever skips the embedder entirely.
4. Expose `knowledge_ingest` / `knowledge_search` / `knowledge_collections` / `knowledge_delete` (sweep) / `knowledge_create_collection` / `knowledge_update_collection` (admin, runtime) as new gRPC RPCs + MCP tool cases (`internal/mcp/tools.go` switch ~:88 + the `Backend` interface in `internal/mcp/mcp.go:52`). Registry state lives in a `knowledge-collections` meta-index (add to the index constants in `internal/store/templates.go`).

### Known gaps to close (real work, not blockers)
- **No "collection" abstraction** anywhere — indices are compile-time constants, tenancy is row-level (`tenant_id`/`team_id` fields), no `collection`/`namespace`. **v1 builds a lightweight collection registry** (name → index → mapping → text field) so new document types (websites, notes) are added by registration, not code.
- **The public search `Query` type carries only `text`+`k`** (`internal/retrieval/retriever.go:16`) — structured search needs date-range, category-equality, and **sort** added to `Query`/`Filter`/`filterClauses`/`buildQuery` (which currently never emits a sort block). This is the core structured-search feature to build.
- gRPC/`Backend` contract is memory-shaped (`Ingest(eventID,text,source)`, `Search(query,k)`) — the doc API needs new messages/RPCs.

## Search requirements

The knowledge search must express, over a collection's index:
- Full-text **relevance** (BM25) over title + body, reusing the existing `buildQuery` BM25 path. (Semantic / kNN is out of v1 — see Retrieval Model.)
- **Filter** by category (equality) and date (range).
- **Sort** by recency.
- Return **exact fields** (`arxiv_id`, `doi`, dates) so the consumer can act on results.

## Retrieval model — FTS-first, embeddings deferred

v1 is **BM25 full-text only**. Rationale: fast, simple ingest (no embedding pass over ~904k abstracts), no dependency on the embed server for the knowledge path, and the existing `buildQuery` already emits a BM25-only body. Accepted tradeoff: **no semantic / paraphrase matching in v1** (a query must share terms with the text).

Embeddings are a **deferred, additive layer**, not a rewrite: because every doc is stored with its full text, a later backfill job can generate vectors and add a `knn_vector` field (or a parallel vector index) with **no re-harvest**. Chunking of long documents is deferred with it (BM25 indexes long text directly; chunking is only needed for embeddings).

## API (MCP tools + gRPC)

Sketch — `collection` is first-class from day one so the same API serves papers, websites, notes:

```jsonc
knowledge_ingest {
  collection: "arxiv",
  harvest_id: "2026-07-09T02:00Z#run",          // stamps every row for mark-and-sweep
  documents: [{
    id: "2401.12345",                           // upsert-by-id
    title: "…",
    text: "…abstract / page body…",             // BM25-indexed field
    source_version: "sha:… | dump:2026-07-08",  // provenance (commit SHA / dump date / crawl ts)
    fields: { categories:["cs.CL"], published_date:"2024-01-15", doi:"…", authors:[…] }
  }]
}   // → { indexed: N }   (server also stamps harvested_at)

knowledge_search {
  collection: "arxiv",
  query: "chain-of-thought code generation",    // BM25; filters/sort optional
  filters: [                                     // generic predicate list, registry-validated
    { field:"categories",     op:"term",  value:"cs.CL" },
    { field:"published_date", op:"range", value:{ gte:"2024-01-01" } }
  ],
  sort: [{ field:"published_date", order:"desc" }],   // optional; default = relevance
  k: 50
}   // → budget-packed [{ id, title, score, fields:{…} }] + overflow_path spill

knowledge_delete {                               // mark-and-sweep
  collection: "arxiv",
  source: "arxiv-oaipmh",
  older_than_harvest_id: "…"                     // delete-by-query: rows this source not seen in the latest run
}   // → { deleted: N }

knowledge_collections {}
// → [{ name, count, text_field, filterable_fields:[…], sortable_fields:[…],
//     newest_harvested_at, newest_doc_date }]    // last two = staleness signal

knowledge_create_collection {                    // admin RPC — runtime, no restart
  name: "readmes", text_field: "body", access: { roles:["eng"] },
  mappings: { repo:{ type:"keyword", filterable:true }, language:{ type:"keyword", filterable:true } }
}   // → { created: "knowledge-readmes-v1" }   (writes meta-index doc + creates index/alias live)

knowledge_update_collection {                    // admin RPC — add field / change access / bump version
  name: "readmes", add_fields: { stars:{ type:"integer", sortable:true } }
}   // → { updated: true }
```

## Query API — shaped for an LLM caller, lean on context

The caller of `knowledge_search` is an **LLM over MCP**, so the API is optimized for retrieval success + minimal context, not human-API elegance. That settles the "generic vs. curated filters" question in favor of generic — with affordances that keep the LLM from guessing blind:

- **Generic predicate filters, validated against the registry.** Filters are a list of `{ field, op (term|range|prefix), value }` plus `sort: [{ field, order }]`, resolved against the collection's registered mapping. `buildQuery` stays collection-agnostic — arXiv passes `categories`/`published_date`, `readmes` passes `repo`/`language`, no per-collection API code. The only per-collection knowledge lives in the registry (text field + which fields are filterable/sortable).
- **Filters are optional.** A strong BM25 query alone should return good results; the LLM is never forced to construct a filter. Default = pure relevance.
- **Discovery without static bloat.** `knowledge_collections` returns each collection's *filterable/sortable fields + text field*, so the LLM learns a collection's schema **on demand** instead of that catalog living permanently in the tool description (which every call would pay for). Lazy schema discovery, not fat static context.
- **Self-correcting errors.** An unknown/unfilterable `field` returns an error that **names the valid fields** rather than a silent empty result — the model repairs its query on the next turn instead of dead-ending.
- **Lean responses via the existing budget-packer.** `knowledge_search` reuses memory_search's response-shaping (`internal/mcp/budget.go` + `spill.go`): budget-pack hits to `ENGRAM_MCP_SEARCH_BUDGET_BYTES`, return `id`/`title`/`score`/snippet + key fields inline, and **spill the full result set to `overflow_path` on disk** rather than flooding the model's context with full abstracts/bodies. No new bloat mechanism — the one memory already ships.

## Access & auth — per-collection RBAC with a public tier

Memory retrieval is tenant-scoped + ACL fail-closed (`filterClauses`, `Filter.Identity`). Knowledge needs a *different* model — reference corpora aren't per-tenant — but "global for everything" is too blunt. v1: a **per-collection access policy declared in the Layer-1 registry**, best-of-both:

- `access: public` → any authenticated caller may read (arXiv, public READMEs/docs).
- `access: roles:[…]` → read restricted to callers holding a listed role (private/internal corpora).
- **Writes** (`knowledge_ingest` / `knowledge_delete`) always require a harvester/admin role, regardless of read policy.

Enforced at the knowledge RPC layer from the caller identity on the existing auth interceptor. **Planning dependency to verify:** engram's memory ACL is tenant/team-based, not role-based today — so whether the caller identity already carries a role claim the knowledge layer can read, or whether a minimal role claim must be added, is net-new work a planner must scope.

## Ranking & enrichment — decision: enrich at query time

arXiv metadata has **no citation counts, no influential-citation, no peer-review flag**, which paper-grabber's scoring formula (`citations·0.4 + influential·0.4 + recency·0.2`) needs. **Decision (v1):** keep the index **pure arXiv metadata + DOI** and enrich the shortlist at **query time** from S2/OpenAlex — simple, fast ingest, and matches today's pipeline. (Rejected for v1: enriching at ingest with citation data to make the index self-ranking — heavier harvest, defer.)

So: the local index does fast candidate **retrieval + recency**; impact **ranking** stays a query-time enrichment step on the selected shortlist.

## Consumer

paper-grabber becomes engram's first knowledge client:
- A harvester that backfills (Kaggle) and incrementally updates (OAI-PMH) the papers collection.
- Its search path gains a fast local arXiv source (via `knowledge_search`) that replaces the fragile live arXiv API for discovery, feeding the existing merge → enrich → categorize pipeline.

## Two-layer config — collection registry vs. sources manifest

Engram has **no config-file mechanism today** — `engram-server` is configured entirely by flags + env vars (`cmd/engram-server/main.go`). A collection/sources config is net-new either way. Per this doc's own stance (*"engram indexes/searches; consumers own the harvest"*), the concern splits in two:

**Layer 1 — collection registry (engram core).** The registry (`name → index → mapping → text_field → access policy`) is the runtime source of truth, held in a durable **OpenSearch `knowledge-collections` meta-index** — *not* a boot-loaded file. This is what makes engram **never restart** for collection changes: create/update are live REST operations. A YAML file is an optional **boot seed**, applied idempotently on startup for reproducibility, but the meta-index is authoritative. Registry is cached in-process and invalidated on write. GitHub repos, crawl seeds, and dump paths are **not** in here — engram never fetches.

**Never-restart — the whole point of Option A.** Every collection change is a runtime OpenSearch call, no process restart:
- **Add a collection type** → `knowledge_create_collection` admin RPC: writes the meta-index doc + creates the index/mapping/alias live.
- **Add a mapping field** → live `PUT mapping` via `knowledge_update_collection`.
- **Change a field type** → new versioned index (`-v2`) + reindex + atomic alias swap — runtime, still no restart.
- **Change access policy** → meta-index doc update, cache-invalidated.

**Layer 2 — sources manifest (harvester/consumer side).** A file, versioned in git, that declares where each collection's content comes from. A separate harvester reads it, runs the typed harvesters, and calls `knowledge_ingest`. A *source* is a typed harvester (`arxiv-kaggle`, `arxiv-oaipmh`, `github-repos`, `web-crawl`); adding a source type = adding a harvester, never touching engram. Multiple sources can feed one collection (Kaggle backfill + nightly OAI-PMH both fill `arxiv`); a collection name here must match a Layer-1 registration.

```yaml
# Layer 2: harvester-side sources manifest
collections:
  - name: arxiv           # must match an engram Layer-1 registration
    sources:
      - { type: arxiv-kaggle, path: arxiv-metadata.json.gz, filter: "cs.*" }   # backfill
      - { type: arxiv-oaipmh, set: cs, schedule: nightly }                      # incremental
  - name: readmes
    sources:
      - type: github-repos
        repos: [anthropics/claude-code, opensearch-project/OpenSearch]
        files: ["README.md", "docs/**/*.md"]     # one doc per file, id = repo+path
  - name: docs-sites
    sources:
      - { type: web-crawl, seeds: ["https://docs.astral.sh/uv/"], max_pages: 500 }
```

**Change-boundary (nothing forces an engram restart):**
- **Adding/removing repos, seeds, or refresh cadence within an existing collection → harvester-only.** The common case; pure Layer-2 churn.
- **Adding a new collection *type* → runtime `knowledge_create_collection` admin RPC.** No restart (meta-index write + live index create).
- **New *source type* (e.g. a new harvester) → harvester-side code only.** Never touches engram.

**Open:** where the Layer-2 manifest + harvester live — inside paper-grabber, or a dedicated small harvester tool that paper-grabber (and future consumers) share.

## Versioning

The memory path gets history for free (append-only + reconciliation stamps `invalid_at`); the knowledge path's upsert-by-id **discards history by default**, so versioning here is an explicit, per-axis choice:

| Axis | What changes | v1 choice |
|---|---|---|
| **Document content** | arXiv v1→v2, README edited, page rewritten | Upsert-by-id, **latest wins**; no per-doc content history. The *source* is the system of record (git keeps SHAs, arXiv keeps all versions); engram is a search index, not an archive — and near-dup versions pollute BM25. |
| **Provenance stamp** | which harvest produced a row | **Always stamp** `source_version` + `harvested_at` per doc: commit SHA (repos), dump date (Kaggle), crawl ts (web). Cheap, always on — unlocks staleness detection and incremental re-harvest. |
| **Index mapping/schema** | a field added/retyped | **Versioned index behind an alias**: `knowledge-arxiv` → `knowledge-arxiv-v1`. Mapping change = create `-v2`, reindex/re-harvest, swap alias atomically. Home: `internal/store/templates.go` template infra. |
| **Sources manifest** | repos/seeds added/dropped | It's a file → **git**; harvester records which manifest revision a run used (ties to the provenance stamp). |
| **Deletion / orphans** | README deleted from repo, docs page 404s, paper withdrawn | **Mark-and-sweep (ships v1).** Upsert-by-id handles *changed* docs but never *removed* ones, so a full harvest of a source stamps every live row with the run's `harvest_id`, then sweeps (delete-by-query) rows for that collection+source whose stamp predates the run. Incremental SHA-diff harvests are the exception: they carry explicit per-file delete events (git reports deletions between SHAs) instead of a full sweep. |
| **Extraction pipeline** | — | **N/A** — the knowledge path has no extraction, so no `-extractor-version` reconciliation like memory has. Stated so nobody reaches for the memory model here. |

Note: mutable + deletable is intrinsic to the knowledge path (upsert + sweep) — it is the *deletable* store, in contrast to the append-only memory path. `knowledge_ingest` needs a delete-by-query surface (per collection+source, by stamp) for the sweep; `KnowledgeStore` exposes it alongside `BulkIndex`.

**Payoff — incremental git harvest:** record the last-harvested commit SHA per repo; incremental harvest re-ingests only files changed since that SHA — the exact analog of OAI-PMH `from=<yesterday>`. Keeps the "config of GitHub repos" cheap to refresh instead of re-cloning nightly.

## Explicitly out of scope
- No PDF downloading, no full-text/LaTeX storage, no blob store.
- No PDF mirror (the ~1.1 TB+ arXiv PDF corpus is not touched).
- No change to engram's memory ingest/search/reconciliation **behavior** (code, API, and reconciliation semantics). This is a *behavioral* boundary only — it does **not** promise zero resource contention: transient memory latency/CPU/IO impact during bulk operations (the one-time backfill especially) is **acceptable**, so no hard isolation (separate cluster/node/tier) is required in v1. Politeness during backfill (bounded `_bulk` batches, lowered `refresh_interval`, off-peak run) is good hygiene, not a hard guarantee.
- No ingest-time citation enrichment (deferred).
- **No embeddings / vector / semantic search in v1** — FTS/BM25 only; vectors are a later additive backfill.
- **No document chunking in v1** (deferred with embeddings).

## Key decisions

| Decision | Choice | Why |
|---|---|---|
| First corpus | arXiv CS metadata only | The source that fails today; free, ~2 GB |
| Files to download | metadata dumps only | Search needs title/abstract/fields, not PDFs |
| Ranking data | enrich at query time | Simple ingest; matches existing pipeline |
| Retrieval | **FTS / BM25 only (v1)** | Fast, simple ingest; no embedding compute; semantic deferred |
| Embeddings | deferred, additive backfill | Docs carry their text — add vectors later with no re-harvest |
| Collection model | **generic registry in v1** | Store must hold other doc types (websites, notes) beyond papers |
| Config shape | **two layers** — Layer-1 registry (engram, **meta-index** source of truth + YAML boot seed) + Layer-2 sources manifest (harvester YAML) | Keeps engram a search platform, not a crawler; adding repos = Layer-2 only |
| Registry store | **OpenSearch meta-index, runtime create/update RPCs** | **Never restart** engram for any collection change (Option A) |
| Access model | **per-collection RBAC** — `public` tier or `roles:[…]`; writes need harvester/admin role | Reference corpora aren't per-tenant; public-by-default with role-gating when needed |
| Re-ingest | **upsert-by-id, latest wins** | Idempotent; handles version updates; no dupes |
| Doc history | **not kept** — source is system of record | Engram is an index; git/arXiv already hold versions; dedup pollutes BM25 |
| Provenance | **stamp `source_version` + `harvested_at`** per doc | Enables staleness detection + incremental re-harvest |
| Deletion | **mark-and-sweep (v1)** — stamp live rows, sweep stale by `harvest_id` | Upsert alone never removes orphaned/withdrawn docs |
| Index schema evolution | **versioned index behind an alias** | Reindex + atomic alias swap on mapping change |
| Filter/sort API | **generic predicate list, registry-validated** | LLM caller; keeps `buildQuery` collection-agnostic, no per-collection surgery |
| Response shaping | **reuse memory_search budget-packer + `overflow_path` spill** | Lean LLM context; no new mechanism (`internal/mcp/budget.go`) |
| Where the corpus lives | per-collection OpenSearch index in engram's local cluster | Reuse the running engine; isolate from memory indices |
| Extraction | skipped for knowledge ingest | Papers/docs are content, not facts to reconcile |
| Storage host | engram indexes/searches; consumers own the harvest | Engram is a search platform, not a blob store |

# Phase-0 Live-Cluster Spike Findings (DW-0.9)

**Cluster:** `opensearchproject/opensearch:3.1.0` (pinned — D14), single node via podman,
security plugin disabled, `-Xms512m -Xmx1g`. Verified `version.number = 3.1.0`,
Lucene 10.2.1.
**Spike code:** `internal/spike/` (`//go:build integration`; run with `make dev-cluster && make integration`).
**Run:** 2026-07-03, all three spikes PASS.

---

## Spike 1 — `op_type=create` 409 flow (`op_create_test.go`)

The write protocol's collision primitive (D11) and guarded close (D10) behave exactly as designed:

| Step | Call | Observed |
|------|------|----------|
| 1 | `PUT /{idx}/_create/{id}` (first) | `201`, `result=created`, `_seq_no=0`, `_primary_term=1` |
| 2 | Same `_create` again (simulated duplicate concurrent extraction) | `409`, `error.type=version_conflict_engine_exception` |
| 3 | `PUT /{idx}/_doc/{id}?if_seq_no=0&if_primary_term=1` (guarded close) | `200`, `_seq_no` advanced to 1 |
| 4 | Same guard replayed (now stale) | `409`, `version_conflict_engine_exception`; write not applied |

**Implications for Phase 2:**
- `Store` implementations must map `version_conflict_engine_exception` (HTTP 409) → `store.ErrConflict` for BOTH the create path and the guarded-update path — one sentinel, two protocol roles (duplicate ADD vs lost guarded close).
- Guard tokens come back on every write response (`_seq_no`/`_primary_term`), so the re-read after a conflict can reuse the GET that fetches current state.

**Bonus finding (from running the suite itself):** two concurrent `Apply` runs raced on
HEAD-then-PUT index creation; the loser got `resource_already_exists_exception` (HTTP 400,
not 409). `store.Apply` now treats that as "unchanged" (test:
`TestApplyToleratesConcurrentCreateRace`). Note for Phase 2: *index* creation collisions
surface as 400 `resource_already_exists_exception`, unlike *document* create collisions
(409 `version_conflict_engine_exception`).

## Spike 2 — RRF pipeline shape (`rrf_test.go`)

Hybrid query (one `match` clause + one `knn` clause) through search pipeline `engram-rrf`
(`score-ranker-processor`, `technique=rrf`, `rank_constant=60`) against an index cut from
the checked-in semantic template:

- Returns a **single fused ranked list**; scores strictly descending.
- Scores are **rank-based, not score-based**: top hit (top-ranked in both sub-queries)
  scored `0.032787 ≈ 1/61 + 1/61 = 2/(rank_constant+1)`. Observed list:
  `[both 0.032787, bm25-only 0.032002, knn-only 0.016129, neither 0.015625]` — each score
  decomposes into `Σ 1/(60+rank_i)` over the sub-queries the doc appears in.
- A doc matching **both** signals outranks any single-signal doc — the fusion property
  Phase 1's DW-1.3 non-inferiority gate relies on.
- Caveat for Phase 1: with small `k`, every doc gets *some* kNN rank (vectors always have
  finite distance), so "neither" docs still fuse in at the tail. Relevance cutoffs must
  come from `size`/`min_score` policy, not from assuming kNN excludes non-matches.
- The pipeline applies via `?search_pipeline=engram-rrf` at query time; no index-level
  default needed (keeps non-hybrid queries pipeline-free).

## Spike 3 — Filtered-kNN recall (`filtered_knn_test.go`)

Exact template vector config (1024-dim, Faiss HNSW `m=16`/`ef_construction=128`, **SQfp16
encoder**, `space_type=innerproduct`), 300 docs, filter as a `filter` clause **inside** the
`knn` query (OpenSearch efficient filtering), ground truth by local brute-force inner
product:

| Filter selectivity | Matching docs | recall@10 |
|--------------------|---------------|-----------|
| 40% ("common") | 120/300 | **1.00** |
| 8% ("uncommon") | 24/300 | **1.00** |
| 2% ("rare") | 6/300 | **1.00** (6/6) |

- **No recall collapse** at any selectivity; at 2% the engine returned every matching doc
  (OpenSearch switches to exact search when the filtered candidate set is small — exactly
  the behavior we want; no post-filter starvation observed).
- Filter leaked zero out-of-bucket docs.
- SQfp16 quantization cost no measurable recall at this corpus size; re-verify at S1 volume
  in the Phase-1 perf harness (DW-1.5) where quantization error can matter.

## Verdict

All three architecture-critical mechanisms (create-collision idempotency, rank-based RRF
fusion, filtered-kNN with efficient filtering + SQfp16) work as the plan assumes on the
pinned 3.1.0 cluster. No plan changes required.

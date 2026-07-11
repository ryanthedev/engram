# memory_search — breadth-first results + deliberate drill-down

**Summary:** Reshape `memory_search` so a **result** is just enough to decide *read / don't-read*, not the full record, then add a deliberate `memory_read(id)` drill-down for the full body — because a full read spends the caller's context tokens (and may be a heavy operation). Pairs with a **format-follows-function** wire decision: lean text for the results list, structured for the single-record drill.

**Date:** 2026-07-11
**Status:** confirmed (grill + cold-read verification passed 2026-07-11)

**Still open (decisions for planning):**
- **Always-breadth is decided as the default** (see Decided); the residual open question is narrower: whether to *also* offer an opt-in `detail=full` escape param on `memory_search` for the rare caller that knows up front it wants bodies. Lean: ship always-breadth first, add the param only if a real need appears.
- **Snippet computation locus** — in `projectFields` (changes the gRPC payload for all consumers) vs. the MCP presentation layer (keeps gRPC full-fidelity). Lean: MCP layer, so `memory_read` can reuse the full `text` already in the `Hit`.
- **Snippet rules** for episodic `text`: length (~200 chars?), lead vs. extractive-around-match, and newline/whitespace normalization (required to de-risk a line-oriented format).
- **Exact compact-line schema** for the results format: field order, delimiter, how the `id` is marked so drill-down can round-trip it unambiguously.
- Whether the **graph** tier ever needs a drill at all (its statement is already the whole record).

**Decided in this research:**
- **A result is not the payload.** The hit's job is the read/no-read decision. No **cost hint** on the hit (considered and dropped — relevance + gist is enough; bytes/op-weight add noise).
- **Two-phase, but only episodic is genuinely two-phase.** Semantic and graph hits already *are* the whole memory (a one-sentence `statement`); their only "more" is provenance/version history, which is the existing `Audit` RPC. Episodic is the sole tier where the result (snippet) and the record (full `text`) differ.
- **Drill address = the hit's `id` + its `source`.** The `_id` alone is ambiguous — episodic and semantic live in different indices — so `memory_read` must take `(id, source)` (both already on every hit) to route to the right getter. Semantic ids are version-pinned (content-addressed): the referenced doc is **immutable in content**, so a drill returns the same statement the result showed. One caveat (not "no staleness"): reconciliation can close a predecessor's validity interval **in place** (stamps `invalid_at` on the same doc), so a drilled semantic id may carry a now-closed interval even though its content is unchanged.
- **Format-follows-function.** (a) Un-nest the double-encoded `fields_json` (a JSON *string* today) into a real object — a free win regardless of everything else. (b) Render the **results list as a compact line format** (grug-style: address + tags + one-line snippet), token-lean and de-risked by short normalized snippets. (c) `memory_read` returns **structured** output (one record; fidelity > tokens). The *format* rendering is an **MCP-presentation-layer** change — the gRPC/protobuf `Hit` stays structured. **Note the boundary tension (open decision below):** episodic snippet *truncation* is a different matter — if done in `projectFields` it changes what crosses gRPC for all consumers; if the goal is presentation-only, truncation must live in the MCP layer and the full `text` must still cross gRPC (which also lets `memory_read` reuse it without a second fetch).
- **Sanctioned full-context escape hatch:** the search envelope's `hint` should point at engram's own `overflow_path` spill, so a caller that needs the full set never invents a private path (see Motivation).

---

## Motivation

An agent (herderp session, 2026-07-10) needed the full text of specific `memory_search` hits and, instead of using engram tooling, ran a `jq` probe over **Claude Code's private `tool-results/` cache** (`.../mcp-plugin_engram_engram-memory_search-*.txt`) — a fragile, undocumented, and stale path. The question "why did it bypass our beautiful toolset?" unwound into two distinct problems.

### Problem 1 (root cause of the incident) — a stale deploy, already fixed 2026-07-11

The response-slimming feature (branch `feature/memory-search-response-shaping`, merged 2026-07-09) strips embeddings + bookkeeping at the retrieval boundary and budget-packs hits. But the **deployed** engram server was a **stale binary predating that merge**. Live `memory_search` was returning, inside every hit's `fields_json`, the full raw document — including a **1024-float `text_embedding` / `fact_embedding` vector** plus `attempts`, `claim_lease_until`, `owner_agent_id`, `tenant_id`, `team_id`, `scope`, `processed_at`, `created_at`. A 3-hit query returned ~40 KB, mostly embedding floats drowning the actual `text`. *That* noise is why the LLM bailed to `jq` (it was extracting `.text` out of an embedding wall).

**Fixed during this research:** rebuilt the `engram-local` image from HEAD `8ed5258` and recreated only the `local-engramd-1` container (`--no-deps --force-recreate`; OpenSearch volume untouched). Verified: the same query now returns ~3 KB of pure signal — the projection allowlist is live (`episodic → {text, kind, occurred_at, event_id, source_ids}`; embeddings gone). Data intact (`engram-e2e-os`, 216 episodic / 881 semantic / 1454 graph entities).

> **Operational risk flagged (not part of this build):** the live memory backend is riding on the **e2e/local compose stack** (`deploy/local/docker-compose.yml`, `ENGRAM_ADDR=localhost:7071` → `engram-e2e-os` on :9201). `make e2e` / `make e2e-down` run `down -v` and **would wipe those 881 semantic memories** (see commit `8ed5258`). The persistent store should not be the ephemeral test stack.

### Problem 2 (the actual design work) — a search hit is the full read masquerading as a search hit

Even *with* slimming deployed and noise gone, the design is still "one-phase, content-inline." Episodic `text` bodies are fat (a single memory was ~1 KB) so a wide search either blows the byte budget (hits get omitted/spilled) or forces the caller to spend context reading bodies it didn't want. The caller had no blessed way to say "show me candidates cheaply, then let me pull the one I care about" — so it invented one (the `jq` cache probe). Grug's two-phase `grug-search → grug-read` is exactly that missing shape.

## The principle

**A search result exists to let the caller decide whether spending context on the full record is worth it.** A full read costs tokens, and reading a record may be a heavier operation than a fetch (e.g. a semantic `Audit` pulls the whole bi-temporal version history). So the result must convey **relevance** (score) and **aboutness/gist** (what it is) — enough to choose — and nothing more. The body is fetched only on a deliberate second call.

**When it pays (stated honestly):** breadth-first is a net token win only under *selectivity* — when the caller reads few of the many episodic hits it sees. If a caller almost always reads most bodies, the two-phase shape *adds* a round-trip instead of saving tokens. Semantic and graph carry no penalty (their result is already the whole memory; there is no drill). So the win is concentrated exactly where the bodies are fat and the read rate is low — episodic — which is also where the noise problem lived.

## Design

### Two-phase retrieval, scoped per tier

| Tier | Result (breadth) | Drill (`memory_read(id)`) | Net-new work |
|---|---|---|---|
| **episodic** | `id`, `source`, `score`, snippet of `text` (normalized, ~200 chars), `kind`, `occurred_at`, `event_id`, `source_ids` | the **untruncated `text`** — that is the only delta over the result | new episodic store getter (realtime GET by `_id`, mirrors `GetFact`) **+ the wire path below** |
| **semantic** | `id`, `source`, `score`, `statement`, `subject`/`predicate`/`object`, `valid_at` — *already the whole memory* | provenance + full version history (via `Audit`) | store read exists (`Audit`/`GetFact`); **still needs the wire path below** |
| **graph** | `id`, `source`, `score`, `statement`, `subject`/`predicate`/`object`, `hop` — *already whole* | (likely nothing) | none |

**Delivering `memory_read` is more than a store getter.** The MCP `Backend` seam (`internal/mcp/mcp.go:52`) exposes only `Ingest`/`Search`/`Status`, and there is no gRPC RPC for episodic get-by-id. The full net-new surface: a `Read(id, source)` gRPC RPC (episodic getter; reuse `Audit` for semantic) → an `engramclient` method → a `Backend.Read` seam method → an MCP tool registration. Only the *semantic store* read (`GetFact`/`Audit`) exists today; every layer above it is new for both tiers.

Addressing uses the `id` **and `source`** on every hit (the `_id` alone can't pick the index). Semantic ids are content-addressed (`FactDocID = sha256(content_key·valid_at)`, `internal/memory/ids.go:~27`; `ContentKey` at `:19`), so the drilled document's *content* is immutable — but see the in-place validity-interval caveat under Decided.

**Snippet computation locus is an open decision (see Format-follows-function):** truncating in `projectFields` (`internal/retrieval/opensearch.go:322`) is simplest but alters the gRPC payload for all consumers; truncating in the MCP layer keeps gRPC full-fidelity (and lets `memory_read` reuse the already-fetched full `text`). No OpenSearch highlighting is required either way.

### Correctness constraint — `memory_read` must not bypass the ACL barricade

`memory_search` is fail-closed: it authorizes every hit through `filterAuthorized`/`Enforcer` **before** projecting away the ACL fields (`internal/retrieval/opensearch.go:206–290`; the projection deliberately runs *after* auth because `recordFromHit` reads `tenant_id`/`team_id`/`scope`/`owner_agent_id`, `:289`). The semantic drill is already safe — gRPC `Audit` checks `s.ACL.CanRead` (`internal/server/server.go`). The **new episodic get-by-id getter is the risk**: a by-id read that skipped authorization would be an ACL bypass (read any episodic doc by guessing/holding an id). Requirement: the episodic getter must fetch the doc *with* its ACL fields, run the same fail-closed `CanRead`/`Enforcer` check, and only then project to the returned shape — never the reverse order.

### Format-follows-function

- **Un-nest `fields_json`** (string-of-JSON → real object). Free token win; kills the `\"` escaping tax; no downside. Do this regardless of the rest.
- **Results list → compact line format**, e.g.:
  ```
  episodic 0.03 2M4mTp8Br4HeSAzEWtT0  herderp/ghost-detheme  2026-07-10
    Preference/decision: the ghost command output was de-themed to plain language...
  ```
  Leanest option (~40–50% off today's escaped JSON). Viable specifically *because* the snippets are short and normalizable (single-line). Positional fields require a documented schema.
- **`memory_read` → structured JSON.** One record; correctness and completeness matter more than tokens.
- Scope: this is an **MCP-presentation-layer** concern (`internal/mcp`). The gRPC `Hit` and the store representations stay structured; only the string the tool hands the model changes. Note existing programmatic consumers (`engramclient`, tests) if the tool-output contract is versioned.

## Grounding (code as of `8ed5258`)

- Hit struct (4 fields: `id`/`score`/`source`/`fields_json`): `internal/mcp/mcp.go:32`; built `internal/server/server.go:144`; client map `internal/engramclient/client.go:142`.
- Projection allowlist wired into the live path: `internal/retrieval/opensearch.go:295` (call), `:306` (allowlist), `:322` (`projectFields`), `:528` (`_source` excludes embeddings).
- Budget-pack + spill: `internal/mcp/budget.go`, `internal/mcp/spill.go` (`overflow_path`, `hint`).
- IDs: semantic doc id `FactDocID = sha256(content_key·valid_at)` `internal/memory/ids.go:~27` (`ContentKey` `:19`); episodic server-assigned/random `internal/store/opensearch.go:116`; `event_id` is a client idempotency field, not the doc id (`internal/server/server.go:79`).
- Reconciliation never re-keys (UPDATE = new id + `Supersedes`, close predecessor in place): `internal/worker/worker.go:377`.
- **By-id read already exists for semantic, unexposed via MCP:** `store.GetFact` `internal/store/facts.go:26`; `AuditFact` `internal/store/audit.go:17`; gRPC `Server.Audit` `internal/server/server.go:165` (tenant fail-closed). No episodic get-by-id today (only `FindByEventID`, may return several, `internal/store/facts.go:222`).

## Out of scope / rejected

- **Cost hint on the result** — considered (byte size / `read_cost: cheap|heavy`) and dropped; relevance + gist suffice.
- **Grug-style mutable read-a-path with re-writable addresses** — engram stays append-only; drill uses immutable, version-pinned ids (a feature, not a limitation: you read exactly what you saw).
- **A new by-id retrieval that returns "current truth" for a superseded semantic id** — the drill returns the version the result showed; chasing the live head is a separate concern (the `Supersedes` chain / `Audit` already models it).

## What comes next

`/code-foundations:plan .code-foundations/research/2026-07-11-memory-search-results-drilldown.md` — decompose into phases (episodic snippet projection + a fail-closed by-id getter, `memory_read` MCP tool wiring `Audit`, compact-line renderer + `fields` un-nesting, `hint`→`overflow_path`), resolving the four open decisions above and honoring the ACL correctness constraint.

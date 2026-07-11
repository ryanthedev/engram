# Engram → Obsidian Vault Exporter

**What this is:** A one-way exporter that turns engram's graph + semantic memory into a folder of
Obsidian-compatible markdown notes, so anyone can *wander their own memory as a graph* in real
Obsidian.

**Date:** 2026-07-08 · **Status:** confirmed (grilled + cold-read verified against source)

**Still open:**
- **Riskiest assumption** — "for a lot of people to have their own" presumes an install base. Engram
  is pre-build and, in practice, single-instance today. This ships a distributable CLI, but the
  audience is aspirational until engram has users. Not a design blocker; a go/no-go framing risk.
- The vault is only interesting once extraction has populated the semantic + graph tiers. Current
  instance: **183 episodic, 0 semantic, 0 graph** — a vault built today would be empty of nodes.

---

## Purpose

Engram already *is* a Zettelkasten under the hood — the graph tier is deduped entities (nodes) joined
by predicate-typed edges (links), extracted automatically from ingested events. The value: let a
person open that graph in Obsidian and wander it. The links are auto-extracted, deduped, and denser
than any hand-authored vault.

The draw is **wandering the graph** — explicitly *not* preserving engram's richer structure. We
knowingly trade away typed edges and the bi-temporal time dimension (see Non-goals) because Obsidian
can't render them and the user doesn't need them for wandering.

## Actors

- **Runs it:** any engram user, against their own tenant-scoped memory, via a CLI.
- **Views it:** the same user, in real Obsidian (the exporter ships no UI of its own).

## Decisions (locked)

| # | Decision | Rationale |
|---|---|---|
| D1 | **Exporter, not a clone.** Output is `.md` files; real Obsidian is the viewer. | Days not months; the graph view + backlinks + aliases are ~free and cover most of the "wow." |
| D2 | **Note unit = one note per entity.** Facts render as wikilinked bullets in the body. | Clean, wander-able nodes with dense links. A note-per-fact yields thousands of confetti notes and a hairball. |
| D3 | **Read path = new server-side export endpoint; CLI is a thin HTTP client.** | Honors tenancy/ACL and matches every existing subcommand (`search`, `status`, `ingest`). Going straight to OpenSearch would bypass who-owns-what. |
| D4 | **Homonyms: filename by display name, disambiguate on collision; links piped `[[file\|Display]]`.** Filenames must be **deterministic across runs** (D6 clobbers): sanitize Obsidian/FS-illegal chars, and on collision append a short prefix of the entity `id` (a stable sha256 — same input → same suffix every run). | Engram keeps homonyms as distinct entities sharing a `name_key`. Obsidian filenames must be unique and links resolve by filename — piped links keep visible text clean, resolution exact; deterministic names keep links intact across re-exports. |
| D5 | **Drop expired: emit an edge only when the edge is live AND both endpoints are live-and-exported.** Live is tier-specific: entity `expired_at == nil` (`Entity.Live()`), edge `invalid_at == nil && expired_at == nil` (`Edge.Live()`, `graph.go:121`). Print a dropped-count summary. | Keeps the wander graph clean; no dangling links to phantom notes, no tombstone litter. The stricter edge test also drops the superseded-not-expired ("true then, false now") edges we explicitly don't want. |
| D6 | **Clobber-and-regenerate: the tool owns the output dir.** Refuse a dir with foreign files unless `--force`. | It's a snapshot, not a workspace. Engram is append-only (no write-back), so hand-edits can never flow home — don't fake a sync. |
| D7 | **Ship simple on literal-nodes: export every entity, document the litter.** | For any fact with a non-empty object, extraction upserts that object as an entity with no literal/entity classification (`internal/graph/stage.go:60`; empty-object retractions are skipped at `stage.go:57`) — so `"Ryan bornIn 1985"` makes a `1985.md`. A heuristic filter would silently drop real leaf entities; the fix belongs upstream in extraction. |
| D8 | **Episodic tier excluded.** No daily/journal notes. | `occurred_at` is a client-supplied real-world event time that **defaults to `time.Now()` when the client omits it** (`internal/server/server.go:78-81`). Current ingesting clients don't pass it, so every event carries the ingest day (verified: an event whose text says "Decision (2026-07-07)" carries `occurred_at: 2026-07-08`) — daily notes would collapse to one day. Decision holds for current data; **revisit if clients start populating `occurred_at`.** |

## Data mapping (engram → Obsidian)

| Engram source | Obsidian artifact |
|---|---|
| Graph **entity** (`name` + `aliases[]`, `mention_count`, provenance) | One note; filename from `name` (+ collision suffix, D4); `aliases:` in frontmatter; H1 = display name |
| Graph **edge** (`from`→`to`, `predicate`, `statement`) | A wikilinked bullet in the *from* note: `- <predicate> [[to-file\|To Name]]` — predicate survives as bullet text even though Obsidian's graph-view line is unlabeled |
| Entity `mention_count` + entity `valid_at`, `source_ids`, `owner_agent_id` | Note frontmatter properties (**note: `valid_at` is currently unreliable** — `worker.go:363` derives fact/graph `valid_at` from the event's `occurred_at`, which today defaults to ingest time, see D8) |

**The exporter reads the graph tiers only** — `engram-graph-entities-000001` and `engram-graph-edges-000001`. The semantic tier is **not** read: edges already carry the resolved `from`/`to` entity ids, `predicate`, and `statement`, and entities already carry name/aliases — so `engram-semantic-000001` adds nothing the graph doesn't already give the vault.

## Non-goals / knowingly lost

- **Typed relationships** — Obsidian links are untyped; the `predicate` survives only as bullet text, not in the graph view.
- **Bi-temporal truth** — "true then, false now" has no Obsidian representation; expired records are simply dropped (D5), not archived.
- **Freshness / write-back** — the vault is a point-in-time snapshot; re-run to refresh (D6).

## Known limitations (to document in the tool)

1. Literal objects (dates, numbers, one-off values) appear as entity notes (D7).
2. Graph-view relationship lines are unlabeled (predicate is only in note bodies).
3. Snapshot, not live — no sync, no write-back; re-export to refresh.
4. Frontmatter `valid_at` is only as good as `occurred_at` — currently ingest-time for clients that
   don't populate it (D8), so treat time metadata as unreliable until upstream ingestion improves.

## Build constraints (feasibility)

- **No enumerate/scan/dump API exists.** Every read path is a *targeted* query. The export endpoint
  must add paginated full-index iteration (OpenSearch `search_after`/PIT) over
  `engram-graph-entities-000001` and `engram-graph-edges-000001` (the semantic index is not read — see data mapping).
- **Filter to live records, per tier** — entities `expired_at == nil` (`Entity.Live()`); edges
  `invalid_at == nil && expired_at == nil` (`Edge.Live()`). These are *not* the same test (D5).
- **Tenant-scoped** — the endpoint reads only the caller's tenant/scope (D3 depends on this).
- **The graph is re-derivable** from semantic triples via `GraphStage`; the exporter reads the
  already-landed graph indices, it does not re-derive.

## What comes next

Take this into planning:
```
/code-foundations:plan .code-foundations/research/2026-07-08-obsidian-vault-exporter.md
```

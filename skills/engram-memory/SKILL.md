---
name: engram-memory
description: >-
  Cross-session memory via Engram (memory_search, memory_read,
  memory_ingest). Recall at the start of non-trivial work and when the user
  says "remember", "like last time", "what did we decide", "as I mentioned",
  "my usual", or asks what past sessions settled. Ingest preferences,
  decisions, corrections, outcomes. Not for facts in this conversation,
  derivable from the repo, or secrets.
---

# Engram memory

Search before you assume; ingest after you learn.

## Recall: `memory_search`

Search at the start of non-trivial work, on any back-reference, and before
asking the user to repeat something. Phrase `query` in natural language.
Results are text rows, best first (no scores):

```
memory (2)
semantic:3f9a1c07be42	2026-10-02	Ryan prefers Conventional Commits.
episodic:_BZ7ZqEB43TC-uT2hEr0	2026-10-22	Token-cost baseline, measured …
```

- A semantic row is the whole fact; an episodic row is the event's first 200
  characters. For either's full record, pass the first column as-is to
  `memory_read` (`id: "semantic:3f9a1c07be42"`); it returns JSON. Each read
  costs context: read only when the row is not enough.
- Keep `k` at 5–10 (default 10). Output caps near 6KB and then ends
  `... N more omitted for size; narrow the query`. A bigger `k` cannot recover
  those; narrow the query or filter (`since`, `until`, `sources`, `subject`).
- `expand: true` adds a `graph (N, …)` block: statements linked to the hits,
  not matches, with no ids and no `memory_read`. Use it for surrounding
  context. Any filter drops graph results.
- When hits conflict, the later date usually wins; tell the user. If nothing
  relevant returns, say so.

`knowledge_search` rows (curated collections) drill with `memory_read` too.

## Remember: `memory_ingest`

Ingest what a future session needs and cannot get from code or git:
preferences, conventions, decisions and why, corrections, outcomes, and
anything the user asks you to remember. Not secrets, not what is already in
this conversation or the repo, not task progress.

- `text` must stand alone and stay short: name the subject ("Ryan prefers…"),
  point first. You pay for it in output tokens now, and its first 200
  characters in every search it matches.
- `event_id` is the idempotency key; each id is extracted once. A repeat under
  the same id never becomes new facts, so reuse an id only to retry. An update
  or correction gets a new id (`pref-commit-style-2`).
- Extraction is async: a new fact can take seconds to become searchable.
- `scope`: leave unset (private to this agent) for anything about a project,
  employer, codebase, person, or account, and when unsure; work and personal
  machines must not leak into each other. `org` reaches every agent of this
  user on every machine: only portable know-how (tool behavior, editor/shell
  config, idioms, general preferences). It needs an org grant; if rejected,
  write private and tell the user. `team` (with `team`) only when the user
  names one.

## When calls fail

`memory_status` shows health, whose memory this is, and per-tier counts. If
every call is `Unauthenticated`, check `ENGRAM_ADDR` before the token: unset,
it defaults to `localhost:7070`, but the local stack is `localhost:7071`, and a
token works only against the store it was minted on. Connection refused means
engramd is not running at `ENGRAM_ADDR`. Either way, tell the user you are
working without memory.

---
name: engram-memory
description: >-
  Durable cross-session memory via the Engram MCP server (memory_ingest /
  memory_search / memory_status). Use this whenever the user shares something
  worth remembering across sessions — a preference, a decision, a fact about
  them or their project, a correction, a convention, an outcome — OR whenever
  the current task depends on something that might have been established
  earlier: the user says "remember", "like last time", "what did we decide",
  "you already know", "as I mentioned", "my usual", or asks a question whose
  answer lives in past context rather than in this conversation or the repo.
  Also recall proactively at the start of a non-trivial task so you build on
  prior context instead of starting cold. Prefer this over guessing from
  memory or asking the user to repeat themselves. Do NOT use it for facts that
  are already in the current conversation, derivable from the codebase, or for
  secrets/tokens.
---

# Engram memory

Engram is a persistent, cross-session memory store. Unlike the conversation
window (which vanishes when the session ends) or the codebase (which holds
*what the code is*, not *what was decided or preferred*), Engram remembers
durable facts across every session for this identity. It's reached through
three MCP tools:

| Tool | Purpose | Key args | Returns |
|------|---------|----------|---------|
| `memory_search` | Recall — hybrid BM25+vector search over memory | `query` (req), `k` | `{ hits: [{ id, score, source, fields_json }] }` |
| `memory_ingest` | Remember — append one durable event | `event_id` (req), `text` (req), `source` | `{ id }` |
| `memory_status` | Health + identity + per-tier counts | — | `{ healthy, tenant_id, user_id, agent_id, episodic_count, semantic_count, opensearch_version }` |

The core discipline is simple: **search before you assume, ingest after you
learn.** The two failure modes this skill exists to prevent are (1) asking the
user something they already told you in a past session, and (2) letting a
durable fact evaporate when the session ends.

## When to recall (`memory_search`)

Search whenever the answer might live in past context rather than in front of
you. You don't need the user to say "search your memory" — infer it:

- **At the start of a non-trivial task.** One quick search on the task's
  subject ("auth refactor decisions", "how the user likes commit messages")
  lets you build on prior work instead of relitigating it. Cheap insurance.
- **Explicit back-references** — "like last time", "what did we decide about
  X", "you already know my setup", "as I mentioned before", "my usual deploy
  flow". These are unanswerable from the current window by definition, so they
  are a direct signal to search.
- **Before asking the user to repeat themselves.** If you're about to ask
  "what's your preferred X?", search first — they may have told you already.

Phrase the `query` in natural language describing what you're looking for, not
keywords. `k` defaults to a server-chosen value; raise it (e.g. `k: 10`) when
you want broader recall, lower it when you want only the top hit.

**Reading hits:** each hit's `fields_json` is a JSON string holding the stored
record (for semantic facts, typically `subject` / `predicate` / `object` /
`statement` plus bi-temporal validity). `score` is the fused relevance rank
(higher = better). Treat a hit as *evidence*, not gospel — if two hits
conflict, the one with the later validity usually wins, but surface the
conflict to the user rather than silently picking. If a search returns nothing
relevant, say so plainly instead of inventing an answer.

## When to remember (`memory_ingest`)

Ingest when something surfaces that a *future* session would benefit from and
that isn't already recoverable from the code or git history. Good candidates:

- **Preferences & conventions** — "I always want X", "we use Y here", "don't
  do Z". These recur across sessions and are painful to rediscover.
- **Decisions & their rationale** — "we chose Postgres over Dynamo because…".
  The *why* is the part that evaporates; capture it.
- **Corrections** — when the user corrects you, that correction is a durable
  fact ("the token lives in .zshrc.local, not .env"). Ingest it so you don't
  repeat the mistake next week.
- **Outcomes** — "the migration worked", "that approach failed because…".
- **Explicit asks** — any time the user says "remember that…", ingest it
  verbatim-in-substance.

Write the `text` as a **self-contained statement** that will still make sense
with zero surrounding context — the future reader has none. Prefer
"Ryan prefers commit messages in Conventional Commits format" over
"prefers that format". A subject | predicate | object shape
(`alice | prefers | dark-mode`) extracts cleanly, but plain prose is fine too;
the server does the extraction.

`event_id` is an **idempotency key** — the same id will not create a duplicate.
Derive a stable, descriptive id from the content (e.g. `pref-commit-style`,
`decision-db-postgres`) so a re-ingest updates rather than duplicates. Only use
a fresh unique id when you genuinely mean "this is a new, distinct event".

`source` is optional provenance — set it to where the fact came from
(`chat`, a filename, a PR) when that lineage would help a future reader trust
or trace it.

Ingestion is **asynchronous**: extraction and reconciliation happen after the
call returns the durable id, so a fact you just ingested may take a few seconds
to become searchable. Don't ingest-then-immediately-search expecting a hit.

## What NOT to store

Storing the wrong things is worse than storing nothing — it clutters recall and
can leak. Skip:

- **Secrets** — tokens, passwords, API keys, private keys. Engram is memory,
  not a vault.
- **Anything already in the current conversation** — no need to echo it back
  into memory mid-session.
- **Facts derivable from the codebase or git** — file paths, function
  signatures, who changed what. Search the repo/`git log` instead; that's
  ground truth and never goes stale.
- **Ephemeral task state** — "currently on step 3". That belongs in the task
  list, not durable memory.

## Health & troubleshooting

If a memory call errors or you're unsure the store is reachable, call
`memory_status`. `healthy: true` confirms the server is up; the returned
`tenant_id` / `user_id` / `agent_id` tell you *whose* memory you're reading and
writing (memory is scoped to this identity — you won't see another agent's
private memories). The `*_count` fields tell you whether anything has been
stored yet.

Common failures and what they mean:

- **Every call rejected / auth error** — `ENGRAM_TOKEN` is missing or invalid.
  The token binds the session to an identity; without it every gRPC call is
  refused. It's set in the environment (e.g. `~/.zshrc.local`).
- **Connection refused / status unreachable** — `engramd` isn't running at
  `ENGRAM_ADDR` (default `localhost:7071`). Bring the local stack up with
  `make e2e-up` from the repo root, or point `ENGRAM_ADDR` at a live instance.

Surface these to the user honestly rather than silently degrading — if memory
is down, say "Engram is unreachable, working without memory this session" so
they know recall/persistence is off.
